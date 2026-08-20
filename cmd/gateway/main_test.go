//go:build integration && !windows

package main

import (
	"bytes"
	"context"
	"crypto/rand"
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

	"github.com/nativegatewayhq/gateway/internal/apikey"
	"github.com/nativegatewayhq/gateway/internal/database"
	"github.com/nativegatewayhq/gateway/internal/httpserver"
	"github.com/nativegatewayhq/gateway/internal/observability"
	"github.com/nativegatewayhq/gateway/internal/ratelimit"
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
		"GATEWAY_RATE_LIMIT_MODE=required",
		"GATEWAY_REDIS_URL="+os.Getenv("TEST_REDIS_URL"),
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

func TestGatewayProcessEnforcesStoredAPIKeyRateLimit(t *testing.T) {
	databaseURL, redisURL := os.Getenv("TEST_DATABASE_URL"), os.Getenv("TEST_REDIS_URL")
	if databaseURL == "" || redisURL == "" {
		t.Skip("TEST_DATABASE_URL and TEST_REDIS_URL are required")
	}
	ctx := context.Background()
	pool, err := database.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := database.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	record, raw, err := apikey.GenerateForProjectWithPolicy(rand.Reader, "process limited", "project_legacy", nil, apikey.RateLimitPolicy{RequestsPerMinute: 1, Burst: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := apikey.NewPostgresStore(pool).Create(ctx, record); err != nil {
		t.Fatal(err)
	}
	defer pool.Exec(ctx, `DELETE FROM service_api_keys WHERE id=$1`, record.ID)
	limiter, err := ratelimit.NewRedis(redisURL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer limiter.Close()
	decision, err := limiter.Allow(ctx, record.ID, ratelimit.Policy{RequestsPerMinute: 1, Burst: 1})
	if err != nil || !decision.Allowed {
		t.Fatalf("preconsume=%+v error=%v", decision, err)
	}

	executable, address := buildGateway(t), availableAddress(t)
	command := exec.Command(executable)
	command.Env = append(gatewayEnvironment(address), "GATEWAY_RATE_LIMIT_MODE=required", "GATEWAY_REDIS_URL="+redisURL)
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waitForHealth(t, "http://"+address+"/health/ready", command, &output)
	request, _ := http.NewRequest(http.MethodGet, "http://"+address+"/v1/models", nil)
	request.Header.Set("Authorization", "Bearer "+raw)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		_ = command.Process.Kill()
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusTooManyRequests || response.Header.Get("Retry-After") == "" || !strings.Contains(string(body), "rate_limit_exceeded") {
		_ = command.Process.Kill()
		t.Fatalf("response=%d headers=%v body=%s logs=%s", response.StatusCode, response.Header, body, output.String())
	}
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("exit=%v logs=%s", err, output.String())
	}
	if strings.Contains(output.String(), raw) {
		t.Fatal("process logs leaked API key")
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

func TestGatewayProcessRedisLossKeepsLivenessAndFailsReadiness(t *testing.T) {
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	executable, address := buildGateway(t), availableAddress(t)
	command := exec.Command(executable)
	command.Env = append(gatewayEnvironment(address), "GATEWAY_RATE_LIMIT_MODE=required", "GATEWAY_REDIS_URL=redis://127.0.0.1:1/0", "GATEWAY_RATE_LIMIT_TIMEOUT=20ms")
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waitForHealth(t, "http://"+address+"/health/live", command, &output)
	response, err := (&http.Client{Timeout: time.Second}).Get("http://" + address + "/health/ready")
	if err != nil {
		_ = command.Process.Kill()
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		_ = command.Process.Kill()
		t.Fatalf("ready=%d logs=%s", response.StatusCode, output.String())
	}
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("exit=%v logs=%s", err, output.String())
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
