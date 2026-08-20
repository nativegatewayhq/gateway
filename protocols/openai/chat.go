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
type ChatBilling interface {
	Begin(context.Context, chatbilling.BeginRequest) (chatbilling.Charge, error)
	Replay(context.Context, chatbilling.BeginRequest) (chatbilling.Charge, bool, error)
	CompleteUsage(context.Context, string, chatpricing.Usage, billing.ResponseSnapshot) (chatbilling.Charge, error)
	Release(context.Context, string, billing.ResponseSnapshot) (chatbilling.Charge, error)
	MarkReconciling(context.Context, string, string, *billing.ResponseSnapshot) error
	MarkReconcilingUsage(context.Context, string, string, *billing.ResponseSnapshot, chatpricing.Usage) error
}

type ChatHandler struct {
	common           *Handler
	models           ChatRegistry
	executor         ChatExecutor
	availability     ChannelProviderAvailability
	health           providerhealth.Gate
	maximumBodyBytes int64
	telemetry        *telemetry.Recorder
	billing          ChatBilling
}

func NewBillableChatHandler(logger *slog.Logger, auth Authenticator, models ChatRegistry, executor ChatExecutor, availability ChannelProviderAvailability, health providerhealth.Gate, maximumBodyBytes int64, chargeBilling ChatBilling) *ChatHandler {
	handler := NewChatHandler(logger, auth, models, executor, availability, health, maximumBodyBytes)
	handler.billing = chargeBilling
	return handler
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
	if h.billing != nil {
		h.serveBillable(tracked, r, principal, route, body)
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

func (h *ChatHandler) serveBillable(w http.ResponseWriter, r *http.Request, principal apikey.Principal, route chatoperation.Model, body []byte) {
	maximumOutput, err := extractOutputLimit(body)
	if err != nil || route.MaximumInputTokens < 1 || route.MaximumOutputTokens < 1 || maximumOutput < 1 || maximumOutput > route.MaximumOutputTokens || int64(len(body)) > route.MaximumInputTokens {
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
		fingerprint = idempotency.Fingerprint("openai", chatoperation.Completions, route.ID, route.ChannelID, "application/json", body)
	}
	beginRequest := chatbilling.BeginRequest{RequestID: requestid.FromContext(r.Context()), OrganizationID: principal.OrganizationID, ProjectID: principal.ProjectID, APIKeyID: principal.APIKeyID, Model: route.ID, ChannelID: route.ChannelID, IdempotencyKey: key, Fingerprint: fingerprint, MaximumInputTokens: int64(len(body)), MaximumOutputTokens: maximumOutput}
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
	permit, ok := h.acquireHealth(w, r, route.ChannelID)
	if !ok {
		return
	}
	charge, beginErr := h.billing.Begin(r.Context(), beginRequest)
	if beginErr != nil {
		_ = h.health.Release(context.WithoutCancel(r.Context()), permit)
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
	response, executeErr := h.execute(r.Context(), route, h.executor, openaiProvider.ChatRequest{ChannelID: route.ChannelID, ContentType: r.Header.Get("Content-Type"), Accept: r.Header.Get("Accept"), UserAgent: r.UserAgent(), ContentLength: int64(len(body)), Body: bytes.NewReader(body)})
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
