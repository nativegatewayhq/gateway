package openai

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/nativegatewayhq/gateway/internal/apikey"
	"github.com/nativegatewayhq/gateway/internal/audiobilling"
	"github.com/nativegatewayhq/gateway/internal/idempotency"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	"github.com/nativegatewayhq/gateway/internal/providerhealth"
	"github.com/nativegatewayhq/gateway/internal/requestid"
	"github.com/nativegatewayhq/gateway/internal/telemetry"
	audiooperation "github.com/nativegatewayhq/gateway/operations/audio"
	openaiProvider "github.com/nativegatewayhq/gateway/providers/openai"
)

var translationTemperature = regexp.MustCompile(`^(?:0(?:\.[0-9]+)?|1(?:\.0+)?)$`)

type TranslationRegistry interface {
	Resolve(string) (audiooperation.TranslationModel, error)
}

type TranslationExecutor interface {
	Create(context.Context, openaiProvider.TranslationRequest) (*http.Response, error)
}

type TranslationBilling interface {
	Begin(context.Context, audiobilling.TranslationBeginRequest) (audiobilling.TranslationCharge, error)
	Complete(context.Context, string, audiobilling.TranslationEvidence) (audiobilling.TranslationCharge, error)
	Release(context.Context, string, string) (audiobilling.TranslationCharge, error)
	MarkReconciling(context.Context, string, string, *audiobilling.TranslationEvidence) error
}

type TranslationHandler struct {
	common                                                                         *Handler
	models                                                                         TranslationRegistry
	executor                                                                       TranslationExecutor
	health                                                                         providerhealth.Gate
	spoolSlots                                                                     chan struct{}
	maximumRequestBytes, maximumFileBytes, maximumFieldBytes, maximumResponseBytes int64
	tempDir                                                                        string
	telemetry                                                                      *telemetry.Recorder
	billing                                                                        TranslationBilling
	assets                                                                         AudioAssetMaterializer
}

func (handler *TranslationHandler) SetAudioAssets(assets AudioAssetMaterializer) {
	handler.assets = assets
}

func NewBillableTranslationHandler(logger *slog.Logger, authenticator Authenticator, models TranslationRegistry, executor TranslationExecutor, health providerhealth.Gate, requestBytes, fileBytes, fieldBytes, responseBytes int64, spoolLimit int, billing TranslationBilling) *TranslationHandler {
	handler := NewTranslationHandler(logger, authenticator, models, executor, health, requestBytes, fileBytes, fieldBytes, responseBytes, spoolLimit)
	handler.billing = billing
	return handler
}

func NewTranslationHandler(logger *slog.Logger, authenticator Authenticator, models TranslationRegistry, executor TranslationExecutor, health providerhealth.Gate, requestBytes, fileBytes, fieldBytes, responseBytes int64, spoolLimit int) *TranslationHandler {
	if health == nil {
		health = providerhealth.NoopGate{}
	}
	if spoolLimit < 1 {
		spoolLimit = 1
	}
	return &TranslationHandler{common: NewImagesHandler(logger, authenticator, nil, nil, 1), models: models, executor: executor, health: health, spoolSlots: make(chan struct{}, spoolLimit), maximumRequestBytes: requestBytes, maximumFileBytes: fileBytes, maximumFieldBytes: fieldBytes, maximumResponseBytes: responseBytes}
}

func (handler *TranslationHandler) SetTelemetry(recorder *telemetry.Recorder) {
	handler.telemetry = recorder
	handler.common.SetTelemetry(recorder)
}

func (handler *TranslationHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	tracked := &statusWriter{ResponseWriter: writer}
	started := time.Now()
	outcome := "neutral"
	defer func() {
		if recover() != nil && !tracked.wroteHeader {
			writeError(tracked, 500, "server_error", "internal_error", "internal server error")
			outcome = "failure"
		}
		handler.common.logger.Info("openai translation request completed", "request_id", requestid.FromContext(request.Context()), "protocol", "openai", "operation", audiooperation.Translation, "status", tracked.statusCode(), "outcome", outcome, "duration", time.Since(started))
	}()
	if request.Method != http.MethodPost {
		tracked.Header().Set("Allow", http.MethodPost)
		writeError(tracked, 405, "invalid_request_error", "method_not_allowed", "method not allowed")
		return
	}
	principal, ok := handler.common.authenticate(tracked, request)
	if !ok {
		return
	}
	if handler.invalidLimits() {
		writeError(tracked, 503, "server_error", "translation_unavailable", "translation unavailable")
		return
	}
	mediaType, parameters, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" || parameters["boundary"] == "" || len(parameters["boundary"]) > 200 {
		writeError(tracked, 400, "invalid_request_error", "invalid_multipart", "invalid translation request")
		return
	}
	select {
	case handler.spoolSlots <- struct{}{}:
		defer func() { <-handler.spoolSlots }()
	default:
		writeError(tracked, 503, "server_error", "spool_capacity_exhausted", "translation capacity unavailable")
		return
	}
	helper := NewTranscriptionHandler(handler.common.logger, nil, nil, nil, nil, handler.maximumRequestBytes, handler.maximumFileBytes, handler.maximumFieldBytes, handler.maximumResponseBytes, 1)
	helper.tempDir = handler.tempDir
	assetID, assetRequested := requestedAudioAsset(request.Header)
	if assetRequested && handler.assets == nil {
		writeError(tracked, 503, "server_error", "audio_asset_unavailable", "audio asset unavailable")
		return
	}
	form, err := helper.parseMultipart(tracked, request, parameters["boundary"], assetRequested)
	if err != nil {
		status, code := 400, "invalid_multipart"
		if errors.Is(err, errTranscriptionTooLarge) {
			status, code = 413, "request_too_large"
		}
		writeError(tracked, status, "invalid_request_error", code, "invalid translation request")
		return
	}
	defer form.file.Cleanup()
	if assetRequested {
		materialized, materializeErr := handler.assets.Materialize(request.Context(), principal, assetID)
		if materializeErr != nil {
			writeError(tracked, 404, "invalid_request_error", "asset_not_found", "audio asset not found")
			return
		}
		defer handler.assets.Release(context.WithoutCancel(request.Context()), materialized)
		if materializeErr = applyMaterializedAudio(form, materialized); materializeErr != nil {
			writeError(tracked, 503, "server_error", "audio_asset_unavailable", "audio asset unavailable")
			return
		}
	}
	if handler.models == nil || handler.executor == nil {
		writeError(tracked, 503, "server_error", "provider_unavailable", "provider unavailable")
		return
	}
	route, err := handler.models.Resolve(form.model)
	if errors.Is(err, audiooperation.ErrModelNotFound) {
		writeError(tracked, 404, "invalid_request_error", "model_not_found", "model not found")
		return
	}
	if err != nil || route.Provider != providercredentials.OpenAI || route.ChannelID == "" {
		writeError(tracked, 503, "server_error", "provider_unavailable", "provider unavailable")
		return
	}
	if err = validateTranslationForm(form, route.Capabilities); err != nil {
		writeError(tracked, 400, "invalid_request_error", "unsupported_translation_option", "translation option is not supported for model")
		return
	}
	if !handler.common.authorizeModel(tracked, request, principal, "openai", audiooperation.Translation, route.ID) {
		return
	}
	permit, ok := handler.acquireHealth(tracked, request, route.ChannelID)
	if !ok {
		return
	}
	charge, ok := handler.beginCharge(tracked, request, principal, route, form)
	if !ok {
		handler.releaseHealth(request, permit)
		return
	}
	outbound, contentType, length, err := helper.buildMultipart(form, route.ProviderModel)
	if err != nil {
		handler.releaseHealth(request, permit)
		if charge.ID != "" {
			if _, releaseErr := handler.billing.Release(context.WithoutCancel(request.Context()), charge.ID, "local_spool_failure"); releaseErr != nil {
				_ = handler.billing.MarkReconciling(context.WithoutCancel(request.Context()), charge.ID, "release_failed", nil)
			}
		}
		writeError(tracked, 503, "server_error", "spool_unavailable", "translation capacity unavailable")
		return
	}
	defer outbound.Close()
	defer os.Remove(outbound.Name())
	response, executeErr := handler.execute(request.Context(), route, openaiProvider.TranslationRequest{ChannelID: route.ChannelID, ContentType: contentType, ContentLength: length, Accept: request.Header.Get("Accept"), UserAgent: request.UserAgent(), Body: outbound})
	if executeErr != nil {
		handler.observe(request, permit, nil, executeErr)
		outcome = "failure"
		if charge.ID != "" {
			_ = handler.billing.MarkReconciling(context.WithoutCancel(request.Context()), charge.ID, "executor_uncertain", nil)
		}
		handler.writeExecutorError(tracked, executeErr)
		return
	}
	defer response.Body.Close()
	body, readErr := readBounded(response.Body, handler.maximumResponseBytes)
	if readErr != nil || !validTranscriptionResponseType(response.Header.Get("Content-Type")) {
		handler.observe(request, permit, response, openaiProvider.ErrTranslationUpstream)
		outcome = "failure"
		if charge.ID != "" {
			_ = handler.billing.MarkReconciling(context.WithoutCancel(request.Context()), charge.ID, "invalid_provider_response", nil)
		}
		writeError(tracked, 502, "server_error", "invalid_provider_response", "invalid provider response")
		return
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		if charge.ID != "" {
			if _, releaseErr := handler.billing.Release(context.WithoutCancel(request.Context()), charge.ID, "provider_non_2xx"); releaseErr != nil {
				_ = handler.billing.MarkReconciling(context.WithoutCancel(request.Context()), charge.ID, "release_failed", nil)
			}
		}
		copyTranscriptionHeaders(tracked.Header(), response.Header)
		tracked.Header().Set("Content-Length", strconv.Itoa(len(body)))
		tracked.WriteHeader(response.StatusCode)
		_, _ = tracked.Write(body)
		handler.observe(request, permit, response, nil)
		outcome = "failure"
		return
	}
	if charge.ID != "" {
		duration, durationErr := extractTranslationDuration(body)
		if durationErr != nil {
			_ = handler.billing.MarkReconciling(context.WithoutCancel(request.Context()), charge.ID, "duration_invalid", nil)
			handler.observe(request, permit, response, openaiProvider.ErrTranslationUpstream)
			outcome = "failure"
			writeError(tracked, 502, "server_error", "invalid_provider_duration", "invalid provider response")
			return
		}
		evidence := audiobilling.TranslationEvidence{SchemaVersion: "openai-translation-duration-json-v1", DurationMilliseconds: duration, Status: response.StatusCode, Headers: map[string][]string(response.Header), SHA256: sha256.Sum256(body)}
		if _, settleErr := handler.billing.Complete(context.WithoutCancel(request.Context()), charge.ID, evidence); settleErr != nil {
			_ = handler.billing.MarkReconciling(context.WithoutCancel(request.Context()), charge.ID, "settlement_failed", &evidence)
			handler.observe(request, permit, response, settleErr)
			outcome = "failure"
			writeError(tracked, 503, "server_error", "billing_unavailable", "billing unavailable")
			return
		}
	}
	copyTranscriptionHeaders(tracked.Header(), response.Header)
	tracked.Header().Set("Content-Length", strconv.Itoa(len(body)))
	tracked.WriteHeader(response.StatusCode)
	_, _ = tracked.Write(body)
	handler.observe(request, permit, response, nil)
	if response.StatusCode >= 200 && response.StatusCode < 400 {
		outcome = "success"
	} else {
		outcome = "failure"
	}
}

func (handler *TranslationHandler) beginCharge(writer http.ResponseWriter, request *http.Request, principal apikey.Principal, route audiooperation.TranslationModel, form *transcriptionForm) (audiobilling.TranslationCharge, bool) {
	if handler.billing == nil {
		return audiobilling.TranslationCharge{}, true
	}
	if form.responseFormat != "verbose_json" {
		writeError(writer, 400, "invalid_request_error", "unsupported_billing_response_format", "response format cannot be billed")
		return audiobilling.TranslationCharge{}, false
	}
	key := request.Header.Get("Idempotency-Key")
	if !idempotency.Valid(key) {
		writeError(writer, 400, "invalid_request_error", "invalid_idempotency_key", "valid Idempotency-Key is required")
		return audiobilling.TranslationCharge{}, false
	}
	fingerprint, err := translationFingerprint(form, route)
	if err != nil {
		writeError(writer, 503, "server_error", "spool_unavailable", "translation capacity unavailable")
		return audiobilling.TranslationCharge{}, false
	}
	charge, err := handler.billing.Begin(request.Context(), audiobilling.TranslationBeginRequest{RequestID: requestid.FromContext(request.Context()), OrganizationID: principal.OrganizationID, ProjectID: principal.ProjectID, APIKeyID: principal.APIKeyID, Model: route.ID, ChannelID: route.ChannelID, IdempotencyKey: key, Fingerprint: fingerprint})
	if err == nil {
		return charge, true
	}
	status, code := 503, "billing_unavailable"
	if errors.Is(err, audiobilling.ErrConflict) || errors.Is(err, audiobilling.ErrPending) {
		status, code = 409, "idempotency_conflict"
	} else if errors.Is(err, audiobilling.ErrInvalid) {
		status, code = 400, "invalid_request"
	}
	writeError(writer, status, "invalid_request_error", code, "request could not be billed")
	return audiobilling.TranslationCharge{}, false
}

func translationFingerprint(form *transcriptionForm, route audiooperation.TranslationModel) ([32]byte, error) {
	hash := sha256.New()
	for _, value := range []string{"openai", audiooperation.Translation, route.ID, route.ChannelID, route.ProviderModel, form.filename, form.fileType, form.assetID} {
		_, _ = hash.Write([]byte(strconv.Itoa(len(value))))
		_, _ = hash.Write([]byte{':'})
		_, _ = hash.Write([]byte(value))
	}
	fields := append([]transcriptionField(nil), form.fields...)
	sort.Slice(fields, func(i, j int) bool { return fields[i].name < fields[j].name })
	for _, field := range fields {
		_, _ = hash.Write([]byte(field.name))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(field.value))
		_, _ = hash.Write([]byte{0})
	}
	file, err := form.file.Open()
	if err != nil {
		return [32]byte{}, err
	}
	_, err = io.Copy(hash, file)
	file.Close()
	if err != nil {
		return [32]byte{}, err
	}
	var fingerprint [32]byte
	copy(fingerprint[:], hash.Sum(nil))
	return fingerprint, nil
}

func validateTranslationForm(form *transcriptionForm, capability audiooperation.TranslationCapabilities) error {
	format := form.responseFormat
	if format == "" {
		format = "json"
	}
	allowed := false
	for _, candidate := range capability.ResponseFormats {
		allowed = allowed || candidate == format
	}
	if !allowed || form.stream {
		return errTranscriptionInvalid
	}
	for _, field := range form.fields {
		switch field.name {
		case "model", "response_format":
		case "prompt":
			if !capability.Prompt {
				return errTranscriptionInvalid
			}
		case "temperature":
			if !capability.Temperature || !translationTemperature.MatchString(field.value) {
				return errTranscriptionInvalid
			}
		default:
			return errTranscriptionInvalid
		}
	}
	return nil
}

func (handler *TranslationHandler) invalidLimits() bool {
	return handler.maximumRequestBytes < 1 || handler.maximumFileBytes < 1 || handler.maximumFileBytes > handler.maximumRequestBytes || handler.maximumFieldBytes < 1 || handler.maximumResponseBytes < 1
}

func (handler *TranslationHandler) acquireHealth(writer http.ResponseWriter, request *http.Request, channel string) (providerhealth.Permit, bool) {
	snapshot, err := handler.health.Inspect(request.Context(), channel)
	if err != nil || snapshot.State == providerhealth.Open {
		writeError(writer, 503, "server_error", "provider_unavailable", "provider unavailable")
		return providerhealth.Permit{}, false
	}
	if snapshot.State == providerhealth.HalfOpen {
		permit, claimErr := handler.health.ClaimProbe(request.Context(), channel, requestid.FromContext(request.Context()))
		if claimErr != nil {
			writeError(writer, 503, "server_error", "provider_unavailable", "provider unavailable")
			return providerhealth.Permit{}, false
		}
		return permit, true
	}
	return providerhealth.Permit{ChannelID: channel}, true
}

func (handler *TranslationHandler) releaseHealth(request *http.Request, permit providerhealth.Permit) {
	_, _ = handler.health.Observe(context.WithoutCancel(request.Context()), providerhealth.Observation{ChannelID: permit.ChannelID, ObservationID: requestid.FromContext(request.Context()), Outcome: providerhealth.Neutral, Permit: permit})
}

func (handler *TranslationHandler) observe(request *http.Request, permit providerhealth.Permit, response *http.Response, err error) {
	result := providerhealth.Neutral
	switch {
	case errors.Is(err, openaiProvider.ErrTranslationTimeout):
		result = providerhealth.Timeout
	case err != nil:
		result = providerhealth.Connection
	case response.StatusCode == 429:
		result = providerhealth.RateLimited
	case response.StatusCode >= 500:
		result = providerhealth.ServerError
	case response.StatusCode >= 200 && response.StatusCode < 400:
		result = providerhealth.Success
	}
	_, _ = handler.health.Observe(context.WithoutCancel(request.Context()), providerhealth.Observation{ChannelID: permit.ChannelID, ObservationID: requestid.FromContext(request.Context()), Outcome: result, Permit: permit})
}

func (handler *TranslationHandler) execute(ctx context.Context, route audiooperation.TranslationModel, input openaiProvider.TranslationRequest) (response *http.Response, err error) {
	if handler.telemetry != nil {
		providerContext, span, started := handler.telemetry.StartProvider(ctx, string(route.Provider), "openai", audiooperation.Translation)
		defer func() {
			if recover() != nil {
				response, err = nil, errProviderPanic
			}
			outcome := "success"
			if errors.Is(err, openaiProvider.ErrTranslationTimeout) {
				outcome = "timeout"
			} else if err != nil {
				outcome = "failure"
			}
			handler.telemetry.EndProvider(providerContext, span, started, telemetry.ProviderRecord{Provider: string(route.Provider), Protocol: "openai", Operation: audiooperation.Translation, Outcome: outcome})
		}()
		return handler.executor.Create(providerContext, input)
	}
	defer func() {
		if recover() != nil {
			response, err = nil, errProviderPanic
		}
	}()
	return handler.executor.Create(ctx, input)
}

func (handler *TranslationHandler) writeExecutorError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, providercredentials.ErrCredentialUnavailable):
		writeError(writer, 503, "server_error", "provider_unavailable", "provider unavailable")
	case errors.Is(err, openaiProvider.ErrTranslationTimeout):
		writeError(writer, 504, "server_error", "upstream_timeout", "provider request timed out")
	case errors.Is(err, openaiProvider.ErrTranslationCanceled):
		writeError(writer, 499, "server_error", "request_canceled", "request canceled")
	default:
		writeError(writer, 502, "server_error", "upstream_unavailable", "provider unavailable")
	}
}
