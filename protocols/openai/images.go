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
	Resolve(string, imageoperation.Operation, imageoperation.MediaType) (imageoperation.ModelRoute, error)
	List() []imageoperation.ModelRoute
}

type Executor interface {
	Generate(context.Context, openaiimages.Request) (*http.Response, error)
}

type Handler struct {
	logger        *slog.Logger
	authenticator Authenticator
	models        ModelRegistry
	executors     map[providercredentials.ProviderID]Executor
	maxBodyBytes  int64
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
	defer func() {
		if recover() != nil {
			if !tracked.wroteHeader {
				writeError(tracked, http.StatusInternalServerError, "server_error", "internal_error", "internal server error")
			}
			handler.logger.Error("openai image request panic recovered", "request_id", requestid.FromContext(request.Context()))
		}
		handler.logger.Info("openai image request completed",
			"request_id", requestid.FromContext(request.Context()),
			"protocol", "openai",
			"operation", "image.generate",
			"provider", string(provider),
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
	if !handler.authenticate(tracked, request) {
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
	route, err := handler.models.Resolve(model, imageoperation.Generate, imageoperation.JSON)
	if err != nil {
		if errors.Is(err, imageoperation.ErrModelNotFound) {
			writeError(tracked, http.StatusNotFound, "invalid_request_error", "model_not_found", "model not found")
		} else {
			writeError(tracked, http.StatusBadRequest, "invalid_request_error", "unsupported_capability", "model does not support operation")
		}
		return
	}
	provider = route.Provider
	logModel = model
	executor := handler.executors[provider]
	if executor == nil {
		writeError(tracked, http.StatusServiceUnavailable, "server_error", "provider_unavailable", "provider unavailable")
		return
	}
	response, err := executor.Generate(request.Context(), openaiimages.Request{
		ContentType: request.Header.Get("Content-Type"),
		Accept:      request.Header.Get("Accept"),
		UserAgent:   request.UserAgent(),
		Body:        bytes.NewReader(body),
	})
	if err != nil {
		handler.writeExecutorError(tracked, err)
		return
	}
	defer response.Body.Close()
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

func (handler *Handler) authenticate(writer http.ResponseWriter, request *http.Request) bool {
	if handler.authenticator == nil {
		writeError(writer, http.StatusServiceUnavailable, "server_error", "authentication_unavailable", "authentication service unavailable")
		return false
	}
	raw, err := apikey.Extract(request)
	if err != nil {
		if errors.Is(err, apikey.ErrAmbiguous) {
			writeError(writer, http.StatusBadRequest, "invalid_request_error", "ambiguous_authentication", "multiple credential locations are not allowed")
			return false
		}
		writeError(writer, http.StatusUnauthorized, "invalid_request_error", "authentication_required", "authentication required")
		return false
	}
	if _, err := handler.authenticator.Authenticate(request.Context(), raw); err != nil {
		if errors.Is(err, apikey.ErrUnavailable) {
			writeError(writer, http.StatusServiceUnavailable, "server_error", "authentication_unavailable", "authentication service unavailable")
			return false
		}
		writeError(writer, http.StatusUnauthorized, "invalid_request_error", "authentication_required", "authentication required")
		return false
	}
	return true
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
