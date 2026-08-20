// Package gemini implements the Gemini Developer API protocol facade.
package gemini

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/nativegatewayhq/gateway/internal/apikey"
	"github.com/nativegatewayhq/gateway/internal/billing"
	"github.com/nativegatewayhq/gateway/internal/costquota"
	"github.com/nativegatewayhq/gateway/internal/idempotency"
	"github.com/nativegatewayhq/gateway/internal/imagestorage"
	"github.com/nativegatewayhq/gateway/internal/ledger"
	"github.com/nativegatewayhq/gateway/internal/networkauth"
	"github.com/nativegatewayhq/gateway/internal/pricing"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	"github.com/nativegatewayhq/gateway/internal/providerhealth"
	"github.com/nativegatewayhq/gateway/internal/ratelimit"
	"github.com/nativegatewayhq/gateway/internal/requestid"
	"github.com/nativegatewayhq/gateway/internal/spendcap"
	"github.com/nativegatewayhq/gateway/internal/telemetry"
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

type ResultManager interface {
	Transform(context.Context, imagestorage.TransformInput) ([]byte, error)
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
	weighted      imageoperation.WeightedSampler
	health        providerhealth.Gate
	results       ResultManager
	telemetry     *telemetry.Recorder
}

func (handler *Handler) SetResultManager(manager ResultManager)    { handler.results = manager }
func (handler *Handler) SetTelemetry(recorder *telemetry.Recorder) { handler.telemetry = recorder }

func NewHandler(logger *slog.Logger, authenticator Authenticator, executor Executor, maxBodyBytes int64) *Handler {
	return NewHandlerWithHealth(logger, authenticator, executor, maxBodyBytes, providerhealth.NoopGate{})
}

func NewHandlerWithHealth(logger *slog.Logger, authenticator Authenticator, executor Executor, maxBodyBytes int64, health providerhealth.Gate) *Handler {
	weighted, _ := imageoperation.NewWeightedSampler(rand.Reader)
	if health == nil {
		health = providerhealth.NoopGate{}
	}
	return &Handler{logger: logger, authenticator: authenticator, executor: executor, maxBodyBytes: maxBodyBytes, weighted: weighted, health: health}
}

func NewBillableHandler(logger *slog.Logger, authenticator Authenticator, models ModelRegistry, executor Executor, maxBodyBytes int64, chargeBilling Billing) *Handler {
	return NewBillableHandlerWithAvailability(logger, authenticator, models, executor, maxBodyBytes, chargeBilling, nil)
}

func NewBillableHandlerWithAvailability(logger *slog.Logger, authenticator Authenticator, models ModelRegistry, executor Executor, maxBodyBytes int64, chargeBilling Billing, availability ProviderAvailability) *Handler {
	return NewBillableHandlerWithAvailabilityAndHealth(logger, authenticator, models, executor, maxBodyBytes, chargeBilling, availability, providerhealth.NoopGate{})
}

func NewBillableHandlerWithAvailabilityAndHealth(logger *slog.Logger, authenticator Authenticator, models ModelRegistry, executor Executor, maxBodyBytes int64, chargeBilling Billing, availability ProviderAvailability, health providerhealth.Gate) *Handler {
	handler := NewHandlerWithHealth(logger, authenticator, executor, maxBodyBytes, health)
	handler.models = models
	handler.billing = chargeBilling
	handler.availability = availability
	if health != nil {
		handler.health = health
	}
	return handler
}

func (handler *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	tracked := &statusWriter{ResponseWriter: writer}
	started := time.Now()
	model, _ := modelFromRequest(request)
	providerModel := model
	candidateID, channelID, routingPolicy := "", "", "fixed"
	if legacyChannel, ok := providercredentials.LegacyChannel(providercredentials.Google); ok {
		channelID = legacyChannel
	}
	fallbackDepth := 0
	var charge *billing.Charge
	var healthPermit providerhealth.Permit
	dispatched := false
	defer func() {
		if recover() != nil {
			if healthPermit.ChannelID != "" {
				if dispatched {
					handler.observeHealth(request, channelID, healthPermit, nil, errProviderPanic)
				} else {
					handler.releaseHealthPermit(request, healthPermit)
				}
			}
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
	var candidates []imageoperation.RoutingDecision
	if handler.billing != nil {
		if handler.models == nil {
			writeError(tracked, http.StatusServiceUnavailable, "UNAVAILABLE", "billing unavailable")
			return
		}
		var routeErr error
		candidates, routeErr = handler.models.Candidates("gemini", model, imageoperation.Generate, imageoperation.JSON)
		if routeErr != nil {
			writeError(tracked, http.StatusPreconditionFailed, "FAILED_PRECONDITION", "model is not enabled for billed image generation")
			return
		}
	}
	if !handler.authorizeModel(tracked, request, principal, model) {
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
	if handler.billing == nil {
		permit, allowed := handler.claimFixedHealth(tracked, request, channelID)
		if !allowed {
			return
		}
		healthPermit = permit
	}
	if handler.billing != nil {
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
			RequestID: requestid.FromContext(request.Context()), OrganizationID: principal.OrganizationID, ProjectID: principal.ProjectID, APIKeyID: principal.APIKeyID,
			Protocol: "gemini", Operation: string(imageoperation.Generate), Model: model, ChannelID: candidates[0].ChannelID,
			Quantity: selector.Quantity, Size: selector.Size, Quality: selector.Quality,
			IdempotencyKey: idempotencyKey, RequestFingerprint: fingerprint, LegacyFingerprints: legacyFingerprints,
		}
		replayed, found, billingErr := handler.billing.Replay(request.Context(), base)
		if billingErr != nil {
			handler.writeBillingError(tracked, request, billingErr)
			return
		}
		if found {
			if handler.telemetry != nil {
				handler.telemetry.Billing(request.Context(), telemetry.BillingRecord{Protocol: "gemini", Operation: string(imageoperation.Generate), Transition: "replay", Outcome: "replay"})
			}
			handler.writeSnapshot(tracked, replayed.Response, true)
			return
		}
		selection, selected := handler.selectNewBillableCandidate(tracked, request, candidates, base)
		if !selected {
			return
		}
		charge, fallbackDepth, healthPermit = selection.charge, selection.rank, selection.permit
		providerModel = selection.decision.ProviderModel
		candidateID, channelID, routingPolicy = selection.decision.CandidateID, selection.decision.ChannelID, string(selection.decision.Policy)
	}

	if handler.executor == nil {
		handler.releaseHealthPermit(request, healthPermit)
		writeError(tracked, http.StatusServiceUnavailable, "UNAVAILABLE", "provider unavailable")
		return
	}
	dispatched = true
	if handler.telemetry != nil {
		handler.telemetry.Route(request.Context(), telemetry.RouteRecord{Protocol: "gemini", Operation: string(imageoperation.Generate), Policy: routingPolicy, Outcome: "success"})
	}
	response, err := handler.executeProvider(request.Context(), google.GenerateContentRequest{
		Model:       providerModel,
		ChannelID:   channelID,
		Query:       request.URL.Query(),
		ContentType: request.Header.Get("Content-Type"),
		Accept:      request.Header.Get("Accept"),
		UserAgent:   request.UserAgent(),
		APIClient:   request.Header.Get("x-goog-api-client"),
		Body:        bytes.NewReader(body),
	})
	handler.observeHealth(request, channelID, healthPermit, response, err)
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
		if response.StatusCode >= 200 && response.StatusCode <= 299 && handler.results != nil {
			managedBody, storageErr := handler.results.Transform(request.Context(), imagestorage.TransformInput{Protocol: "gemini", Provider: "google", ChannelID: channelID, RequestID: requestid.FromContext(request.Context()), ChargeID: charge.ID, Body: responseBody})
			if storageErr != nil {
				handler.reconciliationError(tracked, request.Context(), charge.ID, geminiKnownObservation(true, billing.StorageFailed, snapshot))
				return
			}
			snapshot.Body = managedBody
		}
		completed, completeErr := handler.complete(request.Context(), charge.ID, response.StatusCode >= 200 && response.StatusCode <= 299, snapshot)
		if completeErr != nil {
			handler.reconciliationError(tracked, request.Context(), charge.ID, geminiKnownObservation(response.StatusCode >= 200 && response.StatusCode <= 299, billing.SettlementFailed, snapshot))
			return
		}
		handler.writeSnapshot(tracked, completed.Response, false)
		return
	}
	if response.StatusCode >= 200 && response.StatusCode <= 299 && handler.results != nil {
		responseBody, readErr := readBounded(response.Body, handler.results.MaximumResponseBytes())
		if readErr != nil {
			handler.writeSnapshot(tracked, handler.storageErrorSnapshot(), false)
			return
		}
		managedBody, storageErr := handler.results.Transform(request.Context(), imagestorage.TransformInput{Protocol: "gemini", Provider: "google", ChannelID: channelID, RequestID: requestid.FromContext(request.Context()), Body: responseBody})
		if storageErr != nil {
			handler.writeSnapshot(tracked, handler.storageErrorSnapshot(), false)
			return
		}
		handler.writeSnapshot(tracked, billing.ResponseSnapshot{Status: response.StatusCode, Headers: safeGeminiResponseHeaders(response.Header), Body: managedBody}, false)
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

func (handler *Handler) executeProvider(ctx context.Context, providerRequest google.GenerateContentRequest) (response *http.Response, err error) {
	if handler.telemetry == nil {
		return handler.executor.GenerateContent(ctx, providerRequest)
	}
	providerCtx, span, started := handler.telemetry.StartProvider(ctx, "google", "gemini", string(imageoperation.Generate))
	defer func() {
		if recovered := recover(); recovered != nil {
			handler.telemetry.EndProvider(providerCtx, span, started, telemetry.ProviderRecord{Provider: "google", Protocol: "gemini", Operation: string(imageoperation.Generate), Outcome: "failure"})
			panic(recovered)
		}
	}()
	response, err = handler.executor.GenerateContent(providerCtx, providerRequest)
	handler.telemetry.EndProvider(providerCtx, span, started, telemetry.ProviderRecord{Provider: "google", Protocol: "gemini", Operation: string(imageoperation.Generate), Outcome: geminiProviderTelemetryOutcome(response, err)})
	return response, err
}

func geminiProviderTelemetryOutcome(response *http.Response, err error) string {
	switch {
	case errors.Is(err, google.ErrTimeout):
		return "timeout"
	case errors.Is(err, google.ErrCanceled):
		return "canceled"
	case err != nil || response == nil:
		return "connection"
	case response.StatusCode >= 200 && response.StatusCode <= 299:
		return "success"
	case response.StatusCode == http.StatusTooManyRequests:
		return "rate_limited"
	case response.StatusCode >= 500:
		return "server_error"
	default:
		return "neutral"
	}
}

func (handler *Handler) storageErrorSnapshot() billing.ResponseSnapshot {
	body := []byte(`{"error":{"code":502,"message":"managed image result unavailable","status":"UNAVAILABLE"}}`)
	return billing.ResponseSnapshot{Status: http.StatusBadGateway, Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: body}
}

func (handler *Handler) authorizeModel(writer http.ResponseWriter, request *http.Request, principal apikey.Principal, model string) bool {
	if principal.AuthorizeModel("gemini", string(imageoperation.Generate), model) {
		if handler.telemetry != nil {
			handler.telemetry.Authentication(request.Context(), telemetry.AuthenticationRecord{Protocol: "gemini", Stage: "model_authorization", Outcome: "success"})
		}
		return true
	}
	if handler.telemetry != nil {
		handler.telemetry.Authentication(request.Context(), telemetry.AuthenticationRecord{Protocol: "gemini", Stage: "model_authorization", Outcome: "failure"})
	}
	handler.logger.Info("API key model authorization denied", "request_id", requestid.FromContext(request.Context()), "api_key_id", principal.APIKeyID, "project_id", principal.ProjectID, "protocol", "gemini", "operation", string(imageoperation.Generate), "model", model, "category", "denied")
	writeError(writer, http.StatusForbidden, "PERMISSION_DENIED", "API key is not permitted to use this model")
	return false
}

func geminiProviderConfigured(ctx context.Context, availability ProviderAvailability, decision imageoperation.RoutingDecision) bool {
	if availability == nil {
		return true
	}
	if channelAvailability, ok := availability.(interface {
		ConfiguredChannel(context.Context, string, providercredentials.ProviderID) bool
	}); ok {
		return channelAvailability.ConfiguredChannel(ctx, decision.ChannelID, decision.Provider)
	}
	for _, configured := range availability.ConfiguredProviders() {
		if configured == decision.Provider {
			return true
		}
	}
	return false
}

func (handler *Handler) logCandidateSkip(request *http.Request, decision imageoperation.RoutingDecision, category string) {
	handler.logger.InfoContext(request.Context(), "gemini routing candidate skipped", "request_id", requestid.FromContext(request.Context()), "channel_id", decision.ChannelID, "provider", string(decision.Provider), "category", category)
	if handler.telemetry != nil {
		handler.telemetry.Route(request.Context(), telemetry.RouteRecord{Protocol: "gemini", Operation: string(imageoperation.Generate), Policy: string(decision.Policy), Outcome: "failure", Rejection: category})
	}
}

func (handler *Handler) logSpendCapSkip(request *http.Request, decision imageoperation.RoutingDecision, err error) {
	attributes := []any{"request_id", requestid.FromContext(request.Context()), "channel_id", decision.ChannelID, "provider", string(decision.Provider), "category", "spend_cap_exhausted"}
	var limitErr *spendcap.LimitError
	if errors.As(err, &limitErr) {
		attributes = append(attributes, "period", limitErr.Period, "reset_at", limitErr.ResetAt)
	}
	handler.logger.Info("gemini routing candidate skipped", attributes...)
	if handler.telemetry != nil {
		handler.telemetry.Route(request.Context(), telemetry.RouteRecord{Protocol: "gemini", Operation: string(imageoperation.Generate), Policy: string(decision.Policy), Outcome: "failure", Rejection: "spend_cap_exhausted"})
	}
}

func (handler *Handler) logCredentialSkip(request *http.Request, decision imageoperation.RoutingDecision) {
	handler.logger.Info("gemini routing candidate skipped", "request_id", requestid.FromContext(request.Context()), "channel_id", decision.ChannelID, "provider", string(decision.Provider), "category", "credential_unavailable")
	if handler.telemetry != nil {
		handler.telemetry.Route(request.Context(), telemetry.RouteRecord{Protocol: "gemini", Operation: string(imageoperation.Generate), Policy: string(decision.Policy), Outcome: "failure", Rejection: "credential_unavailable"})
	}
}

func (handler *Handler) healthyCandidates(request *http.Request, candidates []imageoperation.RoutingDecision) ([]imageoperation.RoutingDecision, error) {
	healthy := make([]imageoperation.RoutingDecision, 0, len(candidates))
	for _, candidate := range candidates {
		snapshot, err := handler.health.Inspect(request.Context(), candidate.ChannelID)
		if err != nil {
			return nil, err
		}
		if snapshot.State == providerhealth.Open {
			handler.logCandidateSkip(request, candidate, "circuit_open")
			continue
		}
		healthy = append(healthy, candidate)
	}
	return healthy, nil
}

func (handler *Handler) claimFixedHealth(writer http.ResponseWriter, request *http.Request, channelID string) (providerhealth.Permit, bool) {
	snapshot, err := handler.health.Inspect(request.Context(), channelID)
	if err != nil {
		handler.writeHealthError(writer, err)
		return providerhealth.Permit{}, false
	}
	if snapshot.State == providerhealth.Open {
		writeError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "provider unavailable")
		return providerhealth.Permit{}, false
	}
	permit, err := handler.health.ClaimProbe(request.Context(), channelID, requestid.FromContext(request.Context()))
	if errors.Is(err, providerhealth.ErrOpen) || errors.Is(err, providerhealth.ErrProbeBusy) {
		writeError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "provider unavailable")
		return providerhealth.Permit{}, false
	}
	if err != nil {
		handler.writeHealthError(writer, err)
		return providerhealth.Permit{}, false
	}
	return permit, true
}

func (handler *Handler) writeHealthError(writer http.ResponseWriter, _ error) {
	writeError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "provider health unavailable")
}

func (handler *Handler) releaseHealthPermit(request *http.Request, permit providerhealth.Permit) {
	if !permit.Probe {
		return
	}
	if err := handler.health.Release(context.WithoutCancel(request.Context()), permit); err != nil {
		handler.logger.Warn("provider health permit release failed", "request_id", requestid.FromContext(request.Context()), "channel_id", permit.ChannelID, "category", "health_unavailable")
	}
}

func (handler *Handler) observeHealth(request *http.Request, channelID string, permit providerhealth.Permit, response *http.Response, executionErr error) {
	outcome := providerhealth.Neutral
	if executionErr != nil {
		if errors.Is(executionErr, providercredentials.ErrCredentialUnavailable) {
			handler.releaseHealthPermit(request, permit)
			return
		}
		switch {
		case errors.Is(executionErr, google.ErrTimeout):
			outcome = providerhealth.Timeout
		case errors.Is(executionErr, google.ErrCanceled):
			outcome = providerhealth.Neutral
		default:
			outcome = providerhealth.Connection
		}
	} else if response != nil {
		switch {
		case response.StatusCode >= 200 && response.StatusCode <= 299:
			outcome = providerhealth.Success
		case response.StatusCode == http.StatusTooManyRequests:
			outcome = providerhealth.RateLimited
		case response.StatusCode >= 500:
			outcome = providerhealth.ServerError
		}
	}
	observation := providerhealth.Observation{ChannelID: channelID, ObservationID: requestid.FromContext(request.Context()), Outcome: outcome, Permit: permit}
	if _, err := handler.health.Observe(context.WithoutCancel(request.Context()), observation); err != nil {
		handler.logger.Warn("provider health observation failed", "request_id", requestid.FromContext(request.Context()), "provider", "google", "channel_id", channelID, "category", "health_unavailable")
	}
}

type geminiBillableCandidateAttempt struct {
	decision imageoperation.RoutingDecision
	quote    *billing.BoundQuote
	rank     int
}

type geminiBillableSelection struct {
	decision imageoperation.RoutingDecision
	charge   *billing.Charge
	rank     int
	permit   providerhealth.Permit
}

func (handler *Handler) selectNewBillableCandidate(writer http.ResponseWriter, request *http.Request, candidates []imageoperation.RoutingDecision, base billing.BeginRequest) (geminiBillableSelection, bool) {
	var healthErr error
	candidates, healthErr = handler.healthyCandidates(request, candidates)
	if healthErr != nil {
		handler.writeHealthError(writer, healthErr)
		return geminiBillableSelection{}, false
	}
	if len(candidates) == 0 {
		writeError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "provider unavailable")
		return geminiBillableSelection{}, false
	}
	if len(candidates) > 0 && candidates[0].Policy == imageoperation.Weighted {
		return handler.selectWeightedCandidate(writer, request, candidates, base)
	}
	lowestCost := len(candidates) > 0 && candidates[0].Policy == imageoperation.LowestCost
	maximumEvaluations := 1
	if lowestCost {
		maximumEvaluations = 2
	}
	for evaluation := 0; evaluation < maximumEvaluations; evaluation++ {
		attempts, prepareErr := handler.prepareBillableAttempts(request, candidates, base)
		if prepareErr != nil {
			handler.writeBillingError(writer, request, prepareErr)
			return geminiBillableSelection{}, false
		}
		retryEvaluation := false
		for _, candidateAttempt := range attempts {
			candidate := candidateAttempt.decision
			permit, permitErr := handler.health.ClaimProbe(request.Context(), candidate.ChannelID, requestid.FromContext(request.Context()))
			if permitErr != nil {
				if errors.Is(permitErr, providerhealth.ErrOpen) || errors.Is(permitErr, providerhealth.ErrProbeBusy) {
					handler.logCandidateSkip(request, candidate, "circuit_unavailable")
					continue
				}
				handler.writeHealthError(writer, permitErr)
				return geminiBillableSelection{}, false
			}
			attempt := base
			attempt.ChannelID = candidate.ChannelID
			if candidateAttempt.quote != nil {
				attempt.RoutingPolicy = string(imageoperation.LowestCost)
				attempt.CostRank = candidateAttempt.rank
				attempt.EvaluationAt = candidateAttempt.quote.EvaluatedAt
				attempt.ExpectedQuote = candidateAttempt.quote
			} else {
				if candidate.Provider != providercredentials.Google || handler.executor == nil {
					handler.releaseHealthPermit(request, permit)
					handler.logCandidateSkip(request, candidate, "provider_unavailable")
					continue
				}
				if !geminiProviderConfigured(request.Context(), handler.availability, candidate) {
					handler.releaseHealthPermit(request, permit)
					handler.logCredentialSkip(request, candidate)
					continue
				}
				if _, quoteErr := handler.billing.Quote(request.Context(), attempt); quoteErr != nil {
					if errors.Is(quoteErr, pricing.ErrPriceUnavailable) || errors.Is(quoteErr, pricing.ErrMarginViolation) {
						handler.releaseHealthPermit(request, permit)
						handler.logCandidateSkip(request, candidate, "price_unavailable")
						continue
					}
					handler.writeBillingError(writer, request, quoteErr)
					handler.releaseHealthPermit(request, permit)
					return geminiBillableSelection{}, false
				}
			}
			started, beginErr := handler.beginBilling(request.Context(), attempt)
			if beginErr != nil {
				handler.releaseHealthPermit(request, permit)
				if lowestCost && errors.Is(beginErr, billing.ErrPriceSnapshotChanged) {
					if evaluation == 0 {
						retryEvaluation = true
						break
					}
					handler.writeBillingError(writer, request, beginErr)
					return geminiBillableSelection{}, false
				}
				if errors.Is(beginErr, spendcap.ErrExceeded) {
					handler.logSpendCapSkip(request, candidate, beginErr)
					continue
				}
				if errors.Is(beginErr, pricing.ErrPriceUnavailable) || errors.Is(beginErr, pricing.ErrMarginViolation) {
					handler.logCandidateSkip(request, candidate, "price_race_unavailable")
					continue
				}
				handler.writeBillingError(writer, request, beginErr)
				return geminiBillableSelection{}, false
			}
			if started.Replay {
				handler.releaseHealthPermit(request, permit)
				handler.writeSnapshot(writer, started.Response, true)
				return geminiBillableSelection{}, false
			}
			return geminiBillableSelection{decision: candidate, charge: &started, rank: candidateAttempt.rank, permit: permit}, true
		}
		if retryEvaluation {
			continue
		}
		writeError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "provider unavailable")
		return geminiBillableSelection{}, false
	}
	handler.writeBillingError(writer, request, billing.ErrPriceSnapshotChanged)
	return geminiBillableSelection{}, false
}

func (handler *Handler) selectWeightedCandidate(writer http.ResponseWriter, request *http.Request, candidates []imageoperation.RoutingDecision, base billing.BeginRequest) (geminiBillableSelection, bool) {
	remaining := make([]imageoperation.RoutingDecision, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Provider != providercredentials.Google || handler.executor == nil {
			handler.logCandidateSkip(request, candidate, "provider_unavailable")
			continue
		}
		if !geminiProviderConfigured(request.Context(), handler.availability, candidate) {
			handler.logCredentialSkip(request, candidate)
			continue
		}
		remaining = append(remaining, candidate)
	}
	for rank := 0; len(remaining) > 0; rank++ {
		candidate, err := handler.weighted.Pick(remaining)
		if err != nil {
			handler.writeBillingError(writer, request, err)
			return geminiBillableSelection{}, false
		}
		remaining = removeWeightedCandidate(remaining, candidate.CandidateID)
		permit, err := handler.health.ClaimProbe(request.Context(), candidate.ChannelID, requestid.FromContext(request.Context()))
		if err != nil {
			if errors.Is(err, providerhealth.ErrOpen) || errors.Is(err, providerhealth.ErrProbeBusy) {
				handler.logCandidateSkip(request, candidate, "circuit_unavailable")
				continue
			}
			handler.writeHealthError(writer, err)
			return geminiBillableSelection{}, false
		}
		attempt := base
		attempt.ChannelID = candidate.ChannelID
		attempt.RoutingPolicy = string(imageoperation.Weighted)
		attempt.CostRank = rank
		if _, err := handler.billing.Quote(request.Context(), attempt); err != nil {
			if errors.Is(err, pricing.ErrPriceUnavailable) || errors.Is(err, pricing.ErrMarginViolation) {
				handler.releaseHealthPermit(request, permit)
				handler.logCandidateSkip(request, candidate, "price_unavailable")
				continue
			}
			handler.writeBillingError(writer, request, err)
			handler.releaseHealthPermit(request, permit)
			return geminiBillableSelection{}, false
		}
		started, err := handler.beginBilling(request.Context(), attempt)
		if err != nil {
			handler.releaseHealthPermit(request, permit)
			if errors.Is(err, spendcap.ErrExceeded) {
				handler.logSpendCapSkip(request, candidate, err)
				continue
			}
			if errors.Is(err, pricing.ErrPriceUnavailable) || errors.Is(err, pricing.ErrMarginViolation) {
				handler.logCandidateSkip(request, candidate, "price_race_unavailable")
				continue
			}
			handler.writeBillingError(writer, request, err)
			return geminiBillableSelection{}, false
		}
		if started.Replay {
			handler.releaseHealthPermit(request, permit)
			handler.writeSnapshot(writer, started.Response, true)
			return geminiBillableSelection{}, false
		}
		return geminiBillableSelection{decision: candidate, charge: &started, rank: rank, permit: permit}, true
	}
	writeError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "provider unavailable")
	return geminiBillableSelection{}, false
}

func removeWeightedCandidate(candidates []imageoperation.RoutingDecision, candidateID string) []imageoperation.RoutingDecision {
	for index := range candidates {
		if candidates[index].CandidateID == candidateID {
			return append(candidates[:index:index], candidates[index+1:]...)
		}
	}
	return candidates
}

func (handler *Handler) prepareBillableAttempts(request *http.Request, candidates []imageoperation.RoutingDecision, base billing.BeginRequest) ([]geminiBillableCandidateAttempt, error) {
	if len(candidates) == 0 || candidates[0].Policy != imageoperation.LowestCost {
		attempts := make([]geminiBillableCandidateAttempt, 0, len(candidates))
		for index, candidate := range candidates {
			attempts = append(attempts, geminiBillableCandidateAttempt{decision: candidate, rank: index})
		}
		return attempts, nil
	}
	evaluatedAt := time.Now().UTC().Truncate(time.Microsecond)
	estimates := make(map[string]pricing.Estimate, len(candidates))
	for _, candidate := range candidates {
		if candidate.Provider != providercredentials.Google || handler.executor == nil {
			handler.logCandidateSkip(request, candidate, "provider_unavailable")
			continue
		}
		if !geminiProviderConfigured(request.Context(), handler.availability, candidate) {
			handler.logCredentialSkip(request, candidate)
			continue
		}
		quoteRequest := base
		quoteRequest.ChannelID = candidate.ChannelID
		quoteRequest.RoutingPolicy = string(imageoperation.LowestCost)
		quoteRequest.EvaluationAt = evaluatedAt
		estimate, err := handler.billing.Quote(request.Context(), quoteRequest)
		if err != nil {
			if errors.Is(err, pricing.ErrPriceUnavailable) || errors.Is(err, pricing.ErrMarginViolation) {
				handler.logCandidateSkip(request, candidate, "price_unavailable")
				continue
			}
			return nil, err
		}
		estimates[candidate.ChannelID] = estimate
	}
	ordered, err := imageoperation.OrderLowestCost(candidates, estimates, evaluatedAt, base.Quantity)
	if err != nil {
		return nil, err
	}
	attempts := make([]geminiBillableCandidateAttempt, 0, len(ordered))
	for rank, candidate := range ordered {
		quote := billing.BoundQuote{PriceID: candidate.Estimate.PriceID, ChannelID: candidate.Estimate.ChannelID, Currency: candidate.Estimate.Currency, EstimatedCost: candidate.Estimate.EstimatedCost, MaximumSale: candidate.Estimate.MaximumSale, EvaluatedAt: candidate.Estimate.EvaluatedAt}
		attempts = append(attempts, geminiBillableCandidateAttempt{decision: candidate.Decision, quote: &quote, rank: rank})
	}
	return attempts, nil
}

func (handler *Handler) authenticate(writer http.ResponseWriter, request *http.Request) (principal apikey.Principal, allowed bool) {
	defer func() {
		if handler.telemetry != nil {
			outcome := "failure"
			if allowed {
				outcome = "success"
			}
			handler.telemetry.Authentication(request.Context(), telemetry.AuthenticationRecord{Protocol: "gemini", Stage: "authenticate", Outcome: outcome})
		}
	}()
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
	principal, err = handler.authenticator.Authenticate(request.Context(), raw)
	if err != nil {
		var denied *networkauth.DeniedError
		if errors.As(err, &denied) {
			if handler.telemetry != nil {
				handler.telemetry.Authentication(request.Context(), telemetry.AuthenticationRecord{Protocol: "gemini", Stage: "network", Outcome: "failure"})
			}
			attributes := []any{"request_id", requestid.FromContext(request.Context()), "api_key_id", denied.APIKeyID, "project_id", denied.ProjectID, "category", "network_not_allowed"}
			if denied.ClientIP.IsValid() {
				attributes = append(attributes, "client_ip", denied.ClientIP.String())
			}
			handler.logger.Info("API key client network denied", attributes...)
			writeError(writer, http.StatusForbidden, "PERMISSION_DENIED", "API key is not permitted from this network")
			return apikey.Principal{}, false
		}
		var limited *ratelimit.LimitError
		if errors.As(err, &limited) {
			if handler.telemetry != nil {
				handler.telemetry.Authentication(request.Context(), telemetry.AuthenticationRecord{Protocol: "gemini", Stage: "rate_limit", Outcome: "rate_limited"})
			}
			handler.logger.Info("API key request rate limited", "request_id", requestid.FromContext(request.Context()), "api_key_id", limited.APIKeyID, "project_id", limited.ProjectID, "outcome", "limited", "retry_after_ms", limited.Decision.RetryAfter.Milliseconds())
			writeGeminiRateLimitHeaders(writer, limited.Decision)
			writeError(writer, http.StatusTooManyRequests, "RESOURCE_EXHAUSTED", "rate limit exceeded")
			return apikey.Principal{}, false
		}
		if errors.Is(err, ratelimit.ErrUnavailable) {
			handler.logger.Warn("API key rate limiter unavailable", "request_id", requestid.FromContext(request.Context()), "outcome", "unavailable")
			writeError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "rate limit service unavailable")
			return apikey.Principal{}, false
		}
		if errors.Is(err, apikey.ErrUnavailable) {
			writeError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "authentication service unavailable")
			return apikey.Principal{}, false
		}
		writeError(writer, http.StatusUnauthorized, "UNAUTHENTICATED", "authentication required")
		return apikey.Principal{}, false
	}
	if principal.RateLimitState != nil {
		if handler.telemetry != nil {
			handler.telemetry.Authentication(request.Context(), telemetry.AuthenticationRecord{Protocol: "gemini", Stage: "rate_limit", Outcome: "success"})
		}
		writer.Header().Set("X-RateLimit-Limit", strconv.FormatInt(principal.RateLimitState.Limit, 10))
		writer.Header().Set("X-RateLimit-Remaining", strconv.FormatInt(principal.RateLimitState.Remaining, 10))
		writer.Header().Set("X-RateLimit-Reset", strconv.FormatInt(principal.RateLimitState.ResetAt.Unix(), 10))
		handler.logger.Debug("API key request rate allowed", "request_id", requestid.FromContext(request.Context()), "api_key_id", principal.APIKeyID, "project_id", principal.ProjectID, "outcome", "allowed")
	}
	return principal, true
}

func writeGeminiRateLimitHeaders(writer http.ResponseWriter, decision ratelimit.Decision) {
	retrySeconds := int64((decision.RetryAfter + time.Second - 1) / time.Second)
	if retrySeconds < 1 {
		retrySeconds = 1
	}
	writer.Header().Set("Retry-After", strconv.FormatInt(retrySeconds, 10))
	writer.Header().Set("X-RateLimit-Limit", strconv.FormatInt(decision.Limit, 10))
	writer.Header().Set("X-RateLimit-Remaining", strconv.FormatInt(decision.Remaining, 10))
	writer.Header().Set("X-RateLimit-Reset", strconv.FormatInt(decision.ResetAt.Unix(), 10))
}

func (handler *Handler) complete(ctx context.Context, chargeID string, success bool, snapshot billing.ResponseSnapshot) (billing.Charge, error) {
	settlementContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	charge, err := handler.billing.Complete(settlementContext, chargeID, success, snapshot)
	if handler.telemetry != nil {
		protocol, operation := telemetry.ProtocolOperation(ctx)
		transition, outcome := "release", "success"
		if success {
			transition = "capture"
		}
		if err != nil {
			outcome = "failure"
		}
		handler.telemetry.Billing(ctx, telemetry.BillingRecord{Protocol: protocol, Operation: operation, Transition: transition, Outcome: outcome})
	}
	return charge, err
}

func (handler *Handler) beginBilling(ctx context.Context, request billing.BeginRequest) (billing.Charge, error) {
	charge, err := handler.billing.Begin(ctx, request)
	if handler.telemetry != nil {
		outcome := "success"
		if err != nil {
			outcome = "failure"
		}
		handler.telemetry.Billing(ctx, telemetry.BillingRecord{Protocol: request.Protocol, Operation: request.Operation, Transition: "begin", Outcome: outcome})
	}
	return charge, err
}

func (handler *Handler) reconciliationError(writer http.ResponseWriter, ctx context.Context, chargeID string, observation billing.Observation) {
	markContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = handler.billing.MarkReconciling(markContext, chargeID, observation)
	if handler.telemetry != nil {
		protocol, operation := telemetry.ProtocolOperation(ctx)
		handler.telemetry.Billing(ctx, telemetry.BillingRecord{Protocol: protocol, Operation: operation, Transition: "reconciling", Outcome: "success"})
	}
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

func (handler *Handler) writeBillingError(writer http.ResponseWriter, request *http.Request, err error) {
	switch {
	case errors.Is(err, costquota.ErrExceeded):
		var limited *costquota.LimitError
		if errors.As(err, &limited) {
			handler.logger.Info("cost quota exceeded", "request_id", requestid.FromContext(request.Context()), "api_key_id", limited.APIKeyID, "project_id", limited.ProjectID, "scope_type", limited.ScopeType, "period", limited.Period, "period_reset", limited.ResetAt, "category", "quota_exceeded")
		}
		writeGeminiQuotaHeaders(writer, err)
		writeError(writer, http.StatusTooManyRequests, "RESOURCE_EXHAUSTED", "cost quota exceeded")
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

func writeGeminiQuotaHeaders(writer http.ResponseWriter, err error) {
	var limited *costquota.LimitError
	if !errors.As(err, &limited) {
		return
	}
	seconds := int64(time.Until(limited.ResetAt).Seconds())
	if seconds < 1 {
		seconds = 1
	}
	writer.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
	writer.Header().Set("X-Quota-Reset", strconv.FormatInt(limited.ResetAt.Unix(), 10))
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
var errProviderPanic = errors.New("provider execution panic")

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
