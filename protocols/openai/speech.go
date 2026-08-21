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
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	"github.com/nativegatewayhq/gateway/internal/providerhealth"
	"github.com/nativegatewayhq/gateway/internal/requestid"
	"github.com/nativegatewayhq/gateway/internal/telemetry"
	audiooperation "github.com/nativegatewayhq/gateway/operations/audio"
	openaiProvider "github.com/nativegatewayhq/gateway/providers/openai"
)

var (
	errSpeechStreamWrite    = errors.New("speech stream write failed")
	errSpeechStreamTooLarge = errors.New("speech stream too large")
)

type SpeechRegistry interface {
	Resolve(string) (audiooperation.Model, error)
}

type SpeechExecutor interface {
	Create(context.Context, openaiProvider.SpeechRequest) (*http.Response, error)
}

type SpeechHandler struct {
	common               *Handler
	models               SpeechRegistry
	executor             SpeechExecutor
	health               providerhealth.Gate
	maximumRequestBytes  int64
	maximumResponseBytes int64
	telemetry            *telemetry.Recorder
}

func NewSpeechHandler(logger *slog.Logger, authenticator Authenticator, models SpeechRegistry, executor SpeechExecutor, health providerhealth.Gate, maximumRequestBytes, maximumResponseBytes int64) *SpeechHandler {
	if health == nil {
		health = providerhealth.NoopGate{}
	}
	return &SpeechHandler{common: NewImagesHandler(logger, authenticator, nil, nil, 1), models: models, executor: executor, health: health, maximumRequestBytes: maximumRequestBytes, maximumResponseBytes: maximumResponseBytes}
}

func (handler *SpeechHandler) SetTelemetry(recorder *telemetry.Recorder) {
	handler.telemetry = recorder
	handler.common.SetTelemetry(recorder)
}

func (handler *SpeechHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	tracked := &statusWriter{ResponseWriter: writer}
	started := time.Now()
	providerOutcome := "neutral"
	defer func() {
		if recover() != nil && !tracked.wroteHeader {
			writeError(tracked, http.StatusInternalServerError, "server_error", "internal_error", "internal server error")
			providerOutcome = "failure"
		}
		handler.common.logger.Info("openai speech request completed", "request_id", requestid.FromContext(request.Context()), "protocol", "openai", "operation", audiooperation.Speech, "status", tracked.statusCode(), "outcome", providerOutcome, "duration", time.Since(started))
	}()
	if request.Method != http.MethodPost {
		tracked.Header().Set("Allow", http.MethodPost)
		writeError(tracked, http.StatusMethodNotAllowed, "invalid_request_error", "method_not_allowed", "method not allowed")
		return
	}
	principal, ok := handler.common.authenticate(tracked, request)
	if !ok {
		return
	}
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(tracked, http.StatusBadRequest, "invalid_request_error", "invalid_content_type", "content type must be application/json")
		return
	}
	if handler.maximumRequestBytes < 1 || request.ContentLength > handler.maximumRequestBytes {
		writeError(tracked, http.StatusRequestEntityTooLarge, "invalid_request_error", "request_too_large", "request body too large")
		return
	}
	body, err := readBounded(request.Body, handler.maximumRequestBytes)
	if err != nil {
		status := http.StatusBadRequest
		code := "invalid_request"
		if errors.Is(err, errBodyTooLarge) {
			status, code = http.StatusRequestEntityTooLarge, "request_too_large"
		}
		writeError(tracked, status, "invalid_request_error", code, "invalid request body")
		return
	}
	envelope, err := parseSpeechEnvelope(body)
	if err != nil {
		writeError(tracked, http.StatusBadRequest, "invalid_request_error", "invalid_request", "invalid speech request")
		return
	}
	if handler.models == nil || handler.executor == nil || handler.maximumResponseBytes < 1 {
		writeError(tracked, http.StatusServiceUnavailable, "server_error", "provider_unavailable", "provider unavailable")
		return
	}
	route, err := handler.models.Resolve(envelope.Model)
	if errors.Is(err, audiooperation.ErrModelNotFound) {
		writeError(tracked, http.StatusNotFound, "invalid_request_error", "model_not_found", "model not found")
		return
	}
	if err != nil || route.Provider != providercredentials.OpenAI || route.ChannelID == "" {
		writeError(tracked, http.StatusServiceUnavailable, "server_error", "provider_unavailable", "provider unavailable")
		return
	}
	if !handler.common.authorizeModel(tracked, request, principal, "openai", audiooperation.Speech, route.ID) {
		return
	}
	permit, ok := handler.acquireHealth(tracked, request, route.ChannelID)
	if !ok {
		return
	}
	response, executeErr := handler.execute(request.Context(), route, openaiProvider.SpeechRequest{ChannelID: route.ChannelID, ContentType: "application/json", Accept: request.Header.Get("Accept"), UserAgent: request.Header.Get("User-Agent"), ContentLength: int64(len(body)), Body: bytes.NewReader(body)})
	if executeErr != nil {
		handler.observe(request, permit, nil, executeErr)
		providerOutcome = "failure"
		handler.writeExecutorError(tracked, executeErr)
		return
	}
	defer response.Body.Close()
	providerOutcome = "success"
	if response.StatusCode < 200 || response.StatusCode > 299 {
		handler.observe(request, permit, response, nil)
		handler.writeProviderError(tracked, response)
		return
	}
	contentType, err := speechContentType(response.Header.Get("Content-Type"))
	if err != nil || response.ContentLength > handler.maximumResponseBytes {
		handler.observe(request, permit, response, openaiProvider.ErrSpeechUpstream)
		providerOutcome = "failure"
		writeError(tracked, http.StatusBadGateway, "server_error", "invalid_provider_response", "invalid provider response")
		return
	}
	copySpeechHeaders(tracked.Header(), response.Header)
	tracked.Header().Set("Content-Type", contentType)
	if response.ContentLength >= 0 {
		tracked.Header().Set("Content-Length", strconv.FormatInt(response.ContentLength, 10))
	}
	tracked.WriteHeader(response.StatusCode)
	if err = relaySpeech(tracked, response.Body, handler.maximumResponseBytes, response.ContentLength); err != nil {
		handler.observe(request, permit, response, err)
		providerOutcome = "failure"
		return
	}
	handler.observe(request, permit, response, nil)
}

type speechEnvelope struct{ Model string }

func parseSpeechEnvelope(body []byte) (speechEnvelope, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return speechEnvelope{}, errors.New("invalid object")
	}
	seen := map[string]bool{}
	model, input := "", ""
	validVoice := false
	for decoder.More() {
		keyToken, err := decoder.Token()
		key, ok := keyToken.(string)
		if err != nil || !ok || seen[key] {
			return speechEnvelope{}, errors.New("duplicate or invalid key")
		}
		seen[key] = true
		var raw json.RawMessage
		if decoder.Decode(&raw) != nil {
			return speechEnvelope{}, errors.New("invalid value")
		}
		switch key {
		case "model":
			if json.Unmarshal(raw, &model) != nil {
				return speechEnvelope{}, errors.New("invalid model")
			}
		case "input":
			if json.Unmarshal(raw, &input) != nil {
				return speechEnvelope{}, errors.New("invalid input")
			}
		case "voice":
			validVoice = validSpeechVoice(raw)
		}
	}
	if _, err = decoder.Token(); err != nil || decoder.Decode(&struct{}{}) != io.EOF || model == "" || model != strings.TrimSpace(model) || len(model) > 200 || input == "" || utf8.RuneCountInString(input) > 4096 || !validVoice {
		return speechEnvelope{}, errors.New("invalid speech request")
	}
	return speechEnvelope{Model: model}, nil
}

func validSpeechVoice(raw json.RawMessage) bool {
	var voice string
	if json.Unmarshal(raw, &voice) == nil {
		return voice != "" && voice == strings.TrimSpace(voice) && len(voice) <= 200
	}
	var object struct {
		ID string `json:"id"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(&object) == nil && decoder.Decode(&struct{}{}) == io.EOF && object.ID != "" && object.ID == strings.TrimSpace(object.ID) && len(object.ID) <= 200
}

func speechContentType(value string) (string, error) {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return "", err
	}
	switch strings.ToLower(mediaType) {
	case "audio/mpeg", "audio/mp3", "audio/opus", "audio/aac", "audio/flac", "audio/wav", "audio/x-wav", "audio/l16", "application/octet-stream":
		return mediaType, nil
	default:
		return "", errors.New("unsupported speech type")
	}
}

func copySpeechHeaders(destination, source http.Header) {
	for _, name := range []string{"Content-Disposition", "Cache-Control"} {
		for _, value := range source.Values(name) {
			if len(value) <= 1024 && !strings.ContainsAny(value, "\r\n") {
				destination.Add(name, value)
			}
		}
	}
}

func relaySpeech(writer http.ResponseWriter, source io.Reader, maximum int64, expected ...int64) error {
	limited := &io.LimitedReader{R: source, N: maximum}
	buffer := make([]byte, 32*1024)
	var total int64
	for {
		count, readErr := limited.Read(buffer)
		if count > 0 {
			total += int64(count)
			written, writeErr := writer.Write(buffer[:count])
			if writeErr != nil || written != count {
				return errSpeechStreamWrite
			}
			_ = http.NewResponseController(writer).Flush()
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return readErr
		}
		if limited.N == 0 {
			break
		}
	}
	if len(expected) > 0 && expected[0] >= 0 && total != expected[0] {
		return io.ErrUnexpectedEOF
	}
	var extra [1]byte
	if count, readErr := source.Read(extra[:]); count > 0 {
		return errSpeechStreamTooLarge
	} else if readErr != nil && !errors.Is(readErr, io.EOF) {
		return readErr
	}
	return nil
}

func (handler *SpeechHandler) writeProviderError(writer http.ResponseWriter, response *http.Response) {
	body, err := readBounded(response.Body, min(handler.maximumRequestBytes, 1<<20))
	if err != nil {
		writeError(writer, http.StatusBadGateway, "server_error", "invalid_provider_response", "invalid provider response")
		return
	}
	if contentType, _, parseErr := mime.ParseMediaType(response.Header.Get("Content-Type")); parseErr == nil && (contentType == "application/json" || contentType == "text/plain") {
		writer.Header().Set("Content-Type", contentType)
	}
	if retry := response.Header.Get("Retry-After"); retry != "" && len(retry) <= 64 && !strings.ContainsAny(retry, "\r\n") {
		writer.Header().Set("Retry-After", retry)
	}
	writer.WriteHeader(response.StatusCode)
	_, _ = writer.Write(body)
}

func (handler *SpeechHandler) writeExecutorError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, providercredentials.ErrCredentialUnavailable):
		writeError(writer, http.StatusServiceUnavailable, "server_error", "provider_unavailable", "provider unavailable")
	case errors.Is(err, openaiProvider.ErrSpeechTimeout):
		writeError(writer, http.StatusGatewayTimeout, "server_error", "upstream_timeout", "provider request timed out")
	case errors.Is(err, openaiProvider.ErrSpeechCanceled):
		writeError(writer, 499, "server_error", "request_canceled", "request canceled")
	default:
		writeError(writer, http.StatusBadGateway, "server_error", "upstream_unavailable", "provider unavailable")
	}
}

func (handler *SpeechHandler) acquireHealth(writer http.ResponseWriter, request *http.Request, channel string) (providerhealth.Permit, bool) {
	snapshot, err := handler.health.Inspect(request.Context(), channel)
	if err != nil || snapshot.State == providerhealth.Open {
		writeError(writer, http.StatusServiceUnavailable, "server_error", "provider_unavailable", "provider unavailable")
		return providerhealth.Permit{}, false
	}
	if snapshot.State == providerhealth.HalfOpen {
		permit, claimErr := handler.health.ClaimProbe(request.Context(), channel, requestid.FromContext(request.Context()))
		if claimErr != nil {
			writeError(writer, http.StatusServiceUnavailable, "server_error", "provider_unavailable", "provider unavailable")
			return providerhealth.Permit{}, false
		}
		return permit, true
	}
	return providerhealth.Permit{ChannelID: channel}, true
}

func (handler *SpeechHandler) observe(request *http.Request, permit providerhealth.Permit, response *http.Response, err error) {
	outcome := providerhealth.Neutral
	switch {
	case errors.Is(err, openaiProvider.ErrSpeechTimeout):
		outcome = providerhealth.Timeout
	case errors.Is(err, errSpeechStreamWrite):
		outcome = providerhealth.Neutral
	case err != nil:
		outcome = providerhealth.Connection
	case response.StatusCode == http.StatusTooManyRequests:
		outcome = providerhealth.RateLimited
	case response.StatusCode >= 500:
		outcome = providerhealth.ServerError
	case response.StatusCode >= 200 && response.StatusCode < 400:
		outcome = providerhealth.Success
	}
	_, _ = handler.health.Observe(context.WithoutCancel(request.Context()), providerhealth.Observation{ChannelID: permit.ChannelID, ObservationID: requestid.FromContext(request.Context()), Outcome: outcome, Permit: permit})
}

func (handler *SpeechHandler) execute(ctx context.Context, route audiooperation.Model, input openaiProvider.SpeechRequest) (response *http.Response, err error) {
	providerContext := ctx
	if handler.telemetry == nil {
		defer func() {
			if recover() != nil {
				response, err = nil, errProviderPanic
			}
		}()
		return handler.executor.Create(ctx, input)
	}
	providerContext, traceSpan, started := handler.telemetry.StartProvider(ctx, string(route.Provider), "openai", audiooperation.Speech)
	defer func() {
		if recover() != nil {
			response, err = nil, errProviderPanic
		}
		outcome := "success"
		if errors.Is(err, openaiProvider.ErrSpeechTimeout) {
			outcome = "timeout"
		} else if errors.Is(err, openaiProvider.ErrSpeechCanceled) {
			outcome = "canceled"
		} else if err != nil {
			outcome = "failure"
		}
		handler.telemetry.EndProvider(providerContext, traceSpan, started, telemetry.ProviderRecord{Provider: string(route.Provider), Protocol: "openai", Operation: audiooperation.Speech, Outcome: outcome})
	}()
	return handler.executor.Create(providerContext, input)
}
