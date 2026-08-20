// Package openai implements the OpenAI native protocol facade.
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
	"github.com/nativegatewayhq/gateway/internal/idempotency"
	"github.com/nativegatewayhq/gateway/internal/ledger"
	"github.com/nativegatewayhq/gateway/internal/pricing"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	"github.com/nativegatewayhq/gateway/internal/requestid"
	imageoperation "github.com/nativegatewayhq/gateway/operations/image"
	"github.com/nativegatewayhq/gateway/providers/openaiimages"
)

var errBodyTooLarge = errors.New("request body too large")

type Authenticator interface {
	Authenticate(context.Context, string) (apikey.Principal, error)
}

type ModelRegistry interface {
	Resolve(string, imageoperation.Operation, imageoperation.MediaType) (imageoperation.RoutingDecision, error)
	Candidates(string, string, imageoperation.Operation, imageoperation.MediaType) ([]imageoperation.RoutingDecision, error)
	List() []imageoperation.ModelRoute
}

type Executor interface {
	Generate(context.Context, openaiimages.Request) (*http.Response, error)
}

type Billing interface {
	Begin(context.Context, billing.BeginRequest) (billing.Charge, error)
	Replay(context.Context, billing.BeginRequest) (billing.Charge, bool, error)
	Quote(context.Context, billing.BeginRequest) (pricing.Estimate, error)
	Complete(context.Context, string, bool, billing.ResponseSnapshot) (billing.Charge, error)
	MarkReconciling(context.Context, string, billing.Observation) error
	MaximumResponseBytes() int64
}

type Handler struct {
	logger        *slog.Logger
	authenticator Authenticator
	models        ModelRegistry
	executors     map[providercredentials.ProviderID]Executor
	maxBodyBytes  int64
	billing       Billing
	availability  ProviderAvailability
}

func NewBillableImagesHandler(logger *slog.Logger, authenticator Authenticator, models ModelRegistry, executors map[providercredentials.ProviderID]Executor, maxBodyBytes int64, chargeBilling Billing) *Handler {
	return NewBillableImagesHandlerWithAvailability(logger, authenticator, models, executors, maxBodyBytes, chargeBilling, nil)
}

func NewBillableImagesHandlerWithAvailability(logger *slog.Logger, authenticator Authenticator, models ModelRegistry, executors map[providercredentials.ProviderID]Executor, maxBodyBytes int64, chargeBilling Billing, availability ProviderAvailability) *Handler {
	handler := NewImagesHandler(logger, authenticator, models, executors, maxBodyBytes)
	handler.billing = chargeBilling
	handler.availability = availability
	return handler
}

func NewImagesHandler(logger *slog.Logger, authenticator Authenticator, models ModelRegistry, executors map[providercredentials.ProviderID]Executor, maxBodyBytes int64) *Handler {
	cloned := make(map[providercredentials.ProviderID]Executor, len(executors))
	for provider, executor := range executors {
		cloned[provider] = executor
	}
	return &Handler{logger: logger, authenticator: authenticator, models: models, executors: cloned, maxBodyBytes: maxBodyBytes}
}

func (handler *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	tracked := &statusWriter{ResponseWriter: writer}
	started := time.Now()
	model := "invalid"
	logModel := "invalid"
	provider := providercredentials.ProviderID("")
	candidateID, channelID, routingPolicy := "", "", ""
	fallbackDepth := 0
	var charge *billing.Charge
	defer func() {
		if recover() != nil {
			if charge != nil {
				handler.reconciliationError(tracked, request.Context(), charge.ID, billing.Observation{Outcome: billing.Unknown, Reason: billing.ProviderPanic})
			} else if !tracked.wroteHeader {
				writeError(tracked, http.StatusInternalServerError, "server_error", "internal_error", "internal server error")
			}
			handler.logger.Error("openai image request panic recovered", "request_id", requestid.FromContext(request.Context()))
		}
		handler.logger.Info("openai image request completed",
			"request_id", requestid.FromContext(request.Context()),
			"protocol", "openai",
			"operation", "image.generate",
			"provider", string(provider),
			"candidate_id", candidateID,
			"channel_id", channelID,
			"routing_policy", routingPolicy,
			"fallback_depth", fallbackDepth,
			"model", logModel,
			"status", tracked.statusCode(),
			"duration", time.Since(started),
		)
	}()

	if request.Method != http.MethodPost {
		tracked.Header().Set("Allow", http.MethodPost)
		writeError(tracked, http.StatusMethodNotAllowed, "invalid_request_error", "method_not_allowed", "method not allowed")
		return
	}
	principal, authenticated := handler.authenticate(tracked, request)
	if !authenticated {
		return
	}
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		writeError(tracked, http.StatusBadRequest, "invalid_request_error", "invalid_content_type", "content type must be application/json")
		return
	}
	if handler.maxBodyBytes <= 0 || request.ContentLength > handler.maxBodyBytes {
		writeError(tracked, http.StatusRequestEntityTooLarge, "invalid_request_error", "request_too_large", "request body too large")
		return
	}
	body, err := readBounded(request.Body, handler.maxBodyBytes)
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			writeError(tracked, http.StatusRequestEntityTooLarge, "invalid_request_error", "request_too_large", "request body too large")
			return
		}
		if errors.Is(request.Context().Err(), context.Canceled) {
			writeError(tracked, 499, "server_error", "request_canceled", "request canceled")
			return
		}
		writeError(tracked, http.StatusBadRequest, "invalid_request_error", "invalid_request", "could not read request body")
		return
	}
	model, err = extractModel(body)
	if err != nil {
		model = "invalid"
		writeError(tracked, http.StatusBadRequest, "invalid_request_error", "invalid_model", "request must contain one model")
		return
	}
	if handler.models == nil {
		writeError(tracked, http.StatusServiceUnavailable, "server_error", "provider_unavailable", "provider unavailable")
		return
	}
	candidates, err := handler.models.Candidates("openai", model, imageoperation.Generate, imageoperation.JSON)
	if err != nil {
		if errors.Is(err, imageoperation.ErrModelNotFound) {
			writeError(tracked, http.StatusNotFound, "invalid_request_error", "model_not_found", "model not found")
		} else {
			writeError(tracked, http.StatusBadRequest, "invalid_request_error", "unsupported_capability", "model does not support operation")
		}
		return
	}
	route := candidates[0]
	logModel = model
	if handler.billing != nil {
		selector, selectorErr := imageoperation.ParseOpenAIJSONPricingSelector(body)
		if selectorErr != nil {
			writeError(tracked, http.StatusBadRequest, "invalid_request_error", "invalid_pricing_selector", "request contains unsupported billing options")
			return
		}
		idempotencyKey, keyErr := idempotency.Extract(request.Header)
		if keyErr != nil {
			writeError(tracked, http.StatusBadRequest, "invalid_request_error", "invalid_idempotency_key", "idempotency key is invalid")
			return
		}
		var fingerprint [32]byte
		legacyFingerprints := make([][32]byte, 0, len(candidates))
		if idempotencyKey != "" {
			fingerprint = idempotency.Fingerprint("openai", string(imageoperation.Generate), selector.Model, "logical-route-v1", mediaType, body)
			for _, candidate := range candidates {
				legacyFingerprints = append(legacyFingerprints, idempotency.Fingerprint("openai", string(imageoperation.Generate), selector.Model, candidate.ChannelID, mediaType, body))
			}
		}
		base := billing.BeginRequest{RequestID: requestid.FromContext(request.Context()), OrganizationID: principal.OrganizationID, ProjectID: principal.ProjectID, Protocol: "openai", Operation: string(imageoperation.Generate), Model: selector.Model, ChannelID: candidates[0].ChannelID, Quantity: selector.Quantity, Size: selector.Size, Quality: selector.Quality, IdempotencyKey: idempotencyKey, RequestFingerprint: fingerprint, LegacyFingerprints: legacyFingerprints}
		replayed, found, replayErr := handler.billing.Replay(request.Context(), base)
		if replayErr != nil {
			handler.writeBillingError(tracked, replayErr)
			return
		}
		if found {
			for index, candidate := range candidates {
				if candidate.ChannelID == replayed.ChannelID {
					provider, candidateID, channelID, routingPolicy, fallbackDepth = candidate.Provider, candidate.CandidateID, candidate.ChannelID, string(candidate.Policy), index
					break
				}
			}
			handler.writeSnapshot(tracked, replayed.Response, true)
			return
		}
		selected := false
		for index, candidate := range candidates {
			if handler.executors[candidate.Provider] == nil {
				handler.logCandidateSkip(request, candidate, "executor_unavailable")
				continue
			}
			if !providerConfigured(handler.availability, candidate.Provider) {
				handler.logCandidateSkip(request, candidate, "credential_unavailable")
				continue
			}
			attempt := base
			attempt.ChannelID = candidate.ChannelID
			if _, quoteErr := handler.billing.Quote(request.Context(), attempt); quoteErr != nil {
				if errors.Is(quoteErr, pricing.ErrPriceUnavailable) || errors.Is(quoteErr, pricing.ErrMarginViolation) {
					handler.logCandidateSkip(request, candidate, "price_unavailable")
					continue
				}
				handler.writeBillingError(tracked, quoteErr)
				return
			}
			startedCharge, billingErr := handler.billing.Begin(request.Context(), attempt)
			if billingErr != nil {
				if errors.Is(billingErr, pricing.ErrPriceUnavailable) || errors.Is(billingErr, pricing.ErrMarginViolation) {
					handler.logCandidateSkip(request, candidate, "price_race_unavailable")
					continue
				}
				handler.writeBillingError(tracked, billingErr)
				return
			}
			if startedCharge.Replay {
				handler.writeSnapshot(tracked, startedCharge.Response, true)
				return
			}
			route, charge, fallbackDepth, selected = candidate, &startedCharge, index, true
			break
		}
		if !selected {
			writeError(tracked, http.StatusServiceUnavailable, "server_error", "provider_unavailable", "provider unavailable")
			return
		}
	}
	provider = route.Provider
	candidateID, channelID, routingPolicy = route.CandidateID, route.ChannelID, string(route.Policy)
	outboundBody, rewriteErr := imageoperation.RewriteJSONModel(body, route.ProviderModel)
	if rewriteErr != nil {
		writeError(tracked, http.StatusBadRequest, "invalid_request_error", "invalid_model", "request must contain one model")
		return
	}
	executor := handler.executors[provider]
	if executor == nil {
		writeError(tracked, http.StatusServiceUnavailable, "server_error", "provider_unavailable", "provider unavailable")
		return
	}
	response, err := executor.Generate(request.Context(), openaiimages.Request{
		ContentType: request.Header.Get("Content-Type"),
		Accept:      request.Header.Get("Accept"),
		UserAgent:   request.UserAgent(),
		Body:        bytes.NewReader(outboundBody),
	})
	if err != nil {
		if charge != nil {
			snapshot := handler.executorErrorSnapshot(err)
			if errors.Is(err, providercredentials.ErrCredentialUnavailable) {
				completed, completeErr := handler.complete(request.Context(), charge.ID, false, snapshot)
				if completeErr == nil {
					handler.writeSnapshot(tracked, completed.Response, false)
					return
				}
				handler.reconciliationError(tracked, request.Context(), charge.ID, knownObservation(false, billing.SettlementFailed, snapshot))
				return
			}
			reason := billing.ExecutorConnection
			if errors.Is(err, openaiimages.ErrTimeout) {
				reason = billing.ExecutorTimeout
			}
			handler.reconciliationError(tracked, request.Context(), charge.ID, billing.Observation{Outcome: billing.Unknown, Reason: reason})
			return
		}
		handler.writeExecutorError(tracked, err)
		return
	}
	defer response.Body.Close()
	if charge != nil {
		responseBody, readErr := readBounded(response.Body, handler.billing.MaximumResponseBytes())
		if readErr != nil {
			handler.reconciliationError(tracked, request.Context(), charge.ID, knownObservation(response.StatusCode >= 200 && response.StatusCode <= 299, billing.ResponseUnavailable, handler.responseUnavailableSnapshot()))
			return
		}
		snapshot := billing.ResponseSnapshot{Status: response.StatusCode, Headers: safeResponseHeaders(response.Header), Body: responseBody}
		completed, completeErr := handler.complete(request.Context(), charge.ID, response.StatusCode >= 200 && response.StatusCode <= 299, snapshot)
		if completeErr != nil {
			handler.reconciliationError(tracked, request.Context(), charge.ID, knownObservation(response.StatusCode >= 200 && response.StatusCode <= 299, billing.SettlementFailed, snapshot))
			return
		}
		handler.writeSnapshot(tracked, completed.Response, false)
		return
	}
	copyResponseHeaders(tracked.Header(), response.Header)
	tracked.WriteHeader(response.StatusCode)
	if _, err := io.Copy(tracked, response.Body); err != nil {
		handler.logger.Warn("openai image upstream response copy failed",
			"request_id", requestid.FromContext(request.Context()),
			"provider", string(provider),
			"category", "response_copy_failed",
		)
	}
}

func providerConfigured(availability ProviderAvailability, provider providercredentials.ProviderID) bool {
	if availability == nil {
		return true
	}
	for _, configured := range availability.ConfiguredProviders() {
		if configured == provider {
			return true
		}
	}
	return false
}

func (handler *Handler) logCandidateSkip(request *http.Request, decision imageoperation.RoutingDecision, category string) {
	handler.logger.Info("image routing candidate skipped", "request_id", requestid.FromContext(request.Context()), "model", decision.Model, "candidate_id", decision.CandidateID, "channel_id", decision.ChannelID, "provider", string(decision.Provider), "category", category)
}

func (handler *Handler) selectBillableCandidate(writer http.ResponseWriter, request *http.Request, candidates []imageoperation.RoutingDecision, base billing.BeginRequest) (imageoperation.RoutingDecision, *billing.Charge, int, bool) {
	replayed, found, replayErr := handler.billing.Replay(request.Context(), base)
	if replayErr != nil {
		handler.writeBillingError(writer, replayErr)
		return imageoperation.RoutingDecision{}, nil, 0, false
	}
	if found {
		handler.writeSnapshot(writer, replayed.Response, true)
		return imageoperation.RoutingDecision{}, nil, 0, false
	}
	for index, candidate := range candidates {
		if handler.executors[candidate.Provider] == nil {
			handler.logCandidateSkip(request, candidate, "executor_unavailable")
			continue
		}
		if !providerConfigured(handler.availability, candidate.Provider) {
			handler.logCandidateSkip(request, candidate, "credential_unavailable")
			continue
		}
		attempt := base
		attempt.ChannelID = candidate.ChannelID
		if _, quoteErr := handler.billing.Quote(request.Context(), attempt); quoteErr != nil {
			if errors.Is(quoteErr, pricing.ErrPriceUnavailable) || errors.Is(quoteErr, pricing.ErrMarginViolation) {
				handler.logCandidateSkip(request, candidate, "price_unavailable")
				continue
			}
			handler.writeBillingError(writer, quoteErr)
			return imageoperation.RoutingDecision{}, nil, 0, false
		}
		started, beginErr := handler.billing.Begin(request.Context(), attempt)
		if beginErr != nil {
			if errors.Is(beginErr, pricing.ErrPriceUnavailable) || errors.Is(beginErr, pricing.ErrMarginViolation) {
				handler.logCandidateSkip(request, candidate, "price_race_unavailable")
				continue
			}
			handler.writeBillingError(writer, beginErr)
			return imageoperation.RoutingDecision{}, nil, 0, false
		}
		if started.Replay {
			handler.writeSnapshot(writer, started.Response, true)
			return imageoperation.RoutingDecision{}, nil, 0, false
		}
		return candidate, &started, index, true
	}
	writeError(writer, http.StatusServiceUnavailable, "server_error", "provider_unavailable", "provider unavailable")
	return imageoperation.RoutingDecision{}, nil, 0, false
}

func (handler *Handler) authenticate(writer http.ResponseWriter, request *http.Request) (apikey.Principal, bool) {
	if handler.authenticator == nil {
		writeError(writer, http.StatusServiceUnavailable, "server_error", "authentication_unavailable", "authentication service unavailable")
		return apikey.Principal{}, false
	}
	raw, err := apikey.Extract(request)
	if err != nil {
		if errors.Is(err, apikey.ErrAmbiguous) {
			writeError(writer, http.StatusBadRequest, "invalid_request_error", "ambiguous_authentication", "multiple credential locations are not allowed")
			return apikey.Principal{}, false
		}
		writeError(writer, http.StatusUnauthorized, "invalid_request_error", "authentication_required", "authentication required")
		return apikey.Principal{}, false
	}
	principal, err := handler.authenticator.Authenticate(request.Context(), raw)
	if err != nil {
		if errors.Is(err, apikey.ErrUnavailable) {
			writeError(writer, http.StatusServiceUnavailable, "server_error", "authentication_unavailable", "authentication service unavailable")
			return apikey.Principal{}, false
		}
		writeError(writer, http.StatusUnauthorized, "invalid_request_error", "authentication_required", "authentication required")
		return apikey.Principal{}, false
	}
	return principal, true
}

func (handler *Handler) complete(ctx context.Context, chargeID string, success bool, snapshot billing.ResponseSnapshot) (billing.Charge, error) {
	settlementContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return handler.billing.Complete(settlementContext, chargeID, success, snapshot)
}

func (handler *Handler) reconciliationError(writer http.ResponseWriter, ctx context.Context, chargeID string, observation billing.Observation) {
	markContext, markCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer markCancel()
	_ = handler.billing.MarkReconciling(markContext, chargeID, observation)
	writeError(writer, http.StatusServiceUnavailable, "server_error", "billing_reconciliation_required", "billing settlement is pending")
}

func knownObservation(success bool, reason billing.Reason, snapshot billing.ResponseSnapshot) billing.Observation {
	outcome := billing.KnownFailure
	if success {
		outcome = billing.KnownSuccess
	}
	return billing.Observation{Outcome: outcome, Reason: reason, Snapshot: snapshot}
}

func (handler *Handler) responseUnavailableSnapshot() billing.ResponseSnapshot {
	body, _ := json.Marshal(errorEnvelope{Error: errorBody{Message: "provider response unavailable", Type: "server_error", Code: "provider_response_unavailable"}})
	return billing.ResponseSnapshot{Status: http.StatusBadGateway, Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: body}
}

func (handler *Handler) writeBillingError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ledger.ErrInsufficientFunds):
		writeError(writer, http.StatusPaymentRequired, "invalid_request_error", "insufficient_credits", "insufficient credits")
	case errors.Is(err, billing.ErrRequestConflict):
		writeError(writer, http.StatusConflict, "invalid_request_error", "idempotency_conflict", "idempotency key conflicts with another request")
	case errors.Is(err, billing.ErrRequestPending):
		writeError(writer, http.StatusConflict, "invalid_request_error", "idempotency_in_progress", "idempotent request is still in progress")
	case errors.Is(err, billing.ErrAlreadySettled):
		writeError(writer, http.StatusConflict, "invalid_request_error", "request_conflict", "request identifier is already settled")
	case errors.Is(err, billing.ErrInvalidRequest):
		writeError(writer, http.StatusBadRequest, "invalid_request_error", "invalid_billing_request", "request cannot be billed")
	case errors.Is(err, pricing.ErrPriceUnavailable), errors.Is(err, pricing.ErrMarginViolation), errors.Is(err, ledger.ErrTenantUnavailable):
		writeError(writer, http.StatusServiceUnavailable, "server_error", "billing_unavailable", "billing unavailable")
	default:
		writeError(writer, http.StatusServiceUnavailable, "server_error", "billing_unavailable", "billing unavailable")
	}
}

func (handler *Handler) executorErrorSnapshot(err error) billing.ResponseSnapshot {
	status, code, message := http.StatusInternalServerError, "internal_error", "internal server error"
	switch {
	case errors.Is(err, providercredentials.ErrCredentialUnavailable):
		status, code, message = http.StatusServiceUnavailable, "provider_unavailable", "provider unavailable"
	case errors.Is(err, openaiimages.ErrTimeout):
		status, code, message = http.StatusGatewayTimeout, "upstream_timeout", "provider request timed out"
	case errors.Is(err, openaiimages.ErrCanceled):
		status, code, message = 499, "request_canceled", "request canceled"
	case errors.Is(err, openaiimages.ErrUpstream):
		status, code, message = http.StatusBadGateway, "upstream_unavailable", "provider unavailable"
	}
	body, _ := json.Marshal(errorEnvelope{Error: errorBody{Message: message, Type: "server_error", Code: code}})
	return billing.ResponseSnapshot{Status: status, Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: body}
}

func (handler *Handler) writeSnapshot(writer http.ResponseWriter, snapshot billing.ResponseSnapshot, replay bool) {
	for key, values := range snapshot.Headers {
		for _, value := range values {
			writer.Header().Add(key, value)
		}
	}
	if replay {
		writer.Header().Set("Idempotency-Replayed", "true")
	}
	writer.WriteHeader(snapshot.Status)
	_, _ = writer.Write(snapshot.Body)
}

func safeResponseHeaders(source http.Header) map[string][]string {
	result := map[string][]string{}
	for _, key := range []string{"Content-Type", "Retry-After"} {
		if values := source.Values(key); len(values) > 0 {
			result[key] = append([]string(nil), values...)
		}
	}
	return result
}

func (handler *Handler) writeExecutorError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, providercredentials.ErrCredentialUnavailable):
		writeError(writer, http.StatusServiceUnavailable, "server_error", "provider_unavailable", "provider unavailable")
	case errors.Is(err, openaiimages.ErrTimeout):
		writeError(writer, http.StatusGatewayTimeout, "server_error", "upstream_timeout", "provider request timed out")
	case errors.Is(err, openaiimages.ErrCanceled):
		writeError(writer, 499, "server_error", "request_canceled", "request canceled")
	case errors.Is(err, openaiimages.ErrUpstream):
		writeError(writer, http.StatusBadGateway, "server_error", "upstream_unavailable", "provider unavailable")
	default:
		writeError(writer, http.StatusInternalServerError, "server_error", "internal_error", "internal server error")
	}
}

func extractModel(body []byte) (string, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return "", errors.New("request must be an object")
	}
	model := ""
	modelCount := 0
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return "", err
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return "", err
		}
		if key == "model" {
			modelCount++
			if err := json.Unmarshal(value, &model); err != nil {
				return "", err
			}
		}
	}
	if _, err := decoder.Token(); err != nil {
		return "", err
	}
	if decoder.Decode(&struct{}{}) != io.EOF || modelCount != 1 || strings.TrimSpace(model) == "" || model != strings.TrimSpace(model) || len(model) > 200 {
		return "", errors.New("invalid model")
	}
	return model, nil
}

func readBounded(body io.Reader, maximum int64) ([]byte, error) {
	if body == nil {
		return nil, nil
	}
	content, err := io.ReadAll(io.LimitReader(body, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(content)) > maximum {
		return nil, errBodyTooLarge
	}
	return content, nil
}

func copyResponseHeaders(destination, source http.Header) {
	for _, header := range []string{"Content-Type", "Retry-After"} {
		for _, value := range source.Values(header) {
			destination.Add(header, value)
		}
	}
}

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    string  `json:"code"`
}

func writeError(writer http.ResponseWriter, status int, errorType, code, message string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(errorEnvelope{Error: errorBody{Message: message, Type: errorType, Code: code}})
}

type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (writer *statusWriter) WriteHeader(status int) {
	if writer.wroteHeader {
		return
	}
	writer.status = status
	writer.wroteHeader = true
	writer.ResponseWriter.WriteHeader(status)
}

func (writer *statusWriter) Write(content []byte) (int, error) {
	if !writer.wroteHeader {
		writer.WriteHeader(http.StatusOK)
	}
	return writer.ResponseWriter.Write(content)
}

func (writer *statusWriter) statusCode() int {
	if writer.status == 0 {
		return http.StatusOK
	}
	return writer.status
}
