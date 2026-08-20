package fal

import (
	"testing"

	imageoperation "github.com/nativegatewayhq/gateway/operations/image"
)

func TestRequestedOutputUsage(t *testing.T) {
	for _, test := range []struct {
		body string
		want int64
		ok   bool
	}{{`{"prompt":"cat"}`, 1, true}, {`{"prompt":"cat","num_images":4}`, 4, true}, {`{"num_images":0}`, 0, false}, {`{"num_images":"2"}`, 0, false}, {`{"num_images":12}`, 0, false}} {
		usage, err := requestedOutputUsage([]byte(test.body), imageoperation.UsageCapability{Dimension: "output", Unit: "image", DefaultQuantity: 1, MaximumQuantity: 10, RequestExtractor: "fal-input-num_images-v1", ResultExtractor: "fal-output-v1"})
		if test.ok && (err != nil || usage.Quantity != test.want || usage.ExtractorVersion != "fal-input-num_images-v1") {
			t.Fatalf("body=%s usage=%+v err=%v", test.body, usage, err)
		}
		if !test.ok && err == nil {
			t.Fatalf("body=%s accepted", test.body)
		}
	}
}
