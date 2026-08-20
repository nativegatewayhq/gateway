package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	"github.com/nativegatewayhq/gateway/internal/requestid"
	"github.com/nativegatewayhq/gateway/internal/telemetry"
	responsesoperation "github.com/nativegatewayhq/gateway/operations/responses"
	openaiProvider "github.com/nativegatewayhq/gateway/providers/openai"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"
)

type ResponsesRegistry interface {
	Resolve(string) (responsesoperation.Model, error)
}
type ResponsesExecutor interface {
	Create(context.Context, openaiProvider.ResponsesRequest) (*http.Response, error)
}
type ResponsesHandler struct {
	common           *Handler
	models           ResponsesRegistry
	executor         ResponsesExecutor
	availability     ChannelProviderAvailability
	maximumBodyBytes int64
	telemetry        *telemetry.Recorder
}

func NewResponsesHandler(logger *slog.Logger, auth Authenticator, models ResponsesRegistry, executor ResponsesExecutor, availability ChannelProviderAvailability, maximum int64) *ResponsesHandler {
	return &ResponsesHandler{common: NewImagesHandler(logger, auth, nil, nil, 1), models: models, executor: executor, availability: availability, maximumBodyBytes: maximum}
}
func (h *ResponsesHandler) SetTelemetry(r *telemetry.Recorder) {
	h.telemetry = r
	h.common.telemetry = r
}
func (h *ResponsesHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tracked := &statusWriter{ResponseWriter: w}
	started := time.Now()
	defer func() {
		h.common.logger.Info("openai responses request completed", "request_id", requestid.FromContext(r.Context()), "protocol", "openai", "operation", responsesoperation.Create, "status", tracked.statusCode(), "duration", time.Since(started))
	}()
	if r.Method != http.MethodPost {
		tracked.Header().Set("Allow", http.MethodPost)
		writeError(tracked, 405, "invalid_request_error", "method_not_allowed", "method not allowed")
		return
	}
	principal, ok := h.common.authenticate(tracked, r)
	if !ok {
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		writeError(tracked, 400, "invalid_request_error", "invalid_content_type", "content type must be application/json")
		return
	}
	if h.maximumBodyBytes < 1 || r.ContentLength > h.maximumBodyBytes {
		writeError(tracked, 413, "invalid_request_error", "request_too_large", "request body too large")
		return
	}
	body, err := readBounded(r.Body, h.maximumBodyBytes)
	if err != nil {
		writeError(tracked, 413, "invalid_request_error", "request_too_large", "request body too large")
		return
	}
	model, stream, err := extractResponsesEnvelope(body)
	if err != nil {
		writeError(tracked, 400, "invalid_request_error", "invalid_request", "request must contain one model and valid stream option")
		return
	}
	if stream {
		writeError(tracked, 400, "invalid_request_error", "streaming_not_supported", "streaming is not supported")
		return
	}
	route, err := h.models.Resolve(model)
	if errors.Is(err, responsesoperation.ErrModelNotFound) {
		writeError(tracked, 404, "invalid_request_error", "model_not_found", "model not found")
		return
	}
	if err != nil || h.executor == nil {
		writeError(tracked, 503, "server_error", "provider_unavailable", "provider unavailable")
		return
	}
	if !h.common.authorizeModel(tracked, r, principal, "openai", responsesoperation.Create, model) {
		return
	}
	if h.availability == nil || !h.availability.ConfiguredChannel(r.Context(), route.ChannelID, route.Provider) {
		writeError(tracked, 503, "server_error", "provider_unavailable", "provider unavailable")
		return
	}
	response, err := h.execute(r.Context(), route, h.executor, openaiProvider.ResponsesRequest{ChannelID: route.ChannelID, ContentType: r.Header.Get("Content-Type"), Accept: r.Header.Get("Accept"), UserAgent: r.UserAgent(), ContentLength: int64(len(body)), Body: bytes.NewReader(body)})
	if err != nil {
		switch {
		case errors.Is(err, providercredentials.ErrCredentialUnavailable):
			writeError(tracked, 503, "server_error", "provider_unavailable", "provider unavailable")
		case errors.Is(err, openaiProvider.ErrResponsesTimeout):
			writeError(tracked, 504, "server_error", "provider_timeout", "provider request timed out")
		case errors.Is(err, openaiProvider.ErrResponsesCanceled):
			writeError(tracked, 499, "server_error", "request_canceled", "request canceled")
		default:
			writeError(tracked, 502, "server_error", "provider_unavailable", "provider unavailable")
		}
		return
	}
	defer response.Body.Close()
	responseBody, err := readBounded(response.Body, h.maximumBodyBytes)
	if err != nil {
		writeError(tracked, 502, "server_error", "provider_response_too_large", "provider response exceeded the configured limit")
		return
	}
	copyResponseHeaders(tracked.Header(), response.Header)
	tracked.WriteHeader(response.StatusCode)
	_, _ = tracked.Write(responseBody)
}
func (h *ResponsesHandler) execute(ctx context.Context, route responsesoperation.Model, executor ResponsesExecutor, input openaiProvider.ResponsesRequest) (*http.Response, error) {
	if h.telemetry == nil {
		return executor.Create(ctx, input)
	}
	providerCtx, span, started := h.telemetry.StartProvider(ctx, string(route.Provider), "openai", responsesoperation.Create)
	response, err := executor.Create(providerCtx, input)
	outcome := "success"
	if errors.Is(err, openaiProvider.ErrResponsesTimeout) {
		outcome = "timeout"
	} else if err != nil {
		outcome = "failure"
	}
	h.telemetry.EndProvider(providerCtx, span, started, telemetry.ProviderRecord{Provider: string(route.Provider), Protocol: "openai", Operation: responsesoperation.Create, Outcome: outcome})
	return response, err
}
func extractResponsesEnvelope(body []byte) (string, bool, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return "", false, errors.New("invalid JSON")
	}
	model := ""
	stream := false
	modelCount, streamCount := 0, 0
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return "", false, err
		}
		switch keyToken.(string) {
		case "model":
			modelCount++
			if decoder.Decode(&model) != nil {
				return "", false, errors.New("invalid model")
			}
		case "stream":
			streamCount++
			if decoder.Decode(&stream) != nil {
				return "", false, errors.New("invalid stream")
			}
		default:
			var raw json.RawMessage
			if decoder.Decode(&raw) != nil {
				return "", false, errors.New("invalid field")
			}
		}
	}
	if _, err = decoder.Token(); err != nil || decoder.Decode(&struct{}{}) != io.EOF || modelCount != 1 || streamCount > 1 || model == "" || len(model) > 200 || strings.TrimSpace(model) != model {
		return "", false, errors.New("invalid envelope")
	}
	return model, stream, nil
}
