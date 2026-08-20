package image

import (
	"errors"
	"sort"
	"time"

	"github.com/nativegatewayhq/gateway/internal/ledger"
	"github.com/nativegatewayhq/gateway/internal/pricing"
)

var ErrInvalidRouteQuote = errors.New("invalid route quote")

type QuotedCandidate struct {
	Decision RoutingDecision
	Estimate pricing.Estimate
}

func OrderLowestCost(candidates []RoutingDecision, estimates map[string]pricing.Estimate, evaluatedAt time.Time, quantity int64) ([]QuotedCandidate, error) {
	if evaluatedAt.IsZero() || quantity < 1 {
		return nil, ErrInvalidRouteQuote
	}
	evaluatedAt = evaluatedAt.UTC()
	ordered := make([]QuotedCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		estimate, exists := estimates[candidate.ChannelID]
		if !exists {
			continue
		}
		if candidate.Policy != LowestCost || estimate.PriceID == "" || estimate.ChannelID != candidate.ChannelID || estimate.Currency != ledger.Currency || estimate.Quantity != quantity || estimate.EstimatedCost <= 0 || estimate.MaximumSale <= 0 || !estimate.EvaluatedAt.Equal(evaluatedAt) {
			return nil, ErrInvalidRouteQuote
		}
		ordered = append(ordered, QuotedCandidate{Decision: candidate, Estimate: estimate})
	}
	sort.Slice(ordered, func(left, right int) bool {
		if ordered[left].Estimate.EstimatedCost != ordered[right].Estimate.EstimatedCost {
			return ordered[left].Estimate.EstimatedCost < ordered[right].Estimate.EstimatedCost
		}
		if ordered[left].Decision.Priority != ordered[right].Decision.Priority {
			return ordered[left].Decision.Priority < ordered[right].Decision.Priority
		}
		return ordered[left].Decision.CandidateID < ordered[right].Decision.CandidateID
	})
	return ordered, nil
}
