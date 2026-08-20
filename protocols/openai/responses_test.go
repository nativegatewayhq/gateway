package openai

import (
	"bytes"
	"context"
	responsesoperation "github.com/nativegatewayhq/gateway/operations/responses"
	openaiProvider "github.com/nativegatewayhq/gateway/providers/openai"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type responsesExecutorFunc func(context.Context, openaiProvider.ResponsesRequest) (*http.Response, error)

func (f responsesExecutorFunc) Create(ctx context.Context, r openaiProvider.ResponsesRequest) (*http.Response, error) {
	return f(ctx, r)
}
func TestResponsesPreservesNativeRequestAndResponse(t *testing.T) {
	registry, _ := responsesoperation.NewRegistry([]string{"gpt-4.1"})
	input := `{"model":"gpt-4.1","input":[{"role":"user","content":[{"type":"input_text","text":"secret"}]}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"future":{"x":1}}`
	output := `{"id":"resp_1","object":"response","output":[{"type":"function_call","arguments":"{\"x\":1}"}]}`
	handler := NewResponsesHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), acceptingAuth(t), registry, responsesExecutorFunc(func(_ context.Context, r openaiProvider.ResponsesRequest) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		if !bytes.Equal(body, []byte(input)) {
			t.Fatalf("body=%s", body)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}, "Authorization": {"provider-secret"}}, Body: io.NopCloser(strings.NewReader(output))}, nil
	}), channelAvailability{"channel_00000000000000000000000000000001": true}, 4096)
	request := chatRequest(input)
	request.URL.Path = "/v1/responses"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, request)
	if w.Code != 200 || w.Body.String() != output || w.Header().Get("Authorization") != "" {
		t.Fatalf("status=%d headers=%v body=%q", w.Code, w.Header(), w.Body.String())
	}
}
func TestResponsesRejectsStreamBeforeDispatch(t *testing.T) {
	registry, _ := responsesoperation.NewRegistry([]string{"gpt-4.1"})
	calls := 0
	handler := NewResponsesHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), acceptingAuth(t), registry, responsesExecutorFunc(func(context.Context, openaiProvider.ResponsesRequest) (*http.Response, error) {
		calls++
		return nil, nil
	}), channelAvailability{"channel_00000000000000000000000000000001": true}, 4096)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, chatRequest(`{"model":"gpt-4.1","stream":true}`))
	if w.Code != 400 || calls != 0 {
		t.Fatalf("status=%d calls=%d", w.Code, calls)
	}
}
