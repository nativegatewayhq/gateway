package gemini

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	chargebilling "github.com/nativegatewayhq/gateway/internal/billing"
	"github.com/nativegatewayhq/gateway/internal/imagestorage"
)

type geminiResultManagerFake struct {
	inputs []imagestorage.TransformInput
	body   []byte
}

func (fake *geminiResultManagerFake) Transform(_ context.Context, input imagestorage.TransformInput) ([]byte, error) {
	fake.inputs = append(fake.inputs, input)
	return fake.body, nil
}
func (*geminiResultManagerFake) MaximumResponseBytes() int64 { return 1024 * 1024 }

func TestBillableGeminiStoresManagedBodyBeforeCapture(t *testing.T) {
	billing := &geminiBillingFake{beginCharge: chargebilling.Charge{ID: "charge_test"}}
	executor := &stubExecutor{response: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"candidates":[]}`))}}
	handler := billableGeminiHandler(billing, executor)
	results := &geminiResultManagerFake{body: []byte(`{"candidates":[{"content":{"parts":[{"fileData":{"fileUri":"https://cdn.example/image.png"}}]}}]}`)}
	handler.SetResultManager(results)
	request := geminiRequest(strings.NewReader(`{"contents":[]}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 || response.Body.String() != string(results.body) || !billing.completeOK || string(billing.snapshot.Body) != string(results.body) || len(results.inputs) != 1 || results.inputs[0].Protocol != "gemini" {
		t.Fatalf("response=%d %s billing=%+v inputs=%+v", response.Code, response.Body.String(), billing, results.inputs)
	}
}
