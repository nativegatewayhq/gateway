//go:build integration

package imagestorage

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nativegatewayhq/gateway/internal/database"
)

type countingObjectStore struct{ puts atomic.Int32 }

func (store *countingObjectStore) Put(_ context.Context, object Object, body io.Reader) (StoredObject, error) {
	store.puts.Add(1)
	_, _ = io.Copy(io.Discard, body)
	time.Sleep(100 * time.Millisecond)
	return StoredObject{Key: object.Key, URL: "https://cdn.example.test/assets/" + object.Key, ContentType: object.ContentType, Size: object.Size, SHA256: object.SHA256}, nil
}
func (*countingObjectStore) Ready(context.Context) error { return nil }

func assetPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	ctx := context.Background()
	admin, err := database.Open(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("image_assets_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	separator := "?"
	if strings.Contains(base, "?") {
		separator = "&"
	}
	pool, err := database.Open(ctx, base+separator+"search_path="+schema)
	if err != nil {
		admin.Close()
		t.Fatal(err)
	}
	if err := database.Migrate(ctx, pool); err != nil {
		pool.Close()
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		admin.Close()
	})
	return pool
}

func TestAssetStoreConcurrentBeginAndIdempotentAvailable(t *testing.T) {
	store := NewAssetStore(assetPool(t))
	ctx := context.Background()
	digest := sha256.Sum256([]byte("image"))
	id, _ := AssetID("openai", "request_asset", 0)
	asset := Asset{ID: id, RequestID: "request_asset", Protocol: "openai", Provider: "openai", ChannelID: "channel_00000000000000000000000000000001", ResultIndex: 0, ObjectKey: "images/openai/request_asset/000-image.png", ContentType: "image/png", ByteLength: 5, SHA256: digest}
	var wait sync.WaitGroup
	errors := make(chan error, 8)
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			stored, err := store.Begin(ctx, asset)
			if err == nil && (stored.ID != id || stored.State != Pending) {
				err = fmt.Errorf("unexpected asset: %+v", stored)
			}
			errors <- err
		}()
	}
	wait.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	claimed, ok, err := store.Claim(ctx, id, "worker-one", time.Minute)
	if err != nil || !ok || claimed.LeaseOwner != "worker-one" {
		t.Fatalf("claim=%+v ok=%v err=%v", claimed, ok, err)
	}
	if _, ok, err := store.Claim(ctx, id, "worker-two", time.Minute); err != nil || ok {
		t.Fatalf("second claim ok=%v err=%v", ok, err)
	}
	available, err := store.MarkAvailable(ctx, id, "worker-one")
	if err != nil || available.State != Available {
		t.Fatalf("available=%+v err=%v", available, err)
	}
	if _, err := store.MarkAvailable(ctx, id, "worker-one"); err != nil {
		t.Fatalf("idempotent available=%v", err)
	}
	conflict := asset
	conflict.ObjectKey = "images/openai/request_asset/different.png"
	if _, err := store.Begin(ctx, conflict); err == nil {
		t.Fatal("content conflict accepted")
	}
}

func TestManagersAcrossSharedStoreIssueOneObjectPut(t *testing.T) {
	assets := NewAssetStore(assetPool(t))
	config := collectorTestConfig(t)
	objects := &countingObjectStore{}
	collectorOne, _ := NewCollector(config)
	collectorTwo, _ := NewCollector(config)
	first, _ := NewManager(config, collectorOne, objects, assets)
	second, _ := NewManager(config, collectorTwo, objects, assets)
	input := TransformInput{Protocol: "openai", Provider: "openai", ChannelID: "channel_00000000000000000000000000000001", RequestID: "request_shared_managers", Body: []byte(`{"data":[{"b64_json":"` + testPNGBase64() + `"}]}`)}
	results := make(chan []byte, 2)
	errors := make(chan error, 2)
	for _, manager := range []*Manager{first, second} {
		go func(manager *Manager) {
			result, err := manager.Transform(context.Background(), input)
			results <- result
			errors <- err
		}(manager)
	}
	var bodies [][]byte
	for range 2 {
		bodies = append(bodies, <-results)
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
	}
	if objects.puts.Load() != 1 || string(bodies[0]) != string(bodies[1]) {
		t.Fatalf("puts=%d bodies=%q", objects.puts.Load(), bodies)
	}
}
