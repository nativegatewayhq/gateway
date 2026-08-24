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
	"github.com/nativegatewayhq/gateway/internal/speechstorage"
)

func TestParseSingleRange(t *testing.T) {
	for _, test := range []struct {
		value      string
		start, end int64
		partial    bool
		valid      bool
	}{{"", 0, 9, false, true}, {"bytes=2-5", 2, 5, true, true}, {"bytes=7-", 7, 9, true, true}, {"bytes=-3", 7, 9, true, true}, {"bytes=4-2", 0, 0, false, false}, {"bytes=0-1,3-4", 0, 0, false, false}} {
		start, end, partial, err := parseSingleRange(test.value, 10)
		if (err == nil) != test.valid || (err == nil && (start != test.start || end != test.end || partial != test.partial)) {
			t.Fatalf("range %q=%d-%d partial=%v err=%v", test.value, start, end, partial, err)
		}
	}
}
func TestSpeechAssetPathIsOpaqueAndExact(t *testing.T) {
	id := "speechasset_00000000000000000000000000000001"
	if got, content, ok := speechAssetPath("/v1/audio/speech/assets/" + id + "/content"); !ok || !content || got != id {
		t.Fatalf("path=%q %v %v", got, content, ok)
	}
	if _, _, ok := speechAssetPath("/v1/audio/speech/assets/" + id + "/extra"); ok {
		t.Fatal("extra path accepted")
	}
}

type speechAssetServiceStub struct {
	asset speechstorage.Asset
	body  string
}

func (s *speechAssetServiceStub) Get(context.Context, apikey.Principal, string) (speechstorage.Asset, error) {
	return s.asset, nil
}
func (s *speechAssetServiceStub) Open(context.Context, apikey.Principal, string) (speechstorage.Asset, io.ReadCloser, error) {
	return s.asset, io.NopCloser(strings.NewReader(s.body)), nil
}
func (s *speechAssetServiceStub) Delete(context.Context, apikey.Principal, string) (speechstorage.Asset, error) {
	s.asset.State = speechstorage.Deleting
	return s.asset, nil
}

func TestSpeechAssetMetadataPrivacyAndRangeDelivery(t *testing.T) {
	id := "speechasset_00000000000000000000000000000001"
	service := &speechAssetServiceStub{asset: speechstorage.Asset{ID: id, ObjectKey: "private/object", ContentType: "audio/mpeg", ByteLength: 10, SHA256: [32]byte{1}, State: speechstorage.Available, CreatedAt: time.Unix(1, 0), ExpiresAt: time.Unix(2, 0)}, body: "0123456789"}
	handler := NewSpeechAssetHandler(slog.Default(), acceptingAuth(t), service)
	metadata := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/audio/speech/assets/"+id, nil)
	request.Header.Set("Authorization", "Bearer service-secret")
	handler.ServeHTTP(metadata, request)
	if metadata.Code != 200 || strings.Contains(metadata.Body.String(), "private/object") || strings.Contains(metadata.Body.String(), "sha256") || !strings.Contains(metadata.Body.String(), id) {
		t.Fatalf("metadata=%d %s", metadata.Code, metadata.Body.String())
	}
	content := httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/v1/audio/speech/assets/"+id+"/content", nil)
	request.Header.Set("Authorization", "Bearer service-secret")
	request.Header.Set("Range", "bytes=2-5")
	handler.ServeHTTP(content, request)
	if content.Code != http.StatusPartialContent || content.Body.String() != "2345" || content.Header().Get("Content-Range") != "bytes 2-5/10" {
		t.Fatalf("content=%d headers=%v body=%q", content.Code, content.Header(), content.Body.String())
	}
}
