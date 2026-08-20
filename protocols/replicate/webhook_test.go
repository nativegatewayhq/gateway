package replicate

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nativegatewayhq/gateway/internal/jobs"
	joboperation "github.com/nativegatewayhq/gateway/operations/job"
)

type webhookServiceStub struct {
	request jobs.WebhookObservation
	err     error
	calls   int
}

func (stub *webhookServiceStub) ApplyWebhook(_ context.Context, request jobs.WebhookObservation) (joboperation.Job, bool, error) {
	stub.calls++
	stub.request = request
	return joboperation.Job{ID: request.JobID, Status: request.Observation.Status}, false, stub.err
}

type webhookAdapterStub struct{}

func (webhookAdapterStub) WebhookObservation(_ string, _ []byte) (string, joboperation.Observation, error) {
	body := []byte(`{"id":"job","status":"succeeded"}`)
	return "provider-prediction", joboperation.Observation{Status: joboperation.Succeeded, Snapshot: joboperation.Snapshot{Status: 200, Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: body, SHA256: sha256.Sum256(body)}}, nil
}

func TestSignatureVerifierAcceptsRawBodyAndRotation(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	oldSecret := []byte("0123456789abcdef")
	activeSecret := []byte("fedcba9876543210")
	verifier, err := NewSignatureVerifier([]string{"whsec_" + base64.StdEncoding.EncodeToString(activeSecret), "whsec_" + base64.StdEncoding.EncodeToString(oldSecret)}, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	verifier.now = func() time.Time { return now }
	body := []byte("{\n  \"status\": \"succeeded\"\n}")
	timestamp := strconv.FormatInt(now.Unix(), 10)
	signature := signWebhook(oldSecret, "delivery-1", timestamp, body)
	if !verifier.Verify("delivery-1", timestamp, "v2,bad v1,"+signature, body) {
		t.Fatal("valid rotated signature rejected")
	}
	if verifier.Verify("delivery-1", timestamp, "v1,"+signature, []byte(`{"status":"succeeded"}`)) {
		t.Fatal("re-encoded body accepted")
	}
	if verifier.Verify("delivery-1", strconv.FormatInt(now.Add(-6*time.Minute).Unix(), 10), "v1,"+signature, body) {
		t.Fatal("stale timestamp accepted")
	}
}

func TestWebhookHandlerVerifiesAndAppliesTerminalDelivery(t *testing.T) {
	secret := []byte("0123456789abcdef")
	verifier, _ := NewSignatureVerifier([]string{"whsec_" + base64.StdEncoding.EncodeToString(secret)}, 5*time.Minute)
	now := time.Unix(1_800_000_000, 0)
	verifier.now = func() time.Time { return now }
	service := &webhookServiceStub{}
	handler, err := NewWebhookHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), verifier, service, webhookAdapterStub{}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"id":"provider-prediction","status":"succeeded"}`)
	timestamp := strconv.FormatInt(now.Unix(), 10)
	request := httptest.NewRequest(http.MethodPost, "/internal/webhooks/replicate/job_00000000000000000000000000000000/whk_00000000000000000000000000000000", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Webhook-Id", "delivery-1")
	request.Header.Set("Webhook-Timestamp", timestamp)
	request.Header.Set("Webhook-Signature", "v1,"+signWebhook(secret, "delivery-1", timestamp, body))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || service.calls != 1 || service.request.Token != "whk_00000000000000000000000000000000" || service.request.ProviderJobID != "provider-prediction" {
		t.Fatalf("response=%d calls=%d request=%+v", response.Code, service.calls, service.request)
	}

	bad := httptest.NewRequest(http.MethodPost, request.URL.Path, strings.NewReader(string(body)))
	bad.Header.Set("Content-Type", "application/json")
	bad.Header.Set("Webhook-Id", "delivery-1")
	bad.Header.Set("Webhook-Timestamp", timestamp)
	bad.Header.Set("Webhook-Signature", "v1,invalid")
	badResponse := httptest.NewRecorder()
	handler.ServeHTTP(badResponse, bad)
	if badResponse.Code != http.StatusUnauthorized || service.calls != 1 {
		t.Fatalf("invalid response=%d calls=%d", badResponse.Code, service.calls)
	}

	duplicate := httptest.NewRequest(http.MethodPost, request.URL.Path, strings.NewReader(string(body)))
	duplicate.Header.Set("Content-Type", "application/json")
	duplicate.Header.Add("Webhook-Id", "delivery-1")
	duplicate.Header.Add("Webhook-Id", "delivery-2")
	duplicate.Header.Set("Webhook-Timestamp", timestamp)
	duplicate.Header.Set("Webhook-Signature", "v1,"+signWebhook(secret, "delivery-1", timestamp, body))
	duplicateResponse := httptest.NewRecorder()
	handler.ServeHTTP(duplicateResponse, duplicate)
	if duplicateResponse.Code != http.StatusUnauthorized || service.calls != 1 {
		t.Fatalf("duplicate headers response=%d calls=%d", duplicateResponse.Code, service.calls)
	}

	service.err = context.DeadlineExceeded
	retry := httptest.NewRequest(http.MethodPost, request.URL.Path, strings.NewReader(string(body)))
	retry.Header.Set("Content-Type", "application/json")
	retry.Header.Set("Webhook-Id", "delivery-retry")
	retry.Header.Set("Webhook-Timestamp", timestamp)
	retry.Header.Set("Webhook-Signature", "v1,"+signWebhook(secret, "delivery-retry", timestamp, body))
	retryResponse := httptest.NewRecorder()
	handler.ServeHTTP(retryResponse, retry)
	if retryResponse.Code != http.StatusServiceUnavailable || service.calls != 2 {
		t.Fatalf("retry response=%d calls=%d", retryResponse.Code, service.calls)
	}
}

func signWebhook(secret []byte, deliveryID, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(append([]byte(deliveryID+"."+timestamp+"."), body...))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}
