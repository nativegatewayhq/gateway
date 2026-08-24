//go:build integration

package speechstorage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nativegatewayhq/gateway/internal/apikey"
	"github.com/nativegatewayhq/gateway/internal/audioassets"
	"github.com/nativegatewayhq/gateway/internal/database"
)

type memoryObjects struct {
	values        map[string][]byte
	puts, deletes int
}
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, context.Canceled }

func (s *memoryObjects) Put(_ context.Context, key, _ string, _ int64, _ [32]byte, body io.Reader) error {
	if s.values == nil {
		s.values = map[string][]byte{}
	}
	s.values[key], _ = io.ReadAll(body)
	s.puts++
	return nil
}
func (s *memoryObjects) Get(_ context.Context, key string, _ int64) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(s.values[key])), nil
}
func (s *memoryObjects) Delete(_ context.Context, key string) error {
	delete(s.values, key)
	s.deletes++
	return nil
}
func (s *memoryObjects) Ready(context.Context) error { return nil }

func speechFixture(t *testing.T) (*Repository, *pgxpool.Pool) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	ctx := context.Background()
	admin, err := database.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("speech_outputs_%d", time.Now().UnixNano())
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	config, _ := pgxpool.ParseConfig(url)
	config.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	if err = database.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		admin.Close()
	})
	_, err = pool.Exec(ctx, `INSERT INTO organizations(id,name,slug) VALUES('org_speech_output','Speech','speech'); INSERT INTO projects(id,organization_id,name,slug) VALUES('project_speech_output','org_speech_output','Speech','speech'); INSERT INTO service_api_keys(id,name,key_digest,key_prefix,project_id) VALUES('key_speech_output','Speech',decode(repeat('71',32),'hex'),'ngw_speech','project_speech_output')`)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := NewRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	return repository, pool
}
func speechOwner() apikey.Principal {
	return apikey.Principal{OrganizationID: "org_speech_output", ProjectID: "project_speech_output", APIKeyID: "key_speech_output"}
}

func TestManagedCapturePersistenceOwnershipAndCleanup(t *testing.T) {
	repository, pool := speechFixture(t)
	objects := &memoryObjects{}
	config := DefaultConfig()
	config.Mode = Managed
	config.Endpoint = "http://127.0.0.1:9000"
	config.Region = "auto"
	config.Bucket = "speech"
	config.AccessKeyID = "access"
	config.SecretAccessKey = "secret"
	config.MaximumBytes = 1024
	service, err := NewService(repository, objects, config, "worker")
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := sha256.Sum256([]byte("request"))
	asset, err := service.Begin(context.Background(), speechOwner(), "speech-key", "", fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := service.Begin(context.Background(), speechOwner(), "speech-key", "", fingerprint)
	if err != ErrPending || repeated.ID != "" {
		t.Fatalf("repeat=%+v err=%v", repeated, err)
	}
	result, err := service.Capture(context.Background(), asset, "audio/mpeg", bytes.NewBufferString("private-speech"), io.Discard, int64(len("private-speech")))
	if err != nil || result.Asset.State != Available || objects.puts != 1 {
		t.Fatalf("result=%+v err=%v puts=%d", result, err, objects.puts)
	}
	recoveryFingerprint := sha256.Sum256([]byte("recovery-request"))
	recoveryAsset, err := service.Begin(context.Background(), speechOwner(), "recovery-key", "", recoveryFingerprint)
	if err != nil {
		t.Fatal(err)
	}
	recoveryBody := []byte("recovered-speech")
	recoveryDigest := sha256.Sum256(recoveryBody)
	recoveryKey := "audio/speech/org_speech_output/recovery.mp3"
	if err = repository.MarkCaptured(context.Background(), recoveryAsset.ID, recoveryKey, "audio/mpeg", int64(len(recoveryBody)), recoveryDigest); err != nil {
		t.Fatal(err)
	}
	objects.values[recoveryKey] = recoveryBody
	processed, err := service.RunCleanup(context.Background())
	if err != nil || !processed {
		t.Fatalf("recovery=%v err=%v", processed, err)
	}
	recovered, err := service.Get(context.Background(), speechOwner(), recoveryAsset.ID)
	if err != nil || recovered.State != Available {
		t.Fatalf("recovered=%+v err=%v", recovered, err)
	}
	disconnectFingerprint := sha256.Sum256([]byte("disconnect-request"))
	disconnectAsset, err := service.Begin(context.Background(), speechOwner(), "disconnect-key", "", disconnectFingerprint)
	if err != nil {
		t.Fatal(err)
	}
	disconnectResult, err := service.Capture(context.Background(), disconnectAsset, "audio/mpeg", bytes.NewBufferString("disconnect-speech"), failingWriter{}, int64(len("disconnect-speech")))
	if err != nil || disconnectResult.DownstreamErr == nil || disconnectResult.Asset.State != Available {
		t.Fatalf("disconnect=%+v err=%v", disconnectResult, err)
	}
	if _, err = service.Get(context.Background(), apikey.Principal{OrganizationID: "org_other", ProjectID: "project_other", APIKeyID: "key_other"}, asset.ID); err != ErrDenied {
		t.Fatalf("cross owner err=%v", err)
	}
	if _, err = pool.Exec(context.Background(), `UPDATE speech_output_assets SET object_key='changed' WHERE id=$1`, asset.ID); err == nil {
		t.Fatal("immutable content identity changed")
	}
	_, activeBody, err := service.Open(context.Background(), speechOwner(), asset.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.Delete(context.Background(), speechOwner(), asset.ID); err != nil {
		t.Fatal(err)
	}
	processed, err = service.RunCleanup(context.Background())
	if err != nil || processed || objects.deletes != 0 {
		t.Fatalf("active cleanup=%v err=%v deletes=%d", processed, err, objects.deletes)
	}
	_ = activeBody.Close()
	processed, err = service.RunCleanup(context.Background())
	if err != nil || !processed || objects.deletes != 1 {
		t.Fatalf("cleanup=%v err=%v deletes=%d", processed, err, objects.deletes)
	}
}

func TestManagedSpeechOutputActualPrivateS3RoundTrip(t *testing.T) {
	endpoint := os.Getenv("TEST_AUDIO_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("TEST_AUDIO_S3_ENDPOINT is required")
	}
	repository, _ := speechFixture(t)
	objects, err := audioassets.NewS3(audioassets.S3Config{Endpoint: endpoint, Region: "us-east-1", Bucket: os.Getenv("TEST_AUDIO_S3_BUCKET"), AccessKeyID: os.Getenv("TEST_AUDIO_S3_ACCESS_KEY_ID"), SecretAccessKey: os.Getenv("TEST_AUDIO_S3_SECRET_ACCESS_KEY"), UploadTimeout: time.Minute, DownloadTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	config := DefaultConfig()
	config.Mode = Managed
	config.Endpoint = endpoint
	config.Region = "us-east-1"
	config.Bucket = os.Getenv("TEST_AUDIO_S3_BUCKET")
	config.AccessKeyID = os.Getenv("TEST_AUDIO_S3_ACCESS_KEY_ID")
	config.SecretAccessKey = os.Getenv("TEST_AUDIO_S3_SECRET_ACCESS_KEY")
	service, err := NewService(repository, objects, config, "s3-worker")
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := sha256.Sum256([]byte("s3-request"))
	asset, err := service.Begin(context.Background(), speechOwner(), "s3-speech-key", "", fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	value := "private-speech-s3"
	result, err := service.Capture(context.Background(), asset, "audio/mpeg", bytes.NewBufferString(value), io.Discard, int64(len(value)))
	if err != nil || result.StorageErr != nil {
		t.Fatalf("capture=%+v err=%v", result, err)
	}
	_, body, err := service.Open(context.Background(), speechOwner(), asset.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(body)
	_ = body.Close()
	if string(got) != value {
		t.Fatalf("got=%q", got)
	}
	if _, err = service.Delete(context.Background(), speechOwner(), asset.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = service.RunCleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
}
