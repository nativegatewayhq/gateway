package openai

import (
	"context"
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"time"

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

type TranslationHandler struct {
	common                                                                         *Handler
	models                                                                         TranslationRegistry
	executor                                                                       TranslationExecutor
	health                                                                         providerhealth.Gate
	spoolSlots                                                                     chan struct{}
	maximumRequestBytes, maximumFileBytes, maximumFieldBytes, maximumResponseBytes int64
	tempDir                                                                        string
	telemetry                                                                      *telemetry.Recorder
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
	form, err := helper.parseMultipart(tracked, request, parameters["boundary"])
	if err != nil {
		status, code := 400, "invalid_multipart"
		if errors.Is(err, errTranscriptionTooLarge) {
			status, code = 413, "request_too_large"
		}
		writeError(tracked, status, "invalid_request_error", code, "invalid translation request")
		return
	}
	defer form.file.Cleanup()
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
	outbound, contentType, length, err := helper.buildMultipart(form, route.ProviderModel)
	if err != nil {
		handler.releaseHealth(request, permit)
		writeError(tracked, 503, "server_error", "spool_unavailable", "translation capacity unavailable")
		return
	}
	defer outbound.Close()
	defer os.Remove(outbound.Name())
	response, executeErr := handler.execute(request.Context(), route, openaiProvider.TranslationRequest{ChannelID: route.ChannelID, ContentType: contentType, ContentLength: length, Accept: request.Header.Get("Accept"), UserAgent: request.UserAgent(), Body: outbound})
	if executeErr != nil {
		handler.observe(request, permit, nil, executeErr)
		outcome = "failure"
		handler.writeExecutorError(tracked, executeErr)
		return
	}
	defer response.Body.Close()
	body, readErr := readBounded(response.Body, handler.maximumResponseBytes)
	if readErr != nil || !validTranscriptionResponseType(response.Header.Get("Content-Type")) {
		handler.observe(request, permit, response, openaiProvider.ErrTranslationUpstream)
		outcome = "failure"
		writeError(tracked, 502, "server_error", "invalid_provider_response", "invalid provider response")
		return
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
