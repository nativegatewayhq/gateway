package openai

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	"github.com/nativegatewayhq/gateway/internal/providerhealth"
	"github.com/nativegatewayhq/gateway/internal/requestid"
	"github.com/nativegatewayhq/gateway/internal/telemetry"
	audiooperation "github.com/nativegatewayhq/gateway/operations/audio"
	openaiProvider "github.com/nativegatewayhq/gateway/providers/openai"
)

var (
	errTranscriptionInvalid  = errors.New("invalid transcription multipart")
	errTranscriptionTooLarge = errors.New("transcription multipart too large")
	errTranscriptionCapacity = errors.New("transcription spool capacity exhausted")
)

const transcriptionMemoryThreshold int64 = 1 << 20

const (
	maximumTranscriptionParts            = 32
	maximumTranscriptionPartHeaderNames  = 16
	maximumTranscriptionPartHeaderValues = 16
	maximumTranscriptionPartHeaderBytes  = 1024
)

type TranscriptionRegistry interface {
	Resolve(string) (audiooperation.TranscriptionModel, error)
}
type TranscriptionExecutor interface {
	Create(context.Context, openaiProvider.TranscriptionRequest) (*http.Response, error)
}
type TranscriptionHandler struct {
	common                                                                         *Handler
	models                                                                         TranscriptionRegistry
	executor                                                                       TranscriptionExecutor
	health                                                                         providerhealth.Gate
	spoolSlots                                                                     chan struct{}
	maximumRequestBytes, maximumFileBytes, maximumFieldBytes, maximumResponseBytes int64
	tempDir                                                                        string
	telemetry                                                                      *telemetry.Recorder
}

func NewTranscriptionHandler(logger *slog.Logger, authenticator Authenticator, models TranscriptionRegistry, executor TranscriptionExecutor, health providerhealth.Gate, requestBytes, fileBytes, fieldBytes, responseBytes int64, spoolLimit int) *TranscriptionHandler {
	if health == nil {
		health = providerhealth.NoopGate{}
	}
	if spoolLimit < 1 {
		spoolLimit = 1
	}
	return &TranscriptionHandler{common: NewImagesHandler(logger, authenticator, nil, nil, 1), models: models, executor: executor, health: health, spoolSlots: make(chan struct{}, spoolLimit), maximumRequestBytes: requestBytes, maximumFileBytes: fileBytes, maximumFieldBytes: fieldBytes, maximumResponseBytes: responseBytes}
}
func (h *TranscriptionHandler) SetTelemetry(r *telemetry.Recorder) {
	h.telemetry = r
	h.common.SetTelemetry(r)
}

func (h *TranscriptionHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tracked := &statusWriter{ResponseWriter: w}
	started := time.Now()
	outcome := "neutral"
	defer func() {
		if recover() != nil && !tracked.wroteHeader {
			writeError(tracked, 500, "server_error", "internal_error", "internal server error")
			outcome = "failure"
		}
		h.common.logger.Info("openai transcription request completed", "request_id", requestid.FromContext(r.Context()), "protocol", "openai", "operation", audiooperation.Transcription, "status", tracked.statusCode(), "outcome", outcome, "duration", time.Since(started))
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
	media, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	boundary := params["boundary"]
	if err != nil || media != "multipart/form-data" || boundary == "" || len(boundary) > 70 || strings.ContainsAny(boundary, "\r\n") {
		writeError(tracked, 400, "invalid_request_error", "invalid_multipart", "valid multipart boundary required")
		return
	}
	if h.invalidLimits() || r.ContentLength > h.maximumRequestBytes {
		writeError(tracked, 413, "invalid_request_error", "request_too_large", "request body too large")
		return
	}
	select {
	case h.spoolSlots <- struct{}{}:
		defer func() { <-h.spoolSlots }()
	default:
		writeError(tracked, 503, "server_error", "spool_capacity_exhausted", "transcription capacity unavailable")
		return
	}
	form, err := h.parseMultipart(tracked, r, boundary)
	if err != nil {
		err = classifyMultipart(err)
		status, code := 400, "invalid_multipart"
		if errors.Is(err, errTranscriptionTooLarge) {
			status, code = 413, "request_too_large"
		}
		writeError(tracked, status, "invalid_request_error", code, "invalid transcription request")
		return
	}
	defer form.file.Cleanup()
	if h.models == nil || h.executor == nil {
		writeError(tracked, 503, "server_error", "provider_unavailable", "provider unavailable")
		return
	}
	route, err := h.models.Resolve(form.model)
	if errors.Is(err, audiooperation.ErrModelNotFound) {
		writeError(tracked, 404, "invalid_request_error", "model_not_found", "model not found")
		return
	}
	if err != nil || route.Provider != providercredentials.OpenAI || route.ChannelID == "" {
		writeError(tracked, 503, "server_error", "provider_unavailable", "provider unavailable")
		return
	}
	if err = form.validateCapabilities(route.Capabilities); err != nil {
		writeError(tracked, 400, "invalid_request_error", "unsupported_transcription_option", "transcription option is not supported for model")
		return
	}
	if !h.common.authorizeModel(tracked, r, principal, "openai", audiooperation.Transcription, route.ID) {
		return
	}
	permit, ok := h.acquireHealth(tracked, r, route.ChannelID)
	if !ok {
		return
	}
	outbound, contentType, length, err := h.buildMultipart(form, route.ProviderModel)
	if err != nil {
		h.releaseHealth(r, permit)
		writeError(tracked, 503, "server_error", "spool_unavailable", "transcription capacity unavailable")
		return
	}
	defer outbound.Close()
	defer os.Remove(outbound.Name())
	response, executeErr := h.execute(r.Context(), route, openaiProvider.TranscriptionRequest{ChannelID: route.ChannelID, ContentType: contentType, ContentLength: length, Accept: r.Header.Get("Accept"), UserAgent: r.UserAgent(), Body: outbound})
	if executeErr != nil {
		h.observe(r, permit, nil, executeErr)
		outcome = "failure"
		h.writeExecutorError(tracked, executeErr)
		return
	}
	defer response.Body.Close()
	streaming := isEventStream(response.Header.Get("Content-Type"))
	if streaming && !route.Capabilities.Streaming {
		h.observe(r, permit, response, openaiProvider.ErrTranscriptionUpstream)
		outcome = "failure"
		writeError(tracked, 502, "server_error", "invalid_provider_response", "invalid provider response")
		return
	}
	if streaming {
		copyTranscriptionHeaders(tracked.Header(), response.Header)
		tracked.WriteHeader(response.StatusCode)
		if relayErr := relayTranscription(tracked, response.Body, h.maximumResponseBytes, true); relayErr != nil {
			h.observe(r, permit, response, relayErr)
			outcome = "failure"
			return
		}
		h.observe(r, permit, response, nil)
		outcome = "success"
		return
	}
	body, readErr := readBounded(response.Body, h.maximumResponseBytes)
	if readErr != nil || !validTranscriptionResponseType(response.Header.Get("Content-Type")) {
		h.observe(r, permit, response, openaiProvider.ErrTranscriptionUpstream)
		outcome = "failure"
		writeError(tracked, 502, "server_error", "invalid_provider_response", "invalid provider response")
		return
	}
	copyTranscriptionHeaders(tracked.Header(), response.Header)
	tracked.Header().Set("Content-Length", strconv.Itoa(len(body)))
	tracked.WriteHeader(response.StatusCode)
	_, _ = tracked.Write(body)
	h.observe(r, permit, response, nil)
	outcome = "success"
}

type transcriptionField struct{ name, value string }
type transcriptionForm struct {
	model, filename, fileType string
	fields                    []transcriptionField
	file                      *transcriptionSpool
	stream                    bool
	responseFormat            string
}

func (h *TranscriptionHandler) parseMultipart(w http.ResponseWriter, r *http.Request, boundary string) (*transcriptionForm, error) {
	reader := multipart.NewReader(http.MaxBytesReader(w, r.Body, h.maximumRequestBytes), boundary)
	form := &transcriptionForm{file: newTranscriptionSpool(h.tempDir, transcriptionMemoryThreshold, h.maximumFileBytes)}
	complete := false
	defer func() {
		if !complete {
			form.file.Cleanup()
		}
	}()
	seen := map[string]bool{}
	parts := 0
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, classifyMultipart(err)
		}
		parts++
		if parts > maximumTranscriptionParts || !validTranscriptionPartHeaders(part.Header) {
			part.Close()
			return nil, errTranscriptionInvalid
		}
		name := part.FormName()
		if name == "" || len(name) > 80 || seen[name] || blockedTranscriptionField(name) {
			part.Close()
			return nil, errTranscriptionInvalid
		}
		seen[name] = true
		if name == "file" {
			filename := rawTranscriptionFilename(part.Header.Get("Content-Disposition"))
			if !validTranscriptionFilename(filename) {
				part.Close()
				return nil, errTranscriptionInvalid
			}
			form.filename = filename
			form.fileType = part.Header.Get("Content-Type")
			if len(form.fileType) > 200 || strings.ContainsAny(form.fileType, "\r\n") {
				part.Close()
				return nil, errTranscriptionInvalid
			}
			if _, err = form.file.ReadFrom(part); err != nil {
				part.Close()
				return nil, err
			}
			if form.file.Size() == 0 {
				part.Close()
				return nil, errTranscriptionInvalid
			}
		} else {
			value, err := readPartField(part, h.maximumFieldBytes)
			if err != nil {
				part.Close()
				return nil, err
			}
			if name == "model" {
				form.model = value
			}
			if name == "stream" {
				form.stream = value == "true"
				if value != "true" && value != "false" {
					part.Close()
					return nil, errTranscriptionInvalid
				}
			}
			if name == "response_format" {
				form.responseFormat = value
			}
			form.fields = append(form.fields, transcriptionField{name, value})
		}
		part.Close()
	}
	if form.file.Size() == 0 || form.model == "" || form.model != strings.TrimSpace(form.model) || len(form.model) > 200 || !seen["file"] || !seen["model"] {
		return nil, errTranscriptionInvalid
	}
	complete = true
	return form, nil
}
func (f *transcriptionForm) validateCapabilities(c audiooperation.TranscriptionCapabilities) error {
	format := f.responseFormat
	if format == "" {
		format = "json"
	}
	allowed := false
	for _, v := range c.ResponseFormats {
		if v == format {
			allowed = true
		}
	}
	if !allowed {
		return errTranscriptionInvalid
	}
	if f.stream && !c.Streaming {
		return errTranscriptionInvalid
	}
	for _, field := range f.fields {
		switch field.name {
		case "language":
			if !c.Language {
				return errTranscriptionInvalid
			}
		case "prompt":
			if !c.Prompt {
				return errTranscriptionInvalid
			}
		case "timestamp_granularities[]":
			if !c.Timestamps {
				return errTranscriptionInvalid
			}
		}
	}
	return nil
}
func (h *TranscriptionHandler) buildMultipart(form *transcriptionForm, model string) (*os.File, string, int64, error) {
	file, err := os.CreateTemp(h.tempDir, "gateway-transcription-outbound-*")
	if err != nil {
		return nil, "", 0, err
	}
	cleanup := func(e error) (*os.File, string, int64, error) {
		file.Close()
		os.Remove(file.Name())
		return nil, "", 0, e
	}
	writer := multipart.NewWriter(file)
	for _, field := range form.fields {
		value := field.value
		if field.name == "model" {
			value = model
		}
		if err = writer.WriteField(field.name, value); err != nil {
			return cleanup(err)
		}
	}
	headers := make(textproto.MIMEHeader)
	headers.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, escapeMultipartValue(form.filename)))
	if form.fileType != "" {
		headers.Set("Content-Type", form.fileType)
	}
	part, err := writer.CreatePart(headers)
	if err != nil {
		return cleanup(err)
	}
	source, err := form.file.Open()
	if err != nil {
		return cleanup(err)
	}
	_, err = io.Copy(part, source)
	source.Close()
	if err != nil {
		return cleanup(err)
	}
	if err = writer.Close(); err != nil {
		return cleanup(err)
	}
	info, err := file.Stat()
	if err != nil {
		return cleanup(err)
	}
	// Reconstructing a trusted multipart body adds a new boundary and headers.
	// Bound that overhead independently instead of rejecting a valid inbound
	// request merely because its regenerated wire representation is larger.
	maximumOutboundBytes := h.maximumRequestBytes + maximumTranscriptionParts*h.maximumFieldBytes + 64*1024
	if maximumOutboundBytes < h.maximumRequestBytes || info.Size() > maximumOutboundBytes {
		return cleanup(errTranscriptionTooLarge)
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return cleanup(err)
	}
	return file, writer.FormDataContentType(), info.Size(), nil
}

type transcriptionSpool struct {
	memory                   bytes.Buffer
	path, tempDir            string
	threshold, maximum, size int64
}

func newTranscriptionSpool(dir string, threshold, maximum int64) *transcriptionSpool {
	return &transcriptionSpool{tempDir: dir, threshold: threshold, maximum: maximum}
}
func (s *transcriptionSpool) ReadFrom(r io.Reader) (int64, error) {
	buffer := make([]byte, 32*1024)
	for {
		n, err := r.Read(buffer)
		if n > 0 {
			if s.size+int64(n) > s.maximum {
				return s.size, errTranscriptionTooLarge
			}
			if s.path == "" && s.size+int64(n) > s.threshold {
				file, e := os.CreateTemp(s.tempDir, "gateway-transcription-input-*")
				if e != nil {
					return s.size, e
				}
				_ = file.Chmod(0600)
				if _, e = file.Write(s.memory.Bytes()); e != nil {
					file.Close()
					os.Remove(file.Name())
					return s.size, e
				}
				s.path = file.Name()
				s.memory.Reset()
				if _, e = file.Write(buffer[:n]); e != nil {
					file.Close()
					return s.size, e
				}
				file.Close()
			} else if s.path != "" {
				file, e := os.OpenFile(s.path, os.O_APPEND|os.O_WRONLY, 0600)
				if e != nil {
					return s.size, e
				}
				_, e = file.Write(buffer[:n])
				file.Close()
				if e != nil {
					return s.size, e
				}
			} else {
				_, _ = s.memory.Write(buffer[:n])
			}
			s.size += int64(n)
		}
		if errors.Is(err, io.EOF) {
			return s.size, nil
		}
		if err != nil {
			return s.size, err
		}
	}
}
func (s *transcriptionSpool) Open() (io.ReadCloser, error) {
	if s.path != "" {
		return os.Open(s.path)
	}
	return io.NopCloser(bytes.NewReader(s.memory.Bytes())), nil
}
func (s *transcriptionSpool) Size() int64 { return s.size }
func (s *transcriptionSpool) Cleanup() {
	if s.path != "" {
		_ = os.Remove(s.path)
		s.path = ""
	}
	s.memory.Reset()
}

func (h *TranscriptionHandler) invalidLimits() bool {
	return h.maximumRequestBytes < 1 || h.maximumFileBytes < 1 || h.maximumFileBytes > h.maximumRequestBytes || h.maximumFieldBytes < 1 || h.maximumResponseBytes < 1
}
func readPartField(r io.Reader, max int64) (string, error) {
	body, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return "", err
	}
	if int64(len(body)) > max {
		return "", errTranscriptionTooLarge
	}
	value := string(body)
	if value == "" || strings.ContainsRune(value, '\x00') {
		return "", errTranscriptionInvalid
	}
	return value, nil
}
func classifyMultipart(err error) error {
	if errors.Is(err, errTranscriptionTooLarge) {
		return errTranscriptionTooLarge
	}
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return errTranscriptionTooLarge
	}
	return errTranscriptionInvalid
}
func blockedTranscriptionField(name string) bool {
	switch strings.ToLower(name) {
	case "authorization", "api_key", "key", "base_url", "url", "host", "openai-organization", "openai-project":
		return true
	}
	return false
}
func validTranscriptionPartHeaders(headers textproto.MIMEHeader) bool {
	if len(headers) == 0 || len(headers) > maximumTranscriptionPartHeaderNames {
		return false
	}
	for name, values := range headers {
		if name == "" || len(name) > 80 || strings.ContainsAny(name, "\r\n") || len(values) == 0 || len(values) > maximumTranscriptionPartHeaderValues {
			return false
		}
		for _, value := range values {
			if len(value) > maximumTranscriptionPartHeaderBytes || strings.ContainsAny(value, "\r\n") {
				return false
			}
		}
	}
	return true
}
func validTranscriptionFilename(name string) bool {
	return name != "" && len(name) <= 255 && !filepath.IsAbs(name) && filepath.Base(name) == name && !strings.Contains(name, "..") && !strings.ContainsAny(name, "\x00\r\n")
}
func rawTranscriptionFilename(disposition string) string {
	media, params, err := mime.ParseMediaType(disposition)
	if err != nil || media != "form-data" {
		return ""
	}
	return params["filename"]
}
func escapeMultipartValue(v string) string {
	return strings.NewReplacer("\\", "\\\\", "\"", "\\\"").Replace(v)
}
func isEventStream(value string) bool {
	media, _, err := mime.ParseMediaType(value)
	return err == nil && media == "text/event-stream"
}
func validTranscriptionResponseType(value string) bool {
	media, _, err := mime.ParseMediaType(value)
	if err != nil {
		return false
	}
	switch media {
	case "application/json", "text/plain", "text/vtt", "application/x-subrip":
		return true
	}
	return false
}
func copyTranscriptionHeaders(dst, src http.Header) {
	for _, name := range []string{"Content-Type", "Retry-After", "Cache-Control"} {
		for _, value := range src.Values(name) {
			if len(value) <= 1024 && !strings.ContainsAny(value, "\r\n") {
				dst.Add(name, value)
			}
		}
	}
}
func relayTranscription(w http.ResponseWriter, r io.Reader, maximum int64, flush bool) error {
	limited := &io.LimitedReader{R: r, N: maximum}
	buffer := make([]byte, 32*1024)
	for {
		n, err := limited.Read(buffer)
		if n > 0 {
			written, writeErr := w.Write(buffer[:n])
			if writeErr != nil || written != n {
				return errSpeechStreamWrite
			}
			if flush {
				_ = http.NewResponseController(w).Flush()
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if limited.N == 0 {
			break
		}
	}
	var extra [1]byte
	if n, err := r.Read(extra[:]); n > 0 {
		return errTranscriptionTooLarge
	} else if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func (h *TranscriptionHandler) acquireHealth(w http.ResponseWriter, r *http.Request, channel string) (providerhealth.Permit, bool) {
	snapshot, err := h.health.Inspect(r.Context(), channel)
	if err != nil || snapshot.State == providerhealth.Open {
		writeError(w, 503, "server_error", "provider_unavailable", "provider unavailable")
		return providerhealth.Permit{}, false
	}
	if snapshot.State == providerhealth.HalfOpen {
		permit, e := h.health.ClaimProbe(r.Context(), channel, requestid.FromContext(r.Context()))
		if e != nil {
			writeError(w, 503, "server_error", "provider_unavailable", "provider unavailable")
			return providerhealth.Permit{}, false
		}
		return permit, true
	}
	return providerhealth.Permit{ChannelID: channel}, true
}
func (h *TranscriptionHandler) releaseHealth(r *http.Request, p providerhealth.Permit) {
	_, _ = h.health.Observe(context.WithoutCancel(r.Context()), providerhealth.Observation{ChannelID: p.ChannelID, ObservationID: requestid.FromContext(r.Context()), Outcome: providerhealth.Neutral, Permit: p})
}
func (h *TranscriptionHandler) observe(r *http.Request, p providerhealth.Permit, response *http.Response, err error) {
	out := providerhealth.Neutral
	switch {
	case errors.Is(err, openaiProvider.ErrTranscriptionTimeout):
		out = providerhealth.Timeout
	case errors.Is(err, errSpeechStreamWrite):
		out = providerhealth.Neutral
	case err != nil:
		out = providerhealth.Connection
	case response.StatusCode == 429:
		out = providerhealth.RateLimited
	case response.StatusCode >= 500:
		out = providerhealth.ServerError
	case response.StatusCode >= 200 && response.StatusCode < 400:
		out = providerhealth.Success
	}
	_, _ = h.health.Observe(context.WithoutCancel(r.Context()), providerhealth.Observation{ChannelID: p.ChannelID, ObservationID: requestid.FromContext(r.Context()), Outcome: out, Permit: p})
}
func (h *TranscriptionHandler) execute(ctx context.Context, route audiooperation.TranscriptionModel, input openaiProvider.TranscriptionRequest) (response *http.Response, err error) {
	if h.telemetry != nil {
		providerContext, traceSpan, started := h.telemetry.StartProvider(ctx, string(route.Provider), "openai", audiooperation.Transcription)
		defer func() {
			if recover() != nil {
				response, err = nil, errProviderPanic
			}
			outcome := "success"
			if errors.Is(err, openaiProvider.ErrTranscriptionTimeout) {
				outcome = "timeout"
			} else if err != nil {
				outcome = "failure"
			}
			h.telemetry.EndProvider(providerContext, traceSpan, started, telemetry.ProviderRecord{Provider: string(route.Provider), Protocol: "openai", Operation: audiooperation.Transcription, Outcome: outcome})
		}()
		return h.executor.Create(providerContext, input)
	}
	defer func() {
		if recover() != nil {
			response, err = nil, errProviderPanic
		}
	}()
	return h.executor.Create(ctx, input)
}
func (h *TranscriptionHandler) writeExecutorError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, providercredentials.ErrCredentialUnavailable):
		writeError(w, 503, "server_error", "provider_unavailable", "provider unavailable")
	case errors.Is(err, openaiProvider.ErrTranscriptionTimeout):
		writeError(w, 504, "server_error", "upstream_timeout", "provider request timed out")
	case errors.Is(err, openaiProvider.ErrTranscriptionCanceled):
		writeError(w, 499, "server_error", "request_canceled", "request canceled")
	default:
		writeError(w, 502, "server_error", "upstream_unavailable", "provider unavailable")
	}
}
