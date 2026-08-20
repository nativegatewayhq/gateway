package image

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/nativegatewayhq/gateway/internal/providercredentials"
)

func weightedDecision(id string, weight uint32) RoutingDecision {
	return RoutingDecision{CandidateID: id, Policy: Weighted, Weight: weight, Provider: providercredentials.OpenAI}
}

func entropyValues(values ...uint64) io.Reader {
	content := make([]byte, 8*len(values))
	for index, value := range values {
		binary.BigEndian.PutUint64(content[index*8:], value)
	}
	return bytes.NewReader(content)
}

func TestWeightedSamplerUsesCanonicalCandidateIntervals(t *testing.T) {
	candidates := []RoutingDecision{weightedDecision("candidate_b", 2), weightedDecision("candidate_a", 1)}
	for _, test := range []struct {
		draw uint64
		want string
	}{{3, "candidate_a"}, {1, "candidate_b"}, {2, "candidate_b"}} {
		sampler, err := NewWeightedSampler(entropyValues(test.draw))
		if err != nil {
			t.Fatal(err)
		}
		selected, err := sampler.Pick(candidates)
		if err != nil || selected.CandidateID != test.want {
			t.Fatalf("draw=%d selected=%+v err=%v", test.draw, selected, err)
		}
	}
}

func TestWeightedSamplerRejectsModuloBiasRange(t *testing.T) {
	// For an upper bound of three, zero is below the rejection threshold and four maps to one.
	sampler, _ := NewWeightedSampler(entropyValues(0, 4))
	selected, err := sampler.Pick([]RoutingDecision{weightedDecision("candidate_a", 1), weightedDecision("candidate_b", 2)})
	if err != nil || selected.CandidateID != "candidate_b" {
		t.Fatalf("selected=%+v err=%v", selected, err)
	}
}

func TestWeightedSamplerRejectsInvalidInputAndEntropyFailure(t *testing.T) {
	for _, candidates := range [][]RoutingDecision{
		nil,
		{{CandidateID: "candidate", Policy: Priority, Weight: 1}},
		{{CandidateID: "candidate", Policy: Weighted, Weight: 0}},
		{{CandidateID: "candidate", Policy: Weighted, Weight: MaxCandidateWeight + 1}},
		{weightedDecision("candidate", 1), weightedDecision("candidate", 2)},
	} {
		sampler, _ := NewWeightedSampler(entropyValues(1))
		if _, err := sampler.Pick(candidates); !errors.Is(err, ErrWeightedSampling) {
			t.Fatalf("candidates=%+v err=%v", candidates, err)
		}
	}
	sampler, _ := NewWeightedSampler(bytes.NewReader([]byte{1, 2, 3}))
	if _, err := sampler.Pick([]RoutingDecision{weightedDecision("candidate", 1)}); !errors.Is(err, ErrWeightedSampling) {
		t.Fatalf("short entropy err=%v", err)
	}
	if _, err := NewWeightedSampler(nil); !errors.Is(err, ErrWeightedSampling) {
		t.Fatalf("nil entropy err=%v", err)
	}
}

func TestWeightedSamplerDistributionMatchesIntegerWeights(t *testing.T) {
	const draws = 3_000
	values := make([]uint64, draws)
	for index := range values {
		values[index] = uint64(index + 1) // Avoid the rejection value for upper bound three.
	}
	sampler, _ := NewWeightedSampler(entropyValues(values...))
	counts := map[string]int{}
	candidates := []RoutingDecision{weightedDecision("candidate_a", 1), weightedDecision("candidate_b", 2)}
	for range draws {
		selected, err := sampler.Pick(candidates)
		if err != nil {
			t.Fatal(err)
		}
		counts[selected.CandidateID]++
	}
	if counts["candidate_a"] != 1_000 || counts["candidate_b"] != 2_000 {
		t.Fatalf("counts=%v", counts)
	}
}

func TestWeightedSamplerSerializesSharedEntropy(t *testing.T) {
	const workers = 100
	sampler, _ := NewWeightedSampler(bytes.NewReader(make([]byte, workers*8)))
	candidate := weightedDecision("candidate", 1)
	var group sync.WaitGroup
	errorsFound := make(chan error, workers)
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			selected, err := sampler.Pick([]RoutingDecision{candidate})
			if err != nil || selected.CandidateID != candidate.CandidateID {
				errorsFound <- err
			}
		}()
	}
	group.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Fatalf("shared sampler error=%v", err)
	}
}
