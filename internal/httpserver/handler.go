// Package httpserver defines the Gateway-owned HTTP surface and middleware.
package httpserver

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/nativegatewayhq/gateway/internal/requestid"
)

// ReadyFunc checks dependencies required to accept traffic. A nil check means
// the process has no external readiness dependencies yet.
type ReadyFunc func(context.Context) error

// NewHandler builds the Gateway-owned HTTP routes. Provider-native routes are
// intentionally excluded until their protocol plans are accepted.
func NewHandler(logger *slog.Logger, ready ReadyFunc) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /health/ready", func(writer http.ResponseWriter, request *http.Request) {
		if ready != nil && ready(request.Context()) != nil {
			writeError(writer, request, http.StatusServiceUnavailable, "not_ready", "service is not ready")
			return
		}
		writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		writeError(writer, request, http.StatusNotFound, "not_found", "route not found")
	})

	handler := recovery(logger, mux)
	handler = accessLog(logger, handler)
	handler = requestid.Middleware(handler)
	return handler
}
