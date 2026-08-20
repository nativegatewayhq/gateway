package runway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nativegatewayhq/gateway/internal/apikey"
	"github.com/nativegatewayhq/gateway/internal/billing"
	"github.com/nativegatewayhq/gateway/internal/jobs"
	joboperation "github.com/nativegatewayhq/gateway/operations/job"
	videooperation "github.com/nativegatewayhq/gateway/operations/video"
	providerrunway "github.com/nativegatewayhq/gateway/providers/runway"
)

type authStub struct{ principal apikey.Principal }

func (a authStub) Authenticate(context.Context, string) (apikey.Principal, error) {
	return a.principal, nil
}

type jobsStub struct {
	submitted int
	payload   any
	request   jobs.CreateRequest
	job       joboperation.Job
}

func (j *jobsStub) Submit(_ context.Context, request jobs.CreateRequest, p any) (joboperation.Job, error) {
	j.submitted++
	j.payload = p
	j.request = request
	if j.job.Status.Terminal() {
		j.job.Status = joboperation.Processing
		j.job.Snapshot = joboperation.Snapshot{}
	}
	return j.job, nil
}

type billingStub struct {
	request billing.BeginRequest
	charge  billing.Charge
}

type uploaderStub struct {
	response providerrunway.UploadResponse
	body     []byte
}

func (stub *uploaderStub) CreateEphemeralUpload(_ context.Context, _ string, body []byte) (providerrunway.UploadResponse, error) {
	stub.body = append([]byte(nil), body...)
	return stub.response, nil
}

type assetsStub struct {
	bound, authorized string
	deny              bool
}

func (stub *assetsStub) Bind(_ context.Context, _ joboperation.Owner, _ string, uri string, _ time.Time) error {
	stub.bound = uri
	return nil
}
func (stub *assetsStub) Authorize(_ context.Context, _ joboperation.Owner, _ string, uri string) error {
	stub.authorized = uri
	if stub.deny {
		return errors.New("denied")
	}
	return nil
}

func TestNativeUploadBootstrapBindsURIWithoutReceivingMedia(t *testing.T) {
	registry, _ := videooperation.NewRegistry([]string{"model"})
	principal := apikey.Principal{OrganizationID: "org", ProjectID: "project", APIKeyID: "key", ModelAccessMode: apikey.ModelAccessAll}
	uploader := &uploaderStub{response: providerrunway.UploadResponse{Status: http.StatusOK, Body: []byte(`{"uploadUrl":"https://storage.example","fields":{"key":"secret-form"},"runwayUri":"runway://uploads/asset"}`), URI: "runway://uploads/asset"}}
	assets := &assetsStub{}
	handler := NewHandler(slog.Default(), authStub{principal}, registry, &jobsStub{}, 1<<20)
	handler.SetUploads(uploader, assets)
	request := httptest.NewRequest(http.MethodPost, "/v1/uploads", strings.NewReader(`{"filename":"input.mp4","type":"ephemeral"}`))
	request.Header.Set("Authorization", "Bearer key")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Runway-Version", "2024-11-06")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || assets.bound != "runway://uploads/asset" || !strings.Contains(recorder.Body.String(), "secret-form") {
		t.Fatalf("status=%d body=%s bound=%s", recorder.Code, recorder.Body.String(), assets.bound)
	}
	for _, invalid := range []string{`{"filename":"../input.mp4","type":"ephemeral"}`, `{"filename":"input.exe","type":"ephemeral"}`, `{"filename":"input.mp4","filename":"other.mp4","type":"ephemeral"}`} {
		request = httptest.NewRequest(http.MethodPost, "/v1/uploads", strings.NewReader(invalid))
		request.Header.Set("Authorization", "Bearer key")
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Runway-Version", "2024-11-06")
		recorder = httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("invalid status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	}
}

func TestRunwayURIRequiresAssetAuthorizationBeforeBillingAndDispatch(t *testing.T) {
	registry, _ := videooperation.NewRegistry([]string{"model"})
	principal := apikey.Principal{OrganizationID: "org", ProjectID: "project", APIKeyID: "key", ModelAccessMode: apikey.ModelAccessAll}
	service := &jobsStub{job: joboperation.Job{ID: "job_0123456789abcdef0123456789abcdef", Status: joboperation.Queued}}
	assets := &assetsStub{deny: true}
	handler := NewHandler(slog.Default(), authStub{principal}, registry, service, 1<<20)
	handler.SetUploads(&uploaderStub{}, assets)
	request := httptest.NewRequest(http.MethodPost, "/v1/image_to_video", strings.NewReader(`{"model":"model","promptImage":"runway://uploads/private"}`))
	request.Header.Set("Authorization", "Bearer key")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Runway-Version", "2024-11-06")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden || service.submitted != 0 || assets.authorized != "runway://uploads/private" {
		t.Fatalf("status=%d submitted=%d uri=%s", recorder.Code, service.submitted, assets.authorized)
	}
}

func (stub *billingStub) Begin(_ context.Context, request billing.BeginRequest) (billing.Charge, error) {
	stub.request = request
	return stub.charge, nil
}

func TestManagedSubmitBindsPriceDimensionsChargeAndUsage(t *testing.T) {
	registry, _ := videooperation.NewRegistryWithCapabilities([]string{"logical-video"}, map[string]videooperation.ModelCapability{"logical-video": {ProviderModel: "gen4_turbo", TextToVideo: true}})
	principal := apikey.Principal{OrganizationID: "org", ProjectID: "project", APIKeyID: "key", ModelAccessMode: apikey.ModelAccessAll}
	service := &jobsStub{job: joboperation.Job{ID: "job_0123456789abcdef0123456789abcdef", Protocol: "runway", Status: joboperation.Queued}}
	biller := &billingStub{charge: billing.Charge{ID: "charge_0123456789abcdef0123456789abcdef", Quantity: 25_000_000}}
	handler := NewHandler(slog.Default(), authStub{principal}, registry, service, 1<<20)
	handler.SetBilling(biller)
	request := httptest.NewRequest(http.MethodPost, "/v1/text_to_video", strings.NewReader(`{"model":"logical-video","duration":5,"ratio":"1280:720","audio":false,"promptText":"private"}`))
	request.Header.Set("Authorization", "Bearer key")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Runway-Version", "2024-11-06")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || service.submitted != 1 {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if biller.request.Quantity != 5 || biller.request.Size != "text_to_video" || biller.request.Quality != "ratio=1280:720;audio=false" {
		t.Fatalf("billing request=%+v", biller.request)
	}
	if service.request.ChargeID != biller.charge.ID || service.request.EstimatedUsage == nil || service.request.EstimatedUsage.Quantity != 25_000_000 {
		t.Fatalf("job request=%+v", service.request)
	}
}
func (j *jobsStub) Get(context.Context, joboperation.Owner, string) (joboperation.Job, error) {
	return j.job, nil
}
func (j *jobsStub) Cancel(context.Context, joboperation.Owner, string) (joboperation.Job, error) {
	j.job.Status = joboperation.Canceled
	return j.job, nil
}

func TestNativeSubmitValidatesVersionPreservesWireAndReturnsGatewayID(t *testing.T) {
	registry, _ := videooperation.NewRegistryWithCapabilities([]string{"logical-video"}, map[string]videooperation.ModelCapability{"logical-video": {ProviderModel: "gen4_turbo", TextToVideo: true}})
	principal := apikey.Principal{OrganizationID: "org", ProjectID: "project", APIKeyID: "key", ModelAccessMode: apikey.ModelAccessAll}
	service := &jobsStub{job: joboperation.Job{ID: "job_0123456789abcdef0123456789abcdef", Protocol: "runway", Model: "logical-video", Status: joboperation.Queued}}
	handler := NewHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), authStub{principal}, registry, service, 1<<20)
	body := `{ "unknown" : {"model":"nested"}, "model" : "logical-video", "promptText":"hello" }`
	request := httptest.NewRequest(http.MethodPost, "/v1/text_to_video", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer service-key")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Runway-Version", "2024-11-06")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || service.submitted != 1 {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	payload := service.payload.(providerrunway.SubmitPayload)
	want := `{ "unknown" : {"model":"nested"}, "model" : "gen4_turbo", "promptText":"hello" }`
	if string(payload.Body) != want {
		t.Fatalf("wire changed: %s", payload.Body)
	}
	var response map[string]string
	_ = json.Unmarshal(recorder.Body.Bytes(), &response)
	if response["id"] != service.job.ID {
		t.Fatalf("response=%v", response)
	}
}

func TestNativeBoundaryRejectsVersionBillingAndUnsafeImage(t *testing.T) {
	registry, _ := videooperation.NewRegistry([]string{"model"})
	principal := apikey.Principal{ModelAccessMode: apikey.ModelAccessAll}
	service := &jobsStub{}
	handler := NewHandler(slog.Default(), authStub{principal}, registry, service, 1<<20)
	request := httptest.NewRequest(http.MethodPost, "/v1/image_to_video", strings.NewReader(`{"model":"model","promptImage":"https://127.0.0.1/a.png"}`))
	request.Header.Set("Authorization", "Bearer key")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest || service.submitted != 0 {
		t.Fatalf("version boundary status=%d", recorder.Code)
	}
	request = httptest.NewRequest(http.MethodPost, "/v1/image_to_video", strings.NewReader(`{"model":"model","promptImage":"https://127.0.0.1/a.png"}`))
	request.Header.Set("Authorization", "Bearer key")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Runway-Version", "2024-11-06")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest || service.submitted != 0 {
		t.Fatalf("asset boundary status=%d", recorder.Code)
	}
	handler.SetBillingRequired(true)
	request = httptest.NewRequest(http.MethodPost, "/v1/text_to_video", strings.NewReader(`{"model":"model"}`))
	request.Header.Set("Authorization", "Bearer key")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Runway-Version", "2024-11-06")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable || service.submitted != 0 {
		t.Fatalf("billing boundary status=%d", recorder.Code)
	}
}

func TestRetrieveProjectsNativeStatusAndHidesOtherProtocols(t *testing.T) {
	registry, _ := videooperation.NewRegistry([]string{"model"})
	principal := apikey.Principal{ModelAccessMode: apikey.ModelAccessAll}
	service := &jobsStub{job: joboperation.Job{ID: "job_0123456789abcdef0123456789abcdef", Protocol: "runway", Model: "model", Status: joboperation.Processing, CreatedAt: time.Now()}}
	handler := NewHandler(slog.Default(), authStub{principal}, registry, service, 1<<20)
	request := httptest.NewRequest(http.MethodGet, "/v1/tasks/"+service.job.ID, nil)
	request.Header.Set("Authorization", "Bearer key")
	request.Header.Set("X-Runway-Version", "2024-11-06")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"RUNNING"`) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	service.job.Protocol = "replicate"
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("cross protocol status=%d", recorder.Code)
	}
}
