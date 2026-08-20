// Package gemini implements the Gemini Developer API protocol facade.
package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
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
	"github.com/nativegatewayhq/gateway/providers/google"
)

const maxModelLength = 200

type Authenticator interface {
	Authenticate(context.Context, string) (apikey.Principal, error)
}

type Executor interface {
	GenerateContent(context.Context, google.GenerateContentRequest) (*http.Response, error)
}

type ModelRegistry interface {
	ResolveProtocol(string, string, imageoperation.Operation, imageoperation.MediaType) (imageoperation.RoutingDecision, error)
	Candidates(string, string, imageoperation.Operation, imageoperation.MediaType) ([]imageoperation.RoutingDecision, error)
}

type ProviderAvailability interface {
	ConfiguredProviders() []providercredentials.ProviderID
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
	executor      Executor
	maxBodyBytes  int64
	models        ModelRegistry
	billing       Billing
	availability  ProviderAvailability
}

func NewHandler(logger *slog.Logger, authenticator Authenticator, executor Executor, maxBodyBytes int64) *Handler {
	return &Handler{logger: logger, authenticator: authenticator, executor: executor, maxBodyBytes: maxBodyBytes}
}

func NewBillableHandler(logger *slog.Logger, authenticator Authenticator, models ModelRegistry, executor Executor, maxBodyBytes int64, chargeBilling Billing) *Handler {
	return NewBillableHandlerWithAvailability(logger, authenticator, models, executor, maxBodyBytes, chargeBilling, nil)
}

func NewBillableHandlerWithAvailability(logger *slog.Logger, authenticator Authenticator, models ModelRegistry, executor Executor, maxBodyBytes int64, chargeBilling Billing, availability ProviderAvailability) *Handler {
	handler := NewHandler(logger, authenticator, executor, maxBodyBytes)
	handler.models = models
	handler.billing = chargeBilling
	handler.availability = availability
	return handler
}

func (handler *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	tracked := &statusWriter{ResponseWriter: writer}
	started := time.Now()
	model, _ := modelFromRequest(request)
	providerModel := model
	candidateID, channelID, routingPolicy := "", "", ""
	fallbackDepth := 0
	var charge *billing.Charge
	defer func() {
		if recover() != nil {
			if charge != nil {
				handler.reconciliationError(tracked, request.Context(), charge.ID, billing.Observation{Outcome: billing.Unknown, Reason: billing.ProviderPanic})
			} else if !tracked.wroteHeader {
				writeError(tracked, http.StatusInternalServerError, "INTERNAL", "internal server error")
			}
			handler.logger.Error("gemini request panic recovered", "request_id", requestid.FromContext(request.Context()))
		}
		handler.logger.Info("gemini request completed",
			"request_id", requestid.FromContext(request.Context()),
			"protocol", "gemini",
			"operation", "generateContent",
			"provider", "google",
			"candidate_id", candidateID,
			"channel_id", channelID,
			"routing_policy", routingPolicy,
			"fallback_depth", fallbackDepth,
			"model", safeModelForLog(model),
			"status", tracked.statusCode(),
			"duration", time.Since(started),
		)
	}()

	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		writeError(tracked, http.StatusMethodNotAllowed, "INVALID_ARGUMENT", "method not allowed")
		return
	}
	parsedModel, validPath := modelFromRequest(request)
	if !validPath || !validModel(parsedModel) {
		writeError(tracked, http.StatusBadRequest, "INVALID_ARGUMENT", "invalid model")
		return
	}
	model = parsedModel
	principal, authenticated := handler.authenticate(tracked, request)
	if !authenticated {
		return
	}
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		writeError(tracked, http.StatusBadRequest, "INVALID_ARGUMENT", "content type must be application/json")
		return
	}
	if request.ContentLength > handler.maxBodyBytes {
		writeError(tracked, http.StatusRequestEntityTooLarge, "RESOURCE_EXHAUSTED", "request body too large")
		return
	}
	body, err := readBounded(request.Body, handler.maxBodyBytes)
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			writeError(tracked, http.StatusRequestEntityTooLarge, "RESOURCE_EXHAUSTED", "request body too large")
			return
		}
		if errors.Is(request.Context().Err(), context.Canceled) {
			writeError(tracked, 499, "CANCELLED", "request canceled")
			return
		}
		writeError(tracked, http.StatusBadRequest, "INVALID_ARGUMENT", "could not read request body")
		return
	}
	if handler.billing != nil {
		if handler.models == nil {
			writeError(tracked, http.StatusServiceUnavailable, "UNAVAILABLE", "billing unavailable")
			return
		}
		candidates, routeErr := handler.models.Candidates("gemini", model, imageoperation.Generate, imageoperation.JSON)
		if routeErr != nil {
			writeError(tracked, http.StatusPreconditionFailed, "FAILED_PRECONDITION", "model is not enabled for billed image generation")
			return
		}
		selector, selectorErr := imageoperation.ParseGeminiJSONPricingSelector(model, body)
		if selectorErr != nil {
			writeError(tracked, http.StatusBadRequest, "INVALID_ARGUMENT", "request contains unsupported billing options")
			return
		}
		idempotencyKey, keyErr := idempotency.Extract(request.Header)
		if keyErr != nil {
			writeError(tracked, http.StatusBadRequest, "INVALID_ARGUMENT", "idempotency key is invalid")
			return
		}
		var fingerprint [32]byte
		legacyFingerprints := make([][32]byte, 0, len(candidates))
		if idempotencyKey != "" {
			query := request.URL.Query()
			query.Del("key")
			wireIdentity := request.Method + " " + request.URL.EscapedPath() + "?" + query.Encode() + " " + request.Header.Get("Content-Type")
			fingerprint = idempotency.Fingerprint("gemini", string(imageoperation.Generate), model, "logical-route-v1", wireIdentity, body)
			for _, candidate := range candidates {
				legacyFingerprints = append(legacyFingerprints, idempotency.Fingerprint("gemini", string(imageoperation.Generate), model, candidate.ChannelID, wireIdentity, body))
			}
		}
		base := billing.BeginRequest{
			RequestID: requestid.FromContext(request.Context()), OrganizationID: principal.OrganizationID, ProjectID: principal.ProjectID,
			Protocol: "gemini", Operation: string(imageoperation.Generate), Model: model, ChannelID: candidates[0].ChannelID,
			Quantity: selector.Quantity, Size: selector.Size, Quality: selector.Quality,
			IdempotencyKey: idempotencyKey, RequestFingerprint: fingerprint, LegacyFingerprints: legacyFingerprints,
		}
		replayed, found, billingErr := handler.billing.Replay(request.Context(), base)
		if billingErr != nil {
			handler.writeBillingError(tracked, billingErr)
			return
		}
		if found {
			handler.writeSnapshot(tracked, replayed.Response, true)
			return
		}
		var route imageoperation.RoutingDecision
		selected := false
		for index, candidate := range candidates {
			if candidate.Provider != providercredentials.Google || handler.executor == nil || !geminiProviderConfigured(handler.availability, candidate.Provider) {
				handler.logCandidateSkip(request, candidate, "provider_unavailable")
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
			startedCharge, beginErr := handler.billing.Begin(request.Context(), attempt)
			if beginErr != nil {
				if errors.Is(beginErr, pricing.ErrPriceUnavailable) || errors.Is(beginErr, pricing.ErrMarginViolation) {
					handler.logCandidateSkip(request, candidate, "price_race_unavailable")
					continue
				}
				handler.writeBillingError(tracked, beginErr)
				return
			}
			charge = &startedCharge
			if charge.Replay {
				handler.writeSnapshot(tracked, charge.Response, true)
				return
			}
			route, fallbackDepth, selected = candidate, index, true
			break
		}
		if !selected {
			writeError(tracked, http.StatusServiceUnavailable, "UNAVAILABLE", "provider unavailable")
			return
		}
		providerModel = route.ProviderModel
		candidateID, channelID, routingPolicy = route.CandidateID, route.ChannelID, string(route.Policy)
	}

	response, err := handler.executor.GenerateContent(request.Context(), google.GenerateContentRequest{
		Model:       providerModel,
		Query:       request.URL.Query(),
		ContentType: request.Header.Get("Content-Type"),
		Accept:      request.Header.Get("Accept"),
		UserAgent:   request.UserAgent(),
		APIClient:   request.Header.Get("x-goog-api-client"),
		Body:        bytes.NewReader(body),
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
				handler.reconciliationError(tracked, request.Context(), charge.ID, geminiKnownObservation(false, billing.SettlementFailed, snapshot))
				return
			}
			reason := billing.ExecutorConnection
			if errors.Is(err, google.ErrTimeout) {
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
			handler.reconciliationError(tracked, request.Context(), charge.ID, geminiKnownObservation(response.StatusCode >= 200 && response.StatusCode <= 299, billing.ResponseUnavailable, handler.responseUnavailableSnapshot()))
			return
		}
		snapshot := billing.ResponseSnapshot{Status: response.StatusCode, Headers: safeGeminiResponseHeaders(response.Header), Body: responseBody}
		completed, completeErr := handler.complete(request.Context(), charge.ID, response.StatusCode >= 200 && response.StatusCode <= 299, snapshot)
		if completeErr != nil {
			handler.reconciliationError(tracked, request.Context(), charge.ID, geminiKnownObservation(response.StatusCode >= 200 && response.StatusCode <= 299, billing.SettlementFailed, snapshot))
			return
		}
		handler.writeSnapshot(tracked, completed.Response, false)
		return
	}
	copyResponseHeaders(tracked.Header(), response.Header)
	tracked.WriteHeader(response.StatusCode)
	if _, err := io.Copy(tracked, response.Body); err != nil {
		handler.logger.Warn("gemini upstream response copy failed",
			"request_id", requestid.FromContext(request.Context()),
			"provider", "google",
			"category", "response_copy_failed",
		)
	}
}

func geminiProviderConfigured(availability ProviderAvailability, provider providercredentials.ProviderID) bool {
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
	handler.logger.Info("gemini routing candidate skipped", "request_id", requestid.FromContext(request.Context()), "model", decision.Model, "candidate_id", decision.CandidateID, "channel_id", decision.ChannelID, "provider", string(decision.Provider), "category", category)
}

func (handler *Handler) authenticate(writer http.ResponseWriter, request *http.Request) (apikey.Principal, bool) {
	if handler.authenticator == nil {
		writeError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "authentication service unavailable")
		return apikey.Principal{}, false
	}
	raw, err := apikey.Extract(request)
	if err != nil {
		if errors.Is(err, apikey.ErrAmbiguous) {
			writeError(writer, http.StatusBadRequest, "INVALID_ARGUMENT", "multiple credential locations are not allowed")
			return apikey.Principal{}, false
		}
		writeError(writer, http.StatusUnauthorized, "UNAUTHENTICATED", "authentication required")
		return apikey.Principal{}, false
	}
	principal, err := handler.authenticator.Authenticate(request.Context(), raw)
	if err != nil {
		if errors.Is(err, apikey.ErrUnavailable) {
			writeError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "authentication service unavailable")
			return apikey.Principal{}, false
		}
		writeError(writer, http.StatusUnauthorized, "UNAUTHENTICATED", "authentication required")
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
	markContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = handler.billing.MarkReconciling(markContext, chargeID, observation)
	writeError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "billing settlement is pending")
}

func geminiKnownObservation(success bool, reason billing.Reason, snapshot billing.ResponseSnapshot) billing.Observation {
	outcome := billing.KnownFailure
	if success {
		outcome = billing.KnownSuccess
	}
	return billing.Observation{Outcome: outcome, Reason: reason, Snapshot: snapshot}
}

func (handler *Handler) responseUnavailableSnapshot() billing.ResponseSnapshot {
	body, _ := json.Marshal(errorEnvelope{Error: errorBody{Code: http.StatusBadGateway, Message: "provider response unavailable", Status: "UNAVAILABLE"}})
	return billing.ResponseSnapshot{Status: http.StatusBadGateway, Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: body}
}

func (handler *Handler) executorErrorSnapshot(err error) billing.ResponseSnapshot {
	status, code, message := http.StatusInternalServerError, "INTERNAL", "internal server error"
	switch {
	case errors.Is(err, providercredentials.ErrCredentialUnavailable):
		status, code, message = http.StatusServiceUnavailable, "UNAVAILABLE", "provider unavailable"
	case errors.Is(err, google.ErrTimeout):
		status, code, message = http.StatusGatewayTimeout, "DEADLINE_EXCEEDED", "provider request timed out"
	case errors.Is(err, google.ErrCanceled):
		status, code, message = 499, "CANCELLED", "request canceled"
	case errors.Is(err, google.ErrUpstream):
		status, code, message = http.StatusBadGateway, "UNAVAILABLE", "provider unavailable"
	}
	body, _ := json.Marshal(errorEnvelope{Error: errorBody{Code: status, Message: message, Status: code}})
	return billing.ResponseSnapshot{Status: status, Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: body}
}

func (handler *Handler) writeBillingError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ledger.ErrInsufficientFunds):
		writeError(writer, http.StatusPaymentRequired, "RESOURCE_EXHAUSTED", "insufficient credits")
	case errors.Is(err, billing.ErrRequestConflict):
		writeError(writer, http.StatusConflict, "ALREADY_EXISTS", "idempotency key conflicts with another request")
	case errors.Is(err, billing.ErrRequestPending):
		writeError(writer, http.StatusConflict, "ABORTED", "idempotent request is still in progress")
	case errors.Is(err, billing.ErrAlreadySettled):
		writeError(writer, http.StatusConflict, "ALREADY_EXISTS", "request identifier is already settled")
	case errors.Is(err, billing.ErrInvalidRequest):
		writeError(writer, http.StatusBadRequest, "INVALID_ARGUMENT", "request cannot be billed")
	case errors.Is(err, pricing.ErrPriceUnavailable), errors.Is(err, pricing.ErrMarginViolation), errors.Is(err, ledger.ErrTenantUnavailable):
		writeError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "billing unavailable")
	default:
		writeError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "billing unavailable")
	}
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

func safeGeminiResponseHeaders(source http.Header) map[string][]string {
	result := map[string][]string{}
	for _, key := range []string{"Content-Type", "Retry-After", "X-Goog-Request-Id"} {
		if values := source.Values(key); len(values) > 0 {
			result[key] = append([]string(nil), values...)
		}
	}
	return result
}

func (handler *Handler) writeExecutorError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, providercredentials.ErrCredentialUnavailable):
		writeError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "provider unavailable")
	case errors.Is(err, google.ErrTimeout):
		writeError(writer, http.StatusGatewayTimeout, "DEADLINE_EXCEEDED", "provider request timed out")
	case errors.Is(err, google.ErrCanceled):
		writeError(writer, 499, "CANCELLED", "request canceled")
	case errors.Is(err, google.ErrUpstream):
		writeError(writer, http.StatusBadGateway, "UNAVAILABLE", "provider unavailable")
	case errors.Is(err, google.ErrInvalidRequest):
		writeError(writer, http.StatusInternalServerError, "INTERNAL", "internal server error")
	default:
		writeError(writer, http.StatusInternalServerError, "INTERNAL", "internal server error")
	}
}

func validModel(model string) bool {
	if model == "" || len(model) > maxModelLength {
		return false
	}
	for _, character := range model {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '.' || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}

func modelFromRequest(request *http.Request) (string, bool) {
	const prefix = "/v1beta/models/"
	const suffix = ":generateContent"
	escapedPath := request.URL.EscapedPath()
	if !strings.HasPrefix(escapedPath, prefix) || !strings.HasSuffix(escapedPath, suffix) {
		return "", false
	}
	escapedModel := strings.TrimSuffix(strings.TrimPrefix(escapedPath, prefix), suffix)
	model, err := url.PathUnescape(escapedModel)
	if err != nil || strings.Contains(model, "/") {
		return "", false
	}
	return model, true
}

func safeModelForLog(model string) string {
	if validModel(model) {
		return model
	}
	return "invalid"
}

var errBodyTooLarge = errors.New("request body too large")

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
	for _, header := range []string{"Content-Type", "Retry-After", "X-Goog-Request-Id"} {
		for _, value := range source.Values(header) {
			destination.Add(header, value)
		}
	}
}

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
}

func writeError(writer http.ResponseWriter, code int, status, message string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(code)
	_ = json.NewEncoder(writer).Encode(errorEnvelope{Error: errorBody{Code: code, Message: message, Status: status}})
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
