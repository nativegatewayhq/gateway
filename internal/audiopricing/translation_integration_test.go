//go:build integration

package audiopricing

import (
	"context"
	"testing"
	"time"
)

func TestTranslationPricePublicationIsIdempotentAndAppendOnly(t *testing.T) {
	service, pool := transcriptionPricingFixture(t)
	ctx := context.Background()
	price := TranslationPrice{ChannelID: "channel_00000000000000000000000000000001", Model: "whisper-1", CostPerMinute: 60, SalePerMinute: 90, MaximumDurationMilliseconds: 600_000, EffectiveFrom: time.Now().Add(-time.Hour)}
	published, err := service.PublishTranslation(ctx, price, "translation-price")
	if err != nil {
		t.Fatal(err)
	}
	estimate, err := service.EstimateTranslation(ctx, TranslationPriceRequest{ChannelID: price.ChannelID, Model: price.Model})
	if err != nil || estimate.Price.ID != published.ID || estimate.MaximumCost != 600 || estimate.MaximumSale != 900 {
		t.Fatalf("estimate=%+v err=%v", estimate, err)
	}
	duplicate, err := service.PublishTranslation(ctx, price, "translation-price")
	if err != nil || duplicate.ID != published.ID {
		t.Fatalf("duplicate=%+v err=%v", duplicate, err)
	}
	price.SalePerMinute++
	if _, err = service.PublishTranslation(ctx, price, "translation-price"); err != ErrConflict {
		t.Fatalf("conflict error=%v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE audio_translation_prices SET sale_per_minute=sale_per_minute+1 WHERE id=$1`, published.ID); err == nil {
		t.Fatal("append-only price mutation accepted")
	}
	if _, err = pool.Exec(ctx, `DELETE FROM audio_translation_price_publications WHERE price_id=$1`, published.ID); err == nil {
		t.Fatal("append-only publication deletion accepted")
	}
}
