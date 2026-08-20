package pricing

import (
	"math"
	"testing"
	"time"
)

func TestMultiplyRejectsOverflow(t *testing.T) {
	if value, ok := multiply(25, 4); !ok || value != 100 {
		t.Fatalf("multiply=%d %v", value, ok)
	}
	if _, ok := multiply(math.MaxInt64, 2); ok {
		t.Fatal("overflow was accepted")
	}
	if _, ok := multiply(-1, 1); ok {
		t.Fatal("negative unit was accepted")
	}
}

func TestMarginBoundaryUsesBasisPoints(t *testing.T) {
	if !marginAllowed(8_000, 10_000, 2_000) {
		t.Fatal("exact 20% margin was rejected")
	}
	if marginAllowed(8_001, 10_000, 2_000) {
		t.Fatal("price below 20% margin was accepted")
	}
	if !marginAllowed(math.MaxInt64-1, math.MaxInt64, 0) {
		t.Fatal("large valid values overflowed")
	}
}

func TestCanonicalPriceUsesIntegerCurrencyAndMicroseconds(t *testing.T) {
	instant := time.Date(2026, 8, 20, 1, 2, 3, 456789123, time.FixedZone("offset", 9*60*60))
	price := canonicalPrice(Price{ID: "client-controlled", EffectiveFrom: instant})
	if price.ID != "" || price.Size != "default" || price.Quality != "default" || price.Currency != "USD_TICKS" {
		t.Fatalf("price=%+v", price)
	}
	if price.EffectiveFrom.Location() != time.UTC || price.EffectiveFrom.Nanosecond() != 456789000 {
		t.Fatalf("effective_from=%v", price.EffectiveFrom)
	}
}

func TestCanonicalRequestDefaultsOnlyOmittedQuantity(t *testing.T) {
	request := canonicalRequest(Request{})
	if request.Quantity != 1 || request.Size != "default" || request.Quality != "default" {
		t.Fatalf("request=%+v", request)
	}
	if validRequest(canonicalRequest(Request{Quantity: -1})) {
		t.Fatal("negative quantity was accepted")
	}
}
