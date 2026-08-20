package gemini

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nativegatewayhq/gateway/internal/apikey"
	"github.com/nativegatewayhq/gateway/internal/chatpricing"
	geminioperation "github.com/nativegatewayhq/gateway/operations/gemini"
)

func geminiCompletedStream() string {
	return "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"gate\"}]}}],\"usageMetadata\":{\"promptTokenCount\":4,\"candidatesTokenCount\":1,\"totalTokenCount\":5}}\n\n" +
		"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"way\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":4,\"cachedContentTokenCount\":1,\"toolUsePromptTokenCount\":2,\"candidatesTokenCount\":2,\"thoughtsTokenCount\":1,\"totalTokenCount\":7}}\n\n"
}

func TestGeminiStreamPreservesWireAndObservesCumulativeUsage(t *testing.T) {
	input := geminiCompletedStream()
	writer := httptest.NewRecorder()
	result, err := relayGeminiStream(writer, strings.NewReader(input), 8192, true)
	if err != nil || writer.Body.String() != input || !result.Terminal || !result.UsageFound || result.TerminalDigest == ([32]byte{}) {
		t.Fatalf("result=%+v err=%v wire=%q", result, err, writer.Body.String())
	}
	want := chatpricing.Usage{PromptTokens: 6, CachedInputTokens: 1, CompletionTokens: 3, ToolUsePromptTokens: 2, ThoughtsTokens: 1}
	if result.Usage != want {
		t.Fatalf("usage=%+v want=%+v", result.Usage, want)
	}
}

func TestGeminiStreamAcceptsCRLFCommentsMultilineAndUnknownFields(t *testing.T) {
	input := ": keep-alive\r\n\r\n" +
		"data: {\"future\":true,\r\n" +
		"data: \"candidates\":[{\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":1,\"candidatesTokenCount\":1,\"totalTokenCount\":2}}\r\n\r\n"
	writer := httptest.NewRecorder()
	result, err := relayGeminiStream(writer, strings.NewReader(input), 4096, true)
	if err != nil || writer.Body.String() != input || !result.Terminal || result.Usage.PromptTokens != 1 {
		t.Fatalf("result=%+v err=%v wire=%q", result, err, writer.Body.String())
	}
}

func TestGeminiStreamRejectsRegressionDuplicateAndTruncatedEvent(t *testing.T) {
	regression := "data: {\"usageMetadata\":{\"promptTokenCount\":4,\"candidatesTokenCount\":2,\"totalTokenCount\":6}}\n\n" +
		"data: {\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":2,\"totalTokenCount\":5}}\n\n"
	duplicate := "data: {\"usageMetadata\":{\"promptTokenCount\":1,\"promptTokenCount\":1,\"candidatesTokenCount\":1,\"totalTokenCount\":2}}\n\n"
	truncated := "data: {\"candidates\":[]}"
	for name, stream := range map[string]string{"regression": regression, "duplicate": duplicate, "truncated": truncated} {
		t.Run(name, func(t *testing.T) {
			if _, err := relayGeminiStream(httptest.NewRecorder(), strings.NewReader(stream), 4096, true); !errors.Is(err, errGeminiStreamProtocol) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

type geminiFailingWriter struct{ header http.Header }

func (writer *geminiFailingWriter) Header() http.Header       { return writer.header }
func (writer *geminiFailingWriter) WriteHeader(int)           {}
func (writer *geminiFailingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func (writer *geminiFailingWriter) Flush()                    {}

type geminiNonFlushingWriter struct{ header http.Header }

func (writer *geminiNonFlushingWriter) Header() http.Header            { return writer.header }
func (writer *geminiNonFlushingWriter) WriteHeader(int)                {}
func (writer *geminiNonFlushingWriter) Write(body []byte) (int, error) { return len(body), nil }

func TestManagedGeminiStreamCapturesUsageAndPreservesNativeSSE(t *testing.T) {
	models, _ := geminioperation.NewRegistryWithLimits([]string{"gemini-2.5-pro"}, map[string]geminioperation.Limits{"gemini-2.5-pro": {MaximumInputTokens: 4096, MaximumOutputTokens: 100}})
	stream := geminiCompletedStream()
	executor := &stubExecutor{response: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}, "Set-Cookie": {"secret=1"}}, Body: io.NopCloser(strings.NewReader(stream))}}
	tokenBilling := &geminiLLMBillingFake{}
	principal := apikey.Principal{OrganizationID: "org_test", ProjectID: "project_test", APIKeyID: "key_test"}
	handler := NewBillableHandlerWithLLMTokenBilling(testLogger(io.Discard), &stubAuthenticator{principal: principal}, nil, executor, 8192, &geminiBillingFake{}, tokenBilling, nil, nil, models)
	request := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:streamGenerateContent?alt=sse&key=service-key", strings.NewReader(`{"contents":[{"parts":[{"text":"secret"}]}],"generationConfig":{"maxOutputTokens":20}}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "gemini-stream-key")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 || response.Body.String() != stream || !tokenBilling.streamComplete || tokenBilling.streamReconcile || tokenBilling.beginRequest.DeliveryMode != "stream" {
		t.Fatalf("status=%d body=%q billing=%+v", response.Code, response.Body.String(), tokenBilling)
	}
	if executor.request.Action != "streamGenerateContent" || !executor.request.Streaming || response.Header().Get("Set-Cookie") != "" {
		t.Fatalf("request=%+v headers=%v", executor.request, response.Header())
	}
}

func TestBYOKGeminiStreamRelaysNativeSSEWithoutBillingObservation(t *testing.T) {
	models, _ := geminioperation.NewRegistry([]string{"gemini-2.5-pro"})
	stream := "data: {\"future\":true,\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]}}]}\n\n"
	executor := &stubExecutor{response: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}}
	handler := NewHandlerWithLLMModels(testLogger(io.Discard), &stubAuthenticator{}, executor, 8192, nil, models)
	request := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:streamGenerateContent?alt=sse&key=service-key", strings.NewReader(`{"contents":[{"parts":[{"text":"secret"}]}]}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 || response.Body.String() != stream || executor.request.Action != "streamGenerateContent" || !executor.request.Streaming {
		t.Fatalf("status=%d body=%q request=%+v", response.Code, response.Body.String(), executor.request)
	}
}

func TestManagedGeminiStreamMissingUsageAndClientWriteKeepReservation(t *testing.T) {
	models, _ := geminioperation.NewRegistryWithLimits([]string{"gemini-2.5-pro"}, map[string]geminioperation.Limits{"gemini-2.5-pro": {MaximumInputTokens: 4096, MaximumOutputTokens: 100}})
	for name, stream := range map[string]string{"missing_usage": "data: {\"candidates\":[{\"finishReason\":\"STOP\"}]}\n\n", "write": geminiCompletedStream()} {
		t.Run(name, func(t *testing.T) {
			executor := &stubExecutor{response: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}}
			billing := &geminiLLMBillingFake{}
			handler := NewBillableHandlerWithLLMTokenBilling(testLogger(io.Discard), &stubAuthenticator{principal: apikey.Principal{OrganizationID: "org_test", ProjectID: "project_test", APIKeyID: "key_test"}}, nil, executor, 8192, &geminiBillingFake{}, billing, nil, nil, models)
			request := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:streamGenerateContent?alt=sse&key=service-key", strings.NewReader(`{"contents":[],"generationConfig":{"maxOutputTokens":20}}`))
			request.Header.Set("Content-Type", "application/json")
			if name == "write" {
				handler.ServeHTTP(&geminiFailingWriter{header: http.Header{}}, request)
			} else {
				handler.ServeHTTP(httptest.NewRecorder(), request)
			}
			if !billing.streamReconcile || billing.streamComplete {
				t.Fatalf("billing=%+v", billing)
			}
		})
	}
}

func TestManagedGeminiStreamConfirmedNon2xxReleasesAndPreflightFailuresDoNotDispatch(t *testing.T) {
	models, _ := geminioperation.NewRegistryWithLimits([]string{"gemini-2.5-pro"}, map[string]geminioperation.Limits{"gemini-2.5-pro": {MaximumInputTokens: 4096, MaximumOutputTokens: 100}})
	principal := apikey.Principal{OrganizationID: "org_test", ProjectID: "project_test", APIKeyID: "key_test"}
	requestBody := `{"contents":[],"generationConfig":{"maxOutputTokens":20}}`
	t.Run("non-2xx", func(t *testing.T) {
		executor := &stubExecutor{response: &http.Response{StatusCode: 429, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":429}}`))}}
		billing := &geminiLLMBillingFake{}
		handler := NewBillableHandlerWithLLMTokenBilling(testLogger(io.Discard), &stubAuthenticator{principal: principal}, nil, executor, 8192, &geminiBillingFake{}, billing, nil, nil, models)
		request := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:streamGenerateContent?alt=sse&key=service-key", strings.NewReader(requestBody))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != 429 || !billing.released || billing.streamReconcile {
			t.Fatalf("status=%d billing=%+v", response.Code, billing)
		}
	})
	for name, target := range map[string]string{"missing_alt": "/v1beta/models/gemini-2.5-pro:streamGenerateContent?key=service-key", "wrong_alt": "/v1beta/models/gemini-2.5-pro:streamGenerateContent?alt=json&key=service-key"} {
		t.Run(name, func(t *testing.T) {
			executor := &stubExecutor{}
			handler := NewBillableHandlerWithLLMTokenBilling(testLogger(io.Discard), &stubAuthenticator{principal: principal}, nil, executor, 8192, &geminiBillingFake{}, &geminiLLMBillingFake{}, nil, nil, models)
			request := httptest.NewRequest(http.MethodPost, target, strings.NewReader(requestBody))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != 400 || executor.calls != 0 {
				t.Fatalf("status=%d calls=%d", response.Code, executor.calls)
			}
		})
	}
	executor := &stubExecutor{}
	handler := NewBillableHandlerWithLLMTokenBilling(testLogger(io.Discard), &stubAuthenticator{principal: principal}, nil, executor, 8192, &geminiBillingFake{}, &geminiLLMBillingFake{}, nil, nil, models)
	request := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:streamGenerateContent?alt=sse&key=service-key", strings.NewReader(requestBody))
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(&geminiNonFlushingWriter{header: http.Header{}}, request)
	if executor.calls != 0 {
		t.Fatalf("non-flushing writer dispatched %d calls", executor.calls)
	}
}
