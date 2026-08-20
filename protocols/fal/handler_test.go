package fal

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nativegatewayhq/gateway/internal/apikey"
	"github.com/nativegatewayhq/gateway/internal/jobs"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	imageoperation "github.com/nativegatewayhq/gateway/operations/image"
	joboperation "github.com/nativegatewayhq/gateway/operations/job"
)

const testJobID = "job_00000000000000000000000000000000"

type authStub struct{ principal apikey.Principal }

func (stub authStub) Authenticate(context.Context, string) (apikey.Principal, error) {
	return stub.principal, nil
}

type modelsStub struct{}

func (modelsStub) Candidates(_, model string, _ imageoperation.Operation, _ imageoperation.MediaType) ([]imageoperation.RoutingDecision, error) {
	return []imageoperation.RoutingDecision{{Protocol: "fal", Model: model, Provider: providercredentials.Fal, ChannelID: "channel_00000000000000000000000000000005", Policy: imageoperation.Fixed, Usage: imageoperation.UsageCapability{Dimension: "output", Unit: "image", DefaultQuantity: 1, MaximumQuantity: 10, RequestExtractor: "fal-input-num_images-v1", ResultExtractor: "fal-output-v1"}}}, nil
}

type jobsStub struct {
	value         joboperation.Job
	submits, gets int
	cancels       int
	request       jobs.CreateRequest
}

func (stub *jobsStub) Submit(_ context.Context, request jobs.CreateRequest, _ any) (joboperation.Job, error) {
	stub.submits++
	stub.request = request
	stub.value = joboperation.Job{ID: testJobID, Owner: request.Owner, Protocol: "fal", Model: request.Model, Status: joboperation.Queued, Snapshot: joboperation.Snapshot{Status: 200, Body: []byte(`{"request_id":"` + testJobID + `","status_url":"https://gateway.example/fal-ai/flux/dev/requests/` + testJobID + `/status"}`)}}
	return stub.value, nil
}
func (stub *jobsStub) Get(context.Context, joboperation.Owner, string) (joboperation.Job, error) {
	stub.gets++
	return stub.value, nil
}
func (stub *jobsStub) Cancel(context.Context, joboperation.Owner, string) (joboperation.Job, error) {
	stub.cancels++
	stub.value.Status = joboperation.Canceled
	return stub.value, nil
}

func testHandler(service *jobsStub) *Handler {
	principal := apikey.Principal{APIKeyID: "key", ProjectID: "project", OrganizationID: "org"}
	return NewHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), authStub{principal}, modelsStub{}, service, nil, nil, 1<<20, "https://gateway.example")
}

func request(handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Key service-key")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	return response
}

func TestSubmitStatusResultAndCancel(t *testing.T) {
	service := &jobsStub{}
	handler := testHandler(service)
	response := request(handler, http.MethodPost, "/fal-ai/flux/dev", `{"prompt":"cat"}`)
	if response.Code != http.StatusOK || service.submits != 1 || service.request.EstimatedUsage == nil || service.request.EstimatedUsage.Quantity != 1 || !strings.Contains(response.Body.String(), testJobID) {
		t.Fatalf("submit=%d body=%s submits=%d", response.Code, response.Body.String(), service.submits)
	}
	response = request(handler, http.MethodGet, "/fal-ai/flux/dev/requests/"+testJobID+"/status?logs=false", "")
	if response.Code != http.StatusOK || service.gets != 1 {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	service.value.Status = joboperation.Succeeded
	service.value.Snapshot = joboperation.Snapshot{Status: 200, Body: []byte(`{"images":[{"url":"https://delivery.example/image.png"}]}`)}
	response = request(handler, http.MethodGet, "/fal-ai/flux/dev/requests/"+testJobID, "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "delivery.example") {
		t.Fatalf("result=%d body=%s", response.Code, response.Body.String())
	}
	response = request(handler, http.MethodPut, "/fal-ai/flux/dev/requests/"+testJobID+"/cancel", "")
	if response.Code != http.StatusOK || service.cancels != 1 || !strings.Contains(response.Body.String(), "CANCELED") {
		t.Fatalf("cancel=%d body=%s cancels=%d", response.Code, response.Body.String(), service.cancels)
	}
}

func TestSubmitPassesMaximumOutputUsageToJob(t *testing.T) {
	service := &jobsStub{}
	response := request(testHandler(service), http.MethodPost, "/fal-ai/flux/dev", `{"prompt":"cat","num_images":5}`)
	if response.Code != http.StatusOK || service.request.EstimatedUsage == nil || service.request.EstimatedUsage.Quantity != 5 {
		t.Fatalf("status=%d usage=%+v body=%s", response.Code, service.request.EstimatedUsage, response.Body.String())
	}
}

func TestRouteAndWebhookValidationFailClosed(t *testing.T) {
	for _, item := range []struct{ method, path, body string }{
		{http.MethodPost, "/fal-ai/%2fsecret", `{}`},
		{http.MethodPost, "/fal-ai/../secret", `{}`},
		{http.MethodPost, "/fal-ai/flux/dev?fal_webhook=https://evil.example", `{}`},
		{http.MethodPost, "/fal-ai/flux/dev", `{"webhook_url":"https://evil.example"}`},
		{http.MethodGet, "/fal-ai/flux/dev/requests/" + testJobID + "/status?logs=secret", ""},
	} {
		service := &jobsStub{}
		response := request(testHandler(service), item.method, item.path, item.body)
		if response.Code < 400 || service.submits != 0 {
			t.Fatalf("%s status=%d submits=%d", item.path, response.Code, service.submits)
		}
	}
}

func TestModelMismatchIsNotFound(t *testing.T) {
	service := &jobsStub{value: joboperation.Job{ID: testJobID, Protocol: "fal", Model: "fal-ai/flux/dev", Status: joboperation.Queued}}
	response := request(testHandler(service), http.MethodGet, "/fal-ai/other/requests/"+testJobID+"/status", "")
	if response.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestOfficialJavaScriptProxyWireIsTranslatedWithoutFetchingTarget(t *testing.T) {
	service := &jobsStub{}
	req := httptest.NewRequest(http.MethodPost, "/fal/proxy", strings.NewReader(`{"prompt":"cat"}`))
	req.Header.Set("Authorization", "Key service-key")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-fal-target-url", "https://queue.fal.run/fal-ai/flux/dev")
	response := httptest.NewRecorder()
	testHandler(service).ServeHTTP(response, req)
	if response.Code != http.StatusOK || service.submits != 1 || response.Header().Get("X-Fal-Request-Id") != testJobID {
		t.Fatalf("status=%d submits=%d headers=%v body=%s", response.Code, service.submits, response.Header(), response.Body.String())
	}

	bad := httptest.NewRequest(http.MethodPost, "/fal/proxy", strings.NewReader(`{}`))
	bad.Header.Set("Authorization", "Key service-key")
	bad.Header.Set("Content-Type", "application/json")
	bad.Header.Set("x-fal-target-url", "https://evil.example/fal-ai/flux/dev")
	response = httptest.NewRecorder()
	testHandler(&jobsStub{}).ServeHTTP(response, bad)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("bad target status=%d", response.Code)
	}
}
