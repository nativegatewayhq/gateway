package openai

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nativegatewayhq/gateway/internal/apikey"
	"github.com/nativegatewayhq/gateway/internal/audioassets"
	"github.com/nativegatewayhq/gateway/internal/providerhealth"
	openaiProvider "github.com/nativegatewayhq/gateway/providers/openai"
)

type audioAssetServiceStub struct {
	upload audioassets.Upload
	owner  apikey.Principal
	key    string
	asset  audioassets.Asset
	err    error
}

func (stub *audioAssetServiceStub) Create(_ context.Context, owner apikey.Principal, key string, upload audioassets.Upload) (audioassets.Asset, error) {
	stub.owner, stub.key, stub.upload = owner, key, upload
	return stub.asset, stub.err
}
func (stub *audioAssetServiceStub) Get(context.Context, apikey.Principal, string) (audioassets.Asset, error) {
	return stub.asset, stub.err
}
func (stub *audioAssetServiceStub) Delete(context.Context, apikey.Principal, string) (audioassets.Asset, error) {
	stub.asset.State = audioassets.Deleting
	return stub.asset, stub.err
}

func wavBytes() []byte {
	return append([]byte("RIFF\x04\x00\x00\x00WAVEfmt "), bytes.Repeat([]byte{0}, 32)...)
}
func audioAssetUpload(t *testing.T, contentType string, body []byte) *http.Request {
	t.Helper()
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	headers := make(map[string][]string)
	headers["Content-Disposition"] = []string{`form-data; name="file"; filename="private.wav"`}
	headers["Content-Type"] = []string{contentType}
	part, err := writer.CreatePart(headers)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(body)
	_ = writer.Close()
	request := httptest.NewRequest(http.MethodPost, "/v1/audio/assets", &buffer)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Authorization", "Bearer service-secret")
	request.Header.Set("Idempotency-Key", "asset-key")
	return request
}

func TestAudioAssetCreateReturnsOnlyBoundedMetadata(t *testing.T) {
	body := wavBytes()
	digest := sha256.Sum256(body)
	asset := audioassets.Asset{ID: "audasset_00000000000000000000000000000001", ByteLength: int64(len(body)), ContentType: "audio/wav", SHA256: digest, State: audioassets.Available, CreatedAt: time.Unix(10, 0), ExpiresAt: time.Unix(20, 0), ObjectKey: "audio/private"}
	stub := &audioAssetServiceStub{asset: asset}
	handler := NewAudioAssetHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), acceptingAuth(t), stub, 1024, 1, []string{"audio/wav"}, t.TempDir())
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, audioAssetUpload(t, "audio/wav", body))
	if response.Code != 201 || stub.key != "asset-key" || stub.upload.Size != int64(len(body)) || stub.upload.SHA256 != digest || bytes.Contains(response.Body.Bytes(), []byte("audio/private")) || bytes.Contains(response.Body.Bytes(), digest[:]) {
		t.Fatalf("status=%d upload=%+v body=%s", response.Code, stub.upload, response.Body.String())
	}
}
func TestAudioAssetCreateRejectsMIMEAndMagicMismatch(t *testing.T) {
	handler := NewAudioAssetHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), acceptingAuth(t), &audioAssetServiceStub{}, 1024, 1, []string{"audio/wav"}, t.TempDir())
	for _, test := range []struct {
		mime string
		body []byte
	}{{"audio/mpeg", wavBytes()}, {"audio/wav", []byte("not-wave")}} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, audioAssetUpload(t, test.mime, test.body))
		if response.Code != 400 {
			t.Fatalf("mime=%s status=%d body=%s", test.mime, response.Code, response.Body.String())
		}
	}
}

type audioMaterializerStub struct {
	asset           audioassets.Asset
	body            []byte
	calls, releases int
}

func (stub *audioMaterializerStub) Materialize(context.Context, apikey.Principal, string) (audioassets.Materialized, error) {
	stub.calls++
	return audioassets.Materialized{Asset: stub.asset, Lease: audioassets.Lease{ID: "audlease_00000000000000000000000000000001", AssetID: stub.asset.ID, Owner: "test"}, Body: io.NopCloser(bytes.NewReader(stub.body))}, nil
}
func (stub *audioMaterializerStub) Release(context.Context, audioassets.Materialized) error {
	stub.releases++
	return nil
}

func TestTranscriptionAssetReferenceMaterializesNativeFile(t *testing.T) {
	body := wavBytes()
	digest := sha256.Sum256(body)
	assets := &audioMaterializerStub{asset: audioassets.Asset{ID: "audasset_00000000000000000000000000000001", ContentType: "audio/wav", ByteLength: int64(len(body)), SHA256: digest}, body: body}
	calls := 0
	handler := NewTranscriptionHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), acceptingAuth(t), transcriptionRegistry(t, false), transcriptionExecutorFunc(func(_ context.Context, request openaiProvider.TranscriptionRequest) (*http.Response, error) {
		calls++
		multipartBody, _ := io.ReadAll(request.Body)
		if !bytes.Contains(multipartBody, body) || bytes.Contains(multipartBody, []byte("X-Native-Gateway-Audio-Asset")) {
			t.Fatal("materialized body invalid")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader([]byte(`{"text":"ok","usage":{"type":"tokens","input_tokens":1,"input_token_details":{"audio_tokens":1,"text_tokens":0},"output_tokens":1,"total_tokens":2}}`)))}, nil
	}), providerhealth.NoopGate{}, 4096, 2048, 1024, 4096, 1)
	handler.SetAudioAssets(assets)
	contentType, requestBody := transcriptionBody(t, []struct{ name, filename, value string }{{"model", "", "gpt-4o-transcribe"}})
	request := transcriptionRequest(contentType, requestBody)
	request.Header.Set("X-Native-Gateway-Audio-Asset", assets.asset.ID)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 || calls != 1 || assets.calls != 1 || assets.releases != 1 {
		t.Fatalf("status=%d calls=%d assets=%d/%d body=%s", response.Code, calls, assets.calls, assets.releases, response.Body.String())
	}
}

func TestTranslationAssetReferenceMaterializesNativeFile(t *testing.T) {
	body := wavBytes()
	digest := sha256.Sum256(body)
	assets := &audioMaterializerStub{asset: audioassets.Asset{ID: "audasset_00000000000000000000000000000002", ContentType: "audio/wav", ByteLength: int64(len(body)), SHA256: digest}, body: body}
	calls := 0
	handler := NewTranslationHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), acceptingAuth(t), translationRegistry(t), translationExecutorFunc(func(_ context.Context, request openaiProvider.TranslationRequest) (*http.Response, error) {
		calls++
		multipartBody, _ := io.ReadAll(request.Body)
		if !bytes.Contains(multipartBody, body) {
			t.Fatal("materialized translation body missing")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader([]byte(`{"text":"ok"}`)))}, nil
	}), providerhealth.NoopGate{}, 4096, 2048, 1024, 4096, 1)
	handler.SetAudioAssets(assets)
	request := translationRequest(t, []struct{ name, filename, value string }{{"model", "", "translation-public"}, {"response_format", "", "json"}})
	request.Header.Set("X-Native-Gateway-Audio-Asset", assets.asset.ID)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 || calls != 1 || assets.calls != 1 || assets.releases != 1 {
		t.Fatalf("status=%d calls=%d assets=%d/%d body=%s", response.Code, calls, assets.calls, assets.releases, response.Body.String())
	}
}

func TestAudioAssetReferenceRejectsFileAndReferenceBeforeDispatch(t *testing.T) {
	calls := 0
	handler := NewTranscriptionHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), acceptingAuth(t), transcriptionRegistry(t, false), transcriptionExecutorFunc(func(context.Context, openaiProvider.TranscriptionRequest) (*http.Response, error) {
		calls++
		return nil, nil
	}), providerhealth.NoopGate{}, 4096, 2048, 1024, 4096, 1)
	handler.SetAudioAssets(&audioMaterializerStub{})
	contentType, body := transcriptionBody(t, []struct{ name, filename, value string }{{"model", "", "gpt-4o-transcribe"}, {"file", "a.wav", string(wavBytes())}})
	request := transcriptionRequest(contentType, body)
	request.Header.Set("X-Native-Gateway-Audio-Asset", "audasset_00000000000000000000000000000001")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 400 || calls != 0 {
		t.Fatalf("status=%d calls=%d", response.Code, calls)
	}
}
