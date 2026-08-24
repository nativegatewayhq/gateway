package audioassets

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type memoryObjects struct {
	values              map[string][]byte
	puts, gets, deletes int
	err                 error
}

func (store *memoryObjects) Put(_ context.Context, key, _ string, _ int64, _ [32]byte, body io.Reader) error {
	if store.err != nil {
		return store.err
	}
	value, _ := io.ReadAll(body)
	if store.values == nil {
		store.values = map[string][]byte{}
	}
	store.values[key] = value
	store.puts++
	return nil
}
func (store *memoryObjects) Get(_ context.Context, key string, _ int64) (io.ReadCloser, error) {
	if store.err != nil {
		return nil, store.err
	}
	store.gets++
	return io.NopCloser(bytes.NewReader(store.values[key])), nil
}
func (store *memoryObjects) Delete(_ context.Context, key string) error {
	if store.err != nil {
		return store.err
	}
	delete(store.values, key)
	store.deletes++
	return nil
}
func (store *memoryObjects) Ready(context.Context) error { return store.err }

func TestConfigRejectsUnsafeBounds(t *testing.T) {
	config := DefaultConfig()
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	config.Mode = Managed
	config.Endpoint = "http://private.example"
	config.Region = "auto"
	config.Bucket = "audio"
	config.AccessKeyID = "access"
	config.SecretAccessKey = "secret"
	if err := config.Validate(); err == nil {
		t.Fatal("unsafe endpoint accepted")
	}
}

func TestPrivateS3PutGetDeleteAndReadiness(t *testing.T) {
	value := []byte("private-audio")
	putCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") == "" || request.URL.Path != "/bucket/audio/org/object.bin" && request.URL.Path != "/bucket" {
			t.Errorf("request=%s %s auth=%q", request.Method, request.URL.Path, request.Header.Get("Authorization"))
			writer.WriteHeader(400)
			return
		}
		switch request.Method {
		case http.MethodPut:
			putCalls++
			if request.Header.Get("If-None-Match") != "*" || request.Header.Get("x-amz-server-side-encryption") != "AES256" {
				t.Errorf("missing conditional/encryption headers: %v", request.Header)
			}
			authorization := request.Header.Get("Authorization")
			if !strings.Contains(authorization, "x-amz-meta-sha256") || !strings.Contains(authorization, "x-amz-server-side-encryption") {
				t.Errorf("unsigned protected headers: %s", authorization)
			}
			body, _ := io.ReadAll(request.Body)
			if !bytes.Equal(body, value) {
				t.Errorf("put=%q", body)
			}
			if putCalls > 1 {
				writer.WriteHeader(http.StatusPreconditionFailed)
			} else {
				writer.WriteHeader(200)
			}
		case http.MethodGet:
			writer.Header().Set("Content-Length", "13")
			writer.WriteHeader(200)
			_, _ = writer.Write(value)
		case http.MethodDelete:
			writer.WriteHeader(204)
		case http.MethodHead:
			writer.WriteHeader(200)
		}
	}))
	defer server.Close()
	store, err := NewS3(S3Config{Endpoint: server.URL, Region: "auto", Bucket: "bucket", AccessKeyID: "access", SecretAccessKey: "secret", ServerSideEncryption: "AES256", UploadTimeout: time.Second, DownloadTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(value)
	if err = store.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = store.Put(context.Background(), "audio/org/object.bin", "audio/wav", int64(len(value)), digest, bytes.NewReader(value)); err != nil {
		t.Fatal(err)
	}
	if err = store.Put(context.Background(), "audio/org/object.bin", "audio/wav", int64(len(value)), digest, bytes.NewReader(value)); err != nil {
		t.Fatalf("conditional reuse: %v", err)
	}
	body, err := store.Get(context.Background(), "audio/org/object.bin", int64(len(value)))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(body)
	_ = body.Close()
	if !bytes.Equal(got, value) {
		t.Fatalf("got=%q", got)
	}
	if err = store.Delete(context.Background(), "audio/org/object.bin"); err != nil {
		t.Fatal(err)
	}
}
