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
	"github.com/nativegatewayhq/gateway/internal/billing"
	"github.com/nativegatewayhq/gateway/internal/chatbilling"
	"github.com/nativegatewayhq/gateway/internal/chatpricing"
	"github.com/nativegatewayhq/gateway/internal/idempotency"
	"github.com/nativegatewayhq/gateway/internal/ledger"
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
type Billing interface {
	Begin(context.Context, chatbilling.BeginRequest) (chatbilling.Charge, error)
	Replay(context.Context, chatbilling.BeginRequest) (chatbilling.Charge, bool, error)
	CompleteUsage(context.Context, string, chatpricing.Usage, billing.ResponseSnapshot) (chatbilling.Charge, error)
	Release(context.Context, string, billing.ResponseSnapshot) (chatbilling.Charge, error)
	MarkReconciling(context.Context, string, string, *billing.ResponseSnapshot) error
	MarkReconcilingUsage(context.Context, string, string, *billing.ResponseSnapshot, chatpricing.Usage) error
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
	billing         Billing
	telemetry       *telemetry.Recorder
}

func NewBillableHandler(logger *slog.Logger, auth Authenticator, models Registry, executor Executor, availability Availability, health providerhealth.Gate, maximum int64, chargeBilling Billing) *Handler {
	handler := NewHandler(logger, auth, models, executor, availability, health, maximum, true)
	handler.billing = chargeBilling
	return handler
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
	if h.billingRequired && h.billing == nil {
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
	if h.billing != nil {
		h.serveBillable(status, r, principal, route, version, beta, body)
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

func (h *Handler) serveBillable(w http.ResponseWriter, r *http.Request, principal apikey.Principal, route operation.Model, version string, beta []string, body []byte) {
	maximumOutput, err := extractMaximumOutput(body)
	if err != nil || route.MaximumInputTokens < 1 || route.MaximumOutputTokens < 1 || int64(len(body)) > route.MaximumInputTokens || maximumOutput > route.MaximumOutputTokens {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "paid Messages requires valid input and output token limits")
		return
	}
	if h.availability == nil || !h.availability.ConfiguredChannel(r.Context(), route.ChannelID, route.Provider) {
		writeError(w, 503, "api_error", "provider unavailable")
		return
	}
	key, keyErr := idempotency.Extract(r.Header)
	if keyErr != nil {
		writeError(w, 400, "invalid_request_error", "idempotency key is invalid")
		return
	}
	var fingerprint [32]byte
	if key != "" {
		fingerprint = idempotency.Fingerprint("anthropic", operation.CreateMessage, route.ID, route.ChannelID, "application/json", body)
	}
	begin := chatbilling.BeginRequest{Protocol: "anthropic", Operation: operation.CreateMessage, RequestID: requestid.FromContext(r.Context()), OrganizationID: principal.OrganizationID, ProjectID: principal.ProjectID, APIKeyID: principal.APIKeyID, Model: route.ID, ChannelID: route.ChannelID, IdempotencyKey: key, Fingerprint: fingerprint, MaximumInputTokens: int64(len(body)), MaximumOutputTokens: maximumOutput}
	if key != "" {
		replayed, found, replayErr := h.billing.Replay(r.Context(), begin)
		if replayErr != nil {
			h.writeBillingError(w, replayErr)
			return
		}
		if found {
			h.writeSnapshot(w, replayed.Response)
			return
		}
	}
	charge, err := h.billing.Begin(r.Context(), begin)
	if err != nil {
		h.writeBillingError(w, err)
		return
	}
	if charge.Replay {
		h.writeSnapshot(w, charge.Response)
		return
	}
	permit, allowed := h.claim(r, route.ChannelID)
	if !allowed {
		snapshot := gatewayErrorSnapshot(503, "api_error", "provider unavailable")
		settled, releaseErr := h.billing.Release(context.WithoutCancel(r.Context()), charge.ID, snapshot)
		if releaseErr != nil {
			_ = h.billing.MarkReconciling(context.WithoutCancel(r.Context()), charge.ID, "settlement_failed", &snapshot)
			writeError(w, 503, "api_error", "settlement unavailable")
			return
		}
		h.writeSnapshot(w, settled.Response)
		return
	}
	response, executeErr := h.executor.CreateMessage(r.Context(), provider.MessagesRequest{ChannelID: route.ChannelID, ContentType: r.Header.Get("Content-Type"), Accept: r.Header.Get("Accept"), UserAgent: r.UserAgent(), Version: version, Beta: beta, ContentLength: int64(len(body)), Body: bytes.NewReader(body)})
	h.observe(r, route.ChannelID, permit, response, executeErr)
	if executeErr != nil {
		if errors.Is(executeErr, providercredentials.ErrCredentialUnavailable) {
			snapshot := gatewayErrorSnapshot(503, "api_error", "provider unavailable")
			settled, releaseErr := h.billing.Release(context.WithoutCancel(r.Context()), charge.ID, snapshot)
			if releaseErr == nil {
				h.writeSnapshot(w, settled.Response)
				return
			}
			_ = h.billing.MarkReconciling(context.WithoutCancel(r.Context()), charge.ID, "settlement_failed", &snapshot)
			writeError(w, 503, "api_error", "settlement unavailable")
			return
		}
		reason := "executor_connection_lost"
		if errors.Is(executeErr, provider.ErrTimeout) {
			reason = "executor_timeout"
		}
		_ = h.billing.MarkReconciling(context.WithoutCancel(r.Context()), charge.ID, reason, nil)
		if errors.Is(executeErr, provider.ErrTimeout) {
			writeError(w, 504, "api_error", "provider request timed out")
		} else {
			writeError(w, 502, "api_error", "provider unavailable")
		}
		return
	}
	defer response.Body.Close()
	responseBody, readErr := readBounded(response.Body, h.maximum)
	if readErr != nil {
		_ = h.billing.MarkReconciling(context.WithoutCancel(r.Context()), charge.ID, "response_unavailable", nil)
		writeError(w, 502, "api_error", "provider response exceeded the configured limit")
		return
	}
	snapshot := billing.ResponseSnapshot{Status: response.StatusCode, Headers: safeResponseHeaders(response.Header), Body: responseBody}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		settled, settleErr := h.billing.Release(context.WithoutCancel(r.Context()), charge.ID, snapshot)
		if settleErr != nil {
			_ = h.billing.MarkReconciling(context.WithoutCancel(r.Context()), charge.ID, "settlement_failed", &snapshot)
			writeError(w, 503, "api_error", "settlement unavailable")
			return
		}
		h.writeSnapshot(w, settled.Response)
		return
	}
	usage, usageErr := extractUsage(responseBody)
	if usageErr != nil || usage.PromptTokens > charge.MaximumInputTokens || usage.CompletionTokens > charge.MaximumOutputTokens {
		_ = h.billing.MarkReconciling(context.WithoutCancel(r.Context()), charge.ID, "usage_invalid", &snapshot)
		writeError(w, 503, "api_error", "usage settlement unavailable")
		return
	}
	settled, settleErr := h.billing.CompleteUsage(context.WithoutCancel(r.Context()), charge.ID, usage, snapshot)
	if settleErr != nil {
		_ = h.billing.MarkReconcilingUsage(context.WithoutCancel(r.Context()), charge.ID, "settlement_failed", &snapshot, usage)
		writeError(w, 503, "api_error", "settlement unavailable")
		return
	}
	h.writeSnapshot(w, settled.Response)
}

func (h *Handler) writeBillingError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, chatbilling.ErrConflict), errors.Is(err, chatbilling.ErrPending):
		writeError(w, 409, "invalid_request_error", "idempotency conflict or request pending")
	case errors.Is(err, ledger.ErrInsufficientFunds):
		writeError(w, 402, "billing_error", "insufficient funds")
	case errors.Is(err, chatpricing.ErrUnavailable), errors.Is(err, chatpricing.ErrMargin):
		writeError(w, 503, "api_error", "price unavailable")
	default:
		writeError(w, 503, "api_error", "billing unavailable")
	}
}
func (h *Handler) writeSnapshot(w http.ResponseWriter, snapshot billing.ResponseSnapshot) {
	for name, values := range snapshot.Headers {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	w.WriteHeader(snapshot.Status)
	_, _ = w.Write(snapshot.Body)
}
func safeResponseHeaders(header http.Header) map[string][]string {
	result := map[string][]string{}
	for _, name := range []string{"Content-Type", "Retry-After", "Request-Id"} {
		if values := header.Values(name); len(values) > 0 {
			result[name] = append([]string(nil), values...)
		}
	}
	return result
}
func gatewayErrorSnapshot(status int, kind, message string) billing.ResponseSnapshot {
	body, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]string{"type": kind, "message": message}})
	body = append(body, '\n')
	return billing.ResponseSnapshot{Status: status, Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: body}
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
func extractMaximumOutput(body []byte) (int64, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return 0, errors.New("invalid")
	}
	var result int64
	seen := false
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return 0, err
		}
		key := keyToken.(string)
		if key == "max_tokens" {
			if seen {
				return 0, errors.New("duplicate max_tokens")
			}
			seen = true
			var number json.Number
			if err := decoder.Decode(&number); err != nil {
				return 0, err
			}
			result, err = number.Int64()
			if err != nil || result < 1 {
				return 0, errors.New("invalid max_tokens")
			}
		} else {
			var discard any
			if err := decoder.Decode(&discard); err != nil {
				return 0, err
			}
		}
	}
	if _, err := decoder.Token(); err != nil || !seen || decoder.Decode(new(any)) != io.EOF {
		return 0, errors.New("invalid")
	}
	return result, nil
}
func extractUsage(body []byte) (chatpricing.Usage, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return chatpricing.Usage{}, errors.New("invalid")
	}
	seen := false
	var usage chatpricing.Usage
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return chatpricing.Usage{}, err
		}
		key := keyToken.(string)
		if key == "usage" {
			if seen {
				return chatpricing.Usage{}, errors.New("duplicate usage")
			}
			seen = true
			usage, err = decodeUsage(decoder)
			if err != nil {
				return chatpricing.Usage{}, err
			}
		} else {
			var discard any
			if err := decoder.Decode(&discard); err != nil {
				return chatpricing.Usage{}, err
			}
		}
	}
	if _, err := decoder.Token(); err != nil || !seen || decoder.Decode(new(any)) != io.EOF {
		return chatpricing.Usage{}, errors.New("invalid")
	}
	return usage, nil
}
func decodeUsage(decoder *json.Decoder) (chatpricing.Usage, error) {
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return chatpricing.Usage{}, errors.New("invalid usage")
	}
	values := map[string]int64{}
	seen := map[string]bool{}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return chatpricing.Usage{}, err
		}
		key := keyToken.(string)
		switch key {
		case "input_tokens", "output_tokens", "cache_creation_input_tokens", "cache_read_input_tokens":
			if seen[key] {
				return chatpricing.Usage{}, errors.New("duplicate usage field")
			}
			seen[key] = true
			var number json.Number
			if err := decoder.Decode(&number); err != nil {
				return chatpricing.Usage{}, err
			}
			value, err := number.Int64()
			if err != nil || value < 0 {
				return chatpricing.Usage{}, errors.New("invalid usage field")
			}
			values[key] = value
		default:
			var discard any
			if err := decoder.Decode(&discard); err != nil {
				return chatpricing.Usage{}, err
			}
		}
	}
	if _, err := decoder.Token(); err != nil || !seen["input_tokens"] || !seen["output_tokens"] {
		return chatpricing.Usage{}, errors.New("missing usage field")
	}
	input, cached, write := values["input_tokens"], values["cache_read_input_tokens"], values["cache_creation_input_tokens"]
	if input > int64(^uint64(0)>>1)-cached || input+cached > int64(^uint64(0)>>1)-write {
		return chatpricing.Usage{}, errors.New("usage overflow")
	}
	return chatpricing.Usage{PromptTokens: input + cached + write, CachedInputTokens: cached, CacheWriteTokens: write, CompletionTokens: values["output_tokens"]}, nil
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
