//go:build integration

package audioassets

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"os"
	"testing"
	"time"
)

func TestPrivateS3RoundTrip(t *testing.T) {
	endpoint := os.Getenv("TEST_AUDIO_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("TEST_AUDIO_S3_ENDPOINT is required")
	}
	store, err := NewS3(S3Config{Endpoint: endpoint, Region: "us-east-1", Bucket: os.Getenv("TEST_AUDIO_S3_BUCKET"), AccessKeyID: os.Getenv("TEST_AUDIO_S3_ACCESS_KEY_ID"), SecretAccessKey: os.Getenv("TEST_AUDIO_S3_SECRET_ACCESS_KEY"), UploadTimeout: time.Minute, DownloadTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err = store.Ready(ctx); err != nil {
		t.Fatalf("ready: %v", err)
	}
	value := []byte("plan-060-private-audio")
	digest := sha256.Sum256(value)
	key := "audio/integration/plan060.bin"
	if err = store.Put(ctx, key, "audio/wav", int64(len(value)), digest, bytes.NewReader(value)); err != nil {
		t.Fatalf("put: %v", err)
	}
	body, err := store.Get(ctx, key, int64(len(value)))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	got, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil || !bytes.Equal(got, value) {
		t.Fatalf("got=%q err=%v", got, err)
	}
	if err = store.Delete(ctx, key); err != nil {
		t.Fatalf("delete: %v", err)
	}
}
