package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nativegatewayhq/gateway/internal/observability"
	"github.com/nativegatewayhq/gateway/internal/requestid"
)

func TestHealthEndpoints(t *testing.T) {
	t.Parallel()

	for _, path := range []string{"/health/live", "/health/ready"} {
		t.Run(path, func(t *testing.T) {
			response := serveRequest(t, NewHandler(discardLogger(), nil), http.MethodGet, path, "")
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", response.Code)
			}
			if got := response.Header().Get("Content-Type"); got != "application/json" {
				t.Fatalf("Content-Type = %q", got)
			}
			if body := response.Body.String(); body != "{\"status\":\"ok\"}\n" {
				t.Fatalf("body = %q", body)
			}
			if !requestid.Valid(response.Header().Get(requestid.HeaderName)) {
				t.Fatalf("invalid request ID %q", response.Header().Get(requestid.HeaderName))
			}
		})
	}
}

func TestGeminiRouteIsMountedWithoutProtectingHealth(t *testing.T) {
	t.Parallel()
	gemini := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusAccepted)
	})
	handler := NewHandler(discardLogger(), nil, Routes{Gemini: gemini})
	response := serveRequest(t, handler, http.MethodPost, "/v1beta/models/gemini-image:generateContent", "")
	if response.Code != http.StatusAccepted {
		t.Fatalf("Gemini status = %d", response.Code)
	}
	health := serveRequest(t, handler, http.MethodGet, "/health/live", "")
	if health.Code != http.StatusOK {
		t.Fatalf("health status = %d", health.Code)
	}
}

func TestOpenAIImagesRouteIsMountedWithoutProtectingHealth(t *testing.T) {
	t.Parallel()
	images := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusAccepted)
	})
	handler := NewHandler(discardLogger(), nil, Routes{OpenAIImages: images})
	response := serveRequest(t, handler, http.MethodPost, "/v1/images/generations", "")
	if response.Code != http.StatusAccepted {
		t.Fatalf("OpenAI Images status = %d", response.Code)
	}
	health := serveRequest(t, handler, http.MethodGet, "/health/live", "")
	if health.Code != http.StatusOK {
		t.Fatalf("health status = %d", health.Code)
	}
}

func TestOpenAIImageEditsRouteIsMounted(t *testing.T) {
	t.Parallel()
	edits := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusAccepted) })
	handler := NewHandler(discardLogger(), nil, Routes{OpenAIImageEdits: edits})
	response := serveRequest(t, handler, http.MethodPost, "/v1/images/edits", "")
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestOpenAIModelsRouteIsMounted(t *testing.T) {
	t.Parallel()
	models := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusAccepted) })
	handler := NewHandler(discardLogger(), nil, Routes{OpenAIModels: models})
	if response := serveRequest(t, handler, http.MethodGet, "/v1/models", ""); response.Code != http.StatusAccepted {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestOpenAIChatRouteIsExact(t *testing.T) {
	chat := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusCreated) })
	handler := NewHandler(discardLogger(), nil, Routes{OpenAIChat: chat})
	for path, want := range map[string]int{"/v1/chat/completions": http.StatusCreated, "/v1/chat/other": http.StatusNotFound} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, nil))
		if response.Code != want {
			t.Fatalf("%s=%d", path, response.Code)
		}
	}
}

func TestOpenAISpeechRouteIsExact(t *testing.T) {
	speech := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusCreated) })
	handler := NewHandler(discardLogger(), nil, Routes{OpenAISpeech: speech})
	for path, want := range map[string]int{"/v1/audio/speech": http.StatusCreated, "/v1/audio/transcriptions": http.StatusNotFound} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, nil))
		if response.Code != want {
			t.Fatalf("%s=%d", path, response.Code)
		}
	}
}

func TestOpenAISpeechAssetRoutesAreMounted(t *testing.T) {
	assets := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusCreated) })
	handler := NewHandler(discardLogger(), nil, Routes{OpenAISpeechAssets: assets})
	for path, want := range map[string]int{"/v1/audio/speech/assets/speechasset_00000000000000000000000000000001": http.StatusCreated, "/v1/audio/speech/assets/speechasset_00000000000000000000000000000001/content": http.StatusCreated, "/v1/audio/speech": http.StatusNotFound} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != want {
			t.Fatalf("%s=%d", path, response.Code)
		}
	}
}

func TestOpenAITranscriptionsRouteIsExact(t *testing.T) {
	transcriptions := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusCreated) })
	handler := NewHandler(discardLogger(), nil, Routes{OpenAITranscriptions: transcriptions})
	for path, want := range map[string]int{"/v1/audio/transcriptions": http.StatusCreated, "/v1/audio/translations": http.StatusNotFound} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, nil))
		if response.Code != want {
			t.Fatalf("%s=%d", path, response.Code)
		}
	}
}

func TestOpenAITranslationsRouteIsExact(t *testing.T) {
	translations := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusCreated) })
	handler := NewHandler(discardLogger(), nil, Routes{OpenAITranslations: translations})
	for path, want := range map[string]int{"/v1/audio/translations": http.StatusCreated, "/v1/audio/transcriptions": http.StatusNotFound, "/v1/audio/translations/extra": http.StatusNotFound} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, nil))
		if response.Code != want {
			t.Fatalf("%s=%d", path, response.Code)
		}
	}
}

func TestOpenAIAudioAssetRoutesAreMounted(t *testing.T) {
	assets := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusCreated) })
	handler := NewHandler(discardLogger(), nil, Routes{OpenAIAudioAssets: assets})
	for path, want := range map[string]int{
		"/v1/audio/assets":           http.StatusCreated,
		"/v1/audio/assets/asset_123": http.StatusCreated,
		"/v1/audio/asset":            http.StatusNotFound,
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, nil))
		if response.Code != want {
			t.Fatalf("%s=%d", path, response.Code)
		}
	}
}

func TestReplicatePredictionRoutesAreMounted(t *testing.T) {
	t.Parallel()
	predictions := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusAccepted) })
	handler := NewHandler(discardLogger(), nil, Routes{Replicate: predictions})
	for _, item := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/v1/predictions"},
		{http.MethodGet, "/v1/predictions/job_00000000000000000000000000000000"},
		{http.MethodPost, "/v1/predictions/job_00000000000000000000000000000000/cancel"},
	} {
		if response := serveRequest(t, handler, item.method, item.path, ""); response.Code != http.StatusAccepted {
			t.Fatalf("%s %s status = %d", item.method, item.path, response.Code)
		}
	}
}

func TestReplicateWebhookRouteIsMountedAboveFalWildcard(t *testing.T) {
	t.Parallel()
	webhook := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusNoContent) })
	fal := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusTeapot) })
	handler := NewHandler(discardLogger(), nil, Routes{ReplicateWebhook: webhook, Fal: fal})
	response := serveRequest(t, handler, http.MethodPost, "/internal/webhooks/replicate/job_00000000000000000000000000000000/token", "")
	if response.Code != http.StatusNoContent {
		t.Fatalf("status=%d", response.Code)
	}
}

func TestFalWebhookRouteIsMountedAboveFalWildcard(t *testing.T) {
	t.Parallel()
	webhook := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusNoContent) })
	fal := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusTeapot) })
	handler := NewHandler(discardLogger(), nil, Routes{FalWebhook: webhook, Fal: fal})
	response := serveRequest(t, handler, http.MethodPost, "/internal/webhooks/fal/job_00000000000000000000000000000000/token", "")
	if response.Code != http.StatusNoContent {
		t.Fatalf("status=%d", response.Code)
	}
}

func TestPluginWebhookRouteIsMountedAboveFalWildcard(t *testing.T) {
	webhook := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusNoContent) })
	fal := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusTeapot) })
	handler := NewHandler(discardLogger(), nil, Routes{PluginWebhook: webhook, Fal: fal})
	response := serveRequest(t, handler, http.MethodPost, "/internal/webhooks/plugin/job/token", "{}")
	if response.Code != http.StatusNoContent {
		t.Fatalf("status=%d", response.Code)
	}
}

func TestFalWildcardRouteIsMountedBelowOwnedRoutes(t *testing.T) {
	t.Parallel()
	fal := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusAccepted) })
	handler := NewHandler(discardLogger(), nil, Routes{Fal: fal})
	if response := serveRequest(t, handler, http.MethodPost, "/fal-ai/flux/dev", ""); response.Code != http.StatusAccepted {
		t.Fatalf("fal status = %d", response.Code)
	}
	if response := serveRequest(t, handler, http.MethodGet, "/health/live", ""); response.Code != http.StatusOK {
		t.Fatalf("health status = %d", response.Code)
	}
}

func TestReadinessFailure(t *testing.T) {
	t.Parallel()

	handler := NewHandler(discardLogger(), func(context.Context) error { return errors.New("database password=secret") })
	response := serveRequest(t, handler, http.MethodGet, "/health/ready", "req_ready")

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.Code)
	}
	if strings.Contains(response.Body.String(), "database") || strings.Contains(response.Body.String(), "secret") {
		t.Fatalf("response leaked readiness error: %s", response.Body.String())
	}
	assertError(t, response, "not_ready", "req_ready")
}

func TestNotFoundUsesGatewayError(t *testing.T) {
	t.Parallel()

	response := serveRequest(t, NewHandler(discardLogger(), nil), http.MethodGet, "/does-not-exist?key=secret", "req_not_found")
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.Code)
	}
	if strings.Contains(response.Body.String(), "secret") {
		t.Fatalf("response leaked query: %s", response.Body.String())
	}
	assertError(t, response, "not_found", "req_not_found")
}

func TestRecoveryHidesPanicAndRequestSecrets(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	logger := observability.NewLogger(&output, slog.LevelInfo)
	panicking := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("provider_key=secret-provider-key")
	})
	handler := requestid.Middleware(accessLog(logger, recovery(logger, panicking)))
	response := serveRequest(t, handler, http.MethodGet, "/panic?key=secret-query-key", "req_panic")

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", response.Code)
	}
	assertError(t, response, "internal_error", "req_panic")
	combined := response.Body.String() + output.String()
	for _, secret := range []string{"secret-provider-key", "secret-query-key", "Authorization", "Cookie"} {
		if strings.Contains(combined, secret) {
			t.Fatalf("panic handling leaked %q: %s", secret, combined)
		}
	}
}

func TestAccessLogContainsRequiredFieldsAndNoSecrets(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	logger := observability.NewLogger(&output, slog.LevelInfo)
	handler := NewHandler(logger, nil)
	request := httptest.NewRequest(http.MethodGet, "/health/live?key=secret-query", nil)
	request.Header.Set(requestid.HeaderName, "req_log")
	request.Header.Set("Authorization", "Bearer secret-authorization")
	request.Header.Set("Cookie", "session=secret-cookie")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	logOutput := output.String()
	for _, field := range []string{"request_id", "req_log", "method", "GET", "route", "GET /health/live", "status", "duration"} {
		if !strings.Contains(logOutput, field) {
			t.Errorf("log missing %q: %s", field, logOutput)
		}
	}
	for _, secret := range []string{"secret-query", "secret-authorization", "secret-cookie", "Authorization", "Cookie"} {
		if strings.Contains(logOutput, secret) {
			t.Errorf("log leaked %q: %s", secret, logOutput)
		}
	}
}

func serveRequest(t *testing.T, handler http.Handler, method, target, incomingRequestID string) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequest(method, target, nil)
	if incomingRequestID != "" {
		request.Header.Set(requestid.HeaderName, incomingRequestID)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func assertError(t *testing.T, response *httptest.ResponseRecorder, wantCode, wantRequestID string) {
	t.Helper()

	var envelope errorEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if envelope.Error.Code != wantCode || envelope.Error.RequestID != wantRequestID {
		t.Fatalf("error = %+v, want code=%q request_id=%q", envelope.Error, wantCode, wantRequestID)
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
}
