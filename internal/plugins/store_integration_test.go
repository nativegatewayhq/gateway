//go:build integration

package plugins

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nativegatewayhq/gateway/internal/database"
	manifest "github.com/nativegatewayhq/gateway/plugin-sdk/manifest/v1"
)

func pluginPool(t *testing.T) *pgxpool.Pool {
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
	schema := fmt.Sprintf("plugins_%d", time.Now().UnixNano())
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	separator := "?"
	if strings.Contains(base, "?") {
		separator = "&"
	}
	pool, err := database.Open(ctx, base+separator+"search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	if err = database.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		admin.Close()
	})
	return pool
}

func TestStorePublishesImmutableChannelSnapshot(t *testing.T) {
	pool := pluginPool(t)
	registry, err := NewRegistry([]manifest.Validated{validated(t, "provider.example", "openai")}, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(pool)
	if err = store.Sync(context.Background(), registry); err != nil {
		t.Fatal(err)
	}
	if err = store.Sync(context.Background(), registry); err != nil {
		t.Fatal(err)
	}
	binding := registry.Bindings()[0]
	var provider, id, version, model, protocol string
	var digest []byte
	if err = pool.QueryRow(context.Background(), `SELECT c.provider,s.plugin_id,s.plugin_version,s.manifest_digest,s.model,s.protocol FROM provider_channels c JOIN plugin_channel_snapshots s ON s.channel_id=c.id WHERE c.id=$1`, binding.ChannelID).Scan(&provider, &id, &version, &digest, &model, &protocol); err != nil {
		t.Fatal(err)
	}
	if provider != "plugin" || id != binding.PluginID || version != binding.Version || model != binding.Model || protocol != "openai" || len(digest) != 32 {
		t.Fatal("snapshot mismatch")
	}
	if _, err = pool.Exec(context.Background(), `UPDATE plugin_channel_snapshots SET plugin_version='1.0.1' WHERE channel_id=$1`, binding.ChannelID); err == nil {
		t.Fatal("immutable snapshot updated")
	}
}
