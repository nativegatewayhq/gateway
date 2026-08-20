package openai

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nativegatewayhq/gateway/internal/apikey"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	"github.com/nativegatewayhq/gateway/internal/ratelimit"
	imageoperation "github.com/nativegatewayhq/gateway/operations/image"
	"github.com/nativegatewayhq/gateway/providers/openaiimages"
)

func multipartEdit(t *testing.T, model string) ([]byte, string) {
	t.Helper()
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	if err := writer.WriteField("model", model); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("prompt", "secret edit prompt"); err != nil {
		t.Fatal(err)
	}
	part, err := writer.CreateFormFile("image", "secret-name.png")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("raw-image-bytes"))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes(), writer.FormDataContentType()
}

func TestEditHandlerRateLimitStopsBeforeMultipartSpool(t *testing.T) {
	handler := NewEditHandler(slog.Default(), authFunc(func(context.Context, string) (apikey.Principal, error) {
		return apikey.Principal{}, &ratelimit.LimitError{Decision: ratelimit.Decision{Limit: 10, RetryAfter: time.Second, ResetAt: time.Unix(2_000_000_000, 0)}}
	}), testRegistry(t), nil, 1024, 1)
	request := httptest.NewRequest(http.MethodPost, "/v1/images/edits", panicReader{})
	request.Header.Set("Authorization", "Bearer service-secret")
	request.Header.Set("Content-Type", "multipart/form-data; boundary=secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 429 || response.Header().Get("X-RateLimit-Limit") != "10" {
		t.Fatalf("response=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
}

func TestEditHandlerEnforcesJSONAndMultipartModelPermissions(t *testing.T) {
	principal := apikey.Principal{APIKeyID: "key_denied", ModelAccessMode: apikey.ModelAccessAllowlist, ModelPermissions: []apikey.ModelPermission{{Protocol: "openai", Operation: "image.generate", Model: "gpt-image-1"}}}
	auth := authFunc(func(context.Context, string) (apikey.Principal, error) { return principal, nil })
	for _, test := range []struct {
		name, contentType string
		body              []byte
	}{
		{"json", "application/json", []byte(`{"model":"grok-imagine-image-quality","image":"https://example.invalid/image.png"}`)},
		func() struct {
			name, contentType string
			body              []byte
		} {
			body, contentType := multipartEdit(t, "gpt-image-1")
			return struct {
				name, contentType string
				body              []byte
			}{"multipart", contentType, body}
		}(),
	} {
		t.Run(test.name, func(t *testing.T) {
			providerCalls := 0
			handler := NewEditHandler(slog.Default(), auth, testRegistry(t), map[providercredentials.ProviderID]Executor{
				providercredentials.OpenAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) { providerCalls++; return nil, nil }),
				providercredentials.XAI:    executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) { providerCalls++; return nil, nil }),
			}, 1024*1024, 1)
			request := httptest.NewRequest(http.MethodPost, "/v1/images/edits", bytes.NewReader(test.body))
			request.Header.Set("Authorization", "Bearer service-secret")
			request.Header.Set("Content-Type", test.contentType)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != 403 || providerCalls != 0 {
				t.Fatalf("response=%d %s calls=%d", response.Code, response.Body.String(), providerCalls)
			}
		})
	}
}

func TestEditHandlerPreservesOpenAIMultipartAndCleansSpool(t *testing.T) {
	body, contentType := multipartEdit(t, "gpt-image-1")
	tempDir := t.TempDir()
	calls := 0
	handler := NewEditHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), acceptingAuth(t), testRegistry(t), map[providercredentials.ProviderID]Executor{
		providercredentials.OpenAI: executorFunc(func(_ context.Context, request openaiimages.Request) (*http.Response, error) {
			calls++
			if request.Operation != openaiimages.Edit || request.ContentType != contentType || request.ContentLength != int64(len(body)) {
				t.Fatalf("request metadata = %+v", request)
			}
			got, _ := io.ReadAll(request.Body)
			if !bytes.Equal(got, body) {
				t.Fatal("multipart body changed")
			}
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"data":[{"b64_json":"aW1hZ2U="}]}`))}, nil
		}),
	}, int64(len(body)+1), 1)
	handler.tempDir = tempDir
	request := httptest.NewRequest(http.MethodPost, "/v1/images/edits", bytes.NewReader(body))
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("Authorization", "Bearer service-secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 || calls != 1 {
		t.Fatalf("status/calls = %d/%d: %s", response.Code, calls, response.Body.String())
	}
	files, _ := filepath.Glob(filepath.Join(tempDir, "gateway-image-edit-*"))
	if len(files) != 0 {
		t.Fatalf("spool files remain: %v", files)
	}
}

func TestEditHandlerRewritesOnlyMultipartProviderModel(t *testing.T) {
	body, contentType := multipartEdit(t, "logical-edit")
	registry, err := imageoperation.NewRegistry(imageoperation.ModelRoute{Protocol: "openai", Model: "logical-edit", Owner: "gateway", Capabilities: []imageoperation.Capability{{Operation: imageoperation.Edit, MediaType: imageoperation.Multipart}}, Policy: imageoperation.Fixed, FixedCandidateID: "candidate_edit", Candidates: []imageoperation.ChannelCandidate{{ID: "candidate_edit", Provider: providercredentials.OpenAI, ProviderModel: "provider-edit", ChannelID: "channel_00000000000000000000000000000001", Enabled: true}}})
	if err != nil {
		t.Fatal(err)
	}
	called := false
	handler := NewEditHandler(slog.Default(), acceptingAuth(t), registry, map[providercredentials.ProviderID]Executor{providercredentials.OpenAI: executorFunc(func(_ context.Context, request openaiimages.Request) (*http.Response, error) {
		called = true
		_, parameters, _ := mime.ParseMediaType(request.ContentType)
		selector, err := imageoperation.ParseOpenAIMultipartPricingSelector(request.Body, parameters["boundary"])
		if err != nil || selector.Model != "provider-edit" {
			t.Fatalf("selector=%+v error=%v", selector, err)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})}, 4096, 1)
	request := httptest.NewRequest(http.MethodPost, "/v1/images/edits", bytes.NewReader(body))
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("Authorization", "Bearer service-secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 || !called {
		t.Fatalf("response=%d called=%v body=%s", response.Code, called, response.Body.String())
	}
}

func TestEditHandlerPreservesXAIJSON(t *testing.T) {
	body := `{"prompt":"secret","model":"grok-imagine-image-quality","image":{"url":"https://example.invalid/image"}}`
	called := false
	handler := NewEditHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), acceptingAuth(t), testRegistry(t), map[providercredentials.ProviderID]Executor{
		providercredentials.XAI: executorFunc(func(_ context.Context, request openaiimages.Request) (*http.Response, error) {
			called = true
			got, _ := io.ReadAll(request.Body)
			if string(got) != body || request.Operation != openaiimages.Edit {
				t.Fatalf("JSON changed: %q", got)
			}
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"usage":{"cost_in_usd_ticks":1}}`))}, nil
		}),
	}, 1024, 1)
	response := editRequest(handler, body, "application/json")
	if response.Code != 200 || !called || !strings.Contains(response.Body.String(), "cost_in_usd_ticks") {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
}

func TestEditHandlerRejectsWireMismatchLimitsAndCapacity(t *testing.T) {
	openAIBody, multipartType := multipartEdit(t, "gpt-image-1")
	handler := NewEditHandler(slog.Default(), acceptingAuth(t), testRegistry(t), nil, int64(len(openAIBody)-1), 1)
	if response := editRequest(handler, string(openAIBody), multipartType); response.Code != 413 {
		t.Fatalf("multipart limit = %d", response.Code)
	}
	handler = NewEditHandler(slog.Default(), acceptingAuth(t), testRegistry(t), nil, 1024, 1)
	if response := editRequest(handler, `{"model":"gpt-image-1"}`, "application/json"); response.Code != 400 {
		t.Fatalf("JSON/OpenAI mismatch = %d", response.Code)
	}
	xAIBody, xAIType := multipartEdit(t, "grok-imagine-image-quality")
	if response := editRequest(handler, string(xAIBody), xAIType); response.Code != 400 {
		t.Fatalf("multipart/xAI mismatch = %d", response.Code)
	}
	handler.spoolSlots <- struct{}{}
	if response := editRequest(handler, string(openAIBody), multipartType); response.Code != 503 {
		t.Fatalf("capacity = %d", response.Code)
	}
	<-handler.spoolSlots
}

func TestEditHandlerSpoolFileModeAndCleanupOnProviderFailure(t *testing.T) {
	body, contentType := multipartEdit(t, "gpt-image-1")
	tempDir := t.TempDir()
	handler := NewEditHandler(slog.Default(), acceptingAuth(t), testRegistry(t), map[providercredentials.ProviderID]Executor{
		providercredentials.OpenAI: executorFunc(func(_ context.Context, request openaiimages.Request) (*http.Response, error) {
			files, _ := filepath.Glob(filepath.Join(tempDir, "gateway-image-edit-*"))
			if len(files) != 1 {
				t.Fatalf("spool files = %v", files)
			}
			info, err := os.Stat(files[0])
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatalf("spool mode = %v, %v", info.Mode().Perm(), err)
			}
			return nil, openaiimages.ErrUpstream
		}),
	}, int64(len(body)+1), 1)
	handler.tempDir = tempDir
	response := editRequest(handler, string(body), contentType)
	if response.Code != 502 {
		t.Fatalf("status = %d", response.Code)
	}
	files, _ := filepath.Glob(filepath.Join(tempDir, "gateway-image-edit-*"))
	if len(files) != 0 {
		t.Fatalf("spool files remain: %v", files)
	}
}

func editRequest(handler http.Handler, body, contentType string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/v1/images/edits", strings.NewReader(body))
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("Authorization", "Bearer service-secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
