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
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	"github.com/nativegatewayhq/gateway/internal/requestid"
	"github.com/nativegatewayhq/gateway/providers/google"
)

const maxModelLength = 200

type Authenticator interface {
	Authenticate(context.Context, string) (apikey.Principal, error)
}

type Executor interface {
	GenerateContent(context.Context, google.GenerateContentRequest) (*http.Response, error)
}

type Handler struct {
	logger        *slog.Logger
	authenticator Authenticator
	executor      Executor
	maxBodyBytes  int64
}

func NewHandler(logger *slog.Logger, authenticator Authenticator, executor Executor, maxBodyBytes int64) *Handler {
	return &Handler{logger: logger, authenticator: authenticator, executor: executor, maxBodyBytes: maxBodyBytes}
}

func (handler *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	tracked := &statusWriter{ResponseWriter: writer}
	started := time.Now()
	model, _ := modelFromRequest(request)
	defer func() {
		if recover() != nil {
			if !tracked.wroteHeader {
				writeError(tracked, http.StatusInternalServerError, "INTERNAL", "internal server error")
			}
			handler.logger.Error("gemini request panic recovered", "request_id", requestid.FromContext(request.Context()))
		}
		handler.logger.Info("gemini request completed",
			"request_id", requestid.FromContext(request.Context()),
			"protocol", "gemini",
			"operation", "generateContent",
			"provider", "google",
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
	if !handler.authenticate(tracked, request) {
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

	response, err := handler.executor.GenerateContent(request.Context(), google.GenerateContentRequest{
		Model:       model,
		Query:       request.URL.Query(),
		ContentType: request.Header.Get("Content-Type"),
		Accept:      request.Header.Get("Accept"),
		UserAgent:   request.UserAgent(),
		APIClient:   request.Header.Get("x-goog-api-client"),
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
		handler.logger.Warn("gemini upstream response copy failed",
			"request_id", requestid.FromContext(request.Context()),
			"provider", "google",
			"category", "response_copy_failed",
		)
	}
}

func (handler *Handler) authenticate(writer http.ResponseWriter, request *http.Request) bool {
	if handler.authenticator == nil {
		writeError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "authentication service unavailable")
		return false
	}
	raw, err := apikey.Extract(request)
	if err != nil {
		if errors.Is(err, apikey.ErrAmbiguous) {
			writeError(writer, http.StatusBadRequest, "INVALID_ARGUMENT", "multiple credential locations are not allowed")
			return false
		}
		writeError(writer, http.StatusUnauthorized, "UNAUTHENTICATED", "authentication required")
		return false
	}
	if _, err := handler.authenticator.Authenticate(request.Context(), raw); err != nil {
		if errors.Is(err, apikey.ErrUnavailable) {
			writeError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "authentication service unavailable")
			return false
		}
		writeError(writer, http.StatusUnauthorized, "UNAUTHENTICATED", "authentication required")
		return false
	}
	return true
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
