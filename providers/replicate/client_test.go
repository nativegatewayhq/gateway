package replicate

import (
	"context"
	"encoding/json"
	"github.com/nativegatewayhq/gateway/internal/jobs"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	joboperation "github.com/nativegatewayhq/gateway/operations/job"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testClient(t *testing.T, handler http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	registry, err := providercredentials.Load(func(key string) (string, bool) {
		if key == "GATEWAY_REPLICATE_API_TOKEN" {
			return "provider-secret", true
		}
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := New(Config{Endpoint: server.URL, PublicBaseURL: "https://gateway.example", Timeout: time.Second, MaximumBodyBytes: 1 << 20}, registry)
	if err != nil {
		t.Fatal(err)
	}
	return client, server
}
func TestSubmitSanitizesIdentityAndCredential(t *testing.T) {
	client, server := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/predictions" || r.Header.Get("Authorization") != "Bearer provider-secret" {
			t.Fatalf("request=%s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"id":"provider-id","status":"starting","input":{"prompt":"secret"},"urls":{"get":"https://api.replicate.com/v1/predictions/provider-id"}}`))
	}))
	defer server.Close()
	value := joboperation.Job{ID: "job_00000000000000000000000000000000", ChannelID: "channel_00000000000000000000000000000004"}
	result, err := client.Submit(context.Background(), value, SubmitPayload{Body: []byte(`{"version":"owner/model:version","input":{"prompt":"secret"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	if result.ProviderJobID != "provider-id" || result.Observation.Status != joboperation.Queued {
		t.Fatalf("result=%+v", result)
	}
	text := string(result.Observation.Snapshot.Body)
	if strings.Contains(text, "provider-id") || strings.Contains(text, "api.replicate.com") || !strings.Contains(text, value.ID) || !strings.Contains(text, "gateway.example") {
		t.Fatalf("snapshot=%s", text)
	}
}
func TestPollAndCancelUseFixedOriginNotResponseURLs(t *testing.T) {
	calls := []string{}
	client, server := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		_, _ = w.Write([]byte(`{"id":"internal","status":"succeeded","output":["https://delivery.example/output.png"],"urls":{"get":"https://evil.example"}}`))
	}))
	defer server.Close()
	attempt := jobs.ProviderAttempt{JobID: "job_00000000000000000000000000000000", AttemptNo: 1, ProviderJobID: "internal", ChannelID: "channel_00000000000000000000000000000004"}
	if _, err := client.Poll(context.Background(), attempt); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Cancel(context.Background(), attempt); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0] != "GET /v1/predictions/internal" || calls[1] != "POST /v1/predictions/internal/cancel" {
		t.Fatalf("calls=%v", calls)
	}
}
func TestRejectsRedirectOversizeAndMalformedStatus(t *testing.T) {
	for name, handler := range map[string]http.Handler{"redirect": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "https://evil.example")
		w.WriteHeader(302)
	}), "malformed": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"id":"x","status":"other"}`)) })} {
		t.Run(name, func(t *testing.T) {
			client, server := testClient(t, handler)
			defer server.Close()
			_, err := client.Submit(context.Background(), joboperation.Job{ID: "job_00000000000000000000000000000000", ChannelID: "channel_00000000000000000000000000000004"}, SubmitPayload{Body: json.RawMessage(`{"version":"x","input":{}}`)})
			if err == nil {
				t.Fatal("unsafe response accepted")
			}
		})
	}
}
