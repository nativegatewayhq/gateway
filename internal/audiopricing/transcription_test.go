package audiopricing

import (
	"math"
	"testing"
	"time"
)

func tokenTranscriptionPrice() TranscriptionPrice {
	return canonicalTranscriptionPrice(TranscriptionPrice{ChannelID: "channel_00000000000000000000000000000001", Model: "gpt-4o-transcribe", Strategy: TranscriptionTokenStrategy, CostInputPerMillion: 25, CostOutputPerMillion: 100, SaleInputPerMillion: 50, SaleOutputPerMillion: 200, MaximumInputTokens: 1000, MaximumOutputTokens: 2000, EffectiveFrom: time.Now()})
}

func TestTranscriptionTokenActualUsesTypedUsage(t *testing.T) {
	p := tokenTranscriptionPrice()
	usage := TranscriptionUsage{Type: TranscriptionTokens, InputTokens: 100, AudioInputTokens: 90, TextInputTokens: 10, OutputTokens: 20, TotalTokens: 120}
	actual, err := CalculateTranscriptionActual(p, usage, 0)
	if err != nil || actual.Cost != 2 || actual.Sale != 2 {
		t.Fatalf("actual=%+v err=%v", actual, err)
	}
	for _, invalid := range []TranscriptionUsage{{Type: TranscriptionTokens, InputTokens: 100, AudioInputTokens: 99, OutputTokens: 20, TotalTokens: 120}, {Type: TranscriptionTokens, InputTokens: 1001, AudioInputTokens: 1001, OutputTokens: 1, TotalTokens: 1002}, {Type: TranscriptionDuration, DurationMilliseconds: 1}} {
		if _, err = CalculateTranscriptionActual(p, invalid, 0); err == nil {
			t.Fatalf("invalid usage accepted: %+v", invalid)
		}
	}
}

func TestTranscriptionDurationAmountCeilsWithoutFloat(t *testing.T) {
	p := canonicalTranscriptionPrice(TranscriptionPrice{ChannelID: "channel_00000000000000000000000000000001", Model: "gpt-transcribe", Strategy: TranscriptionDurationStrategy, CostPerMinute: 60, SalePerMinute: 120, MaximumDurationMilliseconds: 60_000, EffectiveFrom: time.Now()})
	actual, err := CalculateTranscriptionActual(p, TranscriptionUsage{Type: TranscriptionDuration, DurationMilliseconds: 1}, 0)
	if err != nil || actual.Cost != 1 || actual.Sale != 1 {
		t.Fatalf("actual=%+v err=%v", actual, err)
	}
	if _, ok := durationAmount(math.MaxInt64, math.MaxInt64); ok {
		t.Fatal("duration overflow accepted")
	}
}

func TestTranscriptionPriceStrategiesAreExclusive(t *testing.T) {
	if !validTranscriptionPrice(tokenTranscriptionPrice()) {
		t.Fatal("token price rejected")
	}
	p := tokenTranscriptionPrice()
	p.CostPerMinute = 1
	if validTranscriptionPrice(p) {
		t.Fatal("mixed strategy accepted")
	}
	p = tokenTranscriptionPrice()
	p.SaleInputPerMillion = p.CostInputPerMillion
	if transcriptionRateMarginsOK(p, 1) {
		t.Fatal("per-dimension margin violation accepted")
	}
}

func TestCanonicalTranscriptionPriceMatchesPostgresTimestampPrecision(t *testing.T) {
	from := time.Date(2026, 8, 24, 3, 36, 21, 987654321, time.FixedZone("KST", 9*60*60))
	until := from.Add(time.Hour)
	p := canonicalTranscriptionPrice(TranscriptionPrice{EffectiveFrom: from, EffectiveUntil: &until})
	if p.EffectiveFrom.Location() != time.UTC || p.EffectiveFrom.Nanosecond() != 987654000 {
		t.Fatalf("effective_from=%s", p.EffectiveFrom)
	}
	if p.EffectiveUntil == nil || p.EffectiveUntil.Location() != time.UTC || p.EffectiveUntil.Nanosecond() != 987654000 {
		t.Fatalf("effective_until=%v", p.EffectiveUntil)
	}
}
