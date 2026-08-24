//go:build integration

package audioassets

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nativegatewayhq/gateway/internal/apikey"
	"github.com/nativegatewayhq/gateway/internal/database"
)

func repositoryFixture(t *testing.T) (*Repository, *pgxpool.Pool) {
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
	schema := fmt.Sprintf("audio_assets_%d", time.Now().UnixNano())
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
	_, err = pool.Exec(ctx, `INSERT INTO organizations(id,name,slug) VALUES('org_audio_assets','Audio Assets','audio-assets'),('org_audio_other','Other','audio-other'); INSERT INTO projects(id,organization_id,name,slug) VALUES('project_audio_assets','org_audio_assets','Audio Assets','audio-assets'),('project_audio_other','org_audio_other','Other','audio-other'); INSERT INTO service_api_keys(id,name,key_digest,key_prefix,project_id) VALUES('key_audio_assets','Audio Assets',decode(repeat('61',32),'hex'),'ngw_audio_assets','project_audio_assets'),('key_audio_other','Other',decode(repeat('62',32),'hex'),'ngw_audio_other','project_audio_other')`)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := NewRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	return repository, pool
}

func TestServiceCreateMaterializeDeleteAndCleanup(t *testing.T) {
	repository, pool := repositoryFixture(t)
	_ = pool
	objects := &memoryObjects{}
	service, err := NewService(repository, objects, 24*time.Hour, time.Minute, 1024, "integration-worker")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("private-audio")
	digest := sha256.Sum256(body)
	upload := func() Upload {
		return Upload{ContentType: "audio/wav", Size: int64(len(body)), SHA256: digest, Body: bytes.NewReader(body)}
	}
	ctx := context.Background()
	var wait sync.WaitGroup
	assets := make(chan Asset, 2)
	errs := make(chan error, 2)
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			asset, createErr := service.Create(ctx, assetOwner(), "service-key", upload())
			assets <- asset
			errs <- createErr
		}()
	}
	wait.Wait()
	close(assets)
	close(errs)
	var asset Asset
	success, pending := 0, 0
	for value := range assets {
		if value.ID != "" {
			asset = value
		}
	}
	for createErr := range errs {
		if createErr == nil {
			success++
		} else if createErr == ErrPending {
			pending++
		} else {
			t.Fatal(createErr)
		}
	}
	if success < 1 || success+pending != 2 || objects.puts != 1 {
		t.Fatalf("success=%d pending=%d puts=%d", success, pending, objects.puts)
	}
	materialized, err := service.Materialize(ctx, assetOwner(), asset.ID)
	if err != nil {
		t.Fatal(err)
	}
	read, _ := io.ReadAll(materialized.Body)
	if !bytes.Equal(read, body) {
		t.Fatalf("body=%q", read)
	}
	if _, err = service.Delete(ctx, assetOwner(), asset.ID); err != nil {
		t.Fatal(err)
	}
	if processed, runErr := service.RunCleanup(ctx); runErr != nil || processed {
		t.Fatalf("cleanup raced active lease: processed=%t err=%v", processed, runErr)
	}
	if err = service.Release(ctx, materialized); err != nil {
		t.Fatal(err)
	}
	if processed, runErr := service.RunCleanup(ctx); runErr != nil || !processed || objects.deletes != 1 {
		t.Fatalf("cleanup processed=%t deletes=%d err=%v", processed, objects.deletes, runErr)
	}
}

func assetOwner() apikey.Principal {
	return apikey.Principal{OrganizationID: "org_audio_assets", ProjectID: "project_audio_assets", APIKeyID: "key_audio_assets"}
}
func assetBegin(key string) BeginRequest {
	return BeginRequest{Owner: assetOwner(), IdempotencyKey: key, Fingerprint: [32]byte{1}, ObjectKey: "audio/org_audio_assets/object.wav", ContentType: "audio/wav", ByteLength: 5, SHA256: [32]byte{2}, ExpiresAt: time.Now().Add(24 * time.Hour).UTC().Truncate(time.Microsecond)}
}

func TestRepositoryConcurrentPublicationOwnershipAndAppendOnly(t *testing.T) {
	repository, pool := repositoryFixture(t)
	ctx := context.Background()
	request := assetBegin("same-key")
	var wait sync.WaitGroup
	assets := make(chan Asset, 2)
	errs := make(chan error, 2)
	for range 2 {
		wait.Add(1)
		go func() { defer wait.Done(); asset, err := repository.Begin(ctx, request); assets <- asset; errs <- err }()
	}
	wait.Wait()
	close(assets)
	close(errs)
	var asset Asset
	success, pending := 0, 0
	for candidate := range assets {
		if candidate.ID != "" {
			asset = candidate
		}
	}
	for err := range errs {
		if err == nil {
			success++
		} else if err == ErrPending {
			pending++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || pending != 1 {
		t.Fatalf("success=%d pending=%d asset=%+v", success, pending, asset)
	}
	available, err := repository.MarkAvailable(ctx, asset.ID)
	if err != nil || available.State != Available {
		t.Fatalf("available=%+v err=%v", available, err)
	}
	resolved, err := repository.Resolve(ctx, assetOwner(), asset.ID)
	if err != nil || resolved.SHA256 != request.SHA256 {
		t.Fatalf("resolved=%+v err=%v", resolved, err)
	}
	other := apikey.Principal{OrganizationID: "org_audio_other", ProjectID: "project_audio_other", APIKeyID: "key_audio_other"}
	if _, err = repository.Resolve(ctx, other, asset.ID); err != ErrDenied {
		t.Fatalf("cross tenant err=%v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE audio_input_assets SET object_key='audio/changed.wav' WHERE id=$1`, asset.ID); err == nil {
		t.Fatal("immutable identity mutation accepted")
	}
	if _, err = pool.Exec(ctx, `DELETE FROM audio_input_asset_events WHERE asset_id=$1`, asset.ID); err == nil {
		t.Fatal("append-only event deletion accepted")
	}
	_, lease, err := repository.Acquire(ctx, assetOwner(), asset.ID, "dispatch", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repository.RequestDelete(ctx, assetOwner(), asset.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = repository.Resolve(ctx, assetOwner(), asset.ID); err != ErrDenied {
		t.Fatalf("deleting asset resolved: %v", err)
	}
	if _, _, found, claimErr := repository.ClaimCleanup(ctx, "cleanup", time.Minute); claimErr != nil || found {
		t.Fatalf("active lease cleanup found=%t err=%v", found, claimErr)
	}
	if err = repository.Release(ctx, lease); err != nil {
		t.Fatal(err)
	}
	claimed, cleanupLease, found, err := repository.ClaimCleanup(ctx, "cleanup", time.Minute)
	if err != nil || !found || claimed.ID != asset.ID {
		t.Fatalf("claimed=%+v found=%t err=%v", claimed, found, err)
	}
	if err = repository.MarkDeleted(ctx, cleanupLease); err != nil {
		t.Fatal(err)
	}
}
