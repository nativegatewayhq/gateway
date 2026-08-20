package app

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nativegatewayhq/gateway/internal/config"
	"github.com/nativegatewayhq/gateway/internal/observability"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
)

func TestRunFailsWhenPortIsInUse(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	cfg := config.Config{
		HTTPAddr:        listener.Addr().String(),
		LogLevel:        slog.LevelInfo,
		ShutdownTimeout: time.Second,
	}
	registry, loadErr := providercredentials.Load(func(string) (string, bool) { return "", false })
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	err = Run(context.Background(), cfg, observability.NewLogger(&bytes.Buffer{}, slog.LevelInfo), Dependencies{
		ProviderCredentials: registry,
		Gemini:              http.NotFoundHandler(),
		OpenAIImages:        http.NotFoundHandler(),
	})
	if err == nil || !strings.Contains(err.Error(), "listen failed") {
		t.Fatalf("Run() error = %v, want listen error", err)
	}
}
