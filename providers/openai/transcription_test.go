package openai

import (
	"context"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestTranscriptionUsesFixedWireAndProviderCredential(t *testing.T) {
	credentials, _ := providercredentials.Load(func(key string) (string, bool) {
		if key == "GATEWAY_OPENAI_API_KEY" {
			return "provider-secret", true
		}
		return "", false
	})
	client := &http.Client{Transport: speechRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://api.openai.com/v1/audio/transcriptions" || r.Header.Get("Authorization") != "Bearer provider-secret" || r.Header.Get("OpenAI-Organization") != "" {
			t.Fatalf("url=%s headers=%v", r.URL, r.Header)
		}
		media, params, _ := mimeParse(r.Header.Get("Content-Type"))
		if media != "multipart/form-data" {
			t.Fatal(media)
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		part, _ := mr.NextPart()
		body, _ := io.ReadAll(part)
		if part.FormName() != "model" || string(body) != "gpt-4o-transcribe" {
			t.Fatalf("part=%s body=%s", part.FormName(), body)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"text":"ok"}`))}, nil
	})}
	var body strings.Builder
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("model", "gpt-4o-transcribe")
	_ = mw.Close()
	executor := NewTranscriptionWithClient(credentials, time.Second, time.Second, client)
	response, err := executor.Create(context.Background(), TranscriptionRequest{ChannelID: "channel_00000000000000000000000000000001", ContentType: mw.FormDataContentType(), ContentLength: int64(body.Len()), Body: strings.NewReader(body.String())})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
}
func mimeParse(value string) (string, map[string]string, error) { return mime.ParseMediaType(value) }
