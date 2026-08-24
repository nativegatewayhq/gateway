package openai

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nativegatewayhq/gateway/internal/apikey"
	"github.com/nativegatewayhq/gateway/internal/audiobilling"
	"github.com/nativegatewayhq/gateway/internal/providerhealth"
	"github.com/nativegatewayhq/gateway/internal/speechstorage"
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
	if _, err := relaySpeech(httptest.NewRecorder(), strings.NewReader(strings.Repeat("a", 33)), 32); err != errSpeechStreamTooLarge {
		t.Fatalf("oversize err=%v", err)
	}
	if _, err := relaySpeech(errorWriter{}, strings.NewReader("audio"), 32); err != errSpeechStreamWrite {
		t.Fatalf("write err=%v", err)
	}
}

type speechBillingFake struct {
	begins, completes, releases, reconciles int
	charge                                  audiobilling.Charge
	beginErr                                error
}

func (f *speechBillingFake) Begin(context.Context, audiobilling.BeginRequest) (audiobilling.Charge, error) {
	f.begins++
	return f.charge, f.beginErr
}
func (f *speechBillingFake) Complete(_ context.Context, _ string, e audiobilling.StreamEvidence) (audiobilling.Charge, error) {
	f.completes++
	f.charge.ResponseBytes = e.Bytes
	f.charge.ResponseSHA256 = e.SHA256
	return f.charge, nil
}
func (f *speechBillingFake) Release(context.Context, string, string) (audiobilling.Charge, error) {
	f.releases++
	return f.charge, nil
}
func (f *speechBillingFake) MarkReconciling(context.Context, string, string) error {
	f.reconciles++
	return nil
}

func TestBillableSpeechRequiresIdempotencyAndCapturesOnlyCompleteStream(t *testing.T) {
	registry, _ := audiooperation.NewRegistry([]string{"tts-1"})
	billing := &speechBillingFake{charge: audiobilling.Charge{ID: "asc_00000000000000000000000000000000"}}
	handler := NewBillableSpeechHandler(slog.Default(), acceptingAuth(t), registry, speechExecutorFunc(func(context.Context, openaiProvider.SpeechRequest) (*http.Response, error) {
		return &http.Response{StatusCode: 200, ContentLength: 5, Header: http.Header{"Content-Type": {"audio/mpeg"}}, Body: io.NopCloser(strings.NewReader("audio"))}, nil
	}), providerhealth.NoopGate{}, 256, 256, billing)
	makeRequest := func(key string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", strings.NewReader(`{"model":"tts-1","input":"한😀","voice":"alloy"}`))
		r.Header.Set("Authorization", "Bearer service-secret")
		r.Header.Set("Content-Type", "application/json")
		if key != "" {
			r.Header.Set("Idempotency-Key", key)
		}
		return r
	}
	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, makeRequest(""))
	if missing.Code != http.StatusBadRequest || billing.begins != 0 {
		t.Fatalf("missing status=%d begins=%d", missing.Code, billing.begins)
	}
	ok := httptest.NewRecorder()
	handler.ServeHTTP(ok, makeRequest("speech-key"))
	if ok.Code != 200 || ok.Body.String() != "audio" || billing.begins != 1 || billing.completes != 1 || billing.reconciles != 0 || billing.charge.ResponseBytes != 5 || billing.charge.ResponseSHA256 == ([32]byte{}) {
		t.Fatalf("status=%d billing=%+v", ok.Code, billing)
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

type managedSpeechOutputFake struct {
	asset           speechstorage.Asset
	captured        int
	downstreamError error
}

func (f *managedSpeechOutputFake) Open(context.Context, apikey.Principal, string) (speechstorage.Asset, io.ReadCloser, error) {
	return f.asset, io.NopCloser(strings.NewReader("audio")), nil
}

func (f *managedSpeechOutputFake) Begin(context.Context, apikey.Principal, string, string, [32]byte) (speechstorage.Asset, error) {
	return f.asset, nil
}
func (f *managedSpeechOutputFake) Capture(_ context.Context, asset speechstorage.Asset, _ string, source io.Reader, downstream io.Writer, _ int64) (speechstorage.CaptureResult, error) {
	body, err := io.ReadAll(source)
	if err != nil {
		return speechstorage.CaptureResult{}, err
	}
	f.captured++
	_, writeErr := downstream.Write(body)
	if f.downstreamError != nil {
		writeErr = f.downstreamError
	}
	return speechstorage.CaptureResult{Asset: asset, Bytes: int64(len(body)), SHA256: [32]byte{1}, DownstreamErr: writeErr}, nil
}

func TestManagedSpeechKeepsNativeBodyAndReturnsOpaqueAssetHeader(t *testing.T) {
	registry, _ := audiooperation.NewRegistry([]string{"tts-1"})
	outputs := &managedSpeechOutputFake{asset: speechstorage.Asset{ID: "speechasset_00000000000000000000000000000001", State: speechstorage.Capturing, ExpiresAt: time.Now().Add(time.Hour)}}
	handler := NewSpeechHandler(slog.Default(), acceptingAuth(t), registry, speechExecutorFunc(func(context.Context, openaiProvider.SpeechRequest) (*http.Response, error) {
		return &http.Response{StatusCode: 200, ContentLength: 5, Header: http.Header{"Content-Type": {"audio/mpeg"}}, Body: io.NopCloser(strings.NewReader("audio"))}, nil
	}), providerhealth.NoopGate{}, 256, 256)
	handler.SetManagedOutputs(outputs)
	request := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", strings.NewReader(`{"model":"tts-1","input":"hello","voice":"alloy"}`))
	request.Header.Set("Authorization", "Bearer service-secret")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "managed-speech")
	request.Header.Set("X-Native-Gateway-Delivery", "managed")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != 200 || recorder.Body.String() != "audio" || recorder.Header().Get("X-Native-Gateway-Speech-Asset") != outputs.asset.ID || outputs.captured != 1 {
		t.Fatalf("status=%d headers=%v body=%q captured=%d", recorder.Code, recorder.Header(), recorder.Body.String(), outputs.captured)
	}
}

func TestSpeechRejectsInvalidDeliveryBeforeProvider(t *testing.T) {
	registry, _ := audiooperation.NewRegistry([]string{"tts-1"})
	calls := 0
	handler := NewSpeechHandler(slog.Default(), acceptingAuth(t), registry, speechExecutorFunc(func(context.Context, openaiProvider.SpeechRequest) (*http.Response, error) { calls++; return nil, nil }), providerhealth.NoopGate{}, 256, 256)
	request := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", strings.NewReader(`{"model":"tts-1","input":"hello","voice":"alloy"}`))
	request.Header.Set("Authorization", "Bearer service-secret")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Native-Gateway-Delivery", "public")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest || calls != 0 {
		t.Fatalf("status=%d calls=%d", recorder.Code, calls)
	}
}
