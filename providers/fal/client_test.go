package fal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nativegatewayhq/gateway/internal/jobs"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	joboperation "github.com/nativegatewayhq/gateway/operations/job"
)

func TestQueueLifecycleUsesFixedOriginAndSanitizesIdentity(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Key fal-secret" {
			t.Fatalf("authorization = %q", request.Header.Get("Authorization"))
		}
		mu.Lock()
		calls = append(calls, request.Method+" "+request.URL.Path)
		mu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		switch request.Method + " " + request.URL.Path {
		case "POST /fal-ai/flux/dev":
			if got := request.URL.Query().Get("fal_webhook"); got != "https://gateway.example/internal/webhooks/fal/job/token" {
				t.Fatalf("fal_webhook = %q", got)
			}
			_, _ = writer.Write([]byte(`{"request_id":"provider-secret","status_url":"https://queue.fal.run/private","response_url":"https://queue.fal.run/private"}`))
		case "GET /fal-ai/flux/dev/requests/provider-secret/status":
			_, _ = writer.Write([]byte(`{"status":"COMPLETED","request_id":"provider-secret"}`))
		case "GET /fal-ai/flux/dev/requests/provider-secret":
			_, _ = writer.Write([]byte(`{"request_id":"provider-secret","images":[{"url":"https://delivery.example/image.png"}]}`))
		case "PUT /fal-ai/flux/dev/requests/provider-secret/cancel":
			_, _ = writer.Write([]byte(`{}`))
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	registry, err := providercredentials.Load(func(key string) (string, bool) {
		if key == "GATEWAY_FAL_API_KEY" {
			return "fal-secret", true
		}
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := New(Config{Endpoint: upstream.URL, PublicBaseURL: "https://gateway.example", Timeout: time.Second, MaximumBodyBytes: 1 << 20}, registry)
	if err != nil {
		t.Fatal(err)
	}
	job := joboperation.Job{ID: "job_00000000000000000000000000000000", Model: "fal-ai/flux/dev", ChannelID: "channel_00000000000000000000000000000005"}
	payload, err := (SubmitPayload{Body: []byte(`{"prompt":"cat"}`)}).WithWebhook("https://gateway.example/internal/webhooks/fal/job/token")
	if err != nil {
		t.Fatal(err)
	}
	submitted, err := client.Submit(context.Background(), job, payload)
	if err != nil || submitted.ProviderJobID != "provider-secret" || strings.Contains(string(submitted.Observation.Snapshot.Body), "provider-secret") || strings.Contains(string(submitted.Observation.Snapshot.Body), "queue.fal.run") {
		t.Fatalf("submit = %#v, %v, body=%s", submitted, err, submitted.Observation.Snapshot.Body)
	}
	attempt := jobs.ProviderAttempt{JobID: job.ID, Model: job.Model, ProviderJobID: submitted.ProviderJobID, ChannelID: job.ChannelID}
	observation, err := client.Poll(context.Background(), attempt)
	if err != nil || observation.Status != joboperation.Succeeded || strings.Contains(string(observation.Snapshot.Body), "provider-secret") || !strings.Contains(string(observation.Snapshot.Body), job.ID) {
		t.Fatalf("poll = %#v, %v, body=%s", observation, err, observation.Snapshot.Body)
	}
	canceled, err := client.Cancel(context.Background(), attempt)
	if err != nil || canceled.Status != joboperation.Canceled {
		t.Fatalf("cancel = %#v, %v", canceled, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 4 {
		t.Fatalf("calls = %v", calls)
	}
}

func TestInvalidModelAndRedirectFailClosed(t *testing.T) {
	if validModel("fal-ai/../secret") || validModel("single") || validModel("fal-ai/%2f") {
		t.Fatal("unsafe model accepted")
	}
	redirect := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Location", "https://evil.example/credential-target")
		writer.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	registry, err := providercredentials.Load(func(key string) (string, bool) {
		if key == "GATEWAY_FAL_API_KEY" {
			return "fal-secret", true
		}
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := New(Config{Endpoint: redirect.URL, PublicBaseURL: "https://gateway.example", Timeout: time.Second, MaximumBodyBytes: 1 << 20}, registry)
	if err != nil {
		t.Fatal(err)
	}
	job := joboperation.Job{ID: "job_00000000000000000000000000000000", Model: "fal-ai/flux/dev", ChannelID: "channel_00000000000000000000000000000005"}
	if _, err := client.Submit(context.Background(), job, SubmitPayload{Body: []byte(`{"prompt":"cat"}`)}); err == nil {
		t.Fatal("redirect was accepted")
	}
}

func TestSubmitPayloadInjectsGatewayWebhookQuery(t *testing.T) {
	payload := SubmitPayload{Body: []byte(`{"prompt":"cat"}`)}
	value, err := payload.WithWebhook("https://gateway.example/internal/webhooks/fal/job_00000000000000000000000000000000/whk_00000000000000000000000000000000")
	if err != nil || value.(SubmitPayload).WebhookURL == "" {
		t.Fatalf("value=%+v err=%v", value, err)
	}
	if _, err := payload.WithWebhook("http://gateway.example/callback"); err == nil {
		t.Fatal("insecure callback accepted")
	}
}

func TestWebhookObservationProjectsNativeResultAndTerminalFailures(t *testing.T) {
	registry, _ := providercredentials.Load(func(string) (string, bool) { return "", false })
	client, err := New(Config{Endpoint: "https://queue.fal.run", PublicBaseURL: "https://gateway.example", Timeout: time.Second, MaximumBodyBytes: 1 << 20}, registry)
	if err != nil {
		t.Fatal(err)
	}
	providerID, observation, err := client.WebhookObservation("job_00000000000000000000000000000000", []byte(`{"request_id":"provider-secret","gateway_request_id":"retry-secret","status":"OK","payload":{"images":[{"url":"https://delivery.example/image.png"}]}}`))
	if err != nil || providerID != "provider-secret" || observation.Status != joboperation.Succeeded || strings.Contains(string(observation.Snapshot.Body), "provider-secret") || strings.Contains(string(observation.Snapshot.Body), "retry-secret") {
		t.Fatalf("provider=%q observation=%+v err=%v", providerID, observation, err)
	}
	for status, expected := range map[string]joboperation.Status{"ERROR": joboperation.Failed, "CANCELED": joboperation.Canceled} {
		_, terminal, err := client.WebhookObservation("job_00000000000000000000000000000000", []byte(`{"request_id":"provider","status":"`+status+`"}`))
		if err != nil || terminal.Status != expected {
			t.Fatalf("status=%s terminal=%+v err=%v", status, terminal, err)
		}
	}
}
