//go:build integration

package videostorage

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nativegatewayhq/gateway/internal/database"
)

func videoPool(t *testing.T) *pgxpool.Pool {
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
	schema := fmt.Sprintf("video_storage_test_%d", time.Now().UnixNano())
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
	return pool
}

func TestVideoAssetLeaseAndIdentityAreDurable(t *testing.T) {
	pool := videoPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO organizations(id,name,slug) VALUES('org_video','Video','video'); INSERT INTO projects(id,organization_id,name,slug) VALUES('project_video','org_video','Video','video'); INSERT INTO service_api_keys(id,name,key_digest,key_prefix,project_id) VALUES('key_video','Video',decode(repeat('33',32),'hex'),'ngw_video','project_video'); INSERT INTO async_jobs(id,request_id,organization_id,project_id,api_key_id,protocol,operation,model,provider,channel_id,status,managed_result_required) VALUES('job_0123456789abcdef0123456789abcdef','request-video','org_video','project_video','key_video','runway','video.generate','model','runway','channel_00000000000000000000000000000007','PROCESSING',true)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE async_jobs SET managed_result_required=false WHERE id='job_0123456789abcdef0123456789abcdef'`); err == nil {
		t.Fatal("managed result policy mutation accepted")
	}
	repository, _ := NewRepository(pool)
	id, _ := AssetID("job_0123456789abcdef0123456789abcdef", 0)
	digest := sha256.Sum256([]byte("video"))
	asset, err := repository.Begin(ctx, Asset{ID: id, JobID: "job_0123456789abcdef0123456789abcdef", Provider: "runway", ChannelID: "channel_00000000000000000000000000000007", ResultIndex: 0, ObjectKey: "videos/runway/job/000.mp4", ContentType: "video/mp4", ByteLength: 5, SHA256: digest})
	if err != nil || asset.State != "PENDING" {
		t.Fatalf("asset=%+v err=%v", asset, err)
	}
	claimed, ok, err := repository.Claim(ctx, id, "worker", time.Minute)
	if err != nil || !ok || claimed.LeaseOwner != "worker" {
		t.Fatalf("claim=%+v/%v err=%v", claimed, ok, err)
	}
	available, err := repository.MarkAvailable(ctx, id, "worker")
	if err != nil || available.State != "AVAILABLE" {
		t.Fatalf("available=%+v err=%v", available, err)
	}
	conflict := asset
	conflict.ObjectKey = "videos/runway/job/different.mp4"
	if _, err = repository.Begin(ctx, conflict); err == nil {
		t.Fatal("conflicting identity accepted")
	}
	if _, err = pool.Exec(ctx, `UPDATE video_assets SET object_key='mutated.mp4' WHERE id=$1`, id); err == nil {
		t.Fatal("direct identity mutation accepted")
	}
	var events int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM video_asset_events WHERE asset_id=$1`, id).Scan(&events); err != nil || events != 3 {
		t.Fatalf("events=%d err=%v", events, err)
	}
	if _, err = pool.Exec(ctx, `UPDATE video_asset_events SET category='released' WHERE asset_id=$1`, id); err == nil {
		t.Fatal("event mutation accepted")
	}
}
