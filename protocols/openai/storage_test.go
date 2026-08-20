package openai

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	chargebilling "github.com/nativegatewayhq/gateway/internal/billing"
	"github.com/nativegatewayhq/gateway/internal/imagestorage"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	"github.com/nativegatewayhq/gateway/internal/requestid"
	"github.com/nativegatewayhq/gateway/providers/openaiimages"
)

type resultManagerFake struct {
	inputs []imagestorage.TransformInput
	body   []byte
	err    error
}

func (fake *resultManagerFake) Transform(_ context.Context, input imagestorage.TransformInput) ([]byte, error) {
	fake.inputs = append(fake.inputs, input)
	return fake.body, fake.err
}
func (*resultManagerFake) MaximumResponseBytes() int64 { return 1024 * 1024 }

func TestBillableImagesStoresManagedBodyBeforeCapture(t *testing.T) {
	billing := &billingFake{}
	results := &resultManagerFake{body: []byte(`{"data":[{"url":"https://cdn.example/image.png"}]}`)}
	handler := NewBillableImagesHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), billingAuth(), testRegistry(t), map[providercredentials.ProviderID]Executor{providercredentials.OpenAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"data":[{"b64_json":"native"}]}`))}, nil
	})}, 1024, billing)
	handler.SetResultManager(results)
	response := billableImageRequest(handler, `{"model":"gpt-image-1"}`)
	if response.Code != 200 || response.Body.String() != string(results.body) || !billing.completeOK || string(billing.snapshot.Body) != string(results.body) || len(results.inputs) != 1 || results.inputs[0].ChargeID == "" {
		t.Fatalf("response=%d %s billing=%+v inputs=%+v", response.Code, response.Body.String(), billing, results.inputs)
	}
}

func TestProviderSuccessStorageFailureReconcilesWithoutRelease(t *testing.T) {
	billing := &billingFake{}
	results := &resultManagerFake{err: errors.New("secret storage failure")}
	handler := NewBillableImagesHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), billingAuth(), testRegistry(t), map[providercredentials.ProviderID]Executor{providercredentials.OpenAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"data":[]}`))}, nil
	})}, 1024, billing)
	handler.SetResultManager(results)
	response := billableImageRequest(handler, `{"model":"gpt-image-1"}`)
	if response.Code != http.StatusServiceUnavailable || billing.completeOK || billing.observation.Outcome != chargebilling.KnownSuccess || billing.observation.Reason != chargebilling.StorageFailed || strings.Contains(response.Body.String(), "secret") || len(billing.events) == 0 || billing.events[len(billing.events)-1] != "reconciling" {
		t.Fatalf("response=%d %s complete=%v observation=%+v events=%v", response.Code, response.Body.String(), billing.completeOK, billing.observation, billing.events)
	}
}

func TestBillableJSONAndMultipartEditsStoreManagedBody(t *testing.T) {
	for _, test := range []struct {
		name, model, contentType string
		body                     []byte
		provider                 providercredentials.ProviderID
	}{
		{name: "json", model: "grok-imagine-image-quality", contentType: "application/json", body: []byte(`{"model":"grok-imagine-image-quality","image":{"url":"https://example.invalid/input.png"}}`), provider: providercredentials.XAI},
		func() struct {
			name, model, contentType string
			body                     []byte
			provider                 providercredentials.ProviderID
		} {
			body, contentType := multipartEdit(t, "gpt-image-1")
			return struct {
				name, model, contentType string
				body                     []byte
				provider                 providercredentials.ProviderID
			}{name: "multipart", model: "gpt-image-1", contentType: contentType, body: body, provider: providercredentials.OpenAI}
		}(),
	} {
		t.Run(test.name, func(t *testing.T) {
			billing := &billingFake{}
			results := &resultManagerFake{body: []byte(`{"data":[{"url":"https://cdn.example/edit.png"}]}`)}
			handler := NewBillableEditHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), billingAuth(), testRegistry(t), map[providercredentials.ProviderID]Executor{test.provider: executorFunc(func(_ context.Context, request openaiimages.Request) (*http.Response, error) {
				_, _ = io.Copy(io.Discard, request.Body)
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"data":[]}`))}, nil
			})}, int64(len(test.body)+1024), 1, billing)
			handler.SetResultManager(results)
			request := httptest.NewRequest(http.MethodPost, "/v1/images/edits", bytes.NewReader(test.body))
			request.Header.Set("Content-Type", test.contentType)
			request.Header.Set("Authorization", "Bearer service-secret")
			request.Header.Set(requestid.HeaderName, "managed-edit-"+test.name)
			response := httptest.NewRecorder()
			requestid.Middleware(handler).ServeHTTP(response, request)
			if response.Code != 200 || response.Body.String() != string(results.body) || !billing.completeOK || len(results.inputs) != 1 {
				t.Fatalf("response=%d %s billing=%+v inputs=%+v", response.Code, response.Body.String(), billing, results.inputs)
			}
		})
	}
}
