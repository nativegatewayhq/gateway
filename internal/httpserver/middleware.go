package httpserver

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/nativegatewayhq/gateway/internal/requestid"
)

type responseRecorder struct {
	http.ResponseWriter
	status int
}

func (recorder *responseRecorder) WriteHeader(status int) {
	if recorder.status != 0 {
		return
	}
	recorder.status = status
	recorder.ResponseWriter.WriteHeader(status)
}

func (recorder *responseRecorder) Write(body []byte) (int, error) {
	if recorder.status == 0 {
		recorder.WriteHeader(http.StatusOK)
	}
	return recorder.ResponseWriter.Write(body)
}

func recovery(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		defer func() {
			if recover() == nil {
				return
			}
			logger.Error("request panic recovered", "request_id", requestid.FromContext(request.Context()))
			writeError(writer, request, http.StatusInternalServerError, "internal_error", "internal server error")
		}()
		next.ServeHTTP(writer, request)
	})
}

func accessLog(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		started := time.Now()
		recorder := &responseRecorder{ResponseWriter: writer}
		next.ServeHTTP(recorder, request)

		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}
		route := request.Pattern
		if route == "" {
			route = "unmatched"
		}
		logger.Info("request completed",
			"request_id", requestid.FromContext(request.Context()),
			"method", request.Method,
			"route", route,
			"status", status,
			"duration", time.Since(started),
		)
	})
}
