package app

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/nativegatewayhq/gateway/internal/config"
	"github.com/nativegatewayhq/gateway/internal/observability"
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
	err = Run(context.Background(), cfg, observability.NewLogger(&bytes.Buffer{}, slog.LevelInfo))
	if err == nil || !strings.Contains(err.Error(), "listen failed") {
		t.Fatalf("Run() error = %v, want listen error", err)
	}
}
