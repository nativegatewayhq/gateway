// Package anthropic implements the Anthropic native Messages facade.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/nativegatewayhq/gateway/internal/apikey"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	"github.com/nativegatewayhq/gateway/internal/providerhealth"
	"github.com/nativegatewayhq/gateway/internal/requestid"
	"github.com/nativegatewayhq/gateway/internal/telemetry"
	operation "github.com/nativegatewayhq/gateway/operations/anthropic"
	provider "github.com/nativegatewayhq/gateway/providers/anthropic"
)

type Authenticator interface {
	Authenticate(context.Context, string) (apikey.Principal, error)
}
type Registry interface {
	Resolve(string) (operation.Model, error)
}
type Executor interface {
	CreateMessage(context.Context, provider.MessagesRequest) (*http.Response, error)
}
type Availability interface {
	ConfiguredChannel(context.Context, string, providercredentials.ProviderID) bool
}

type Handler struct {
	logger          *slog.Logger
	auth            Authenticator
	models          Registry
	executor        Executor
	availability    Availability
	health          providerhealth.Gate
	maximum         int64
	billingRequired bool
	telemetry       *telemetry.Recorder
}

func NewHandler(logger *slog.Logger, auth Authenticator, models Registry, executor Executor, availability Availability, health providerhealth.Gate, maximum int64, billingRequired bool) *Handler {
	if health == nil {
		health = providerhealth.NoopGate{}
	}
	return &Handler{logger: logger, auth: auth, models: models, executor: executor, availability: availability, health: health, maximum: maximum, billingRequired: billingRequired}
}
func (h *Handler) SetTelemetry(value *telemetry.Recorder) { h.telemetry = value }

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	status := &statusWriter{ResponseWriter: w}
	defer func() {
		h.logger.Info("anthropic messages request completed", "request_id", requestid.FromContext(r.Context()), "protocol", "anthropic", "operation", operation.CreateMessage, "status", status.code(), "duration", time.Since(started))
	}()
	if r.Method != http.MethodPost {
		status.Header().Set("Allow", http.MethodPost)
		writeError(status, 405, "invalid_request_error", "method not allowed")
		return
	}
	principal, ok := h.authenticate(status, r)
	if !ok {
		return
	}
	// Token settlement is deliberately not part of this foundation. Never dispatch
	// an unreserved paid request or touch its body/provider credential.
	if h.billingRequired {
		writeError(status, 503, "api_error", "Anthropic billing is not configured")
		return
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(media, "application/json") {
		writeError(status, 400, "invalid_request_error", "content type must be application/json")
		return
	}
	version, beta, valid := protocolHeaders(r.Header)
	if !valid {
		writeError(status, 400, "invalid_request_error", "invalid Anthropic protocol headers")
		return
	}
	if h.maximum < 1 || r.ContentLength > h.maximum {
		writeError(status, 413, "request_too_large", "request body too large")
		return
	}
	body, err := readBounded(r.Body, h.maximum)
	if err != nil {
		writeError(status, 413, "request_too_large", "request body too large")
		return
	}
	model, stream, err := envelope(body)
	if err != nil || stream {
		writeError(status, 400, "invalid_request_error", "request must contain one model and stream must be false")
		return
	}
	route, err := h.models.Resolve(model)
	if errors.Is(err, operation.ErrModelNotFound) {
		writeError(status, 404, "not_found_error", "model not found")
		return
	}
	if err != nil || h.executor == nil {
		writeError(status, 503, "api_error", "provider unavailable")
		return
	}
	if !principal.AuthorizeModel("anthropic", operation.CreateMessage, model) {
		writeError(status, 403, "permission_error", "API key is not permitted to use this model")
		return
	}
	if h.availability == nil || !h.availability.ConfiguredChannel(r.Context(), route.ChannelID, route.Provider) {
		writeError(status, 503, "api_error", "provider unavailable")
		return
	}
	permit, allowed := h.claim(r, route.ChannelID)
	if !allowed {
		writeError(status, 503, "api_error", "provider unavailable")
		return
	}
	response, executeErr := h.executor.CreateMessage(r.Context(), provider.MessagesRequest{ChannelID: route.ChannelID, ContentType: r.Header.Get("Content-Type"), Accept: r.Header.Get("Accept"), UserAgent: r.UserAgent(), Version: version, Beta: beta, ContentLength: int64(len(body)), Body: bytes.NewReader(body)})
	h.observe(r, route.ChannelID, permit, response, executeErr)
	if executeErr != nil {
		switch {
		case errors.Is(executeErr, provider.ErrTimeout):
			writeError(status, 504, "api_error", "provider request timed out")
		case errors.Is(executeErr, provider.ErrCanceled):
			writeError(status, 499, "api_error", "request canceled")
		case errors.Is(executeErr, providercredentials.ErrCredentialUnavailable):
			writeError(status, 503, "api_error", "provider unavailable")
		default:
			writeError(status, 502, "api_error", "provider unavailable")
		}
		return
	}
	defer response.Body.Close()
	responseBody, err := readBounded(response.Body, h.maximum)
	if err != nil {
		writeError(status, 502, "api_error", "provider response exceeded the configured limit")
		return
	}
	for _, name := range []string{"Content-Type", "Retry-After", "Request-Id"} {
		for _, value := range response.Header.Values(name) {
			status.Header().Add(name, value)
		}
	}
	status.WriteHeader(response.StatusCode)
	_, _ = status.Write(responseBody)
}

func (h *Handler) authenticate(w http.ResponseWriter, r *http.Request) (apikey.Principal, bool) {
	if h.auth == nil {
		writeError(w, 503, "api_error", "authentication service unavailable")
		return apikey.Principal{}, false
	}
	raw, err := apikey.Extract(r)
	if err != nil {
		if errors.Is(err, apikey.ErrAmbiguous) {
			writeError(w, 400, "invalid_request_error", "multiple credential locations are not allowed")
		} else {
			writeError(w, 401, "authentication_error", "authentication required")
		}
		return apikey.Principal{}, false
	}
	principal, err := h.auth.Authenticate(r.Context(), raw)
	if err != nil {
		if errors.Is(err, apikey.ErrUnavailable) {
			writeError(w, 503, "api_error", "authentication service unavailable")
		} else {
			writeError(w, 401, "authentication_error", "authentication required")
		}
		return apikey.Principal{}, false
	}
	return principal, true
}

func protocolHeaders(header http.Header) (string, []string, bool) {
	versions := header.Values("anthropic-version")
	if len(versions) != 1 || !safeHeader(versions[0], 100) {
		return "", nil, false
	}
	betas := header.Values("anthropic-beta")
	total := 0
	if len(betas) > 16 {
		return "", nil, false
	}
	for _, value := range betas {
		total += len(value)
		if !safeHeader(value, 512) || total > 4096 {
			return "", nil, false
		}
	}
	return versions[0], betas, true
}
func safeHeader(value string, maximum int) bool {
	if value == "" || len(value) > maximum {
		return false
	}
	for _, c := range value {
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}
func envelope(body []byte) (string, bool, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return "", false, errors.New("invalid")
	}
	model, seenModel, stream := "", false, false
	seenStream := false
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return "", false, err
		}
		key := keyToken.(string)
		if key == "model" {
			if seenModel {
				return "", false, errors.New("duplicate model")
			}
			seenModel = true
			if err := decoder.Decode(&model); err != nil || model == "" {
				return "", false, errors.New("invalid model")
			}
		} else if key == "stream" {
			if seenStream {
				return "", false, errors.New("duplicate stream")
			}
			seenStream = true
			if err := decoder.Decode(&stream); err != nil {
				return "", false, err
			}
		} else {
			var discard any
			if err := decoder.Decode(&discard); err != nil {
				return "", false, err
			}
		}
	}
	if _, err := decoder.Token(); err != nil || !seenModel {
		return "", false, errors.New("invalid")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return "", false, errors.New("trailing")
	}
	return model, stream, nil
}
func readBounded(reader io.Reader, maximum int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil || int64(len(body)) > maximum {
		return nil, errors.New("too large")
	}
	return body, nil
}
func writeError(w http.ResponseWriter, status int, kind, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]string{"type": kind, "message": message}})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
		w.ResponseWriter.WriteHeader(code)
	}
}
func (w *statusWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	return w.ResponseWriter.Write(body)
}
func (w *statusWriter) code() int {
	if w.status == 0 {
		return 200
	}
	return w.status
}
func (h *Handler) claim(r *http.Request, channel string) (providerhealth.Permit, bool) {
	snapshot, err := h.health.Inspect(r.Context(), channel)
	if err != nil || snapshot.State == providerhealth.Open {
		return providerhealth.Permit{}, false
	}
	permit, err := h.health.ClaimProbe(r.Context(), channel, requestid.FromContext(r.Context()))
	return permit, err == nil
}
func (h *Handler) observe(r *http.Request, channel string, permit providerhealth.Permit, response *http.Response, err error) {
	outcome := providerhealth.Neutral
	if err != nil {
		if errors.Is(err, providercredentials.ErrCredentialUnavailable) || errors.Is(err, provider.ErrCanceled) {
			if permit.Probe {
				_ = h.health.Release(context.WithoutCancel(r.Context()), permit)
			}
			return
		}
		if errors.Is(err, provider.ErrTimeout) {
			outcome = providerhealth.Timeout
		} else {
			outcome = providerhealth.Connection
		}
	} else if response != nil {
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			outcome = providerhealth.Success
		} else if response.StatusCode == 429 {
			outcome = providerhealth.RateLimited
		} else if response.StatusCode >= 500 {
			outcome = providerhealth.ServerError
		}
	}
	_, _ = h.health.Observe(context.WithoutCancel(r.Context()), providerhealth.Observation{ChannelID: channel, ObservationID: requestid.FromContext(r.Context()), Outcome: outcome, Permit: permit})
}
