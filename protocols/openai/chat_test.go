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

	"github.com/nativegatewayhq/gateway/internal/apikey"
	"github.com/nativegatewayhq/gateway/internal/providerhealth"
	chatoperation "github.com/nativegatewayhq/gateway/operations/chat"
	openaiProvider "github.com/nativegatewayhq/gateway/providers/openai"
)

type chatExecutorFunc func(context.Context, openaiProvider.ChatRequest) (*http.Response, error)

func (f chatExecutorFunc) Complete(ctx context.Context, r openaiProvider.ChatRequest) (*http.Response, error) {
	return f(ctx, r)
}
func chatRegistry(t *testing.T) *chatoperation.Registry {
	t.Helper()
	r, err := chatoperation.NewRegistry([]string{"gpt-4.1"})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func chatHandler(t *testing.T, auth Authenticator, executor ChatExecutor, maximum int64) *ChatHandler {
	t.Helper()
	return NewChatHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), auth, chatRegistry(t), executor, channelAvailability{"channel_00000000000000000000000000000001": true}, providerhealth.NoopGate{}, maximum)
}
func chatRequest(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer service-secret")
	r.Header.Set("Content-Type", "application/json")
	return r
}

func TestChatPreservesNativeBodyAndResponse(t *testing.T) {
	input := `{"model":"gpt-4.1","messages":[{"role":"user","content":"secret prompt"}],"future_field":{"x":1},"stream":false}`
	calls := 0
	handler := chatHandler(t, acceptingAuth(t), chatExecutorFunc(func(_ context.Context, r openaiProvider.ChatRequest) (*http.Response, error) {
		calls++
		body, _ := io.ReadAll(r.Body)
		if !bytes.Equal(body, []byte(input)) {
			t.Fatalf("body=%s", body)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}, "Set-Cookie": {"secret=x"}, "Authorization": {"provider-secret"}}, Body: io.NopCloser(strings.NewReader(`{"id":"chatcmpl_1","choices":[]}`))}, nil
	}), 4096)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, chatRequest(input))
	if w.Code != 200 || calls != 1 || w.Body.String() != `{"id":"chatcmpl_1","choices":[]}` || w.Header().Get("Set-Cookie") != "" || w.Header().Get("Authorization") != "" {
		t.Fatalf("status=%d calls=%d headers=%v body=%q", w.Code, calls, w.Header(), w.Body.String())
	}
}

func TestChatRejectsBeforeDispatch(t *testing.T) {
	calls := 0
	executor := chatExecutorFunc(func(context.Context, openaiProvider.ChatRequest) (*http.Response, error) { calls++; return nil, nil })
	principal := apikey.Principal{ModelAccessMode: apikey.ModelAccessAllowlist, ModelPermissions: []apikey.ModelPermission{{Protocol: "openai", Operation: "image.generate", Model: "gpt-4.1"}}}
	tests := []struct {
		name, body string
		mutate     func(*http.Request)
		status     int
	}{{"stream", `{"model":"gpt-4.1","stream":true}`, nil, 400}, {"trailing", `{"model":"gpt-4.1"}{}`, nil, 400}, {"missing model", `{"messages":[]}`, nil, 400}, {"compressed", `{"model":"gpt-4.1"}`, func(r *http.Request) { r.Header.Set("Content-Encoding", "gzip") }, 415}, {"unauthorized model", `{"model":"gpt-4.1"}`, nil, 403}}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			auth := acceptingAuth(t)
			if test.name == "unauthorized model" {
				auth = authFunc(func(context.Context, string) (apikey.Principal, error) { return principal, nil })
			}
			handler := chatHandler(t, auth, executor, 4096)
			r := chatRequest(test.body)
			if test.mutate != nil {
				test.mutate(r)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != test.status {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
	if calls != 0 {
		t.Fatalf("provider calls=%d", calls)
	}
}

func TestChatBoundsAndMapsExecutorFailures(t *testing.T) {
	handler := chatHandler(t, acceptingAuth(t), chatExecutorFunc(func(context.Context, openaiProvider.ChatRequest) (*http.Response, error) {
		return nil, openaiProvider.ErrChatTimeout
	}), 64)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, chatRequest(`{"model":"gpt-4.1"}`))
	if w.Code != 504 {
		t.Fatalf("timeout=%d", w.Code)
	}
	large := chatHandler(t, acceptingAuth(t), chatExecutorFunc(func(context.Context, openaiProvider.ChatRequest) (*http.Response, error) {
		return nil, errors.New("unexpected")
	}), 8)
	w = httptest.NewRecorder()
	large.ServeHTTP(w, chatRequest(`{"model":"gpt-4.1"}`))
	if w.Code != 413 {
		t.Fatalf("large=%d", w.Code)
	}
	responseLarge := chatHandler(t, acceptingAuth(t), chatExecutorFunc(func(context.Context, openaiProvider.ChatRequest) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", 100)))}, nil
	}), 64)
	w = httptest.NewRecorder()
	responseLarge.ServeHTTP(w, chatRequest(`{"model":"gpt-4.1"}`))
	if w.Code != 502 {
		t.Fatalf("large response=%d", w.Code)
	}
}
