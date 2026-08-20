//go:build integration

package imagestorage

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nativegatewayhq/gateway/internal/database"
)

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
	available, err := store.MarkAvailable(ctx, id)
	if err != nil || available.State != Available {
		t.Fatalf("available=%+v err=%v", available, err)
	}
	if _, err := store.MarkAvailable(ctx, id); err != nil {
		t.Fatalf("idempotent available=%v", err)
	}
	conflict := asset
	conflict.ObjectKey = "images/openai/request_asset/different.png"
	if _, err := store.Begin(ctx, conflict); err == nil {
		t.Fatal("content conflict accepted")
	}
}
