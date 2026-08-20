// Package app composes and runs the Gateway process.
package app

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/nativegatewayhq/gateway/internal/config"
	"github.com/nativegatewayhq/gateway/internal/httpserver"
)

// Error reports a lifecycle failure category without exposing listener or
// configuration values. The wrapped cause remains available to internal
// callers through errors.Is and errors.As.
type Error struct {
	Kind string
	err  error
}

func (err *Error) Error() string { return err.Kind }

func (err *Error) Unwrap() error { return err.err }

func runtimeError(kind string, err error) error {
	return &Error{Kind: kind, err: err}
}

// Run starts the HTTP server and blocks until it fails or ctx is canceled.
func Run(ctx context.Context, cfg config.Config, logger *slog.Logger, ready httpserver.ReadyFunc) error {
	listener, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return runtimeError("listen failed", err)
	}

	server := &http.Server{
		Handler:           httpserver.NewHandler(logger, ready),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	serveErrors := make(chan error, 1)
	go func() {
		serveErrors <- server.Serve(listener)
	}()

	logger.Info("gateway started", "address", listener.Addr().String())

	select {
	case err := <-serveErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return runtimeError("serve failed", err)
	case <-ctx.Done():
	}

	logger.Info("gateway shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		_ = server.Close()
		return runtimeError("graceful shutdown timed out", err)
	}
	if err := <-serveErrors; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return runtimeError("serve failed during shutdown", err)
	}

	logger.Info("gateway stopped")
	return nil
}
