package image

import (
	"errors"
	"testing"
	"time"

	"github.com/nativegatewayhq/gateway/internal/ledger"
	"github.com/nativegatewayhq/gateway/internal/pricing"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
)

func TestOrderLowestCostUsesCostThenPriorityThenCandidateID(t *testing.T) {
	at := time.Date(2026, 8, 20, 10, 0, 0, 123000, time.UTC)
	candidates := []RoutingDecision{
		{CandidateID: "candidate_z", ChannelID: "channel_00000000000000000000000000000001", Provider: providercredentials.OpenAI, Policy: LowestCost, Priority: 1},
		{CandidateID: "candidate_b", ChannelID: "channel_00000000000000000000000000000002", Provider: providercredentials.XAI, Policy: LowestCost, Priority: 2},
		{CandidateID: "candidate_a", ChannelID: "channel_00000000000000000000000000000003", Provider: providercredentials.Google, Policy: LowestCost, Priority: 2},
	}
	estimates := map[string]pricing.Estimate{
		candidates[0].ChannelID: {PriceID: "price_00000000000000000000000000000001", ChannelID: candidates[0].ChannelID, Currency: ledger.Currency, Quantity: 1, EstimatedCost: 20, MaximumSale: 30, EvaluatedAt: at},
		candidates[1].ChannelID: {PriceID: "price_00000000000000000000000000000002", ChannelID: candidates[1].ChannelID, Currency: ledger.Currency, Quantity: 1, EstimatedCost: 10, MaximumSale: 40, EvaluatedAt: at},
		candidates[2].ChannelID: {PriceID: "price_00000000000000000000000000000003", ChannelID: candidates[2].ChannelID, Currency: ledger.Currency, Quantity: 1, EstimatedCost: 10, MaximumSale: 50, EvaluatedAt: at},
	}
	ordered, err := OrderLowestCost(candidates, estimates, at, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(ordered) != 3 || ordered[0].Decision.CandidateID != "candidate_a" || ordered[1].Decision.CandidateID != "candidate_b" || ordered[2].Decision.CandidateID != "candidate_z" {
		t.Fatalf("ordered=%+v", ordered)
	}
}

func TestOrderLowestCostRejectsInvalidBoundQuote(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Microsecond)
	candidate := RoutingDecision{CandidateID: "candidate", ChannelID: "channel_00000000000000000000000000000001", Policy: LowestCost}
	tests := []pricing.Estimate{
		{PriceID: "price_1", ChannelID: "channel_00000000000000000000000000000002", Currency: ledger.Currency, Quantity: 1, EstimatedCost: 1, MaximumSale: 2, EvaluatedAt: at},
		{PriceID: "price_1", ChannelID: candidate.ChannelID, Currency: "USD", Quantity: 1, EstimatedCost: 1, MaximumSale: 2, EvaluatedAt: at},
		{PriceID: "price_1", ChannelID: candidate.ChannelID, Currency: ledger.Currency, Quantity: 1, EstimatedCost: -1, MaximumSale: 2, EvaluatedAt: at},
		{PriceID: "price_1", ChannelID: candidate.ChannelID, Currency: ledger.Currency, Quantity: 1, EstimatedCost: 0, MaximumSale: 2, EvaluatedAt: at},
		{PriceID: "price_1", ChannelID: candidate.ChannelID, Currency: ledger.Currency, Quantity: 2, EstimatedCost: 1, MaximumSale: 2, EvaluatedAt: at},
		{PriceID: "price_1", ChannelID: candidate.ChannelID, Currency: ledger.Currency, Quantity: 1, EstimatedCost: 1, MaximumSale: 2, EvaluatedAt: at.Add(time.Second)},
	}
	for _, estimate := range tests {
		if _, err := OrderLowestCost([]RoutingDecision{candidate}, map[string]pricing.Estimate{candidate.ChannelID: estimate}, at, 1); !errors.Is(err, ErrInvalidRouteQuote) {
			t.Fatalf("estimate=%+v err=%v", estimate, err)
		}
	}
}
