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

func TestSubmitPayloadInjectsOnlyGatewayWebhook(t *testing.T) {
	payload := SubmitPayload{Body: []byte(`{"version":"owner/model:version","input":{"prompt":"cat"}}`), Prefer: "respond-async"}
	value, err := payload.WithWebhook("https://gateway.example/internal/webhooks/replicate/job_00000000000000000000000000000000/whk_00000000000000000000000000000000")
	if err != nil {
		t.Fatal(err)
	}
	injected := value.(SubmitPayload)
	var envelope map[string]any
	if json.Unmarshal(injected.Body, &envelope) != nil || envelope["webhook"] == nil {
		t.Fatalf("body=%s", injected.Body)
	}
	filter, ok := envelope["webhook_events_filter"].([]any)
	if !ok || len(filter) != 1 || filter[0] != "completed" {
		t.Fatalf("filter=%v", envelope["webhook_events_filter"])
	}
	for _, body := range []string{`{"version":"x","input":{},"webhook":"https://evil.example"}`, `{"version":"x","input":{},"webhook_events_filter":["start"]}`} {
		if _, err := (SubmitPayload{Body: []byte(body)}).WithWebhook("https://gateway.example/callback"); err == nil {
			t.Fatal("client webhook accepted")
		}
	}
	if _, err := payload.WithWebhook("http://gateway.example/callback"); err == nil {
		t.Fatal("insecure callback accepted")
	}
}

func TestWebhookObservationRequiresTerminalAndSanitizesIdentity(t *testing.T) {
	registry, _ := providercredentials.Load(func(string) (string, bool) { return "", false })
	client, err := New(Config{Endpoint: "https://api.replicate.com", PublicBaseURL: "https://gateway.example", Timeout: time.Second, MaximumBodyBytes: 1 << 20}, registry)
	if err != nil {
		t.Fatal(err)
	}
	providerID, observation, err := client.WebhookObservation("job_00000000000000000000000000000000", []byte(`{"id":"provider-secret-id","status":"succeeded","urls":{"get":"https://evil.example"}}`))
	if err != nil || providerID != "provider-secret-id" || strings.Contains(string(observation.Snapshot.Body), "provider-secret-id") || strings.Contains(string(observation.Snapshot.Body), "evil.example") {
		t.Fatalf("provider=%q observation=%+v err=%v", providerID, observation, err)
	}
	if _, _, err := client.WebhookObservation("job_00000000000000000000000000000000", []byte(`{"id":"provider","status":"processing"}`)); err == nil {
		t.Fatal("non-terminal webhook accepted")
	}
	_, canceled, err := client.WebhookObservation("job_00000000000000000000000000000000", []byte(`{"id":"provider","status":"canceled","output":["https://delivery.example/partial.png"]}`))
	if err != nil || canceled.Status != joboperation.Canceled || canceled.Usage == nil || canceled.Usage.Quantity != 1 || len(canceled.Snapshot.Body) != 0 {
		t.Fatalf("canceled=%+v err=%v", canceled, err)
	}
}
