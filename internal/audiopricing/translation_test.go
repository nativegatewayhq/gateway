package audiopricing

import (
	"math"
	"testing"
	"time"
)

func translationPrice() TranslationPrice {
	return canonicalTranslationPrice(TranslationPrice{ChannelID: "channel_00000000000000000000000000000001", Model: "whisper-1", CostPerMinute: 60, SalePerMinute: 120, MaximumDurationMilliseconds: 600_000, EffectiveFrom: time.Now()})
}

func TestTranslationDurationPricingCeilsAndEnforcesBound(t *testing.T) {
	price := translationPrice()
	for _, test := range []struct {
		duration, cost, sale int64
	}{{1, 1, 1}, {60_000, 60, 120}, {60_001, 61, 121}, {600_000, 600, 1200}} {
		actual, err := calculateTranslation(price, test.duration, 0)
		if err != nil || actual.Cost != test.cost || actual.Sale != test.sale {
			t.Fatalf("duration=%d actual=%+v err=%v", test.duration, actual, err)
		}
	}
	if _, err := calculateTranslation(price, price.MaximumDurationMilliseconds+1, 0); err != ErrInvalid {
		t.Fatalf("over-bound error=%v", err)
	}
	if _, ok := durationAmount(math.MaxInt64, math.MaxInt64); ok {
		t.Fatal("overflow accepted")
	}
}

func TestTranslationPricingRejectsInsufficientMargin(t *testing.T) {
	price := translationPrice()
	price.SalePerMinute = price.CostPerMinute
	if _, err := calculateTranslation(price, 1_000, 1); err != ErrMargin {
		t.Fatalf("margin error=%v", err)
	}
}
