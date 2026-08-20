package fal

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nativegatewayhq/gateway/internal/jobs"
	joboperation "github.com/nativegatewayhq/gateway/operations/job"
)

func TestFalJWKSVerifierUsesRawBodyAndBoundedCache(t *testing.T) {
	public, private, _ := ed25519.GenerateKey(nil)
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		if request.URL.Path != "/.well-known/jwks.json" {
			t.Fatalf("path=%s", request.URL.Path)
		}
		writer.Header().Set("Cache-Control", "public, max-age=60")
		_, _ = fmt.Fprintf(writer, `{"keys":[{"kty":"OKP","crv":"Ed25519","kid":"key-1","x":%q}]}`, base64.RawURLEncoding.EncodeToString(public))
	}))
	defer server.Close()
	verifier, err := NewFalJWKSVerifier(JWKSConfig{URL: server.URL + "/.well-known/jwks.json", Timeout: time.Second, CacheTTL: 24 * time.Hour, RefreshCooldown: time.Minute, MaximumBodyBytes: 64 * 1024})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	verifier.now = func() time.Time { return now }
	body := []byte("{\n \"request_id\": \"provider-id\"\n}")
	timestamp := strconv.FormatInt(now.Unix(), 10)
	signature := falSignature(private, "provider-id", "user-id", timestamp, body)
	if err := verifier.Verify(context.Background(), "provider-id", "user-id", timestamp, signature, body); err != nil {
		t.Fatal(err)
	}
	if err := verifier.Verify(context.Background(), "provider-id", "user-id", timestamp, signature, body); err != nil || calls.Load() != 1 {
		t.Fatalf("cached err=%v calls=%d", err, calls.Load())
	}
	if err := verifier.Verify(context.Background(), "provider-id", "user-id", timestamp, signature, []byte(`{"request_id":"provider-id"}`)); !errors.Is(err, ErrWebhookSignature) {
		t.Fatalf("re-encoded body=%v", err)
	}
	if err := verifier.Verify(context.Background(), "provider-id", "user-id", strconv.FormatInt(now.Add(-6*time.Minute).Unix(), 10), signature, body); !errors.Is(err, ErrWebhookSignature) {
		t.Fatalf("stale=%v", err)
	}
}

type falWebhookServiceStub struct {
	request jobs.WebhookObservation
	err     error
	calls   int
}

func (stub *falWebhookServiceStub) ApplyWebhook(_ context.Context, request jobs.WebhookObservation) (joboperation.Job, bool, error) {
	stub.calls++
	stub.request = request
	return joboperation.Job{}, false, stub.err
}

type falWebhookAdapterStub struct{}

func (falWebhookAdapterStub) WebhookObservation(_ string, _ []byte) (string, joboperation.Observation, error) {
	body := []byte(`{"image":{"url":"https://delivery.example/image.png"}}`)
	return "provider-id", joboperation.Observation{Status: joboperation.Succeeded, ProviderJobID: "provider-id", Snapshot: joboperation.Snapshot{Status: 200, Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: body, SHA256: sha256.Sum256(body)}}, nil
}

func TestFalWebhookHandlerVerifiesBeforeApplying(t *testing.T) {
	public, private, _ := ed25519.GenerateKey(nil)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(writer, `{"keys":[{"kty":"OKP","crv":"Ed25519","x":%q}]}`, base64.RawURLEncoding.EncodeToString(public))
	}))
	defer server.Close()
	verifier, _ := NewFalJWKSVerifier(JWKSConfig{URL: server.URL + "/.well-known/jwks.json", Timeout: time.Second, CacheTTL: time.Hour, RefreshCooldown: time.Minute, MaximumBodyBytes: 65536})
	now := time.Unix(1_800_000_000, 0)
	verifier.now = func() time.Time { return now }
	service := &falWebhookServiceStub{}
	handler, _ := NewWebhookHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), verifier, service, falWebhookAdapterStub{}, 1024)
	body := []byte(`{"request_id":"provider-id","status":"OK","payload":{"image":{"url":"https://delivery.example/image.png"}}}`)
	timestamp := strconv.FormatInt(now.Unix(), 10)
	request := httptest.NewRequest(http.MethodPost, "/internal/webhooks/fal/job_00000000000000000000000000000000/whk_00000000000000000000000000000000", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Fal-Webhook-Request-Id", "provider-id")
	request.Header.Set("X-Fal-Webhook-User-Id", "user-id")
	request.Header.Set("X-Fal-Webhook-Timestamp", timestamp)
	request.Header.Set("X-Fal-Webhook-Signature", falSignature(private, "provider-id", "user-id", timestamp, body))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || service.calls != 1 || service.request.Provider != "fal" || service.request.ProviderJobID != "provider-id" {
		t.Fatalf("response=%d calls=%d request=%+v", response.Code, service.calls, service.request)
	}
}

func falSignature(private ed25519.PrivateKey, requestID, userID, timestamp string, body []byte) string {
	digest := sha256.Sum256(body)
	message := []byte(requestID + "\n" + userID + "\n" + timestamp + "\n" + hex.EncodeToString(digest[:]))
	return hex.EncodeToString(ed25519.Sign(private, message))
}
