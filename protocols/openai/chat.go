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
	"github.com/nativegatewayhq/gateway/internal/providerhealth"
	"github.com/nativegatewayhq/gateway/internal/requestid"
	"github.com/nativegatewayhq/gateway/internal/spendcap"
	"github.com/nativegatewayhq/gateway/internal/telemetry"
	chatoperation "github.com/nativegatewayhq/gateway/operations/chat"
	openaiProvider "github.com/nativegatewayhq/gateway/providers/openai"
)

type ChatRegistry interface {
	Resolve(string) (chatoperation.Model, error)
}
type RoutedChatRegistry interface {
	ChatRegistry
	Candidates(string, chatoperation.Requirements) ([]chatoperation.Model, error)
}
type ChatExecutor interface {
	Complete(context.Context, openaiProvider.ChatRequest) (*http.Response, error)
}
type ChatBilling interface {
	Quote(context.Context, chatbilling.BeginRequest) (chatpricing.Estimate, error)
	Begin(context.Context, chatbilling.BeginRequest) (chatbilling.Charge, error)
	Replay(context.Context, chatbilling.BeginRequest) (chatbilling.Charge, bool, error)
	CompleteUsage(context.Context, string, chatpricing.Usage, billing.ResponseSnapshot) (chatbilling.Charge, error)
	CompleteStreamUsage(context.Context, string, chatpricing.Usage, [32]byte) (chatbilling.Charge, error)
	Release(context.Context, string, billing.ResponseSnapshot) (chatbilling.Charge, error)
	MarkReconciling(context.Context, string, string, *billing.ResponseSnapshot) error
	MarkReconcilingUsage(context.Context, string, string, *billing.ResponseSnapshot, chatpricing.Usage) error
	MarkStreamReconcilingUsage(context.Context, string, chatpricing.Usage, [32]byte) error
	MarkStreamReconciling(context.Context, string, string, string, string) error
}

type ChatHandler struct {
	common           *Handler
	models           ChatRegistry
	executors        map[providercredentials.ProviderID]ChatExecutor
	availability     ChannelProviderAvailability
	health           providerhealth.Gate
	maximumBodyBytes int64
	telemetry        *telemetry.Recorder
	billing          ChatBilling
	weighted         *chatoperation.WeightedSampler
}

func NewBillableChatHandler(logger *slog.Logger, auth Authenticator, models ChatRegistry, executor ChatExecutor, availability ChannelProviderAvailability, health providerhealth.Gate, maximumBodyBytes int64, chargeBilling ChatBilling) *ChatHandler {
	handler := NewRoutedChatHandler(logger, auth, models, map[providercredentials.ProviderID]ChatExecutor{providercredentials.OpenAI: executor}, availability, health, maximumBodyBytes)
	handler.billing = chargeBilling
	return handler
}

func NewBillableRoutedChatHandler(logger *slog.Logger, auth Authenticator, models ChatRegistry, executors map[providercredentials.ProviderID]ChatExecutor, availability ChannelProviderAvailability, health providerhealth.Gate, maximumBodyBytes int64, chargeBilling ChatBilling) *ChatHandler {
	handler := NewRoutedChatHandler(logger, auth, models, executors, availability, health, maximumBodyBytes)
	handler.billing = chargeBilling
	return handler
}

func NewChatHandler(logger *slog.Logger, auth Authenticator, models ChatRegistry, executor ChatExecutor, availability ChannelProviderAvailability, health providerhealth.Gate, maximumBodyBytes int64) *ChatHandler {
	return NewRoutedChatHandler(logger, auth, models, map[providercredentials.ProviderID]ChatExecutor{providercredentials.OpenAI: executor}, availability, health, maximumBodyBytes)
}

func NewRoutedChatHandler(logger *slog.Logger, auth Authenticator, models ChatRegistry, executors map[providercredentials.ProviderID]ChatExecutor, availability ChannelProviderAvailability, health providerhealth.Gate, maximumBodyBytes int64) *ChatHandler {
	if health == nil {
		health = providerhealth.NoopGate{}
	}
	copyExecutors := make(map[providercredentials.ProviderID]ChatExecutor, len(executors))
	for provider, executor := range executors {
		if executor != nil {
			copyExecutors[provider] = executor
		}
	}
	return &ChatHandler{common: NewImagesHandler(logger, auth, nil, nil, 1), models: models, executors: copyExecutors, availability: availability, health: health, maximumBodyBytes: maximumBodyBytes, weighted: chatoperation.DefaultWeightedSampler()}
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
	if !h.common.authorizeModel(tracked, r, principal, "openai", chatoperation.Completions, model) {
		return
	}
	if h.billing != nil && h.handleEarlyChatReplay(tracked, r, principal, model, stream, body) {
		return
	}
	if h.models == nil || len(h.executors) == 0 {
		writeError(tracked, http.StatusServiceUnavailable, "server_error", "provider_unavailable", "provider unavailable")
		return
	}
	requirements, err := extractChatRequirements(body, stream)
	if err != nil {
		writeError(tracked, http.StatusBadRequest, "invalid_request_error", "unsupported_capability", "request requires an unsupported capability")
		return
	}
	candidates, err := h.chatCandidates(model, requirements)
	if errors.Is(err, chatoperation.ErrModelNotFound) {
		writeError(tracked, http.StatusNotFound, "invalid_request_error", "model_not_found", "model not found")
		return
	}
	if err != nil {
		if errors.Is(err, chatoperation.ErrUnsupported) {
			writeError(tracked, http.StatusBadRequest, "invalid_request_error", "unsupported_capability", "request requires an unsupported capability")
		} else {
			writeError(tracked, http.StatusServiceUnavailable, "server_error", "provider_unavailable", "provider unavailable")
		}
		return
	}
	candidates = h.preflightCandidates(r, candidates)
	if len(candidates) == 0 {
		writeError(tracked, http.StatusServiceUnavailable, "server_error", "provider_unavailable", "provider unavailable")
		return
	}
	if candidates[0].Policy == chatoperation.Weighted {
		selected, sampleErr := h.weighted.Pick(candidates)
		if sampleErr != nil {
			writeError(tracked, http.StatusServiceUnavailable, "server_error", "provider_unavailable", "provider unavailable")
			return
		}
		candidates = moveCandidateFirst(candidates, selected.CandidateID)
	}
	if stream {
		if _, ok := tracked.ResponseWriter.(http.Flusher); !ok {
			writeError(tracked, http.StatusInternalServerError, "server_error", "streaming_unavailable", "streaming unavailable")
			return
		}
		if h.billing != nil {
			h.serveBillableStreamCandidates(tracked, r, principal, candidates, body)
		} else {
			route, executor, permit, selectErr := h.selectBYOKCandidate(r, candidates)
			if selectErr != nil {
				writeError(tracked, http.StatusServiceUnavailable, "server_error", "provider_unavailable", "provider unavailable")
				return
			}
			h.serveBYOKStream(tracked, r, route, executor, permit, body)
		}
		return
	}
	if h.billing != nil {
		h.serveBillableCandidates(tracked, r, principal, candidates, body)
		return
	}
	route, executor, permit, selectErr := h.selectBYOKCandidate(r, candidates)
	if selectErr != nil {
		writeError(tracked, http.StatusServiceUnavailable, "server_error", "provider_unavailable", "provider unavailable")
		return
	}
	dispatched = true
	providerBody, err := rewriteTopLevelModel(body, route.ProviderModel)
	if err != nil {
		_ = h.health.Release(context.WithoutCancel(r.Context()), permit)
		writeError(tracked, http.StatusBadRequest, "invalid_request_error", "invalid_request", "invalid model field")
		return
	}
	response, err := h.execute(r.Context(), route, executor, openaiProvider.ChatRequest{ChannelID: route.ChannelID, ContentType: r.Header.Get("Content-Type"), Accept: r.Header.Get("Accept"), UserAgent: r.UserAgent(), ContentLength: int64(len(providerBody)), Body: bytes.NewReader(providerBody)})
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

func (h *ChatHandler) serveBYOKStream(w http.ResponseWriter, r *http.Request, route chatoperation.Model, executor ChatExecutor, permit providerhealth.Permit, body []byte) {
	providerBody, rewriteErr := rewriteTopLevelModel(body, route.ProviderModel)
	if rewriteErr != nil {
		_ = h.health.Release(context.WithoutCancel(r.Context()), permit)
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_request", "invalid model field")
		return
	}
	response, err := h.execute(r.Context(), route, executor, openaiProvider.ChatRequest{ChannelID: route.ChannelID, ContentType: r.Header.Get("Content-Type"), Accept: r.Header.Get("Accept"), UserAgent: r.UserAgent(), ContentLength: int64(len(providerBody)), Body: bytes.NewReader(providerBody), Streaming: true})
	h.observe(r, permit, response, err)
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
	copyStreamResponseHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	_, _ = relayNativeStream(w, response.Body, h.maximumBodyBytes, false)
}

func (h *ChatHandler) serveBillableStreamCandidates(w http.ResponseWriter, r *http.Request, principal apikey.Principal, candidates []chatoperation.Model, body []byte) {
	if ok, err := streamingUsageRequested(body); err != nil || !ok {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "stream_usage_required", "paid streaming requires stream_options.include_usage=true")
		return
	}
	maximumOutput, err := extractOutputLimit(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_token_limit", "paid Chat requires valid input and output token limits")
		return
	}
	candidates, _, evaluatedAt := h.orderBillableCandidates(r.Context(), candidates, int64(len(body)), maximumOutput)
	if len(candidates) == 0 {
		writeError(w, http.StatusServiceUnavailable, "server_error", "price_unavailable", "price unavailable")
		return
	}
	route := candidates[0]
	if route.MaximumInputTokens < 1 || maximumOutput < 1 || maximumOutput > route.MaximumOutputTokens || int64(len(body)) > route.MaximumInputTokens {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_token_limit", "paid Chat requires valid input and output token limits")
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
		fingerprint = idempotency.Fingerprint("openai", chatoperation.Completions, route.ID, "logical-route", "text/event-stream", body)
	}
	begin := routedBeginRequest(r, principal, route, key, fingerprint, int64(len(body)), maximumOutput, "stream", 0)
	begin.PriceEvaluatedAt = evaluatedAt
	if key != "" {
		if _, found, replayErr := h.billing.Replay(r.Context(), begin); replayErr != nil || found {
			if replayErr == nil {
				replayErr = chatbilling.ErrConflict
			}
			h.writeChatBillingError(w, replayErr)
			return
		}
	}
	route, executor, charge, permit, err := h.beginCandidate(r, principal, candidates, key, fingerprint, int64(len(body)), maximumOutput, "stream", evaluatedAt)
	if err != nil {
		h.writeChatBillingError(w, err)
		return
	}
	h.chatBillingTelemetry(r.Context(), "begin", "success")
	providerBody, rewriteErr := rewriteTopLevelModel(body, route.ProviderModel)
	if rewriteErr != nil {
		_ = h.billing.MarkStreamReconciling(context.WithoutCancel(r.Context()), charge.ID, "stream_protocol_invalid", "provider", "invalid_usage")
		return
	}
	response, executeErr := h.execute(r.Context(), route, executor, openaiProvider.ChatRequest{ChannelID: route.ChannelID, ContentType: r.Header.Get("Content-Type"), Accept: "text/event-stream", UserAgent: r.UserAgent(), ContentLength: int64(len(providerBody)), Body: bytes.NewReader(providerBody), Streaming: true})
	h.observe(r, permit, response, executeErr)
	if executeErr != nil {
		reason := "executor_connection_lost"
		if errors.Is(executeErr, openaiProvider.ErrChatTimeout) {
			reason = "executor_timeout"
		}
		_ = h.billing.MarkStreamReconciling(context.WithoutCancel(r.Context()), charge.ID, reason, "provider", "provider_error")
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
	result, relayErr := relayNativeStream(w, response.Body, h.maximumBodyBytes, true)
	if relayErr != nil || !result.Done || !result.UsageFound || result.Usage.PromptTokens > charge.MaximumInputTokens || result.Usage.CompletionTokens > charge.MaximumOutputTokens {
		reason := "stream_protocol_invalid"
		side, category := "provider", "invalid_usage"
		if errors.Is(relayErr, errStreamWrite) {
			reason = "stream_write_failed"
			side, category = "client", "write_failed"
		} else if !result.UsageFound {
			reason = "stream_usage_missing"
			category = "missing_usage"
		} else if !result.Done {
			category = "missing_done"
		}
		_ = h.billing.MarkStreamReconciling(context.WithoutCancel(r.Context()), charge.ID, reason, side, category)
		h.chatStreamTelemetry(r.Context(), category, side, result.FirstByte, time.Since(streamStarted))
		h.chatBillingTelemetry(r.Context(), "reconciling", "success")
		return
	}
	if _, settleErr := h.billing.CompleteStreamUsage(context.WithoutCancel(r.Context()), charge.ID, result.Usage, result.TerminalDigest); settleErr != nil {
		_ = h.billing.MarkStreamReconcilingUsage(context.WithoutCancel(r.Context()), charge.ID, result.Usage, result.TerminalDigest)
		h.chatBillingTelemetry(r.Context(), "reconciling", "failure")
		h.chatStreamTelemetry(r.Context(), "complete", "none", result.FirstByte, time.Since(streamStarted))
		return
	}
	h.chatBillingTelemetry(r.Context(), "capture", "success")
	h.chatStreamTelemetry(r.Context(), "complete", "none", result.FirstByte, time.Since(streamStarted))
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

func (h *ChatHandler) serveBillableCandidates(w http.ResponseWriter, r *http.Request, principal apikey.Principal, candidates []chatoperation.Model, body []byte) {
	maximumOutput, err := extractOutputLimit(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_token_limit", "paid Chat requires valid input and output token limits")
		return
	}
	candidates, _, evaluatedAt := h.orderBillableCandidates(r.Context(), candidates, int64(len(body)), maximumOutput)
	if len(candidates) == 0 {
		writeError(w, http.StatusServiceUnavailable, "server_error", "price_unavailable", "price unavailable")
		return
	}
	route := candidates[0]
	if route.MaximumInputTokens < 1 || route.MaximumOutputTokens < 1 || maximumOutput < 1 || maximumOutput > route.MaximumOutputTokens || int64(len(body)) > route.MaximumInputTokens {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_token_limit", "paid Chat requires valid input and output token limits")
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
		fingerprint = idempotency.Fingerprint("openai", chatoperation.Completions, route.ID, "logical-route", "application/json", body)
	}
	beginRequest := routedBeginRequest(r, principal, route, key, fingerprint, int64(len(body)), maximumOutput, "non_stream", 0)
	beginRequest.PriceEvaluatedAt = evaluatedAt
	if key != "" {
		replayed, found, replayErr := h.billing.Replay(r.Context(), beginRequest)
		if replayErr != nil {
			h.chatBillingTelemetry(r.Context(), "replay", "failure")
			h.writeChatBillingError(w, replayErr)
			return
		}
		if found {
			h.chatBillingTelemetry(r.Context(), "replay", "replay")
			h.common.writeSnapshot(w, replayed.Response, true)
			return
		}
	}
	route, executor, charge, permit, beginErr := h.beginCandidate(r, principal, candidates, key, fingerprint, int64(len(body)), maximumOutput, "non_stream", evaluatedAt)
	if beginErr != nil {
		h.chatBillingTelemetry(r.Context(), "begin", "failure")
		h.writeChatBillingError(w, beginErr)
		return
	}
	h.chatBillingTelemetry(r.Context(), "begin", "success")
	if charge.Replay {
		_ = h.health.Release(context.WithoutCancel(r.Context()), permit)
		h.common.writeSnapshot(w, charge.Response, true)
		return
	}
	providerBody, rewriteErr := rewriteTopLevelModel(body, route.ProviderModel)
	if rewriteErr != nil {
		_ = h.billing.MarkReconciling(context.WithoutCancel(r.Context()), charge.ID, "usage_invalid", nil)
		return
	}
	response, executeErr := h.execute(r.Context(), route, executor, openaiProvider.ChatRequest{ChannelID: route.ChannelID, ContentType: r.Header.Get("Content-Type"), Accept: r.Header.Get("Accept"), UserAgent: r.UserAgent(), ContentLength: int64(len(providerBody)), Body: bytes.NewReader(providerBody)})
	h.observe(r, permit, response, executeErr)
	if executeErr != nil {
		reason := "executor_connection_lost"
		if errors.Is(executeErr, openaiProvider.ErrChatTimeout) {
			reason = "executor_timeout"
		}
		_ = h.billing.MarkReconciling(context.WithoutCancel(r.Context()), charge.ID, reason, nil)
		h.chatBillingTelemetry(r.Context(), "reconciling", "success")
		switch {
		case errors.Is(executeErr, openaiProvider.ErrChatTimeout):
			writeError(w, http.StatusGatewayTimeout, "server_error", "provider_timeout", "provider request timed out")
		case errors.Is(executeErr, openaiProvider.ErrChatCanceled):
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
		h.chatBillingTelemetry(r.Context(), "reconciling", "success")
		writeError(w, http.StatusBadGateway, "server_error", "provider_response_too_large", "provider response exceeded the configured limit")
		return
	}
	snapshot := billing.ResponseSnapshot{Status: response.StatusCode, Headers: safeResponseHeaders(response.Header), Body: responseBody}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		settled, settleErr := h.billing.Release(context.WithoutCancel(r.Context()), charge.ID, snapshot)
		if settleErr != nil {
			_ = h.billing.MarkReconciling(context.WithoutCancel(r.Context()), charge.ID, "settlement_failed", &snapshot)
			h.chatBillingTelemetry(r.Context(), "reconciling", "failure")
			writeError(w, http.StatusServiceUnavailable, "server_error", "settlement_unavailable", "settlement unavailable")
			return
		}
		h.chatBillingTelemetry(r.Context(), "release", "success")
		h.common.writeSnapshot(w, settled.Response, false)
		return
	}
	usage, usageErr := extractChatUsage(responseBody)
	if usageErr != nil || usage.PromptTokens > charge.MaximumInputTokens || usage.CompletionTokens > charge.MaximumOutputTokens {
		_ = h.billing.MarkReconciling(context.WithoutCancel(r.Context()), charge.ID, "usage_invalid", &snapshot)
		h.chatBillingTelemetry(r.Context(), "reconciling", "success")
		copyResponseHeaders(w.Header(), response.Header)
		w.WriteHeader(response.StatusCode)
		_, _ = w.Write(responseBody)
		return
	}
	settled, settleErr := h.billing.CompleteUsage(context.WithoutCancel(r.Context()), charge.ID, usage, snapshot)
	if settleErr != nil {
		_ = h.billing.MarkReconcilingUsage(context.WithoutCancel(r.Context()), charge.ID, "settlement_failed", &snapshot, usage)
		h.chatBillingTelemetry(r.Context(), "reconciling", "failure")
		writeError(w, http.StatusServiceUnavailable, "server_error", "settlement_unavailable", "settlement unavailable")
		return
	}
	h.chatBillingTelemetry(r.Context(), "capture", "success")
	h.common.writeSnapshot(w, settled.Response, false)
}

func (h *ChatHandler) chatBillingTelemetry(ctx context.Context, transition, outcome string) {
	if h.telemetry != nil {
		h.telemetry.Billing(ctx, telemetry.BillingRecord{Protocol: "openai", Operation: chatoperation.Completions, Transition: transition, Outcome: outcome})
	}
}
func (h *ChatHandler) chatStreamTelemetry(ctx context.Context, category, side string, firstByte, duration time.Duration) {
	if h.telemetry != nil {
		h.telemetry.ChatStream(ctx, telemetry.ChatStreamRecord{TerminalCategory: category, DisconnectSide: side, FirstByte: firstByte, Duration: duration})
	}
}
func copyStreamResponseHeaders(destination, source http.Header) {
	copyResponseHeaders(destination, source)
	for _, value := range source.Values("Cache-Control") {
		destination.Add("Cache-Control", value)
	}
}

func (h *ChatHandler) handleEarlyChatReplay(w http.ResponseWriter, r *http.Request, principal apikey.Principal, model string, stream bool, body []byte) bool {
	key, err := idempotency.Extract(r.Header)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_idempotency_key", "idempotency key is invalid")
		return true
	}
	if key == "" {
		return false
	}
	maximumOutput, err := extractOutputLimit(body)
	if err != nil {
		return false
	}
	delivery, media := "non_stream", "application/json"
	if stream {
		delivery, media = "stream", "text/event-stream"
	}
	fingerprint := idempotency.Fingerprint("openai", chatoperation.Completions, model, "logical-route", media, body)
	request := chatbilling.BeginRequest{RequestID: requestid.FromContext(r.Context()), OrganizationID: principal.OrganizationID, ProjectID: principal.ProjectID, APIKeyID: principal.APIKeyID, Protocol: "openai", Operation: chatoperation.Completions, Model: model, ChannelID: "channel_00000000000000000000000000000001", IdempotencyKey: key, Fingerprint: fingerprint, MaximumInputTokens: int64(len(body)), MaximumOutputTokens: maximumOutput, DeliveryMode: delivery}
	replayed, found, replayErr := h.billing.Replay(r.Context(), request)
	if replayErr != nil {
		h.chatBillingTelemetry(r.Context(), "replay", "failure")
		h.writeChatBillingError(w, replayErr)
		return true
	}
	if !found {
		return false
	}
	if stream {
		h.writeChatBillingError(w, chatbilling.ErrConflict)
		return true
	}
	h.chatBillingTelemetry(r.Context(), "replay", "replay")
	h.common.writeSnapshot(w, replayed.Response, true)
	return true
}

func (h *ChatHandler) chatCandidates(model string, requirements chatoperation.Requirements) ([]chatoperation.Model, error) {
	if routed, ok := h.models.(RoutedChatRegistry); ok {
		return routed.Candidates(model, requirements)
	}
	route, err := h.models.Resolve(model)
	if err != nil {
		return nil, err
	}
	if (requirements.Streaming && !route.Capabilities.Streaming) || (requirements.Tools && !route.Capabilities.Tools) || (requirements.JSONMode && !route.Capabilities.JSONMode) {
		return nil, chatoperation.ErrUnsupported
	}
	return []chatoperation.Model{route}, nil
}

func (h *ChatHandler) preflightCandidates(r *http.Request, candidates []chatoperation.Model) []chatoperation.Model {
	result := make([]chatoperation.Model, 0, len(candidates))
	for _, candidate := range candidates {
		if h.executors[candidate.Provider] == nil {
			h.chatRouteTelemetry(r.Context(), candidate, "failure", "executor_unavailable")
			continue
		}
		if h.availability == nil || !h.availability.ConfiguredChannel(r.Context(), candidate.ChannelID, candidate.Provider) {
			h.chatRouteTelemetry(r.Context(), candidate, "failure", "credential_unavailable")
			continue
		}
		snapshot, err := h.health.Inspect(r.Context(), candidate.ChannelID)
		if err != nil {
			h.chatRouteTelemetry(r.Context(), candidate, "failure", "health_unavailable")
			continue
		}
		if snapshot.State == providerhealth.Open {
			h.chatRouteTelemetry(r.Context(), candidate, "failure", "circuit_open")
			continue
		}
		result = append(result, candidate)
	}
	return result
}

func (h *ChatHandler) orderBillableCandidates(ctx context.Context, candidates []chatoperation.Model, maximumInput, maximumOutput int64) ([]chatoperation.Model, map[string]chatpricing.Estimate, time.Time) {
	evaluatedAt := time.Now().UTC()
	available := make([]chatoperation.Model, 0, len(candidates))
	quotes := make(map[string]chatpricing.Estimate, len(candidates))
	for _, candidate := range candidates {
		if candidate.MaximumInputTokens < maximumInput || candidate.MaximumOutputTokens < maximumOutput {
			continue
		}
		quote, err := h.billing.Quote(ctx, chatbilling.BeginRequest{Protocol: "openai", Operation: chatoperation.Completions, Model: candidate.ID, ChannelID: candidate.ChannelID, MaximumInputTokens: maximumInput, MaximumOutputTokens: maximumOutput, PriceEvaluatedAt: evaluatedAt})
		if err != nil {
			continue
		}
		quotes[candidate.CandidateID] = quote
		available = append(available, candidate)
	}
	if len(available) > 0 && available[0].Policy == chatoperation.LowestCost {
		ordered, err := chatoperation.OrderLowestCost(available, quotes)
		if err != nil {
			return nil, nil, evaluatedAt
		}
		available = ordered
	}
	return available, quotes, evaluatedAt
}

func (h *ChatHandler) beginCandidate(r *http.Request, principal apikey.Principal, candidates []chatoperation.Model, key string, fingerprint [32]byte, maximumInput, maximumOutput int64, delivery string, evaluatedAt time.Time) (chatoperation.Model, ChatExecutor, chatbilling.Charge, providerhealth.Permit, error) {
	var lastErr error = chatpricing.ErrUnavailable
	remaining := append([]chatoperation.Model(nil), candidates...)
	for rank := 0; len(remaining) > 0; rank++ {
		route := remaining[0]
		remaining = remaining[1:]
		permit, err := h.acquireCandidateHealth(r, route.ChannelID)
		if err != nil {
			lastErr = err
			remaining = h.resampleWeighted(route.Policy, remaining)
			continue
		}
		begin := routedBeginRequest(r, principal, route, key, fingerprint, maximumInput, maximumOutput, delivery, rank)
		begin.PriceEvaluatedAt = evaluatedAt
		charge, err := h.billing.Begin(r.Context(), begin)
		if err == nil {
			h.chatRouteTelemetry(r.Context(), route, "success", "")
			return route, h.executors[route.Provider], charge, permit, nil
		}
		_ = h.health.Release(context.WithoutCancel(r.Context()), permit)
		lastErr = err
		rejection := "price_unavailable"
		if errors.Is(err, spendcap.ErrExceeded) {
			rejection = "spend_cap_exhausted"
		}
		h.chatRouteTelemetry(r.Context(), route, "failure", rejection)
		if !errors.Is(err, spendcap.ErrExceeded) && !errors.Is(err, chatpricing.ErrUnavailable) && !errors.Is(err, chatpricing.ErrMargin) {
			return chatoperation.Model{}, nil, chatbilling.Charge{}, providerhealth.Permit{}, err
		}
		remaining = h.resampleWeighted(route.Policy, remaining)
	}
	return chatoperation.Model{}, nil, chatbilling.Charge{}, providerhealth.Permit{}, lastErr
}

func (h *ChatHandler) resampleWeighted(policy chatoperation.Policy, remaining []chatoperation.Model) []chatoperation.Model {
	if policy != chatoperation.Weighted || len(remaining) < 2 {
		return remaining
	}
	selected, err := h.weighted.Pick(remaining)
	if err != nil {
		return nil
	}
	return moveCandidateFirst(remaining, selected.CandidateID)
}

func (h *ChatHandler) selectBYOKCandidate(r *http.Request, candidates []chatoperation.Model) (chatoperation.Model, ChatExecutor, providerhealth.Permit, error) {
	var lastErr error = errors.New("provider unavailable")
	remaining := append([]chatoperation.Model(nil), candidates...)
	for len(remaining) > 0 {
		route := remaining[0]
		remaining = remaining[1:]
		permit, err := h.acquireCandidateHealth(r, route.ChannelID)
		if err != nil {
			lastErr = err
			h.chatRouteTelemetry(r.Context(), route, "failure", "circuit_unavailable")
			remaining = h.resampleWeighted(route.Policy, remaining)
			continue
		}
		h.chatRouteTelemetry(r.Context(), route, "success", "")
		return route, h.executors[route.Provider], permit, nil
	}
	return chatoperation.Model{}, nil, providerhealth.Permit{}, lastErr
}

func (h *ChatHandler) chatRouteTelemetry(ctx context.Context, route chatoperation.Model, outcome, rejection string) {
	if h.telemetry != nil {
		h.telemetry.Route(ctx, telemetry.RouteRecord{Protocol: "openai", Operation: chatoperation.Completions, Policy: string(route.Policy), Outcome: outcome, Rejection: rejection})
	}
}

func (h *ChatHandler) acquireCandidateHealth(r *http.Request, channel string) (providerhealth.Permit, error) {
	snapshot, err := h.health.Inspect(r.Context(), channel)
	if err != nil {
		return providerhealth.Permit{}, err
	}
	if snapshot.State == providerhealth.Open {
		return providerhealth.Permit{}, errors.New("circuit open")
	}
	if snapshot.State == providerhealth.HalfOpen {
		return h.health.ClaimProbe(r.Context(), channel, requestid.FromContext(r.Context()))
	}
	return providerhealth.Permit{ChannelID: channel}, nil
}

func routedBeginRequest(r *http.Request, principal apikey.Principal, route chatoperation.Model, key string, fingerprint [32]byte, maximumInput, maximumOutput int64, delivery string, rank int) chatbilling.BeginRequest {
	return chatbilling.BeginRequest{RequestID: requestid.FromContext(r.Context()), OrganizationID: principal.OrganizationID, ProjectID: principal.ProjectID, APIKeyID: principal.APIKeyID, Protocol: "openai", Operation: chatoperation.Completions, Model: route.ID, ChannelID: route.ChannelID, IdempotencyKey: key, Fingerprint: fingerprint, MaximumInputTokens: maximumInput, MaximumOutputTokens: maximumOutput, DeliveryMode: delivery, CandidateID: route.CandidateID, Provider: string(route.Provider), ProviderModel: route.ProviderModel, RoutingPolicy: string(route.Policy), RouteRank: rank}
}

func moveCandidateFirst(candidates []chatoperation.Model, id string) []chatoperation.Model {
	result := make([]chatoperation.Model, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.CandidateID == id {
			result = append(result, candidate)
			break
		}
	}
	for _, candidate := range candidates {
		if candidate.CandidateID != id {
			result = append(result, candidate)
		}
	}
	return result
}

func extractChatRequirements(body []byte, stream bool) (chatoperation.Requirements, error) {
	var object map[string]json.RawMessage
	if json.Unmarshal(body, &object) != nil {
		return chatoperation.Requirements{}, errors.New("invalid JSON")
	}
	requirements := chatoperation.Requirements{Streaming: stream}
	if raw, ok := object["tools"]; ok {
		var tools []json.RawMessage
		if json.Unmarshal(raw, &tools) != nil {
			return chatoperation.Requirements{}, errors.New("invalid tools")
		}
		requirements.Tools = len(tools) > 0
	}
	if raw, ok := object["tool_choice"]; ok && string(raw) != "null" {
		var choice string
		if json.Unmarshal(raw, &choice) == nil {
			if choice != "none" && choice != "auto" && choice != "required" {
				return chatoperation.Requirements{}, errors.New("unknown tool choice")
			}
			requirements.Tools = requirements.Tools || choice != "none"
		} else {
			var objectChoice map[string]json.RawMessage
			if json.Unmarshal(raw, &objectChoice) != nil {
				return chatoperation.Requirements{}, errors.New("invalid tool choice")
			}
			requirements.Tools = true
		}
	}
	if raw, ok := object["response_format"]; ok {
		var format struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &format) != nil {
			return chatoperation.Requirements{}, errors.New("invalid response format")
		}
		switch format.Type {
		case "text", "":
		case "json_object", "json_schema":
			requirements.JSONMode = true
		default:
			return chatoperation.Requirements{}, errors.New("unknown response format")
		}
	}
	return requirements, nil
}

// rewriteTopLevelModel replaces only the top-level model string bytes. It
// deliberately preserves every other byte, including unknown fields and their order.
func rewriteTopLevelModel(body []byte, providerModel string) ([]byte, error) {
	encoded, err := json.Marshal(providerModel)
	if err != nil {
		return nil, err
	}
	i := skipJSONSpace(body, 0)
	if i >= len(body) || body[i] != '{' {
		return nil, errors.New("invalid object")
	}
	i++
	for {
		i = skipJSONSpace(body, i)
		if i >= len(body) {
			return nil, errors.New("unterminated object")
		}
		if body[i] == '}' {
			break
		}
		keyStart := i
		keyEnd, err := scanJSONString(body, keyStart)
		if err != nil {
			return nil, err
		}
		var key string
		if json.Unmarshal(body[keyStart:keyEnd], &key) != nil {
			return nil, errors.New("invalid key")
		}
		i = skipJSONSpace(body, keyEnd)
		if i >= len(body) || body[i] != ':' {
			return nil, errors.New("missing colon")
		}
		i = skipJSONSpace(body, i+1)
		valueStart := i
		valueEnd, err := scanJSONValue(body, valueStart)
		if err != nil {
			return nil, err
		}
		if key == "model" {
			if valueStart >= len(body) || body[valueStart] != '"' {
				return nil, errors.New("model is not string")
			}
			result := make([]byte, 0, len(body)-valueEnd+valueStart+len(encoded))
			result = append(result, body[:valueStart]...)
			result = append(result, encoded...)
			result = append(result, body[valueEnd:]...)
			return result, nil
		}
		i = skipJSONSpace(body, valueEnd)
		if i < len(body) && body[i] == ',' {
			i++
			continue
		}
		if i < len(body) && body[i] == '}' {
			break
		}
		return nil, errors.New("invalid object separator")
	}
	return nil, errors.New("model missing")
}

func skipJSONSpace(body []byte, i int) int {
	for i < len(body) && (body[i] == ' ' || body[i] == '\n' || body[i] == '\r' || body[i] == '\t') {
		i++
	}
	return i
}
func scanJSONString(body []byte, start int) (int, error) {
	if start >= len(body) || body[start] != '"' {
		return 0, errors.New("string expected")
	}
	escaped := false
	for i := start + 1; i < len(body); i++ {
		if escaped {
			escaped = false
			continue
		}
		if body[i] == '\\' {
			escaped = true
			continue
		}
		if body[i] == '"' {
			return i + 1, nil
		}
		if body[i] < 0x20 {
			return 0, errors.New("invalid string")
		}
	}
	return 0, errors.New("unterminated string")
}
func scanJSONValue(body []byte, start int) (int, error) {
	if start >= len(body) {
		return 0, errors.New("value missing")
	}
	if body[start] == '"' {
		return scanJSONString(body, start)
	}
	if body[start] == '{' || body[start] == '[' {
		stack := []byte{body[start]}
		inString, escaped := false, false
		for i := start + 1; i < len(body); i++ {
			b := body[i]
			if inString {
				if escaped {
					escaped = false
				} else if b == '\\' {
					escaped = true
				} else if b == '"' {
					inString = false
				}
				continue
			}
			if b == '"' {
				inString = true
				continue
			}
			if b == '{' || b == '[' {
				stack = append(stack, b)
				continue
			}
			if b == '}' || b == ']' {
				top := stack[len(stack)-1]
				if (top == '{' && b != '}') || (top == '[' && b != ']') {
					return 0, errors.New("mismatched value")
				}
				stack = stack[:len(stack)-1]
				if len(stack) == 0 {
					return i + 1, nil
				}
			}
		}
		return 0, errors.New("unterminated value")
	}
	i := start
	for i < len(body) && body[i] != ',' && body[i] != '}' && body[i] != ']' && body[i] != ' ' && body[i] != '\n' && body[i] != '\r' && body[i] != '\t' {
		i++
	}
	if i == start {
		return 0, errors.New("empty value")
	}
	return i, nil
}

func extractOutputLimit(body []byte) (int64, error) {
	var object map[string]json.RawMessage
	if json.Unmarshal(body, &object) != nil {
		return 0, errors.New("invalid JSON")
	}
	modern, modernOK := object["max_completion_tokens"]
	legacy, legacyOK := object["max_tokens"]
	if modernOK == legacyOK {
		return 0, errors.New("exactly one output limit required")
	}
	raw := modern
	if legacyOK {
		raw = legacy
	}
	var value int64
	if json.Unmarshal(raw, &value) != nil || value < 1 {
		return 0, errors.New("invalid output limit")
	}
	return value, nil
}
func extractChatUsage(body []byte) (chatpricing.Usage, error) {
	var envelope struct {
		Usage *struct {
			Prompt     json.RawMessage `json:"prompt_tokens"`
			Completion json.RawMessage `json:"completion_tokens"`
			Details    *struct {
				Cached json.RawMessage `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if decoder.Decode(&envelope) != nil || decoder.Decode(&struct{}{}) != io.EOF || envelope.Usage == nil {
		return chatpricing.Usage{}, errors.New("missing usage")
	}
	parse := func(raw json.RawMessage, required bool) (int64, error) {
		if len(raw) == 0 {
			if required {
				return 0, errors.New("missing usage field")
			}
			return 0, nil
		}
		var value int64
		if json.Unmarshal(raw, &value) != nil || value < 0 {
			return 0, errors.New("invalid usage")
		}
		return value, nil
	}
	prompt, err := parse(envelope.Usage.Prompt, true)
	if err != nil {
		return chatpricing.Usage{}, err
	}
	completion, err := parse(envelope.Usage.Completion, true)
	if err != nil {
		return chatpricing.Usage{}, err
	}
	cached := int64(0)
	if envelope.Usage.Details != nil {
		cached, err = parse(envelope.Usage.Details.Cached, false)
		if err != nil {
			return chatpricing.Usage{}, err
		}
	}
	if cached > prompt {
		return chatpricing.Usage{}, errors.New("invalid cached usage")
	}
	return chatpricing.Usage{PromptTokens: prompt, CachedInputTokens: cached, CompletionTokens: completion}, nil
}
func (h *ChatHandler) writeChatBillingError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, chatbilling.ErrConflict):
		writeError(w, http.StatusConflict, "invalid_request_error", "idempotency_conflict", "idempotency key conflicts with an existing request")
	case errors.Is(err, chatbilling.ErrPending):
		writeError(w, http.StatusConflict, "invalid_request_error", "request_pending", "request is still pending")
	case errors.Is(err, ledger.ErrInsufficientFunds):
		writeError(w, http.StatusPaymentRequired, "insufficient_funds", "insufficient_funds", "insufficient funds")
	case errors.Is(err, chatpricing.ErrUnavailable), errors.Is(err, chatpricing.ErrMargin):
		writeError(w, http.StatusServiceUnavailable, "server_error", "price_unavailable", "price unavailable")
	case errors.Is(err, spendcap.ErrExceeded):
		writeError(w, http.StatusServiceUnavailable, "server_error", "provider_unavailable", "provider unavailable")
	default:
		writeError(w, http.StatusServiceUnavailable, "server_error", "billing_unavailable", "billing unavailable")
	}
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
