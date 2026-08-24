//go:build integration

package audiopricing

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nativegatewayhq/gateway/internal/database"
)

func transcriptionPricingFixture(t *testing.T) (*Service, *pgxpool.Pool) {
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
	schema := fmt.Sprintf("transcription_pricing_%d", time.Now().UnixNano())
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
	service, err := New(pool, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return service, pool
}

func TestTranscriptionPricePublicationStrategiesAndAppendOnlyGuard(t *testing.T) {
	service, pool := transcriptionPricingFixture(t)
	ctx := context.Background()
	effective := time.Now().Add(-time.Hour)
	token := TranscriptionPrice{ChannelID: "channel_00000000000000000000000000000001", Model: "gpt-4o-transcribe", Strategy: TranscriptionTokenStrategy, CostInputPerMillion: 2_500_000, CostOutputPerMillion: 10_000_000, SaleInputPerMillion: 3_000_000, SaleOutputPerMillion: 12_000_000, MaximumInputTokens: 16_000, MaximumOutputTokens: 2_000, EffectiveFrom: effective}
	published, err := service.PublishTranscription(ctx, token, "token-price")
	if err != nil {
		t.Fatal(err)
	}
	estimate, err := service.EstimateTranscription(ctx, TranscriptionPriceRequest{ChannelID: token.ChannelID, Model: token.Model})
	if err != nil || estimate.Price.ID != published.ID || estimate.MaximumCost != 60_000 || estimate.MaximumSale != 72_000 {
		t.Fatalf("estimate=%+v err=%v", estimate, err)
	}
	if _, err = service.PublishTranscription(ctx, token, "token-price"); err != nil {
		t.Fatal(err)
	}
	token.SaleInputPerMillion++
	if _, err = service.PublishTranscription(ctx, token, "token-price"); err != ErrConflict {
		t.Fatalf("publication conflict=%v", err)
	}
	duration := TranscriptionPrice{ChannelID: "channel_00000000000000000000000000000001", Model: "gpt-transcribe", Strategy: TranscriptionDurationStrategy, CostPerMinute: 45, SalePerMinute: 60, MaximumDurationMilliseconds: 600_000, EffectiveFrom: effective}
	if _, err = service.PublishTranscription(ctx, duration, "duration-price"); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE audio_transcription_prices SET sale_per_minute=sale_per_minute+1 WHERE model='gpt-transcribe'`); err == nil {
		t.Fatal("append-only price mutation accepted")
	}
}
