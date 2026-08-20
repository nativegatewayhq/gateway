package openai

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/nativegatewayhq/gateway/internal/billing"
	"github.com/nativegatewayhq/gateway/internal/idempotency"
	"github.com/nativegatewayhq/gateway/internal/imagestorage"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	"github.com/nativegatewayhq/gateway/internal/providerhealth"
	"github.com/nativegatewayhq/gateway/internal/requestid"
	"github.com/nativegatewayhq/gateway/internal/telemetry"
	imageoperation "github.com/nativegatewayhq/gateway/operations/image"
	"github.com/nativegatewayhq/gateway/providers/openaiimages"
)

type EditHandler struct {
	common       *Handler
	spoolSlots   chan struct{}
	maxBodyBytes int64
	tempDir      string
}

func NewEditHandler(logger *slog.Logger, authenticator Authenticator, models ModelRegistry, executors map[providercredentials.ProviderID]Executor, maxBodyBytes int64, maxConcurrentSpools int) *EditHandler {
	return NewEditHandlerWithHealth(logger, authenticator, models, executors, maxBodyBytes, maxConcurrentSpools, providerhealth.NoopGate{})
}

func NewEditHandlerWithHealth(logger *slog.Logger, authenticator Authenticator, models ModelRegistry, executors map[providercredentials.ProviderID]Executor, maxBodyBytes int64, maxConcurrentSpools int, health providerhealth.Gate) *EditHandler {
	if maxConcurrentSpools < 1 {
		maxConcurrentSpools = 1
	}
	return &EditHandler{common: NewImagesHandlerWithHealth(logger, authenticator, models, executors, maxBodyBytes, health), spoolSlots: make(chan struct{}, maxConcurrentSpools), maxBodyBytes: maxBodyBytes}
}

func NewBillableEditHandler(logger *slog.Logger, authenticator Authenticator, models ModelRegistry, executors map[providercredentials.ProviderID]Executor, maxBodyBytes int64, maxConcurrentSpools int, chargeBilling Billing) *EditHandler {
	return NewBillableEditHandlerWithAvailability(logger, authenticator, models, executors, maxBodyBytes, maxConcurrentSpools, chargeBilling, nil)
}

func NewBillableEditHandlerWithAvailability(logger *slog.Logger, authenticator Authenticator, models ModelRegistry, executors map[providercredentials.ProviderID]Executor, maxBodyBytes int64, maxConcurrentSpools int, chargeBilling Billing, availability ProviderAvailability) *EditHandler {
	return NewBillableEditHandlerWithAvailabilityAndHealth(logger, authenticator, models, executors, maxBodyBytes, maxConcurrentSpools, chargeBilling, availability, providerhealth.NoopGate{})
}

func NewBillableEditHandlerWithAvailabilityAndHealth(logger *slog.Logger, authenticator Authenticator, models ModelRegistry, executors map[providercredentials.ProviderID]Executor, maxBodyBytes int64, maxConcurrentSpools int, chargeBilling Billing, availability ProviderAvailability, health providerhealth.Gate) *EditHandler {
	handler := NewEditHandlerWithHealth(logger, authenticator, models, executors, maxBodyBytes, maxConcurrentSpools, health)
	handler.common.billing = chargeBilling
	handler.common.availability = availability
	if health != nil {
		handler.common.health = health
	}
	return handler
}

func (handler *EditHandler) SetResultManager(manager ResultManager) {
	handler.common.SetResultManager(manager)
}

func (handler *EditHandler) SetTelemetry(recorder *telemetry.Recorder) {
	handler.common.SetTelemetry(recorder)
}

func (handler *EditHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	tracked := &statusWriter{ResponseWriter: writer}
	started := time.Now()
	provider := providercredentials.ProviderID("")
	logModel := "invalid"
	candidateID, channelID, routingPolicy := "", "", ""
	fallbackDepth := 0
	defer func() {
		if recover() != nil && !tracked.wroteHeader {
			writeError(tracked, 500, "server_error", "internal_error", "internal server error")
		}
		handler.common.logger.Info("openai image edit request completed", "request_id", requestid.FromContext(request.Context()), "protocol", "openai", "operation", "image.edit", "provider", string(provider), "candidate_id", candidateID, "channel_id", channelID, "routing_policy", routingPolicy, "fallback_depth", fallbackDepth, "model", logModel, "status", tracked.statusCode(), "duration", time.Since(started))
	}()
	if request.Method != http.MethodPost {
		tracked.Header().Set("Allow", http.MethodPost)
		writeError(tracked, 405, "invalid_request_error", "method_not_allowed", "method not allowed")
		return
	}
	principal, authenticated := handler.common.authenticate(tracked, request)
	if !authenticated {
		return
	}
	mediaType, parameters, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || (mediaType != "application/json" && mediaType != "multipart/form-data") {
		writeError(tracked, 400, "invalid_request_error", "invalid_content_type", "unsupported edit content type")
		return
	}
	if request.ContentLength > handler.maxBodyBytes {
		writeError(tracked, 413, "invalid_request_error", "request_too_large", "request body too large")
		return
	}
	if mediaType == "application/json" {
		body, readErr := readBounded(request.Body, handler.maxBodyBytes)
		if readErr != nil {
			writeError(tracked, 413, "invalid_request_error", "request_too_large", "request body too large")
			return
		}
		model, modelErr := extractModel(body)
		var selector imageoperation.PricingSelector
		if modelErr == nil && handler.common.billing != nil {
			selector, modelErr = imageoperation.ParseOpenAIJSONPricingSelector(body)
			model = selector.Model
		}
		var candidates []imageoperation.RoutingDecision
		if modelErr == nil {
			candidates, modelErr = handler.common.models.Candidates("openai", model, imageoperation.Edit, imageoperation.JSON)
		}
		if modelErr != nil {
			if modelErr != nil {
				handler.writeRouteError(tracked, modelErr)
			}
			return
		}
		if !handler.common.authorizeModel(tracked, request, principal, "openai", string(imageoperation.Edit), model) {
			return
		}
		route := candidates[0]
		var charge *billing.Charge
		var healthPermit providerhealth.Permit
		if handler.common.billing == nil {
			selectedRoute, permit, selected := handler.common.selectUnbilledCandidate(tracked, request, candidates)
			if !selected {
				return
			}
			route, healthPermit = selectedRoute, permit
		} else {
			idempotencyKey, keyErr := idempotency.Extract(request.Header)
			if keyErr != nil {
				writeError(tracked, http.StatusBadRequest, "invalid_request_error", "invalid_idempotency_key", "idempotency key is invalid")
				return
			}
			var fingerprint [32]byte
			legacyFingerprints := make([][32]byte, 0, len(candidates))
			if idempotencyKey != "" {
				fingerprint = idempotency.Fingerprint("openai", string(imageoperation.Edit), selector.Model, "logical-route-v1", mediaType, body)
				for _, candidate := range candidates {
					legacyFingerprints = append(legacyFingerprints, idempotency.Fingerprint("openai", string(imageoperation.Edit), selector.Model, candidate.ChannelID, mediaType, body))
				}
			}
			base := billing.BeginRequest{RequestID: requestid.FromContext(request.Context()), OrganizationID: principal.OrganizationID, ProjectID: principal.ProjectID, APIKeyID: principal.APIKeyID, Protocol: "openai", Operation: string(imageoperation.Edit), Model: selector.Model, ChannelID: candidates[0].ChannelID, Quantity: selector.Quantity, Size: selector.Size, Quality: selector.Quality, IdempotencyKey: idempotencyKey, RequestFingerprint: fingerprint, LegacyFingerprints: legacyFingerprints}
			selection, selected := handler.common.selectBillableCandidate(tracked, request, candidates, base)
			if !selected {
				return
			}
			route, charge, fallbackDepth, healthPermit = selection.decision, selection.charge, selection.rank, selection.permit
		}
		provider, logModel = route.Provider, model
		candidateID, channelID, routingPolicy = route.CandidateID, route.ChannelID, string(route.Policy)
		outboundBody, rewriteErr := imageoperation.RewriteJSONModel(body, route.ProviderModel)
		if rewriteErr != nil {
			handler.common.releaseHealthPermit(request, healthPermit)
			writeError(tracked, 400, "invalid_request_error", "invalid_model", "request must contain one model")
			return
		}
		handler.execute(tracked, request, route, charge, healthPermit, request.Header.Get("Content-Type"), int64(len(outboundBody)), bytes.NewReader(outboundBody))
		return
	}
	boundary := parameters["boundary"]
	if boundary == "" {
		writeError(tracked, 400, "invalid_request_error", "invalid_multipart", "multipart boundary required")
		return
	}
	select {
	case handler.spoolSlots <- struct{}{}:
		defer func() { <-handler.spoolSlots }()
	default:
		writeError(tracked, 503, "server_error", "spool_capacity_exhausted", "edit capacity unavailable")
		return
	}
	file, err := os.CreateTemp(handler.tempDir, "gateway-image-edit-*")
	if err != nil {
		writeError(tracked, 503, "server_error", "spool_unavailable", "edit capacity unavailable")
		return
	}
	name := file.Name()
	defer os.Remove(name)
	defer file.Close()
	written, err := io.Copy(file, io.LimitReader(request.Body, handler.maxBodyBytes+1))
	if err != nil || written > handler.maxBodyBytes {
		writeError(tracked, 413, "invalid_request_error", "request_too_large", "request body too large")
		return
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		writeError(tracked, 500, "server_error", "internal_error", "internal server error")
		return
	}
	model, err := multipartModel(file, boundary)
	var selector imageoperation.PricingSelector
	if err == nil && handler.common.billing != nil {
		if _, err = file.Seek(0, io.SeekStart); err == nil {
			selector, err = imageoperation.ParseOpenAIMultipartPricingSelector(file, boundary)
			model = selector.Model
		}
	}
	var candidates []imageoperation.RoutingDecision
	if err == nil {
		candidates, err = handler.common.models.Candidates("openai", model, imageoperation.Edit, imageoperation.Multipart)
	}
	if err != nil {
		if err != nil {
			handler.writeRouteError(tracked, err)
		}
		return
	}
	if !handler.common.authorizeModel(tracked, request, principal, "openai", string(imageoperation.Edit), model) {
		return
	}
	route := candidates[0]
	var charge *billing.Charge
	var healthPermit providerhealth.Permit
	if handler.common.billing == nil {
		selectedRoute, permit, selected := handler.common.selectUnbilledCandidate(tracked, request, candidates)
		if !selected {
			return
		}
		route, healthPermit = selectedRoute, permit
	} else {
		idempotencyKey, keyErr := idempotency.Extract(request.Header)
		if keyErr != nil {
			writeError(tracked, http.StatusBadRequest, "invalid_request_error", "invalid_idempotency_key", "idempotency key is invalid")
			return
		}
		var fingerprint [32]byte
		legacyFingerprints := make([][32]byte, 0, len(candidates))
		if idempotencyKey != "" {
			fingerprint, err = fingerprintMultipart(file, written, "logical-route-v1", selector.Model, mediaType)
			if err == nil {
				for _, candidate := range candidates {
					var legacy [32]byte
					legacy, err = fingerprintMultipart(file, written, candidate.ChannelID, selector.Model, mediaType)
					if err != nil {
						break
					}
					legacyFingerprints = append(legacyFingerprints, legacy)
				}
			}
			if err != nil {
				writeError(tracked, http.StatusInternalServerError, "server_error", "internal_error", "internal server error")
				return
			}
		}
		base := billing.BeginRequest{RequestID: requestid.FromContext(request.Context()), OrganizationID: principal.OrganizationID, ProjectID: principal.ProjectID, APIKeyID: principal.APIKeyID, Protocol: "openai", Operation: string(imageoperation.Edit), Model: selector.Model, ChannelID: candidates[0].ChannelID, Quantity: selector.Quantity, Size: selector.Size, Quality: selector.Quality, IdempotencyKey: idempotencyKey, RequestFingerprint: fingerprint, LegacyFingerprints: legacyFingerprints}
		selection, selected := handler.common.selectBillableCandidate(tracked, request, candidates, base)
		if !selected {
			return
		}
		route, charge, fallbackDepth, healthPermit = selection.decision, selection.charge, selection.rank, selection.permit
	}
	provider, logModel = route.Provider, model
	candidateID, channelID, routingPolicy = route.CandidateID, route.ChannelID, string(route.Policy)
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		handler.common.releaseHealthPermit(request, healthPermit)
		writeError(tracked, 500, "server_error", "internal_error", "internal server error")
		return
	}
	providerFile, err := os.CreateTemp(handler.tempDir, "gateway-image-edit-routed-*")
	if err != nil {
		handler.common.releaseHealthPermit(request, healthPermit)
		writeError(tracked, 503, "server_error", "spool_unavailable", "edit capacity unavailable")
		return
	}
	providerName := providerFile.Name()
	defer os.Remove(providerName)
	defer providerFile.Close()
	providerWritten, err := imageoperation.RewriteMultipartModel(file, boundary, route.ProviderModel, providerFile)
	if err != nil || providerWritten > handler.maxBodyBytes {
		handler.common.releaseHealthPermit(request, healthPermit)
		writeError(tracked, 400, "invalid_request_error", "invalid_model", "request must contain one model")
		return
	}
	if _, err := providerFile.Seek(0, io.SeekStart); err != nil {
		handler.common.releaseHealthPermit(request, healthPermit)
		writeError(tracked, 500, "server_error", "internal_error", "internal server error")
		return
	}
	_ = file.Close()
	_ = os.Remove(name)
	handler.execute(tracked, request, route, charge, healthPermit, request.Header.Get("Content-Type"), providerWritten, providerFile)
}

func (handler *EditHandler) writeRouteError(writer http.ResponseWriter, err error) {
	if errors.Is(err, imageoperation.ErrModelNotFound) {
		writeError(writer, 404, "invalid_request_error", "model_not_found", "model not found")
	} else if errors.Is(err, imageoperation.ErrUnsupported) {
		writeError(writer, 400, "invalid_request_error", "unsupported_media_type_for_model", "content type is not supported for model")
	} else {
		writeError(writer, 400, "invalid_request_error", "invalid_model", "request must contain one model")
	}
}

func (handler *EditHandler) execute(writer http.ResponseWriter, request *http.Request, route imageoperation.RoutingDecision, charge *billing.Charge, healthPermit providerhealth.Permit, contentType string, length int64, body io.Reader) {
	dispatched := false
	defer func() {
		if recover() != nil {
			if healthPermit.ChannelID != "" {
				if dispatched {
					handler.common.observeHealth(request, route, healthPermit, nil, errProviderPanic)
				} else {
					handler.common.releaseHealthPermit(request, healthPermit)
				}
			}
			if charge != nil {
				handler.common.reconciliationError(writer, request.Context(), charge.ID, billing.Observation{Outcome: billing.Unknown, Reason: billing.ProviderPanic})
			} else {
				writeError(writer, http.StatusInternalServerError, "server_error", "internal_error", "internal server error")
			}
		}
	}()
	executor := handler.common.executors[route.Provider]
	if executor == nil {
		handler.common.releaseHealthPermit(request, healthPermit)
		writeError(writer, 503, "server_error", "provider_unavailable", "provider unavailable")
		return
	}
	dispatched = true
	if handler.common.telemetry != nil {
		handler.common.telemetry.Route(request.Context(), telemetry.RouteRecord{Protocol: "openai", Operation: string(imageoperation.Edit), Policy: string(route.Policy), Outcome: "success"})
	}
	response, err := handler.common.executeProvider(request.Context(), executor, route, imageoperation.Edit, openaiimages.Request{Operation: openaiimages.Edit, ChannelID: route.ChannelID, ContentType: contentType, ContentLength: length, Accept: request.Header.Get("Accept"), UserAgent: request.UserAgent(), Body: body})
	handler.common.observeHealth(request, route, healthPermit, response, err)
	if err != nil {
		if charge != nil {
			snapshot := handler.common.executorErrorSnapshot(err)
			if errors.Is(err, providercredentials.ErrCredentialUnavailable) {
				completed, completeErr := handler.common.complete(request.Context(), charge.ID, false, snapshot)
				if completeErr == nil {
					handler.common.writeSnapshot(writer, completed.Response, false)
					return
				}
				handler.common.reconciliationError(writer, request.Context(), charge.ID, knownObservation(false, billing.SettlementFailed, snapshot))
				return
			}
			reason := billing.ExecutorConnection
			if errors.Is(err, openaiimages.ErrTimeout) {
				reason = billing.ExecutorTimeout
			}
			handler.common.reconciliationError(writer, request.Context(), charge.ID, billing.Observation{Outcome: billing.Unknown, Reason: reason})
			return
		}
		handler.common.writeExecutorError(writer, err)
		return
	}
	defer response.Body.Close()
	if charge != nil {
		responseBody, readErr := readBounded(response.Body, handler.common.billing.MaximumResponseBytes())
		if readErr != nil {
			handler.common.reconciliationError(writer, request.Context(), charge.ID, knownObservation(response.StatusCode >= 200 && response.StatusCode <= 299, billing.ResponseUnavailable, handler.common.responseUnavailableSnapshot()))
			return
		}
		snapshot := billing.ResponseSnapshot{Status: response.StatusCode, Headers: safeResponseHeaders(response.Header), Body: responseBody}
		if response.StatusCode >= 200 && response.StatusCode <= 299 && handler.common.results != nil {
			managedBody, storageErr := handler.common.results.Transform(request.Context(), imagestorage.TransformInput{Protocol: "openai", Provider: string(route.Provider), ChannelID: route.ChannelID, RequestID: requestid.FromContext(request.Context()), ChargeID: charge.ID, Body: responseBody})
			if storageErr != nil {
				handler.common.reconciliationError(writer, request.Context(), charge.ID, knownObservation(true, billing.StorageFailed, snapshot))
				return
			}
			snapshot.Body = managedBody
		}
		completed, completeErr := handler.common.complete(request.Context(), charge.ID, response.StatusCode >= 200 && response.StatusCode <= 299, snapshot)
		if completeErr != nil {
			handler.common.reconciliationError(writer, request.Context(), charge.ID, knownObservation(response.StatusCode >= 200 && response.StatusCode <= 299, billing.SettlementFailed, snapshot))
			return
		}
		handler.common.writeSnapshot(writer, completed.Response, false)
		return
	}
	if response.StatusCode >= 200 && response.StatusCode <= 299 && handler.common.results != nil {
		responseBody, readErr := readBounded(response.Body, handler.common.results.MaximumResponseBytes())
		if readErr != nil {
			handler.common.writeSnapshot(writer, handler.common.storageErrorSnapshot(), false)
			return
		}
		managedBody, storageErr := handler.common.results.Transform(request.Context(), imagestorage.TransformInput{Protocol: "openai", Provider: string(route.Provider), ChannelID: route.ChannelID, RequestID: requestid.FromContext(request.Context()), Body: responseBody})
		if storageErr != nil {
			handler.common.writeSnapshot(writer, handler.common.storageErrorSnapshot(), false)
			return
		}
		handler.common.writeSnapshot(writer, billing.ResponseSnapshot{Status: response.StatusCode, Headers: safeResponseHeaders(response.Header), Body: managedBody}, false)
		return
	}
	copyResponseHeaders(writer.Header(), response.Header)
	writer.WriteHeader(response.StatusCode)
	_, _ = io.Copy(writer, response.Body)
}

func fingerprintMultipart(file *os.File, size int64, routeIdentity, model, mediaType string) ([32]byte, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return [32]byte{}, err
	}
	return idempotency.FingerprintReader("openai", string(imageoperation.Edit), model, routeIdentity, mediaType, file, size)
}

func multipartModel(reader io.Reader, boundary string) (string, error) {
	multipartReader := multipart.NewReader(reader, boundary)
	model := ""
	count := 0
	for {
		part, err := multipartReader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		if part.FormName() == "model" {
			count++
			value, err := io.ReadAll(io.LimitReader(part, 202))
			if err != nil || len(value) > 200 {
				return "", errors.New("invalid model")
			}
			model = string(value)
		} else {
			_, _ = io.Copy(io.Discard, part)
		}
		part.Close()
	}
	if count != 1 || model == "" || strings.TrimSpace(model) != model {
		return "", errors.New("invalid model")
	}
	return model, nil
}
