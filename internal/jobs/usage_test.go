package jobs

import (
	"testing"

	joboperation "github.com/nativegatewayhq/gateway/operations/job"
)

func TestUsageReasonFailsClosed(t *testing.T) {
	job := joboperation.Job{EstimatedUsage: &joboperation.Usage{Dimension: "output", Unit: "image", Quantity: 3, Provenance: "request", ExtractorVersion: "request-v1", ResultExtractorVersion: "result-v1"}}
	actual := func(quantity int64) *joboperation.Usage {
		return &joboperation.Usage{Dimension: "output", Unit: "image", Quantity: quantity, Provenance: "poll", ExtractorVersion: "result-v1"}
	}
	for _, test := range []struct {
		name   string
		status joboperation.Status
		usage  *joboperation.Usage
		want   string
	}{{"partial", joboperation.Succeeded, actual(2), ""}, {"missing", joboperation.Succeeded, nil, "usage_unknown"}, {"zero", joboperation.Succeeded, actual(0), "usage_unknown"}, {"excess", joboperation.Succeeded, actual(11), "usage_exceeds_estimate"}, {"failed output", joboperation.Failed, actual(1), "partial_terminal_conflict"}, {"failed empty", joboperation.Failed, actual(0), ""}} {
		t.Run(test.name, func(t *testing.T) {
			if got := usageReason(job, joboperation.Observation{Status: test.status, Usage: test.usage}); got != test.want {
				t.Fatalf("reason=%q want=%q", got, test.want)
			}
		})
	}
}

func TestObservedUsageHasIndependentBound(t *testing.T) {
	usage := joboperation.Usage{Dimension: "output", Unit: "image", Quantity: 11, Provenance: "poll", ExtractorVersion: "result-v1"}
	if !joboperation.ValidActualUsage(usage) {
		t.Fatal("bounded Provider excess should remain observable")
	}
	usage.Quantity = joboperation.MaximumObservedUsage + 1
	if joboperation.ValidActualUsage(usage) {
		t.Fatal("unbounded Provider usage accepted")
	}
}
