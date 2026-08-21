package openai

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nativegatewayhq/gateway/internal/providerhealth"
	audiooperation "github.com/nativegatewayhq/gateway/operations/audio"
	openaiProvider "github.com/nativegatewayhq/gateway/providers/openai"
)

type speechExecutorFunc func(context.Context, openaiProvider.SpeechRequest) (*http.Response, error)

func (function speechExecutorFunc) Create(ctx context.Context, request openaiProvider.SpeechRequest) (*http.Response, error) {
	return function(ctx, request)
}

func TestSpeechStreamsNativeBinaryAndSanitizesHeaders(t *testing.T) {
	registry, _ := audiooperation.NewRegistry([]string{"tts-1"})
	called := 0
	handler := NewSpeechHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), acceptingAuth(t), registry, speechExecutorFunc(func(_ context.Context, request openaiProvider.SpeechRequest) (*http.Response, error) {
		called++
		body, _ := io.ReadAll(request.Body)
		if request.ChannelID == "" || request.ContentType != "application/json" || !strings.Contains(string(body), `"input":"private words"`) {
			t.Fatalf("request=%+v body=%s", request, body)
		}
		return &http.Response{StatusCode: 200, ContentLength: 11, Header: http.Header{"Content-Type": {"audio/mpeg"}, "Content-Disposition": {`attachment; filename="speech.mp3"`}, "Set-Cookie": {"secret=1"}, "X-Request-Id": {"provider-secret"}}, Body: io.NopCloser(strings.NewReader("audio-bytes"))}, nil
	}), providerhealth.NoopGate{}, 1024, 1024)
	request := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", strings.NewReader(`{"model":"tts-1","input":"private words","voice":"alloy","response_format":"mp3","future":true}`))
	request.Header.Set("Authorization", "Bearer service-secret")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != 200 || recorder.Body.String() != "audio-bytes" || recorder.Header().Get("Content-Type") != "audio/mpeg" || recorder.Header().Get("Set-Cookie") != "" || recorder.Header().Get("X-Request-Id") != "" || called != 1 {
		t.Fatalf("status=%d headers=%v body=%q called=%d", recorder.Code, recorder.Header(), recorder.Body.String(), called)
	}
}

func TestSpeechRejectsInvalidInputBeforeProvider(t *testing.T) {
	registry, _ := audiooperation.NewRegistry([]string{"tts-1"})
	called := 0
	handler := NewSpeechHandler(slog.Default(), acceptingAuth(t), registry, speechExecutorFunc(func(context.Context, openaiProvider.SpeechRequest) (*http.Response, error) {
		called++
		return nil, nil
	}), providerhealth.NoopGate{}, 256, 32)
	for _, body := range []string{`{"model":"tts-1","model":"tts-1","input":"hello","voice":"alloy"}`, `{"model":"tts-1","input":"","voice":"alloy"}`, `{"model":"tts-1","input":"hello","voice":{"id":""}}`} {
		request := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer service-secret")
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	}
	if called != 0 {
		t.Fatalf("provider called=%d", called)
	}
}

func TestSpeechRejectsDeclaredOversizeAndInvalidMIMEBeforeCommit(t *testing.T) {
	registry, _ := audiooperation.NewRegistry([]string{"tts-1"})
	responses := []*http.Response{
		{StatusCode: 200, ContentLength: 33, Header: http.Header{"Content-Type": {"audio/mpeg"}}, Body: io.NopCloser(strings.NewReader(strings.Repeat("a", 33)))},
		{StatusCode: 200, ContentLength: 2, Header: http.Header{"Content-Type": {"text/html"}}, Body: io.NopCloser(strings.NewReader("no"))},
	}
	handler := NewSpeechHandler(slog.Default(), acceptingAuth(t), registry, speechExecutorFunc(func(context.Context, openaiProvider.SpeechRequest) (*http.Response, error) {
		response := responses[0]
		responses = responses[1:]
		return response, nil
	}), providerhealth.NoopGate{}, 256, 32)
	for range 2 {
		request := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", strings.NewReader(`{"model":"tts-1","input":"hello","voice":"alloy"}`))
		request.Header.Set("Authorization", "Bearer service-secret")
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusBadGateway {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	}
}

func TestRelaySpeechBoundsUnknownLengthAndWriteFailure(t *testing.T) {
	if err := relaySpeech(httptest.NewRecorder(), strings.NewReader(strings.Repeat("a", 33)), 32); err != errSpeechStreamTooLarge {
		t.Fatalf("oversize err=%v", err)
	}
	if err := relaySpeech(errorWriter{}, strings.NewReader("audio"), 32); err != errSpeechStreamWrite {
		t.Fatalf("write err=%v", err)
	}
}

func TestSpeechProviderPanicIsContainedWithoutRedispatch(t *testing.T) {
	registry, _ := audiooperation.NewRegistry([]string{"tts-1"})
	calls := 0
	handler := NewSpeechHandler(slog.Default(), acceptingAuth(t), registry, speechExecutorFunc(func(context.Context, openaiProvider.SpeechRequest) (*http.Response, error) {
		calls++
		panic("sensitive provider panic")
	}), providerhealth.NoopGate{}, 256, 256)
	request := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", strings.NewReader(`{"model":"tts-1","input":"hello","voice":"alloy"}`))
	request.Header.Set("Authorization", "Bearer service-secret")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadGateway || calls != 1 || strings.Contains(recorder.Body.String(), "sensitive") {
		t.Fatalf("status=%d calls=%d body=%s", recorder.Code, calls, recorder.Body.String())
	}
}

type errorWriter struct{}

func (errorWriter) Header() http.Header       { return http.Header{} }
func (errorWriter) WriteHeader(int)           {}
func (errorWriter) Write([]byte) (int, error) { return 0, context.Canceled }
