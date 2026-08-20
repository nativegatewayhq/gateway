package imagestorage

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

func managedTestConfig(endpoint string) Config {
	config := DefaultConfig()
	config.Mode, config.Endpoint, config.Region, config.Bucket = Managed, endpoint, "auto", "gateway-images"
	config.AccessKeyID, config.SecretAccessKey = "access", "secret"
	config.CDNBaseURL = "https://cdn.example.test/assets"
	return config
}

func TestConfigRejectsIncompleteManagedAndUnsafeEndpoints(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"", "http://example.com", "https://user:pass@example.com", "https://example.com?secret=x"} {
		config := managedTestConfig(endpoint)
		if config.Validate() == nil {
			t.Fatalf("accepted endpoint %q", endpoint)
		}
	}
}

func TestObjectKeyIsDeterministicAndBounded(t *testing.T) {
	digest := sha256.Sum256([]byte("image"))
	key, err := ObjectKey("openai", "charge_abc", 2, digest, ".png")
	if err != nil || !strings.HasPrefix(key, "images/openai/charge_abc/002-") || !strings.HasSuffix(key, ".png") {
		t.Fatalf("key=%q err=%v", key, err)
	}
	if _, err := ObjectKey("../openai", "charge", 0, digest, "png"); err == nil {
		t.Fatal("accepted unsafe key")
	}
}

func TestS3PutSignsBoundedBodyAndReturnsCDNURL(t *testing.T) {
	body := []byte("image-bytes")
	digest := sha256.Sum256(body)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPut || request.URL.Path != "/gateway-images/images/openai/request/000-image.png" || request.Header.Get("Authorization") == "" || request.Header.Get("x-amz-content-sha256") != strings.Repeat("0", 64) {
			t.Fatalf("request=%s %s headers=%v", request.Method, request.URL.Path, request.Header)
		}
		read, _ := io.ReadAll(request.Body)
		if !bytes.Equal(read, body) {
			t.Fatalf("body=%q", read)
		}
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	store, err := NewS3(managedTestConfig(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC) }
	// The object digest is deliberately all-zero to prove signing uses the
	// supplied verified digest rather than buffering the body again.
	stored, err := store.Put(context.Background(), Object{Key: "images/openai/request/000-image.png", ContentType: "image/png", Size: int64(len(body))}, bytes.NewReader(body))
	if err != nil || stored.URL != "https://cdn.example.test/assets/images/openai/request/000-image.png" || stored.SHA256 != ([sha256.Size]byte{}) || digest == stored.SHA256 {
		t.Fatalf("stored=%+v err=%v", stored, err)
	}
}

func TestS3ReadyIsBoundedAndRejectsRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, "/elsewhere", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	store, err := NewS3(managedTestConfig(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Ready(context.Background()); !IsUnavailable(err) {
		t.Fatalf("err=%v", err)
	}
}
