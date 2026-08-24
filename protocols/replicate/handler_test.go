package replicate

import (
	"context"
	"encoding/json"
	"github.com/nativegatewayhq/gateway/internal/apikey"
	"github.com/nativegatewayhq/gateway/internal/jobs"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	imageoperation "github.com/nativegatewayhq/gateway/operations/image"
	joboperation "github.com/nativegatewayhq/gateway/operations/job"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type authStub struct{ principal apikey.Principal }

func (stub authStub) Authenticate(context.Context, string) (apikey.Principal, error) {
	return stub.principal, nil
}

type modelsStub struct{}

func (modelsStub) Candidates(_, model string, _ imageoperation.Operation, _ imageoperation.MediaType) ([]imageoperation.RoutingDecision, error) {
	return []imageoperation.RoutingDecision{{Protocol: "replicate", Model: model, Provider: providercredentials.Replicate, ChannelID: "channel_00000000000000000000000000000004", Policy: imageoperation.Fixed, Usage: imageoperation.UsageCapability{Dimension: "output", Unit: "image", DefaultQuantity: 1, MaximumQuantity: 10, RequestExtractor: "replicate-input-num_outputs-v1", ResultExtractor: "replicate-output-v1"}}}, nil
}

type pluginModelsStub struct{}

func (pluginModelsStub) Candidates(_, model string, _ imageoperation.Operation, _ imageoperation.MediaType) ([]imageoperation.RoutingDecision, error) {
	return []imageoperation.RoutingDecision{{Protocol: "replicate", Model: model, Provider: providercredentials.Plugin, ChannelID: "channel_plugin", Policy: imageoperation.Fixed, Usage: imageoperation.UsageCapability{Dimension: "output", Unit: "image", DefaultQuantity: 1, MaximumQuantity: 2, RequestExtractor: "replicate-input-num_outputs-v1", ResultExtractor: "plugin-image-output-v1"}}}, nil
}

type jobsStub struct {
	value            joboperation.Job
	submits, cancels int
	request          jobs.CreateRequest
}

func (stub *jobsStub) Submit(_ context.Context, request jobs.CreateRequest, payload any) (joboperation.Job, error) {
	stub.submits++
	stub.request = request
	stub.value = joboperation.Job{ID: "job_00000000000000000000000000000000", Owner: request.Owner, Model: request.Model, Status: joboperation.Queued, Snapshot: joboperation.Snapshot{Status: 201, Body: []byte(`{"id":"job_00000000000000000000000000000000","status":"starting"}`)}}
	return stub.value, nil
}
func (stub *jobsStub) Get(context.Context, joboperation.Owner, string) (joboperation.Job, error) {
	return stub.value, nil
}
func (stub *jobsStub) Cancel(context.Context, joboperation.Owner, string) (joboperation.Job, error) {
	stub.cancels++
	stub.value.Status = joboperation.Canceled
	stub.value.Snapshot.Body = []byte(`{"id":"job_00000000000000000000000000000000","status":"canceled"}`)
	return stub.value, nil
}
func testHandler(service *jobsStub) *Handler {
	return NewHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), authStub{principal: apikey.Principal{APIKeyID: "key", ProjectID: "project", OrganizationID: "org"}}, modelsStub{}, service, nil, nil, 1<<20, "https://gateway.example")
}
func TestCreateGetCancelNativeRoutes(t *testing.T) {
	service := &jobsStub{}
	handler := testHandler(service)
	request := httptest.NewRequest(http.MethodPost, "/v1/predictions", strings.NewReader(`{"version":"owner/model:version","input":{"prompt":"cat"}}`))
	request.Header.Set("Authorization", "Bearer service")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 201 || service.submits != 1 || service.request.EstimatedUsage == nil || service.request.EstimatedUsage.Quantity != 1 {
		t.Fatalf("create=%d body=%s", response.Code, response.Body.String())
	}
	for _, item := range []struct {
		method, path string
		want         int
	}{{http.MethodGet, "/v1/predictions/job_00000000000000000000000000000000", 200}, {http.MethodPost, "/v1/predictions/job_00000000000000000000000000000000/cancel", 200}} {
		request = httptest.NewRequest(item.method, item.path, nil)
		request.Header.Set("Authorization", "Bearer service")
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != item.want {
			t.Fatalf("%s=%d %s", item.path, response.Code, response.Body.String())
		}
	}
	if service.cancels != 1 {
		t.Fatalf("cancels=%d", service.cancels)
	}
}

func TestCreatePassesMaximumOutputUsageToJob(t *testing.T) {
	service := &jobsStub{}
	request := httptest.NewRequest(http.MethodPost, "/v1/predictions", strings.NewReader(`{"version":"owner/model:version","input":{"prompt":"cat","num_outputs":4}}`))
	request.Header.Set("Authorization", "Bearer service")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	testHandler(service).ServeHTTP(response, request)
	if response.Code != http.StatusCreated || service.request.EstimatedUsage == nil || service.request.EstimatedUsage.Quantity != 4 {
		t.Fatalf("status=%d usage=%+v body=%s", response.Code, service.request.EstimatedUsage, response.Body.String())
	}
}
func TestPluginRouteUsesDurableJobAndManagedResult(t *testing.T) {
	service := &jobsStub{}
	handler := NewHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), authStub{principal: apikey.Principal{APIKeyID: "key", ProjectID: "project", OrganizationID: "org"}}, pluginModelsStub{}, service, nil, nil, 1<<20, "https://gateway.example")
	handler.SetManagedResults(true)
	request := httptest.NewRequest(http.MethodPost, "/v1/predictions", strings.NewReader(`{"version":"example-async-image-v1","input":{"prompt":"cat"}}`))
	request.Header.Set("Authorization", "Bearer service")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || service.request.Provider != "plugin" || !service.request.ManagedResultRequired {
		t.Fatalf("status=%d request=%#v", response.Code, service.request)
	}
}
func TestValidationRejectsWebhookAndInvalidWait(t *testing.T) {
	for name, values := range map[string][2]string{"webhook": {`{"version":"x","input":{},"webhook":"https://evil.example"}`, ""}, "wait": {`{"version":"x","input":{}}`, "wait=61"}} {
		t.Run(name, func(t *testing.T) {
			service := &jobsStub{}
			request := httptest.NewRequest(http.MethodPost, "/v1/predictions", strings.NewReader(values[0]))
			request.Header.Set("Authorization", "Bearer service")
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Prefer", values[1])
			response := httptest.NewRecorder()
			testHandler(service).ServeHTTP(response, request)
			if response.Code < 400 || service.submits != 0 {
				t.Fatalf("response=%d submits=%d", response.Code, service.submits)
			}
		})
	}
}
func TestMinimalResponseUsesGatewayIdentity(t *testing.T) {
	service := &jobsStub{value: joboperation.Job{ID: "job_00000000000000000000000000000000", Model: "x", Status: joboperation.Processing}}
	request := httptest.NewRequest(http.MethodGet, "/v1/predictions/"+service.value.ID, nil)
	request.Header.Set("Authorization", "Bearer service")
	response := httptest.NewRecorder()
	testHandler(service).ServeHTTP(response, request)
	var body map[string]any
	if json.Unmarshal(response.Body.Bytes(), &body) != nil || body["id"] != service.value.ID || strings.Contains(response.Body.String(), "provider") {
		t.Fatalf("body=%s", response.Body.String())
	}
}
