package audiopricing

import (
	"math"
	"testing"
	"time"
)

func TestAmountCeilsAndRejectsOverflow(t *testing.T) {
	for _, tc := range []struct{ q, r, w int64 }{{1, 1, 1}, {1_000_000, 7, 7}, {1_000_001, 7, 8}, {0, 9, 0}} {
		got, ok := Amount(tc.q, tc.r)
		if !ok || got != tc.w {
			t.Fatalf("Amount(%d,%d)=%d/%v", tc.q, tc.r, got, ok)
		}
	}
	if _, ok := Amount(math.MaxInt64, math.MaxInt64); ok {
		t.Fatal("overflow accepted")
	}
}

func TestOnlyCharacterStrategyIsValid(t *testing.T) {
	p := canonical(Price{ChannelID: "channel_00000000000000000000000000000000", Model: "tts-1", CostPerMillion: 2, SalePerMillion: 3, EffectiveFrom: time.Now()})
	if !validPrice(p) {
		t.Fatal("valid price rejected")
	}
	p.Strategy = "duration"
	if validPrice(p) {
		t.Fatal("unsupported strategy accepted")
	}
}
