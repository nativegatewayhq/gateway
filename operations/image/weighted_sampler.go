package image

import (
	"encoding/binary"
	"errors"
	"io"
	"sort"
	"sync"
)

var ErrWeightedSampling = errors.New("weighted sampling failed")

type WeightedSampler interface {
	Pick([]RoutingDecision) (RoutingDecision, error)
}

type weightedSampler struct {
	entropy io.Reader
	mu      sync.Mutex
}

func NewWeightedSampler(entropy io.Reader) (WeightedSampler, error) {
	if entropy == nil {
		return nil, ErrWeightedSampling
	}
	return &weightedSampler{entropy: entropy}, nil
}

func (sampler *weightedSampler) Pick(candidates []RoutingDecision) (RoutingDecision, error) {
	if sampler == nil || sampler.entropy == nil || len(candidates) == 0 || len(candidates) > MaxRouteCandidates {
		return RoutingDecision{}, ErrWeightedSampling
	}
	ordered := append([]RoutingDecision(nil), candidates...)
	sort.Slice(ordered, func(left, right int) bool { return ordered[left].CandidateID < ordered[right].CandidateID })
	var total uint64
	for index, candidate := range ordered {
		if candidate.Policy != Weighted || !validCandidateID(candidate.CandidateID) || candidate.Weight == 0 || candidate.Weight > MaxCandidateWeight || (index > 0 && ordered[index-1].CandidateID == candidate.CandidateID) {
			return RoutingDecision{}, ErrWeightedSampling
		}
		total += uint64(candidate.Weight)
		if total > MaxTotalWeight {
			return RoutingDecision{}, ErrWeightedSampling
		}
	}
	draw, err := sampler.uniform(total)
	if err != nil {
		return RoutingDecision{}, err
	}
	var cumulative uint64
	for _, candidate := range ordered {
		cumulative += uint64(candidate.Weight)
		if draw < cumulative {
			return candidate, nil
		}
	}
	return RoutingDecision{}, ErrWeightedSampling
}

func (sampler *weightedSampler) uniform(upper uint64) (uint64, error) {
	if upper == 0 {
		return 0, ErrWeightedSampling
	}
	threshold := -upper % upper
	var raw [8]byte
	sampler.mu.Lock()
	defer sampler.mu.Unlock()
	for attempt := 0; attempt < 128; attempt++ {
		if _, err := io.ReadFull(sampler.entropy, raw[:]); err != nil {
			return 0, errors.Join(ErrWeightedSampling, err)
		}
		value := binary.BigEndian.Uint64(raw[:])
		if value >= threshold {
			return value % upper, nil
		}
	}
	return 0, ErrWeightedSampling
}
