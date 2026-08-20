//go:build integration && !windows

package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nativegatewayhq/gateway/internal/database"
	"github.com/nativegatewayhq/gateway/internal/httpserver"
	"github.com/nativegatewayhq/gateway/internal/observability"
)

func TestRunRejectsInvalidConfigWithoutEchoingValue(t *testing.T) {
	t.Setenv("GATEWAY_HTTP_ADDR", "secret-listen-address")
	t.Setenv("GATEWAY_LOG_LEVEL", "info")
	t.Setenv("GATEWAY_SHUTDOWN_TIMEOUT", "1s")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := run(&stdout, &stderr); code != 1 {
		t.Fatalf("run() code = %d, want 1", code)
	}
	if strings.Contains(stderr.String(), "secret-listen-address") {
		t.Fatalf("configuration error leaked value: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "GATEWAY_HTTP_ADDR") {
		t.Fatalf("configuration error omitted setting name: %s", stderr.String())
	}
}

func TestRunRejectsMalformedProviderCredentialBeforeDatabaseConnection(t *testing.T) {
	t.Setenv("GATEWAY_HTTP_ADDR", "127.0.0.1:0")
	t.Setenv("GATEWAY_LOG_LEVEL", "info")
	t.Setenv("GATEWAY_SHUTDOWN_TIMEOUT", "1s")
	t.Setenv("GATEWAY_DATABASE_URL", "postgres://gateway:database-secret@127.0.0.1:1/gateway")
	t.Setenv("GATEWAY_GOOGLE_API_KEY", " provider-secret ")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := run(&stdout, &stderr); code != 1 {
		t.Fatalf("run() code = %d, want 1", code)
	}
	combined := stdout.String() + stderr.String()
	for _, secret := range []string{"provider-secret", "database-secret"} {
		if strings.Contains(combined, secret) {
			t.Fatalf("configuration failure leaked %q: %s", secret, combined)
		}
	}
	if !strings.Contains(combined, "GATEWAY_GOOGLE_API_KEY") {
		t.Fatalf("configuration failure omitted setting name: %s", combined)
	}
}

func TestGatewayProcessStartsServesHealthAndStops(t *testing.T) {
	executable := buildGateway(t)
	address := availableAddress(t)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable)
	command.Env = append(gatewayEnvironment(address),
		"GATEWAY_BILLING_MODE=required",
		"GATEWAY_MINIMUM_MARGIN_BPS=1000",
		"GATEWAY_GOOGLE_API_KEY=google-process-secret",
		"GATEWAY_OPENAI_API_KEY=openai-process-secret",
		"GATEWAY_XAI_API_KEY=xai-process-secret",
	)
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Start(); err != nil {
		t.Fatalf("start gateway: %v", err)
	}

	waitForHealth(t, "http://"+address+"/health/ready", command, &output)
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal gateway: %v", err)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("gateway exit: %v\n%s", err, output.String())
	}

	logs := output.String()
	for _, message := range []string{"gateway started", "gateway shutting down", "gateway stopped"} {
		if !strings.Contains(logs, message) {
			t.Errorf("logs missing %q: %s", message, logs)
		}
	}
	for _, secret := range []string{"google-process-secret", "openai-process-secret", "xai-process-secret"} {
		if strings.Contains(logs, secret) {
			t.Fatalf("process logs leaked provider credential %q: %s", secret, logs)
		}
	}
}

func TestGatewayProcessFailsFastWhenPortIsInUse(t *testing.T) {
	executable := buildGateway(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	command := exec.Command(executable)
	command.Env = gatewayEnvironment(listener.Addr().String())
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("gateway succeeded with occupied port: %s", output)
	}
	if !strings.Contains(string(output), "listen failed") {
		t.Fatalf("output missing safe error category: %s", output)
	}
	if strings.Contains(string(output), listener.Addr().String()) {
		t.Fatalf("output leaked configured listen value: %s", output)
	}
}

func TestDatabaseConnectionLossMakesReadinessUnavailable(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	pool, err := database.Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()

	handler := httpserver.NewHandler(observability.NewLogger(io.Discard, 0), pool.Ping)
	request := httptest.NewRequest(http.MethodGet, "/health/ready", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("readiness status = %d, want 503", response.Code)
	}
	if strings.Contains(response.Body.String(), url) {
		t.Fatalf("readiness response leaked database URL: %s", response.Body.String())
	}
}

func buildGateway(t *testing.T) string {
	t.Helper()

	executable := filepath.Join(t.TempDir(), "gateway")
	command := exec.Command("go", "build", "-o", executable, ".")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build gateway: %v\n%s", err, output)
	}
	return executable
}

func availableAddress(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func gatewayEnvironment(address string) []string {
	environment := make([]string, 0, len(os.Environ())+3)
	for _, variable := range os.Environ() {
		if strings.HasPrefix(variable, "GATEWAY_") {
			continue
		}
		environment = append(environment, variable)
	}
	return append(environment,
		"GATEWAY_HTTP_ADDR="+address,
		"GATEWAY_LOG_LEVEL=info",
		"GATEWAY_SHUTDOWN_TIMEOUT=2s",
		"GATEWAY_DATABASE_URL="+os.Getenv("TEST_DATABASE_URL"),
	)
}

func waitForHealth(t *testing.T, endpoint string, command *exec.Cmd, output io.StringWriter) {
	t.Helper()

	client := &http.Client{Timeout: 250 * time.Millisecond}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		response, err := client.Get(endpoint)
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	_ = command.Process.Kill()
	t.Fatalf("gateway did not become ready: %s", output)
}
