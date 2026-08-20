package runway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nativegatewayhq/gateway/internal/jobs"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	joboperation "github.com/nativegatewayhq/gateway/operations/job"
)

func TestClientUsesFixedWireAndSanitizesTaskIdentity(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "Bearer upstream-secret" || r.Header.Get("X-Runway-Version") != APIVersion {
			t.Fatalf("invalid upstream headers: %#v", r.Header)
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/uploads":
			if r.Header.Get("Content-Type") != "application/json" {
				t.Fatalf("upload content type=%s", r.Header.Get("Content-Type"))
			}
			_, _ = io.WriteString(w, `{"uploadUrl":"https://storage.example/upload","fields":{"key":"opaque"},"runwayUri":"runway://uploads/asset"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/text_to_video":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"provider-task"}`)
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"id":"provider-task","status":"SUCCEEDED","output":["https://cdn.example/video.mp4"]}`)
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	registry, err := providercredentials.Load(func(key string) (string, bool) {
		if key == "GATEWAY_RUNWAY_API_KEY" {
			return "upstream-secret", true
		}
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := New(Config{Endpoint: server.URL, Timeout: time.Second, MaximumBodyBytes: 1 << 20}, registry)
	if err != nil {
		t.Fatal(err)
	}
	job := joboperation.Job{ID: "job_0123456789abcdef0123456789abcdef", ChannelID: "channel_00000000000000000000000000000007"}
	upload, err := client.CreateEphemeralUpload(context.Background(), job.ChannelID, []byte(`{"filename":"input.mp4","type":"ephemeral"}`))
	if err != nil || upload.Status != http.StatusOK || upload.URI != "runway://uploads/asset" || strings.Contains(string(upload.Body), "upstream-secret") {
		t.Fatalf("upload: %#v %v", upload, err)
	}
	submitted, err := client.Submit(context.Background(), job, SubmitPayload{Path: "/v1/text_to_video", Body: []byte(`{"model":"gen4_turbo"}`)})
	if err != nil || submitted.ProviderJobID != "provider-task" {
		t.Fatalf("submit: %#v %v", submitted, err)
	}
	attempt := jobs.ProviderAttempt{JobID: job.ID, ProviderJobID: submitted.ProviderJobID, ChannelID: job.ChannelID}
	observed, err := client.Poll(context.Background(), attempt)
	if err != nil || observed.Status != joboperation.Succeeded || strings.Contains(string(observed.Snapshot.Body), "provider-task") || !strings.Contains(string(observed.Snapshot.Body), job.ID) {
		t.Fatalf("poll: %#v %v", observed, err)
	}
	canceled, err := client.Cancel(context.Background(), attempt)
	if err != nil || canceled.Status != joboperation.Canceled {
		t.Fatalf("cancel: %#v %v", canceled, err)
	}
	if requests != 4 {
		t.Fatalf("requests=%d", requests)
	}
}

func TestStatusMapping(t *testing.T) {
	tests := map[string]joboperation.Status{"PENDING": joboperation.Queued, "THROTTLED": joboperation.Queued, "RUNNING": joboperation.Processing, "SUCCEEDED": joboperation.Succeeded, "FAILED": joboperation.Failed, "CANCELED": joboperation.Canceled}
	for native, want := range tests {
		got, _, ok := mapStatus(native)
		if !ok || got != want {
			t.Fatalf("%s => %s", native, got)
		}
	}
	if _, _, ok := mapStatus("UNKNOWN"); ok {
		t.Fatal("unknown status accepted")
	}
}

func TestRunwayCostUsesBoundedFixedPointMicrocredits(t *testing.T) {
	tests := map[string]int64{`{"credits":0}`: 0, `{"credits":12}`: 12_000_000, `{"credits":1.25}`: 1_250_000, `{"credits":0.000001}`: 1}
	for raw, want := range tests {
		got, ok := parseCostMicros([]byte(raw))
		if !ok || got != want {
			t.Fatalf("%s => %d/%v", raw, got, ok)
		}
	}
	for _, raw := range []string{`{"credits":-1}`, `{"credits":1.0000001}`, `{"credits":1e2}`, `{}`, `{"credits":"1"}`} {
		if _, ok := parseCostMicros([]byte(raw)); ok {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestUploadCapabilityValidationIsBounded(t *testing.T) {
	for _, value := range []string{"https://storage.example/upload?signature=opaque", "https://bucket.example/path"} {
		if !validUploadURL(value) {
			t.Fatalf("valid upload URL rejected: %s", value)
		}
	}
	for _, value := range []string{"http://storage.example/upload", "https://user@storage.example/upload", "https://storage.example/upload#fragment"} {
		if validUploadURL(value) {
			t.Fatalf("unsafe upload URL accepted: %s", value)
		}
	}
	if !validUploadURI("runway://uploads/asset") || validUploadURI("runway://bad uri") || validUploadURI("https://example.com") {
		t.Fatal("Runway URI validation mismatch")
	}
}
