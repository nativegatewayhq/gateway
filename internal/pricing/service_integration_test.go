//go:build integration

package pricing

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nativegatewayhq/gateway/internal/database"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
)

func pricingPool(t *testing.T) *pgxpool.Pool {
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
	schema := fmt.Sprintf("pricing_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		admin.Close()
	})
	return pool
}

func TestPublishEstimateVersionAndAppendOnlyAudit(t *testing.T) {
	pool := pricingPool(t)
	service, err := NewService(pool, 2_000)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	channel, err := service.RegisterChannel(ctx, providercredentials.OpenAI, "primary")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	input := Price{ChannelID: channel.ID, Protocol: "openai", Operation: "image.generate", Model: "gpt-image-1", Size: "1024x1024", Quality: "high", UnitCost: 8_000, UnitSale: 10_000, EffectiveFrom: start, EffectiveUntil: &end}
	published, err := service.Publish(ctx, input, "openai-price-v1")
	if err != nil {
		t.Fatal(err)
	}
	retry, err := service.Publish(ctx, input, "openai-price-v1")
	if err != nil || retry.ID != published.ID {
		t.Fatalf("retry=%+v error=%v", retry, err)
	}
	conflict := input
	conflict.UnitSale++
	if _, err := service.Publish(ctx, conflict, "openai-price-v1"); !errors.Is(err, ErrPublicationConflict) {
		t.Fatalf("publication conflict=%v", err)
	}
	estimate, err := service.Estimate(ctx, Request{ChannelID: channel.ID, Protocol: "openai", Operation: "image.generate", Model: "gpt-image-1", Size: "1024x1024", Quality: "high", Quantity: 3, At: start.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if estimate.PriceID != published.ID || estimate.EstimatedCost != 24_000 || estimate.MaximumSale != 30_000 || estimate.Currency != "USD_TICKS" {
		t.Fatalf("estimate=%+v", estimate)
	}
	second := input
	second.EffectiveFrom = end
	second.EffectiveUntil = nil
	second.UnitCost = 9_000
	second.UnitSale = 12_000
	secondPrice, err := service.Publish(ctx, second, "openai-price-v2")
	if err != nil {
		t.Fatal(err)
	}
	estimate, err = service.Estimate(ctx, Request{ChannelID: channel.ID, Protocol: "openai", Operation: "image.generate", Model: "gpt-image-1", Size: "1024x1024", Quality: "high", At: end})
	if err != nil || estimate.PriceID != secondPrice.ID || estimate.MaximumSale != 12_000 {
		t.Fatalf("versioned estimate=%+v error=%v", estimate, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE provider_prices SET unit_sale=unit_sale+1 WHERE id=$1`, published.ID); err == nil {
		t.Fatal("provider price update succeeded")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM provider_prices WHERE id=$1`, published.ID); err == nil {
		t.Fatal("provider price delete succeeded")
	}
}

func TestPublishAndEstimateRunwayVideoCredits(t *testing.T) {
	pool := pricingPool(t)
	service, _ := NewService(pool, 2_000)
	ctx := context.Background()
	start := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	price := VideoPrice{Price: Price{ChannelID: "channel_00000000000000000000000000000007", Protocol: "runway", Operation: "video.generate", Model: "logical-video", Size: "text_to_video", Quality: "ratio=1280:720;audio=false", UnitCost: 10_000, UnitSale: 12_500, EffectiveFrom: start}, CreditsPerSecondMicros: 5 * ProviderCreditScale}
	published, err := service.PublishVideo(ctx, price, "runway-gen4-v1")
	if err != nil {
		t.Fatal(err)
	}
	retry, err := service.PublishVideo(ctx, price, "runway-gen4-v1")
	if err != nil || retry.ID != published.ID {
		t.Fatalf("retry=%+v err=%v", retry, err)
	}
	estimate, err := service.Estimate(ctx, Request{ChannelID: price.ChannelID, Protocol: "runway", Operation: "video.generate", Model: price.Model, Size: price.Size, Quality: price.Quality, Quantity: 5, At: start})
	if err != nil {
		t.Fatal(err)
	}
	if estimate.Quantity != 25*ProviderCreditScale || estimate.EstimatedCost != 250_000 || estimate.MaximumSale != 312_500 {
		t.Fatalf("estimate=%+v", estimate)
	}
	minimum := price
	minimum.Model = "minimum-video"
	minimum.CreditsPerSecondMicros = 2 * ProviderCreditScale
	minimum.MinimumCreditsMicros = 64 * ProviderCreditScale
	if _, err = service.PublishVideo(ctx, minimum, "runway-minimum-v1"); err != nil {
		t.Fatal(err)
	}
	estimate, err = service.Estimate(ctx, Request{ChannelID: minimum.ChannelID, Protocol: "runway", Operation: "video.generate", Model: minimum.Model, Size: minimum.Size, Quality: minimum.Quality, Quantity: 5, At: start})
	if err != nil || estimate.Quantity != 64*ProviderCreditScale {
		t.Fatalf("minimum=%+v err=%v", estimate, err)
	}
	if _, err = pool.Exec(ctx, `UPDATE video_credit_prices SET minimum_credits_micros=0 WHERE price_id=$1`, published.ID); err == nil {
		t.Fatal("video price mutation accepted")
	}
}

func TestPriceAvailabilityMarginAndOverlap(t *testing.T) {
	pool := pricingPool(t)
	service, _ := NewService(pool, 2_001)
	ctx := context.Background()
	channel, err := service.RegisterChannel(ctx, providercredentials.XAI, "xai-primary")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	price := Price{ChannelID: channel.ID, Protocol: "openai", Operation: "image.edit", Model: "grok-imagine-image-quality", UnitCost: 8_000, UnitSale: 10_000, EffectiveFrom: start}
	if _, err := service.Publish(ctx, price, "xai-edit-v1"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Estimate(ctx, Request{ChannelID: channel.ID, Protocol: "openai", Operation: "image.edit", Model: price.Model, At: start.Add(-time.Nanosecond)}); !errors.Is(err, ErrPriceUnavailable) {
		t.Fatalf("future price error=%v", err)
	}
	if _, err := service.Estimate(ctx, Request{ChannelID: channel.ID, Protocol: "openai", Operation: "image.edit", Model: price.Model, Quality: "high", At: start}); !errors.Is(err, ErrPriceUnavailable) {
		t.Fatalf("inexact selector error=%v", err)
	}
	expired := price
	expired.Model = "expired-model"
	expired.EffectiveUntil = pointerTime(start.Add(time.Hour))
	if _, err := service.Publish(ctx, expired, "expired-price"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Estimate(ctx, Request{ChannelID: channel.ID, Protocol: "openai", Operation: "image.edit", Model: expired.Model, At: start.Add(time.Hour)}); !errors.Is(err, ErrPriceUnavailable) {
		t.Fatalf("expired price error=%v", err)
	}
	if _, err := service.Estimate(ctx, Request{ChannelID: channel.ID, Protocol: "openai", Operation: "image.edit", Model: price.Model, At: start}); !errors.Is(err, ErrMarginViolation) {
		t.Fatalf("margin error=%v", err)
	}
	overlap := price
	overlap.EffectiveFrom = start.Add(time.Minute)
	if _, err := service.Publish(ctx, overlap, "xai-edit-overlap"); !errors.Is(err, ErrPriceOverlap) {
		t.Fatalf("overlap error=%v", err)
	}
	if err := service.SetChannelStatus(ctx, channel.ID, "disabled"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Estimate(ctx, Request{ChannelID: channel.ID, Protocol: "openai", Operation: "image.edit", Model: price.Model, At: start}); !errors.Is(err, ErrPriceUnavailable) {
		t.Fatalf("disabled channel error=%v", err)
	}
	if err := service.SetChannelStatus(ctx, channel.ID, "active"); err != nil {
		t.Fatal(err)
	}
	withoutMargin, _ := NewService(pool, 0)
	overflowPrice := Price{ChannelID: channel.ID, Protocol: "openai", Operation: "image.generate", Model: "overflow-model", UnitCost: 0, UnitSale: int64(^uint64(0) >> 1), EffectiveFrom: start}
	if _, err := withoutMargin.Publish(ctx, overflowPrice, "overflow-price"); err != nil {
		t.Fatal(err)
	}
	if _, err := withoutMargin.Estimate(ctx, Request{ChannelID: channel.ID, Protocol: "openai", Operation: "image.generate", Model: overflowPrice.Model, Quantity: 2, At: start}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("overflow estimate error=%v", err)
	}
}

func pointerTime(value time.Time) *time.Time { return &value }

func TestConcurrentPublicationKeyHasOnePriceEffect(t *testing.T) {
	pool := pricingPool(t)
	service, _ := NewService(pool, 0)
	ctx := context.Background()
	channel, err := service.RegisterChannel(ctx, providercredentials.OpenAI, "concurrent")
	if err != nil {
		t.Fatal(err)
	}
	price := Price{ChannelID: channel.ID, Protocol: "openai", Operation: "image.generate", Model: "gpt-image-1", UnitCost: 1, UnitSale: 2, EffectiveFrom: time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)}
	results := make(chan Price, 2)
	errorsChannel := make(chan error, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, err := service.Publish(ctx, price, "same-publication")
			results <- result
			errorsChannel <- err
		}()
	}
	wait.Wait()
	close(results)
	close(errorsChannel)
	for err := range errorsChannel {
		if err != nil {
			t.Fatal(err)
		}
	}
	ids := map[string]struct{}{}
	for result := range results {
		ids[result.ID] = struct{}{}
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM provider_prices`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || count != 1 {
		t.Fatalf("ids=%v count=%d", ids, count)
	}
}
