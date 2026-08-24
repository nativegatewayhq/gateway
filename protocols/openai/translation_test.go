package openai

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nativegatewayhq/gateway/internal/providerhealth"
	audiooperation "github.com/nativegatewayhq/gateway/operations/audio"
	openaiProvider "github.com/nativegatewayhq/gateway/providers/openai"
)

type translationExecutorFunc func(context.Context, openaiProvider.TranslationRequest) (*http.Response, error)

func (function translationExecutorFunc) Create(ctx context.Context, request openaiProvider.TranslationRequest) (*http.Response, error) {
	return function(ctx, request)
}

func translationRegistry(t *testing.T) *audiooperation.TranslationRegistry {
	t.Helper()
	registry, err := audiooperation.NewTranslationRegistry([]string{"translation-public"}, map[string]string{"translation-public": "whisper-1"}, map[string]audiooperation.TranslationCapabilities{"translation-public": {ResponseFormats: []string{"json", "verbose_json", "text", "srt", "vtt"}, Prompt: true, Temperature: true}})
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func translationRequest(t *testing.T, fields []struct{ name, filename, value string }) *http.Request {
	t.Helper()
	contentType, body := transcriptionBody(t, fields)
	request := httptest.NewRequest(http.MethodPost, "/v1/audio/translations", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("Authorization", "Bearer service-secret")
	return request
}

func TestTranslationPreservesNativeMultipartAndResponse(t *testing.T) {
	calls := 0
	handler := NewTranslationHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), acceptingAuth(t), translationRegistry(t), translationExecutorFunc(func(_ context.Context, request openaiProvider.TranslationRequest) (*http.Response, error) {
		calls++
		mediaType, parameters, err := mime.ParseMediaType(request.ContentType)
		if err != nil || mediaType != "multipart/form-data" {
			t.Fatalf("content-type=%s err=%v", request.ContentType, err)
		}
		reader := multipart.NewReader(request.Body, parameters["boundary"])
		values := map[string]string{}
		for {
			part, partErr := reader.NextPart()
			if errors.Is(partErr, io.EOF) {
				break
			}
			if partErr != nil {
				t.Fatal(partErr)
			}
			body, _ := io.ReadAll(part)
			values[part.FormName()] = string(body)
		}
		if values["model"] != "whisper-1" || values["prompt"] != "English style" || values["temperature"] != "0.25" || values["file"] != "audio-bytes" {
			t.Fatalf("values=%v", values)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}, "X-Request-Id": {"provider-private"}}, Body: io.NopCloser(strings.NewReader(`{"text":"native english"}`))}, nil
	}), providerhealth.NoopGate{}, 1<<20, 1<<19, 1024, 1<<20, 2)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, translationRequest(t, []struct{ name, filename, value string }{{"model", "", "translation-public"}, {"prompt", "", "English style"}, {"temperature", "", "0.25"}, {"response_format", "", "json"}, {"file", "voice.wav", "audio-bytes"}}))
	if response.Code != 200 || response.Body.String() != `{"text":"native english"}` || response.Header().Get("X-Request-Id") != "" || calls != 1 {
		t.Fatalf("status=%d headers=%v body=%s calls=%d", response.Code, response.Header(), response.Body.String(), calls)
	}
}

func TestTranslationPreservesEveryNativeResponseFormat(t *testing.T) {
	for _, testCase := range []struct{ format, contentType, body string }{{"json", "application/json", `{"text":"english"}`}, {"verbose_json", "application/json", `{"language":"english","duration":1.25,"text":"english","segments":[]}`}, {"text", "text/plain", "english"}, {"srt", "application/x-subrip", "1\n00:00:00,000 --> 00:00:01,000\nenglish\n"}, {"vtt", "text/vtt", "WEBVTT\n\n00:00.000 --> 00:01.000\nenglish\n"}} {
		t.Run(testCase.format, func(t *testing.T) {
			handler := NewTranslationHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), acceptingAuth(t), translationRegistry(t), translationExecutorFunc(func(context.Context, openaiProvider.TranslationRequest) (*http.Response, error) {
				return &http.Response{StatusCode: 201, Header: http.Header{"Content-Type": {testCase.contentType}, "Cache-Control": {"private, no-store"}}, Body: io.NopCloser(strings.NewReader(testCase.body))}, nil
			}), providerhealth.NoopGate{}, 1<<20, 1<<19, 1024, 1<<20, 1)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, translationRequest(t, []struct{ name, filename, value string }{{"model", "", "translation-public"}, {"response_format", "", testCase.format}, {"file", "voice.wav", "audio"}}))
			if response.Code != 201 || response.Header().Get("Content-Type") != testCase.contentType || response.Header().Get("Cache-Control") != "private, no-store" || response.Body.String() != testCase.body {
				t.Fatalf("status=%d headers=%v body=%q", response.Code, response.Header(), response.Body.String())
			}
		})
	}
}

func TestTranslationRejectsOperationSpecificOptionsBeforeDispatch(t *testing.T) {
	for _, testCase := range []struct{ name, field, value string }{{"language", "language", "ko"}, {"stream", "stream", "true"}, {"timestamps", "timestamp_granularities[]", "word"}, {"unknown", "future_option", "x"}, {"negative temperature", "temperature", "-0.1"}, {"large temperature", "temperature", "1.1"}, {"exponent temperature", "temperature", "1e-1"}, {"format", "response_format", "diarized_json"}} {
		t.Run(testCase.name, func(t *testing.T) {
			calls := 0
			handler := NewTranslationHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), acceptingAuth(t), translationRegistry(t), translationExecutorFunc(func(context.Context, openaiProvider.TranslationRequest) (*http.Response, error) {
				calls++
				return nil, nil
			}), providerhealth.NoopGate{}, 1<<20, 1<<19, 1024, 1<<20, 1)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, translationRequest(t, []struct{ name, filename, value string }{{"model", "", "translation-public"}, {testCase.field, "", testCase.value}, {"file", "voice.wav", "audio"}}))
			if response.Code != 400 || calls != 0 {
				t.Fatalf("status=%d calls=%d body=%s", response.Code, calls, response.Body.String())
			}
		})
	}
}

func TestTranslationFaultsNeverRedispatchAndCleanupSpools(t *testing.T) {
	for _, testCase := range []struct {
		name string
		call func(context.Context, openaiProvider.TranslationRequest) (*http.Response, error)
	}{{"timeout", func(context.Context, openaiProvider.TranslationRequest) (*http.Response, error) {
		return nil, openaiProvider.ErrTranslationTimeout
	}}, {"reset", func(context.Context, openaiProvider.TranslationRequest) (*http.Response, error) {
		return nil, openaiProvider.ErrTranslationUpstream
	}}, {"panic", func(context.Context, openaiProvider.TranslationRequest) (*http.Response, error) {
		panic("provider panic")
	}}} {
		t.Run(testCase.name, func(t *testing.T) {
			directory := t.TempDir()
			calls := 0
			handler := NewTranslationHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), acceptingAuth(t), translationRegistry(t), translationExecutorFunc(func(ctx context.Context, request openaiProvider.TranslationRequest) (*http.Response, error) {
				calls++
				return testCase.call(ctx, request)
			}), providerhealth.NoopGate{}, 2<<20, 2<<20, 1024, 1<<20, 1)
			handler.tempDir = directory
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, translationRequest(t, []struct{ name, filename, value string }{{"model", "", "translation-public"}, {"file", "voice.wav", strings.Repeat("a", int(transcriptionMemoryThreshold+1))}}))
			if calls != 1 || response.Code < 499 {
				t.Fatalf("calls=%d status=%d", calls, response.Code)
			}
			files, _ := filepath.Glob(filepath.Join(directory, "gateway-transcription-*"))
			if len(files) != 0 {
				t.Fatalf("temporary files=%v", files)
			}
		})
	}
}

func TestTranslationClientCancellationDoesNotRedispatch(t *testing.T) {
	calls := 0
	handler := NewTranslationHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), acceptingAuth(t), translationRegistry(t), translationExecutorFunc(func(ctx context.Context, _ openaiProvider.TranslationRequest) (*http.Response, error) {
		calls++
		<-ctx.Done()
		return nil, openaiProvider.ErrTranslationCanceled
	}), providerhealth.NoopGate{}, 1<<20, 1<<19, 1024, 1<<20, 1)
	request := translationRequest(t, []struct{ name, filename, value string }{{"model", "", "translation-public"}, {"file", "voice.wav", "audio"}})
	ctx, cancel := context.WithCancel(request.Context())
	cancel()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request.WithContext(ctx))
	if calls != 1 || response.Code != 499 {
		t.Fatalf("calls=%d status=%d", calls, response.Code)
	}
}

func TestTranslationResponseLimitFailsClosed(t *testing.T) {
	handler := NewTranslationHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), acceptingAuth(t), translationRegistry(t), translationExecutorFunc(func(context.Context, openaiProvider.TranslationRequest) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", 33)))}, nil
	}), providerhealth.NoopGate{}, 1024, 512, 128, 32, 1)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, translationRequest(t, []struct{ name, filename, value string }{{"model", "", "translation-public"}, {"file", "voice.wav", "audio"}}))
	if response.Code != 502 || strings.Contains(response.Body.String(), strings.Repeat("x", 10)) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
