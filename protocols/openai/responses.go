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
	responsesoperation "github.com/nativegatewayhq/gateway/operations/responses"
	openaiProvider "github.com/nativegatewayhq/gateway/providers/openai"
)

type ResponsesRegistry interface {
	Resolve(string) (responsesoperation.Model, error)
}
type RoutedResponsesRegistry interface {
	ResponsesRegistry
	Candidates(string, responsesoperation.Requirements) ([]responsesoperation.Model, error)
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
type ResponsesQuoter interface {
	Quote(context.Context, chatbilling.BeginRequest) (chatpricing.Estimate, error)
}
type ResponsesHandler struct {
	common           *Handler
	models           ResponsesRegistry
	executors        map[providercredentials.ProviderID]ResponsesExecutor
	availability     ChannelProviderAvailability
	health           providerhealth.Gate
	maximumBodyBytes int64
	telemetry        *telemetry.Recorder
	billing          ResponsesBilling
	weighted         *responsesoperation.WeightedSampler
}

func NewResponsesHandler(logger *slog.Logger, auth Authenticator, models ResponsesRegistry, executor ResponsesExecutor, availability ChannelProviderAvailability, maximum int64) *ResponsesHandler {
	return NewRoutedResponsesHandler(logger, auth, models, map[providercredentials.ProviderID]ResponsesExecutor{providercredentials.OpenAI: executor}, availability, providerhealth.NoopGate{}, maximum)
}
func NewBillableResponsesHandler(logger *slog.Logger, auth Authenticator, models ResponsesRegistry, executor ResponsesExecutor, availability ChannelProviderAvailability, maximum int64, chargeBilling ResponsesBilling) *ResponsesHandler {
	handler := NewResponsesHandler(logger, auth, models, executor, availability, maximum)
	handler.billing = chargeBilling
	return handler
}
func NewRoutedResponsesHandler(logger *slog.Logger, auth Authenticator, models ResponsesRegistry, executors map[providercredentials.ProviderID]ResponsesExecutor, availability ChannelProviderAvailability, health providerhealth.Gate, maximum int64) *ResponsesHandler {
	if health == nil {
		health = providerhealth.NoopGate{}
	}
	copyExecutors := make(map[providercredentials.ProviderID]ResponsesExecutor, len(executors))
	for provider, executor := range executors {
		if executor != nil {
			copyExecutors[provider] = executor
		}
	}
	return &ResponsesHandler{common: NewImagesHandler(logger, auth, nil, nil, 1), models: models, executors: copyExecutors, availability: availability, health: health, maximumBodyBytes: maximum, weighted: responsesoperation.DefaultWeightedSampler()}
}
func NewBillableRoutedResponsesHandler(logger *slog.Logger, auth Authenticator, models ResponsesRegistry, executors map[providercredentials.ProviderID]ResponsesExecutor, availability ChannelProviderAvailability, health providerhealth.Gate, maximum int64, chargeBilling ResponsesBilling) *ResponsesHandler {
	handler := NewRoutedResponsesHandler(logger, auth, models, executors, availability, health, maximum)
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
	if !h.common.authorizeModel(tracked, r, principal, "openai", responsesoperation.Create, model) {
		return
	}
	if h.billing != nil && h.handleEarlyResponsesReplay(tracked, r, principal, model, stream, body) {
		return
	}
	requirements, err := extractResponsesRequirements(body, stream)
	if err != nil {
		writeError(tracked, http.StatusBadRequest, "invalid_request_error", "unsupported_capability", "request requires an unsupported capability")
		return
	}
	candidates, err := h.responsesCandidates(model, requirements)
	if errors.Is(err, responsesoperation.ErrModelNotFound) {
		writeError(tracked, 404, "invalid_request_error", "model_not_found", "model not found")
		return
	}
	if err != nil {
		if errors.Is(err, responsesoperation.ErrUnsupported) {
			writeError(tracked, http.StatusBadRequest, "invalid_request_error", "unsupported_capability", "request requires an unsupported capability")
		} else {
			writeError(tracked, 503, "server_error", "provider_unavailable", "provider unavailable")
		}
		return
	}
	candidates = h.preflightResponsesCandidates(r, candidates)
	if len(candidates) == 0 {
		writeError(tracked, 503, "server_error", "provider_unavailable", "provider unavailable")
		return
	}
	if candidates[0].Policy == responsesoperation.Weighted {
		selected, sampleErr := h.weighted.Pick(candidates)
		if sampleErr != nil {
			writeError(tracked, 503, "server_error", "provider_unavailable", "provider unavailable")
			return
		}
		candidates = moveResponsesCandidateFirst(candidates, selected.CandidateID)
	}
	if stream {
		if _, ok := tracked.ResponseWriter.(http.Flusher); !ok {
			writeError(tracked, http.StatusInternalServerError, "server_error", "streaming_unavailable", "streaming unavailable")
			return
		}
		if h.billing != nil {
			h.serveBillableStreamCandidates(tracked, r, principal, candidates, body)
		} else {
			route, executor, permit, selectErr := h.selectResponsesCandidate(r, candidates)
			if selectErr != nil {
				writeError(tracked, 503, "server_error", "provider_unavailable", "provider unavailable")
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
	route, executor, permit, selectErr := h.selectResponsesCandidate(r, candidates)
	if selectErr != nil {
		writeError(tracked, 503, "server_error", "provider_unavailable", "provider unavailable")
		return
	}
	providerBody, rewriteErr := rewriteTopLevelModel(body, route.ProviderModel)
	if rewriteErr != nil {
		_ = h.health.Release(context.WithoutCancel(r.Context()), permit)
		writeError(tracked, 400, "invalid_request_error", "invalid_request", "invalid model field")
		return
	}
	response, err := h.execute(r.Context(), route, executor, openaiProvider.ResponsesRequest{ChannelID: route.ChannelID, ContentType: r.Header.Get("Content-Type"), Accept: r.Header.Get("Accept"), UserAgent: r.UserAgent(), ContentLength: int64(len(providerBody)), Body: bytes.NewReader(providerBody)})
	h.observeResponses(r, permit, response, err)
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

func (h *ResponsesHandler) serveBillableCandidates(w http.ResponseWriter, r *http.Request, principal apikey.Principal, candidates []responsesoperation.Model, body []byte) {
	maximumOutput, err := extractResponsesOutputLimit(body)
	if err != nil {
		writeError(w, 400, "invalid_request_error", "invalid_token_limit", "paid Responses requires valid input and output token limits")
		return
	}
	ordered, evaluatedAt := h.orderResponsesCandidates(r.Context(), candidates, int64(len(body)), maximumOutput)
	if len(ordered) == 0 {
		writeError(w, 503, "server_error", "price_unavailable", "price unavailable")
		return
	}
	key, keyErr := idempotency.Extract(r.Header)
	if keyErr != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_idempotency_key", "idempotency key is invalid")
		return
	}
	var fingerprint [32]byte
	if key != "" {
		fingerprint = idempotency.Fingerprint("openai", responsesoperation.Create, ordered[0].ID, "logical-route", "application/json", body)
	}
	route, executor, charge, permit, err := h.beginResponsesCandidate(r, principal, ordered, key, fingerprint, int64(len(body)), maximumOutput, "non_stream", evaluatedAt)
	if err != nil {
		h.writeBillingError(w, err)
		return
	}
	h.serveBillable(w, r, route, executor, charge, permit, body)
}

func (h *ResponsesHandler) serveBillableStreamCandidates(w http.ResponseWriter, r *http.Request, principal apikey.Principal, candidates []responsesoperation.Model, body []byte) {
	maximumOutput, err := extractResponsesOutputLimit(body)
	if err != nil {
		writeError(w, 400, "invalid_request_error", "invalid_token_limit", "paid Responses requires valid input and output token limits")
		return
	}
	ordered, evaluatedAt := h.orderResponsesCandidates(r.Context(), candidates, int64(len(body)), maximumOutput)
	if len(ordered) == 0 {
		writeError(w, 503, "server_error", "price_unavailable", "price unavailable")
		return
	}
	key, keyErr := idempotency.Extract(r.Header)
	if keyErr != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_idempotency_key", "idempotency key is invalid")
		return
	}
	var fingerprint [32]byte
	if key != "" {
		fingerprint = idempotency.Fingerprint("openai", responsesoperation.Create, ordered[0].ID, "logical-route", "text/event-stream", body)
	}
	route, executor, charge, permit, err := h.beginResponsesCandidate(r, principal, ordered, key, fingerprint, int64(len(body)), maximumOutput, "stream", evaluatedAt)
	if err != nil {
		h.writeBillingError(w, err)
		return
	}
	h.serveBillableStream(w, r, route, executor, charge, permit, body)
}

func (h *ResponsesHandler) beginResponsesCandidate(r *http.Request, principal apikey.Principal, candidates []responsesoperation.Model, key string, fingerprint [32]byte, maximumInput, maximumOutput int64, delivery string, evaluatedAt time.Time) (responsesoperation.Model, ResponsesExecutor, chatbilling.Charge, providerhealth.Permit, error) {
	lastErr := error(chatpricing.ErrUnavailable)
	remaining := append([]responsesoperation.Model(nil), candidates...)
	for rank := 0; len(remaining) > 0; rank++ {
		route := remaining[0]
		remaining = remaining[1:]
		permit, err := h.acquireResponsesHealth(r, route.ChannelID)
		if err != nil {
			lastErr = err
			remaining = h.resampleResponses(route.Policy, remaining)
			continue
		}
		begin := routedResponsesBeginRequest(r, principal, route, key, fingerprint, maximumInput, maximumOutput, delivery, rank)
		begin.PriceEvaluatedAt = evaluatedAt
		charge, err := h.billing.Begin(r.Context(), begin)
		if err == nil {
			h.responsesRouteTelemetry(r.Context(), route, "success", "")
			return route, h.executors[route.Provider], charge, permit, nil
		}
		_ = h.health.Release(context.WithoutCancel(r.Context()), permit)
		lastErr = err
		if !errors.Is(err, spendcap.ErrExceeded) && !errors.Is(err, chatpricing.ErrUnavailable) && !errors.Is(err, chatpricing.ErrMargin) {
			return responsesoperation.Model{}, nil, chatbilling.Charge{}, providerhealth.Permit{}, err
		}
		remaining = h.resampleResponses(route.Policy, remaining)
	}
	return responsesoperation.Model{}, nil, chatbilling.Charge{}, providerhealth.Permit{}, lastErr
}

func (h *ResponsesHandler) resampleResponses(policy responsesoperation.Policy, remaining []responsesoperation.Model) []responsesoperation.Model {
	if policy != responsesoperation.Weighted || len(remaining) < 2 {
		return remaining
	}
	selected, err := h.weighted.Pick(remaining)
	if err != nil {
		return nil
	}
	return moveResponsesCandidateFirst(remaining, selected.CandidateID)
}

func (h *ResponsesHandler) responsesCandidates(model string, requirements responsesoperation.Requirements) ([]responsesoperation.Model, error) {
	if h.models == nil {
		return nil, responsesoperation.ErrModelNotFound
	}
	if routed, ok := h.models.(RoutedResponsesRegistry); ok {
		return routed.Candidates(model, requirements)
	}
	route, err := h.models.Resolve(model)
	if err != nil {
		return nil, err
	}
	return []responsesoperation.Model{route}, nil
}

func (h *ResponsesHandler) preflightResponsesCandidates(r *http.Request, candidates []responsesoperation.Model) []responsesoperation.Model {
	result := make([]responsesoperation.Model, 0, len(candidates))
	for _, candidate := range candidates {
		if h.executors[candidate.Provider] == nil || h.availability == nil || !h.availability.ConfiguredChannel(r.Context(), candidate.ChannelID, candidate.Provider) {
			h.responsesRouteTelemetry(r.Context(), candidate, "failure", "unavailable")
			continue
		}
		snapshot, err := h.health.Inspect(r.Context(), candidate.ChannelID)
		if err != nil || snapshot.State == providerhealth.Open {
			h.responsesRouteTelemetry(r.Context(), candidate, "failure", "circuit_unavailable")
			continue
		}
		result = append(result, candidate)
	}
	return result
}

func (h *ResponsesHandler) orderResponsesCandidates(ctx context.Context, candidates []responsesoperation.Model, maximumInput, maximumOutput int64) ([]responsesoperation.Model, time.Time) {
	evaluatedAt := time.Now().UTC()
	quoter, canQuote := h.billing.(ResponsesQuoter)
	available := make([]responsesoperation.Model, 0, len(candidates))
	quotes := make(map[string]chatpricing.Estimate, len(candidates))
	for _, candidate := range candidates {
		if candidate.MaximumInputTokens < maximumInput || candidate.MaximumOutputTokens < maximumOutput {
			continue
		}
		if !canQuote {
			available = append(available, candidate)
			continue
		}
		quote, err := quoter.Quote(ctx, chatbilling.BeginRequest{Protocol: "openai", Operation: responsesoperation.Create, Model: candidate.ID, ChannelID: candidate.ChannelID, MaximumInputTokens: maximumInput, MaximumOutputTokens: maximumOutput, PriceEvaluatedAt: evaluatedAt})
		if err != nil {
			continue
		}
		quotes[candidate.CandidateID], available = quote, append(available, candidate)
	}
	if len(available) > 0 && available[0].Policy == responsesoperation.LowestCost {
		ordered, err := responsesoperation.OrderLowestCost(available, quotes)
		if err != nil {
			return nil, evaluatedAt
		}
		available = ordered
	}
	return available, evaluatedAt
}

func (h *ResponsesHandler) selectResponsesCandidate(r *http.Request, candidates []responsesoperation.Model) (responsesoperation.Model, ResponsesExecutor, providerhealth.Permit, error) {
	remaining := append([]responsesoperation.Model(nil), candidates...)
	for len(remaining) > 0 {
		route := remaining[0]
		remaining = remaining[1:]
		permit, err := h.acquireResponsesHealth(r, route.ChannelID)
		if err == nil {
			h.responsesRouteTelemetry(r.Context(), route, "success", "")
			return route, h.executors[route.Provider], permit, nil
		}
		if route.Policy == responsesoperation.Weighted && len(remaining) > 1 {
			selected, sampleErr := h.weighted.Pick(remaining)
			if sampleErr != nil {
				break
			}
			remaining = moveResponsesCandidateFirst(remaining, selected.CandidateID)
		}
	}
	return responsesoperation.Model{}, nil, providerhealth.Permit{}, errors.New("provider unavailable")
}

func (h *ResponsesHandler) acquireResponsesHealth(r *http.Request, channel string) (providerhealth.Permit, error) {
	snapshot, err := h.health.Inspect(r.Context(), channel)
	if err != nil || snapshot.State == providerhealth.Open {
		return providerhealth.Permit{}, errors.New("circuit unavailable")
	}
	if snapshot.State == providerhealth.HalfOpen {
		return h.health.ClaimProbe(r.Context(), channel, requestid.FromContext(r.Context()))
	}
	return providerhealth.Permit{ChannelID: channel}, nil
}

func (h *ResponsesHandler) observeResponses(r *http.Request, permit providerhealth.Permit, response *http.Response, err error) {
	outcome := providerhealth.Neutral
	switch {
	case errors.Is(err, openaiProvider.ErrResponsesTimeout):
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

func (h *ResponsesHandler) responsesRouteTelemetry(ctx context.Context, route responsesoperation.Model, outcome, rejection string) {
	if h.telemetry != nil {
		h.telemetry.Route(ctx, telemetry.RouteRecord{Protocol: "openai", Operation: responsesoperation.Create, Policy: string(route.Policy), Outcome: outcome, Rejection: rejection})
	}
}

func moveResponsesCandidateFirst(candidates []responsesoperation.Model, id string) []responsesoperation.Model {
	result := make([]responsesoperation.Model, 0, len(candidates))
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

func routedResponsesBeginRequest(r *http.Request, principal apikey.Principal, route responsesoperation.Model, key string, fingerprint [32]byte, maximumInput, maximumOutput int64, delivery string, rank int) chatbilling.BeginRequest {
	return chatbilling.BeginRequest{RequestID: requestid.FromContext(r.Context()), OrganizationID: principal.OrganizationID, ProjectID: principal.ProjectID, APIKeyID: principal.APIKeyID, Protocol: "openai", Operation: responsesoperation.Create, Model: route.ID, ChannelID: route.ChannelID, IdempotencyKey: key, Fingerprint: fingerprint, MaximumInputTokens: maximumInput, MaximumOutputTokens: maximumOutput, DeliveryMode: delivery, CandidateID: route.CandidateID, Provider: string(route.Provider), ProviderModel: route.ProviderModel, RoutingPolicy: string(route.Policy), RouteRank: rank}
}

func (h *ResponsesHandler) serveBYOKStream(w http.ResponseWriter, r *http.Request, route responsesoperation.Model, executor ResponsesExecutor, permit providerhealth.Permit, body []byte) {
	providerBody, rewriteErr := rewriteTopLevelModel(body, route.ProviderModel)
	if rewriteErr != nil {
		_ = h.health.Release(context.WithoutCancel(r.Context()), permit)
		writeError(w, 400, "invalid_request_error", "invalid_request", "invalid model field")
		return
	}
	response, err := h.execute(r.Context(), route, executor, openaiProvider.ResponsesRequest{ChannelID: route.ChannelID, ContentType: r.Header.Get("Content-Type"), Accept: "text/event-stream", UserAgent: r.UserAgent(), ContentLength: int64(len(providerBody)), Body: bytes.NewReader(providerBody), Streaming: true})
	h.observeResponses(r, permit, response, err)
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

func (h *ResponsesHandler) serveBillableStream(w http.ResponseWriter, r *http.Request, route responsesoperation.Model, executor ResponsesExecutor, charge chatbilling.Charge, permit providerhealth.Permit, body []byte) {
	maximumOutput, err := extractResponsesOutputLimit(body)
	if err != nil || route.MaximumInputTokens < 1 || route.MaximumOutputTokens < 1 || maximumOutput > route.MaximumOutputTokens || int64(len(body)) > route.MaximumInputTokens {
		_ = h.health.Release(context.WithoutCancel(r.Context()), permit)
		_, _ = h.billing.Release(context.WithoutCancel(r.Context()), charge.ID, billing.ResponseSnapshot{Status: http.StatusBadRequest, Body: []byte(`{"error":{"message":"invalid token limit","type":"invalid_request_error","code":"invalid_token_limit"}}`)})
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_token_limit", "paid Responses requires valid input and output token limits")
		return
	}
	h.billingTelemetry(r.Context(), "begin", "success")
	providerBody, rewriteErr := rewriteTopLevelModel(body, route.ProviderModel)
	if rewriteErr != nil {
		_ = h.health.Release(context.WithoutCancel(r.Context()), permit)
		_ = h.billing.MarkStreamReconciling(context.WithoutCancel(r.Context()), charge.ID, "stream_protocol_invalid", "gateway", "invalid_request")
		return
	}
	response, executeErr := h.execute(r.Context(), route, executor, openaiProvider.ResponsesRequest{ChannelID: route.ChannelID, ContentType: r.Header.Get("Content-Type"), Accept: "text/event-stream", UserAgent: r.UserAgent(), ContentLength: int64(len(providerBody)), Body: bytes.NewReader(providerBody), Streaming: true})
	h.observeResponses(r, permit, response, executeErr)
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

func (h *ResponsesHandler) serveBillable(w http.ResponseWriter, r *http.Request, route responsesoperation.Model, executor ResponsesExecutor, charge chatbilling.Charge, permit providerhealth.Permit, body []byte) {
	maximumOutput, err := extractResponsesOutputLimit(body)
	if err != nil || route.MaximumInputTokens < 1 || route.MaximumOutputTokens < 1 || maximumOutput > route.MaximumOutputTokens || int64(len(body)) > route.MaximumInputTokens {
		_ = h.health.Release(context.WithoutCancel(r.Context()), permit)
		_, _ = h.billing.Release(context.WithoutCancel(r.Context()), charge.ID, billing.ResponseSnapshot{Status: http.StatusBadRequest, Body: []byte(`{"error":{"message":"invalid token limit","type":"invalid_request_error","code":"invalid_token_limit"}}`)})
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_token_limit", "paid Responses requires valid input and output token limits")
		return
	}
	h.billingTelemetry(r.Context(), "begin", "success")
	if charge.Replay {
		_ = h.health.Release(context.WithoutCancel(r.Context()), permit)
		h.common.writeSnapshot(w, charge.Response, true)
		return
	}
	providerBody, rewriteErr := rewriteTopLevelModel(body, route.ProviderModel)
	if rewriteErr != nil {
		_ = h.health.Release(context.WithoutCancel(r.Context()), permit)
		_ = h.billing.MarkReconciling(context.WithoutCancel(r.Context()), charge.ID, "usage_invalid", nil)
		return
	}
	response, executeErr := h.execute(r.Context(), route, executor, openaiProvider.ResponsesRequest{ChannelID: route.ChannelID, ContentType: r.Header.Get("Content-Type"), Accept: r.Header.Get("Accept"), UserAgent: r.UserAgent(), ContentLength: int64(len(providerBody)), Body: bytes.NewReader(providerBody)})
	h.observeResponses(r, permit, response, executeErr)
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

func (h *ResponsesHandler) handleEarlyResponsesReplay(w http.ResponseWriter, r *http.Request, principal apikey.Principal, model string, stream bool, body []byte) bool {
	key, err := idempotency.Extract(r.Header)
	if err != nil {
		writeError(w, 400, "invalid_request_error", "invalid_idempotency_key", "idempotency key is invalid")
		return true
	}
	if key == "" {
		return false
	}
	maximumOutput, err := extractResponsesOutputLimit(body)
	if err != nil {
		return false
	}
	delivery, media := "non_stream", "application/json"
	if stream {
		delivery, media = "stream", "text/event-stream"
	}
	fingerprint := idempotency.Fingerprint("openai", responsesoperation.Create, model, "logical-route", media, body)
	request := chatbilling.BeginRequest{RequestID: requestid.FromContext(r.Context()), OrganizationID: principal.OrganizationID, ProjectID: principal.ProjectID, APIKeyID: principal.APIKeyID, Protocol: "openai", Operation: responsesoperation.Create, Model: model, ChannelID: "channel_00000000000000000000000000000001", IdempotencyKey: key, Fingerprint: fingerprint, MaximumInputTokens: int64(len(body)), MaximumOutputTokens: maximumOutput, DeliveryMode: delivery}
	replayed, found, replayErr := h.billing.Replay(r.Context(), request)
	if replayErr != nil {
		h.writeBillingError(w, replayErr)
		return true
	}
	if !found {
		return false
	}
	if stream {
		h.writeBillingError(w, chatbilling.ErrConflict)
		return true
	}
	h.common.writeSnapshot(w, replayed.Response, true)
	return true
}

func extractResponsesRequirements(body []byte, stream bool) (responsesoperation.Requirements, error) {
	var object map[string]json.RawMessage
	if json.Unmarshal(body, &object) != nil {
		return responsesoperation.Requirements{}, errors.New("invalid JSON")
	}
	requirements := responsesoperation.Requirements{Streaming: stream}
	if raw, ok := object["background"]; ok {
		var enabled bool
		if json.Unmarshal(raw, &enabled) != nil || enabled {
			return requirements, errors.New("background unsupported")
		}
	}
	if raw, ok := object["previous_response_id"]; ok && string(raw) != "null" {
		var value string
		if json.Unmarshal(raw, &value) != nil || value != "" {
			return requirements, errors.New("response affinity unsupported")
		}
	}
	if raw, ok := object["store"]; ok {
		var enabled bool
		if json.Unmarshal(raw, &enabled) != nil {
			return requirements, errors.New("invalid store")
		}
		requirements.StoredResponse = enabled
	}
	if raw, ok := object["tools"]; ok {
		var tools []struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &tools) != nil {
			return requirements, errors.New("invalid tools")
		}
		for _, tool := range tools {
			switch tool.Type {
			case "function":
				requirements.FunctionTools = true
			case "web_search", "web_search_preview":
				requirements.WebSearch = true
			case "x_search":
				requirements.XSearch = true
			case "code_interpreter":
				requirements.CodeInterpreter = true
			case "image_generation":
				requirements.ImageGeneration = true
			default:
				return requirements, errors.New("unknown tool")
			}
		}
	}
	if raw, ok := object["text"]; ok && string(raw) != "null" {
		var textObject struct {
			Format *struct {
				Type string `json:"type"`
			} `json:"format"`
		}
		if json.Unmarshal(raw, &textObject) != nil {
			return requirements, errors.New("invalid text format")
		}
		if textObject.Format != nil {
			switch textObject.Format.Type {
			case "", "text":
			case "json_object", "json_schema":
				requirements.JSONMode = true
			default:
				return requirements, errors.New("unknown text format")
			}
		}
	}
	return requirements, nil
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
