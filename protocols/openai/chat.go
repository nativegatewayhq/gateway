package openai

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

	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	"github.com/nativegatewayhq/gateway/internal/providerhealth"
	"github.com/nativegatewayhq/gateway/internal/requestid"
	"github.com/nativegatewayhq/gateway/internal/telemetry"
	chatoperation "github.com/nativegatewayhq/gateway/operations/chat"
	openaiProvider "github.com/nativegatewayhq/gateway/providers/openai"
)

type ChatRegistry interface {
	Resolve(string) (chatoperation.Model, error)
}
type ChatExecutor interface {
	Complete(context.Context, openaiProvider.ChatRequest) (*http.Response, error)
}

type ChatHandler struct {
	common           *Handler
	models           ChatRegistry
	executor         ChatExecutor
	availability     ChannelProviderAvailability
	health           providerhealth.Gate
	maximumBodyBytes int64
	telemetry        *telemetry.Recorder
}

func NewChatHandler(logger *slog.Logger, auth Authenticator, models ChatRegistry, executor ChatExecutor, availability ChannelProviderAvailability, health providerhealth.Gate, maximumBodyBytes int64) *ChatHandler {
	if health == nil {
		health = providerhealth.NoopGate{}
	}
	return &ChatHandler{common: NewImagesHandler(logger, auth, nil, nil, 1), models: models, executor: executor, availability: availability, health: health, maximumBodyBytes: maximumBodyBytes}
}
func (h *ChatHandler) SetTelemetry(recorder *telemetry.Recorder) {
	h.telemetry = recorder
	h.common.telemetry = recorder
}

func (h *ChatHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tracked := &statusWriter{ResponseWriter: w}
	started := time.Now()
	dispatched := false
	defer func() {
		h.common.logger.Info("openai chat request completed", "request_id", requestid.FromContext(r.Context()), "protocol", "openai", "operation", chatoperation.Completions, "status", tracked.statusCode(), "dispatched", dispatched, "duration", time.Since(started))
	}()
	if r.Method != http.MethodPost {
		tracked.Header().Set("Allow", http.MethodPost)
		writeError(tracked, http.StatusMethodNotAllowed, "invalid_request_error", "method_not_allowed", "method not allowed")
		return
	}
	principal, ok := h.common.authenticate(tracked, r)
	if !ok {
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		writeError(tracked, http.StatusBadRequest, "invalid_request_error", "invalid_content_type", "content type must be application/json")
		return
	}
	if encoding := strings.TrimSpace(r.Header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
		writeError(tracked, http.StatusUnsupportedMediaType, "invalid_request_error", "unsupported_content_encoding", "compressed request bodies are not supported")
		return
	}
	if h.maximumBodyBytes < 1 || r.ContentLength > h.maximumBodyBytes {
		writeError(tracked, http.StatusRequestEntityTooLarge, "invalid_request_error", "request_too_large", "request body too large")
		return
	}
	body, err := readBounded(r.Body, h.maximumBodyBytes)
	if errors.Is(err, errBodyTooLarge) {
		writeError(tracked, http.StatusRequestEntityTooLarge, "invalid_request_error", "request_too_large", "request body too large")
		return
	}
	if err != nil {
		writeError(tracked, http.StatusBadRequest, "invalid_request_error", "invalid_request", "could not read request body")
		return
	}
	model, stream, err := extractChatEnvelope(body)
	if err != nil {
		writeError(tracked, http.StatusBadRequest, "invalid_request_error", "invalid_request", "request must contain one model and valid stream option")
		return
	}
	if stream {
		writeError(tracked, http.StatusBadRequest, "invalid_request_error", "streaming_not_supported", "streaming is not supported")
		return
	}
	if h.models == nil || h.executor == nil {
		writeError(tracked, http.StatusServiceUnavailable, "server_error", "provider_unavailable", "provider unavailable")
		return
	}
	route, err := h.models.Resolve(model)
	if errors.Is(err, chatoperation.ErrModelNotFound) {
		writeError(tracked, http.StatusNotFound, "invalid_request_error", "model_not_found", "model not found")
		return
	}
	if err != nil {
		writeError(tracked, http.StatusServiceUnavailable, "server_error", "provider_unavailable", "provider unavailable")
		return
	}
	if !h.common.authorizeModel(tracked, r, principal, "openai", chatoperation.Completions, model) {
		return
	}
	if h.availability == nil || !h.availability.ConfiguredChannel(r.Context(), route.ChannelID, route.Provider) {
		writeError(tracked, http.StatusServiceUnavailable, "server_error", "provider_unavailable", "provider unavailable")
		return
	}
	permit, ok := h.acquireHealth(tracked, r, route.ChannelID)
	if !ok {
		return
	}
	dispatched = true
	response, err := h.execute(r.Context(), route, h.executor, openaiProvider.ChatRequest{ChannelID: route.ChannelID, ContentType: r.Header.Get("Content-Type"), Accept: r.Header.Get("Accept"), UserAgent: r.UserAgent(), ContentLength: int64(len(body)), Body: bytes.NewReader(body)})
	h.observe(r, permit, response, err)
	if err != nil {
		switch {
		case errors.Is(err, providercredentials.ErrCredentialUnavailable):
			writeError(tracked, http.StatusServiceUnavailable, "server_error", "provider_unavailable", "provider unavailable")
		case errors.Is(err, openaiProvider.ErrChatTimeout):
			writeError(tracked, http.StatusGatewayTimeout, "server_error", "provider_timeout", "provider request timed out")
		case errors.Is(err, openaiProvider.ErrChatCanceled):
			writeError(tracked, 499, "server_error", "request_canceled", "request canceled")
		default:
			writeError(tracked, http.StatusBadGateway, "server_error", "provider_unavailable", "provider unavailable")
		}
		return
	}
	defer response.Body.Close()
	responseBody, err := readBounded(response.Body, h.maximumBodyBytes)
	if err != nil {
		writeError(tracked, http.StatusBadGateway, "server_error", "provider_response_too_large", "provider response exceeded the configured limit")
		return
	}
	copyResponseHeaders(tracked.Header(), response.Header)
	tracked.WriteHeader(response.StatusCode)
	_, _ = tracked.Write(responseBody)
}

func extractChatEnvelope(body []byte) (string, bool, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return "", false, errors.New("invalid JSON")
	}
	model := ""
	modelCount, streamCount := 0, 0
	stream := false
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return "", false, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return "", false, errors.New("invalid key")
		}
		switch key {
		case "model":
			modelCount++
			if err := decoder.Decode(&model); err != nil {
				return "", false, err
			}
		case "stream":
			streamCount++
			if err := decoder.Decode(&stream); err != nil {
				return "", false, err
			}
		default:
			var raw json.RawMessage
			if err := decoder.Decode(&raw); err != nil {
				return "", false, err
			}
		}
	}
	if _, err = decoder.Token(); err != nil {
		return "", false, err
	}
	if decoder.Decode(&struct{}{}) != io.EOF || modelCount != 1 || streamCount > 1 || model == "" || len(model) > 200 || strings.TrimSpace(model) != model {
		return "", false, errors.New("invalid envelope")
	}
	return model, stream, nil
}

func (h *ChatHandler) acquireHealth(w http.ResponseWriter, r *http.Request, channel string) (providerhealth.Permit, bool) {
	snapshot, err := h.health.Inspect(r.Context(), channel)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "server_error", "provider_health_unavailable", "provider health unavailable")
		return providerhealth.Permit{}, false
	}
	if snapshot.State == providerhealth.Open {
		writeError(w, http.StatusServiceUnavailable, "server_error", "provider_unavailable", "provider unavailable")
		return providerhealth.Permit{}, false
	}
	if snapshot.State == providerhealth.HalfOpen {
		permit, err := h.health.ClaimProbe(r.Context(), channel, requestid.FromContext(r.Context()))
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "server_error", "provider_unavailable", "provider unavailable")
			return providerhealth.Permit{}, false
		}
		return permit, true
	}
	return providerhealth.Permit{ChannelID: channel}, true
}
func (h *ChatHandler) observe(r *http.Request, permit providerhealth.Permit, response *http.Response, err error) {
	outcome := providerhealth.Neutral
	switch {
	case errors.Is(err, openaiProvider.ErrChatTimeout):
		outcome = providerhealth.Timeout
	case err != nil:
		outcome = providerhealth.Connection
	case response.StatusCode == 429:
		outcome = providerhealth.RateLimited
	case response.StatusCode >= 500:
		outcome = providerhealth.ServerError
	case response.StatusCode >= 200 && response.StatusCode < 400:
		outcome = providerhealth.Success
	}
	_, _ = h.health.Observe(context.WithoutCancel(r.Context()), providerhealth.Observation{ChannelID: permit.ChannelID, ObservationID: requestid.FromContext(r.Context()), Outcome: outcome, Permit: permit})
}
func (h *ChatHandler) execute(ctx context.Context, route chatoperation.Model, executor ChatExecutor, input openaiProvider.ChatRequest) (*http.Response, error) {
	if h.telemetry == nil {
		return executor.Complete(ctx, input)
	}
	providerCtx, span, started := h.telemetry.StartProvider(ctx, string(route.Provider), "openai", chatoperation.Completions)
	response, err := executor.Complete(providerCtx, input)
	outcome := "success"
	if errors.Is(err, openaiProvider.ErrChatTimeout) {
		outcome = "timeout"
	} else if err != nil {
		outcome = "failure"
	} else if response.StatusCode >= 500 {
		outcome = "server_error"
	} else if response.StatusCode == 429 {
		outcome = "rate_limited"
	}
	h.telemetry.EndProvider(providerCtx, span, started, telemetry.ProviderRecord{Provider: string(route.Provider), Protocol: "openai", Operation: chatoperation.Completions, Outcome: outcome})
	return response, err
}
