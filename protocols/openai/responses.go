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

	"github.com/nativegatewayhq/gateway/internal/apikey"
	"github.com/nativegatewayhq/gateway/internal/billing"
	"github.com/nativegatewayhq/gateway/internal/chatbilling"
	"github.com/nativegatewayhq/gateway/internal/chatpricing"
	"github.com/nativegatewayhq/gateway/internal/idempotency"
	"github.com/nativegatewayhq/gateway/internal/ledger"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	"github.com/nativegatewayhq/gateway/internal/requestid"
	"github.com/nativegatewayhq/gateway/internal/telemetry"
	responsesoperation "github.com/nativegatewayhq/gateway/operations/responses"
	openaiProvider "github.com/nativegatewayhq/gateway/providers/openai"
)

type ResponsesRegistry interface {
	Resolve(string) (responsesoperation.Model, error)
}
type ResponsesExecutor interface {
	Create(context.Context, openaiProvider.ResponsesRequest) (*http.Response, error)
}
type ResponsesBilling interface {
	Begin(context.Context, chatbilling.BeginRequest) (chatbilling.Charge, error)
	Replay(context.Context, chatbilling.BeginRequest) (chatbilling.Charge, bool, error)
	CompleteUsage(context.Context, string, chatpricing.Usage, billing.ResponseSnapshot) (chatbilling.Charge, error)
	Release(context.Context, string, billing.ResponseSnapshot) (chatbilling.Charge, error)
	MarkReconciling(context.Context, string, string, *billing.ResponseSnapshot) error
	MarkReconcilingUsage(context.Context, string, string, *billing.ResponseSnapshot, chatpricing.Usage) error
	CompleteStreamUsage(context.Context, string, chatpricing.Usage, [32]byte) (chatbilling.Charge, error)
	MarkStreamReconcilingUsage(context.Context, string, chatpricing.Usage, [32]byte) error
	MarkStreamReconciling(context.Context, string, string, string, string) error
}
type ResponsesHandler struct {
	common           *Handler
	models           ResponsesRegistry
	executor         ResponsesExecutor
	availability     ChannelProviderAvailability
	maximumBodyBytes int64
	telemetry        *telemetry.Recorder
	billing          ResponsesBilling
}

func NewResponsesHandler(logger *slog.Logger, auth Authenticator, models ResponsesRegistry, executor ResponsesExecutor, availability ChannelProviderAvailability, maximum int64) *ResponsesHandler {
	return &ResponsesHandler{common: NewImagesHandler(logger, auth, nil, nil, 1), models: models, executor: executor, availability: availability, maximumBodyBytes: maximum}
}
func NewBillableResponsesHandler(logger *slog.Logger, auth Authenticator, models ResponsesRegistry, executor ResponsesExecutor, availability ChannelProviderAvailability, maximum int64, chargeBilling ResponsesBilling) *ResponsesHandler {
	handler := NewResponsesHandler(logger, auth, models, executor, availability, maximum)
	handler.billing = chargeBilling
	return handler
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
	if stream {
		if _, ok := tracked.ResponseWriter.(http.Flusher); !ok {
			writeError(tracked, http.StatusInternalServerError, "server_error", "streaming_unavailable", "streaming unavailable")
			return
		}
		if h.billing != nil {
			h.serveBillableStream(tracked, r, principal, route, body)
		} else {
			h.serveBYOKStream(tracked, r, route, body)
		}
		return
	}
	if h.billing != nil {
		h.serveBillable(tracked, r, principal, route, body)
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

func (h *ResponsesHandler) serveBYOKStream(w http.ResponseWriter, r *http.Request, route responsesoperation.Model, body []byte) {
	if h.availability == nil || !h.availability.ConfiguredChannel(r.Context(), route.ChannelID, route.Provider) {
		writeError(w, http.StatusServiceUnavailable, "server_error", "provider_unavailable", "provider unavailable")
		return
	}
	response, err := h.execute(r.Context(), route, h.executor, openaiProvider.ResponsesRequest{ChannelID: route.ChannelID, ContentType: r.Header.Get("Content-Type"), Accept: "text/event-stream", UserAgent: r.UserAgent(), ContentLength: int64(len(body)), Body: bytes.NewReader(body), Streaming: true})
	if err != nil {
		writeError(w, http.StatusBadGateway, "server_error", "provider_unavailable", "provider unavailable")
		return
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		responseBody, readErr := readBounded(response.Body, h.maximumBodyBytes)
		if readErr != nil {
			writeError(w, http.StatusBadGateway, "server_error", "provider_response_too_large", "provider response exceeded the configured limit")
			return
		}
		copyResponseHeaders(w.Header(), response.Header)
		w.WriteHeader(response.StatusCode)
		_, _ = w.Write(responseBody)
		return
	}
	mediaType, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if mediaErr != nil || !strings.EqualFold(mediaType, "text/event-stream") {
		writeError(w, http.StatusBadGateway, "server_error", "invalid_provider_stream", "invalid provider stream")
		return
	}
	copyStreamResponseHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	_, _ = relayResponsesStream(w, response.Body, h.maximumBodyBytes, false)
}

func (h *ResponsesHandler) serveBillableStream(w http.ResponseWriter, r *http.Request, principal apikey.Principal, route responsesoperation.Model, body []byte) {
	maximumOutput, err := extractResponsesOutputLimit(body)
	if err != nil || route.MaximumInputTokens < 1 || route.MaximumOutputTokens < 1 || maximumOutput > route.MaximumOutputTokens || int64(len(body)) > route.MaximumInputTokens {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_token_limit", "paid Responses requires valid input and output token limits")
		return
	}
	if h.availability == nil || !h.availability.ConfiguredChannel(r.Context(), route.ChannelID, route.Provider) {
		writeError(w, http.StatusServiceUnavailable, "server_error", "provider_unavailable", "provider unavailable")
		return
	}
	key, keyErr := idempotency.Extract(r.Header)
	if keyErr != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_idempotency_key", "idempotency key is invalid")
		return
	}
	var fingerprint [32]byte
	if key != "" {
		fingerprint = idempotency.Fingerprint("openai", responsesoperation.Create, route.ID, route.ChannelID, "text/event-stream", body)
	}
	begin := chatbilling.BeginRequest{Operation: responsesoperation.Create, RequestID: requestid.FromContext(r.Context()), OrganizationID: principal.OrganizationID, ProjectID: principal.ProjectID, APIKeyID: principal.APIKeyID, Model: route.ID, ChannelID: route.ChannelID, IdempotencyKey: key, Fingerprint: fingerprint, MaximumInputTokens: int64(len(body)), MaximumOutputTokens: maximumOutput, DeliveryMode: "stream"}
	if key != "" {
		if _, found, replayErr := h.billing.Replay(r.Context(), begin); replayErr != nil || found {
			if replayErr == nil {
				replayErr = chatbilling.ErrConflict
			}
			h.writeBillingError(w, replayErr)
			return
		}
	}
	charge, err := h.billing.Begin(r.Context(), begin)
	if err != nil {
		h.billingTelemetry(r.Context(), "begin", "failure")
		h.writeBillingError(w, err)
		return
	}
	h.billingTelemetry(r.Context(), "begin", "success")
	response, executeErr := h.execute(r.Context(), route, h.executor, openaiProvider.ResponsesRequest{ChannelID: route.ChannelID, ContentType: r.Header.Get("Content-Type"), Accept: "text/event-stream", UserAgent: r.UserAgent(), ContentLength: int64(len(body)), Body: bytes.NewReader(body), Streaming: true})
	if executeErr != nil {
		reason := "executor_connection_lost"
		if errors.Is(executeErr, openaiProvider.ErrResponsesTimeout) {
			reason = "executor_timeout"
		}
		_ = h.billing.MarkStreamReconciling(context.WithoutCancel(r.Context()), charge.ID, reason, "provider", "provider_error")
		h.billingTelemetry(r.Context(), "reconciling", "success")
		writeError(w, http.StatusBadGateway, "server_error", "provider_unavailable", "provider unavailable")
		return
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		responseBody, readErr := readBounded(response.Body, h.maximumBodyBytes)
		if readErr != nil {
			_ = h.billing.MarkStreamReconciling(context.WithoutCancel(r.Context()), charge.ID, "response_unavailable", "provider", "provider_error")
			writeError(w, http.StatusBadGateway, "server_error", "provider_response_too_large", "provider response exceeded the configured limit")
			return
		}
		snapshot := billing.ResponseSnapshot{Status: response.StatusCode, Headers: safeResponseHeaders(response.Header), Body: responseBody}
		settled, settleErr := h.billing.Release(context.WithoutCancel(r.Context()), charge.ID, snapshot)
		if settleErr != nil {
			_ = h.billing.MarkReconciling(context.WithoutCancel(r.Context()), charge.ID, "settlement_failed", &snapshot)
			writeError(w, http.StatusServiceUnavailable, "server_error", "settlement_unavailable", "settlement unavailable")
			return
		}
		h.billingTelemetry(r.Context(), "release", "success")
		h.common.writeSnapshot(w, settled.Response, false)
		return
	}
	mediaType, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if mediaErr != nil || !strings.EqualFold(mediaType, "text/event-stream") {
		_ = h.billing.MarkStreamReconciling(context.WithoutCancel(r.Context()), charge.ID, "stream_protocol_invalid", "provider", "invalid_usage")
		writeError(w, http.StatusBadGateway, "server_error", "invalid_provider_stream", "invalid provider stream")
		return
	}
	copyStreamResponseHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	streamStarted := time.Now()
	result, relayErr := relayResponsesStream(w, response.Body, h.maximumBodyBytes, true)
	if relayErr != nil || result.Terminal != "complete" || !result.UsageFound || result.Usage.PromptTokens > charge.MaximumInputTokens || result.Usage.CompletionTokens > charge.MaximumOutputTokens {
		reason := "stream_protocol_invalid"
		side, category := "provider", result.Terminal
		if category == "" {
			category = "missing_terminal"
		}
		if errors.Is(relayErr, errStreamWrite) {
			reason, side, category = "stream_write_failed", "client", "write_failed"
		} else if errors.Is(r.Context().Err(), context.Canceled) {
			reason, side, category = "client_disconnect", "client", "client_disconnect"
		} else if errors.Is(relayErr, openaiProvider.ErrResponsesStreamIdle) {
			reason, category = "executor_timeout", "provider_error"
		} else if relayErr != nil && !errors.Is(relayErr, errStreamProtocol) {
			reason, category = "executor_connection_lost", "provider_error"
		} else if result.Terminal == "complete" && !result.UsageFound {
			reason, category = "stream_usage_missing", "missing_usage"
		} else if result.Terminal == "complete" {
			category = "invalid_usage"
		}
		_ = h.billing.MarkStreamReconciling(context.WithoutCancel(r.Context()), charge.ID, reason, side, category)
		h.responsesStreamTelemetry(r.Context(), category, side, result.FirstByte, time.Since(streamStarted))
		h.billingTelemetry(r.Context(), "reconciling", "success")
		return
	}
	if _, settleErr := h.billing.CompleteStreamUsage(context.WithoutCancel(r.Context()), charge.ID, result.Usage, result.TerminalDigest); settleErr != nil {
		_ = h.billing.MarkStreamReconcilingUsage(context.WithoutCancel(r.Context()), charge.ID, result.Usage, result.TerminalDigest)
		h.billingTelemetry(r.Context(), "reconciling", "failure")
		h.responsesStreamTelemetry(r.Context(), "complete", "none", result.FirstByte, time.Since(streamStarted))
		return
	}
	h.billingTelemetry(r.Context(), "capture", "success")
	h.responsesStreamTelemetry(r.Context(), "complete", "none", result.FirstByte, time.Since(streamStarted))
}

func (h *ResponsesHandler) responsesStreamTelemetry(ctx context.Context, category, side string, firstByte, duration time.Duration) {
	if h.telemetry != nil {
		h.telemetry.LLMStream(ctx, telemetry.LLMStreamRecord{Protocol: "openai", Operation: responsesoperation.Create, TerminalCategory: category, DisconnectSide: side, FirstByte: firstByte, Duration: duration})
	}
}

func (h *ResponsesHandler) serveBillable(w http.ResponseWriter, r *http.Request, principal apikey.Principal, route responsesoperation.Model, body []byte) {
	maximumOutput, err := extractResponsesOutputLimit(body)
	if err != nil || route.MaximumInputTokens < 1 || route.MaximumOutputTokens < 1 || maximumOutput > route.MaximumOutputTokens || int64(len(body)) > route.MaximumInputTokens {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_token_limit", "paid Responses requires valid input and output token limits")
		return
	}
	if h.availability == nil || !h.availability.ConfiguredChannel(r.Context(), route.ChannelID, route.Provider) {
		writeError(w, http.StatusServiceUnavailable, "server_error", "provider_unavailable", "provider unavailable")
		return
	}
	key, keyErr := idempotency.Extract(r.Header)
	if keyErr != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_idempotency_key", "idempotency key is invalid")
		return
	}
	var fingerprint [32]byte
	if key != "" {
		fingerprint = idempotency.Fingerprint("openai", responsesoperation.Create, route.ID, route.ChannelID, "application/json", body)
	}
	begin := chatbilling.BeginRequest{Operation: responsesoperation.Create, RequestID: requestid.FromContext(r.Context()), OrganizationID: principal.OrganizationID, ProjectID: principal.ProjectID, APIKeyID: principal.APIKeyID, Model: route.ID, ChannelID: route.ChannelID, IdempotencyKey: key, Fingerprint: fingerprint, MaximumInputTokens: int64(len(body)), MaximumOutputTokens: maximumOutput}
	if key != "" {
		replayed, found, replayErr := h.billing.Replay(r.Context(), begin)
		if replayErr != nil {
			h.billingTelemetry(r.Context(), "replay", "failure")
			h.writeBillingError(w, replayErr)
			return
		}
		if found {
			h.billingTelemetry(r.Context(), "replay", "replay")
			h.common.writeSnapshot(w, replayed.Response, true)
			return
		}
	}
	charge, err := h.billing.Begin(r.Context(), begin)
	if err != nil {
		h.billingTelemetry(r.Context(), "begin", "failure")
		h.writeBillingError(w, err)
		return
	}
	h.billingTelemetry(r.Context(), "begin", "success")
	if charge.Replay {
		h.common.writeSnapshot(w, charge.Response, true)
		return
	}
	response, executeErr := h.execute(r.Context(), route, h.executor, openaiProvider.ResponsesRequest{ChannelID: route.ChannelID, ContentType: r.Header.Get("Content-Type"), Accept: r.Header.Get("Accept"), UserAgent: r.UserAgent(), ContentLength: int64(len(body)), Body: bytes.NewReader(body)})
	if executeErr != nil {
		reason := "executor_connection_lost"
		if errors.Is(executeErr, openaiProvider.ErrResponsesTimeout) {
			reason = "executor_timeout"
		}
		_ = h.billing.MarkReconciling(context.WithoutCancel(r.Context()), charge.ID, reason, nil)
		h.billingTelemetry(r.Context(), "reconciling", "success")
		switch {
		case errors.Is(executeErr, openaiProvider.ErrResponsesTimeout):
			writeError(w, http.StatusGatewayTimeout, "server_error", "provider_timeout", "provider request timed out")
		case errors.Is(executeErr, openaiProvider.ErrResponsesCanceled):
			writeError(w, 499, "server_error", "request_canceled", "request canceled")
		default:
			writeError(w, http.StatusBadGateway, "server_error", "provider_unavailable", "provider unavailable")
		}
		return
	}
	defer response.Body.Close()
	responseBody, readErr := readBounded(response.Body, h.maximumBodyBytes)
	if readErr != nil {
		_ = h.billing.MarkReconciling(context.WithoutCancel(r.Context()), charge.ID, "response_unavailable", nil)
		h.billingTelemetry(r.Context(), "reconciling", "success")
		writeError(w, http.StatusBadGateway, "server_error", "provider_response_too_large", "provider response exceeded the configured limit")
		return
	}
	snapshot := billing.ResponseSnapshot{Status: response.StatusCode, Headers: safeResponseHeaders(response.Header), Body: responseBody}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		settled, settleErr := h.billing.Release(context.WithoutCancel(r.Context()), charge.ID, snapshot)
		if settleErr != nil {
			_ = h.billing.MarkReconciling(context.WithoutCancel(r.Context()), charge.ID, "settlement_failed", &snapshot)
			h.billingTelemetry(r.Context(), "reconciling", "failure")
			writeError(w, http.StatusServiceUnavailable, "server_error", "settlement_unavailable", "settlement unavailable")
			return
		}
		h.billingTelemetry(r.Context(), "release", "success")
		h.common.writeSnapshot(w, settled.Response, false)
		return
	}
	usage, usageErr := extractResponsesUsage(responseBody)
	if usageErr != nil || usage.PromptTokens > charge.MaximumInputTokens || usage.CompletionTokens > charge.MaximumOutputTokens {
		_ = h.billing.MarkReconciling(context.WithoutCancel(r.Context()), charge.ID, "usage_invalid", &snapshot)
		h.billingTelemetry(r.Context(), "reconciling", "success")
		copyResponseHeaders(w.Header(), response.Header)
		w.WriteHeader(response.StatusCode)
		_, _ = w.Write(responseBody)
		return
	}
	settled, settleErr := h.billing.CompleteUsage(context.WithoutCancel(r.Context()), charge.ID, usage, snapshot)
	if settleErr != nil {
		_ = h.billing.MarkReconcilingUsage(context.WithoutCancel(r.Context()), charge.ID, "settlement_failed", &snapshot, usage)
		h.billingTelemetry(r.Context(), "reconciling", "failure")
		writeError(w, http.StatusServiceUnavailable, "server_error", "settlement_unavailable", "settlement unavailable")
		return
	}
	h.billingTelemetry(r.Context(), "capture", "success")
	h.common.writeSnapshot(w, settled.Response, false)
}

func extractResponsesOutputLimit(body []byte) (int64, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return 0, errors.New("invalid JSON")
	}
	var value int64
	count := 0
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return 0, errors.New("invalid JSON")
		}
		if key.(string) == "max_output_tokens" {
			count++
			if decoder.Decode(&value) != nil || value < 1 {
				return 0, errors.New("invalid output limit")
			}
			continue
		}
		var ignored json.RawMessage
		if decoder.Decode(&ignored) != nil {
			return 0, errors.New("invalid JSON")
		}
	}
	if _, err = decoder.Token(); err != nil || decoder.Decode(&struct{}{}) != io.EOF || count != 1 {
		return 0, errors.New("missing output limit")
	}
	return value, nil
}

func extractResponsesUsage(body []byte) (chatpricing.Usage, error) {
	fields, err := collectJSONFields(body, "usage")
	if err != nil || len(fields["usage"]) != 1 || bytes.Equal(fields["usage"][0], []byte("null")) {
		return chatpricing.Usage{}, errors.New("missing usage")
	}
	return parseResponsesUsage(fields["usage"][0])
}

func parseResponsesUsage(raw json.RawMessage) (chatpricing.Usage, error) {
	fields, err := collectJSONFields(raw, "input_tokens", "output_tokens", "input_tokens_details", "output_tokens_details")
	if err != nil || len(fields["input_tokens"]) != 1 || len(fields["output_tokens"]) != 1 || len(fields["input_tokens_details"]) > 1 || len(fields["output_tokens_details"]) > 1 {
		return chatpricing.Usage{}, errors.New("invalid usage")
	}
	parse := func(raw json.RawMessage, required bool) (int64, error) {
		if len(raw) == 0 && !required {
			return 0, nil
		}
		var value int64
		if len(raw) == 0 || json.Unmarshal(raw, &value) != nil || value < 0 {
			return 0, errors.New("invalid usage")
		}
		return value, nil
	}
	input, err := parse(fields["input_tokens"][0], true)
	if err != nil {
		return chatpricing.Usage{}, err
	}
	output, err := parse(fields["output_tokens"][0], true)
	if err != nil {
		return chatpricing.Usage{}, err
	}
	cached, reasoning := int64(0), int64(0)
	if len(fields["input_tokens_details"]) == 1 && !bytes.Equal(fields["input_tokens_details"][0], []byte("null")) {
		details, detailsErr := collectJSONFields(fields["input_tokens_details"][0], "cached_tokens")
		if detailsErr != nil || len(details["cached_tokens"]) > 1 {
			return chatpricing.Usage{}, errors.New("invalid cached usage")
		}
		var cachedRaw json.RawMessage
		if len(details["cached_tokens"]) == 1 {
			cachedRaw = details["cached_tokens"][0]
		}
		cached, err = parse(cachedRaw, false)
		if err != nil {
			return chatpricing.Usage{}, err
		}
	}
	if len(fields["output_tokens_details"]) == 1 && !bytes.Equal(fields["output_tokens_details"][0], []byte("null")) {
		details, detailsErr := collectJSONFields(fields["output_tokens_details"][0], "reasoning_tokens")
		if detailsErr != nil || len(details["reasoning_tokens"]) > 1 {
			return chatpricing.Usage{}, errors.New("invalid reasoning usage")
		}
		var reasoningRaw json.RawMessage
		if len(details["reasoning_tokens"]) == 1 {
			reasoningRaw = details["reasoning_tokens"][0]
		}
		reasoning, err = parse(reasoningRaw, false)
		if err != nil {
			return chatpricing.Usage{}, err
		}
	}
	if cached > input || reasoning > output {
		return chatpricing.Usage{}, errors.New("invalid usage details")
	}
	return chatpricing.Usage{PromptTokens: input, CachedInputTokens: cached, CompletionTokens: output}, nil
}

func collectJSONFields(raw []byte, names ...string) (map[string][]json.RawMessage, error) {
	wanted := make(map[string]bool, len(names))
	fields := make(map[string][]json.RawMessage, len(names))
	for _, name := range names {
		wanted[name] = true
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("invalid object")
	}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil, errors.New("invalid object")
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return nil, errors.New("invalid object")
		}
		name := key.(string)
		if wanted[name] {
			fields[name] = append(fields[name], value)
		}
	}
	if _, err = decoder.Token(); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return nil, errors.New("invalid object")
	}
	return fields, nil
}

func (h *ResponsesHandler) billingTelemetry(ctx context.Context, transition, outcome string) {
	if h.telemetry != nil {
		h.telemetry.Billing(ctx, telemetry.BillingRecord{Protocol: "openai", Operation: responsesoperation.Create, Transition: transition, Outcome: outcome})
	}
}

func (h *ResponsesHandler) writeBillingError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, chatbilling.ErrConflict):
		writeError(w, http.StatusConflict, "invalid_request_error", "idempotency_conflict", "idempotency key conflicts with an existing request")
	case errors.Is(err, chatbilling.ErrPending):
		writeError(w, http.StatusConflict, "invalid_request_error", "request_pending", "request is still pending")
	case errors.Is(err, ledger.ErrInsufficientFunds):
		writeError(w, http.StatusPaymentRequired, "insufficient_funds", "insufficient_funds", "insufficient funds")
	case errors.Is(err, chatpricing.ErrUnavailable), errors.Is(err, chatpricing.ErrMargin):
		writeError(w, http.StatusServiceUnavailable, "server_error", "price_unavailable", "price unavailable")
	default:
		writeError(w, http.StatusServiceUnavailable, "server_error", "billing_unavailable", "billing unavailable")
	}
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
