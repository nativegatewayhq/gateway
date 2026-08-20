package replicate

import (
	"testing"

	imageoperation "github.com/nativegatewayhq/gateway/operations/image"
)

func TestRequestedOutputUsage(t *testing.T) {
	for _, test := range []struct {
		body string
		want int64
		ok   bool
	}{{`{"version":"x","input":{"prompt":"cat"}}`, 1, true}, {`{"version":"x","input":{"num_outputs":3}}`, 3, true}, {`{"version":"x","input":{"num_outputs":0}}`, 0, false}, {`{"version":"x","input":{"num_outputs":1.5}}`, 0, false}, {`{"version":"x","input":{"num_outputs":11}}`, 0, false}} {
		usage, err := requestedOutputUsage([]byte(test.body), imageoperation.UsageCapability{Dimension: "output", Unit: "image", DefaultQuantity: 1, MaximumQuantity: 10, RequestExtractor: "replicate-input-num_outputs-v1", ResultExtractor: "replicate-output-v1"})
		if test.ok && (err != nil || usage.Quantity != test.want || usage.ExtractorVersion != "replicate-input-num_outputs-v1") {
			t.Fatalf("body=%s usage=%+v err=%v", test.body, usage, err)
		}
		if !test.ok && err == nil {
			t.Fatalf("body=%s accepted", test.body)
		}
	}
}
