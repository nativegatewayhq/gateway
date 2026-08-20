package costquota

import (
	"testing"
	"time"
)

func TestBoundsUseUTCCalendarPeriods(t *testing.T) {
	value := time.Date(2024, time.February, 29, 23, 59, 0, 0, time.FixedZone("KST", 9*60*60))
	dayStart, dayEnd := bounds(value, Day)
	if dayStart.Format(time.RFC3339) != "2024-02-29T00:00:00Z" || dayEnd.Sub(dayStart) != 24*time.Hour {
		t.Fatalf("day=%s/%s", dayStart, dayEnd)
	}
	monthStart, monthEnd := bounds(value, Month)
	if monthStart.Format(time.RFC3339) != "2024-02-01T00:00:00Z" || monthEnd.Format(time.RFC3339) != "2024-03-01T00:00:00Z" {
		t.Fatalf("month=%s/%s", monthStart, monthEnd)
	}
}

func TestValidPolicyRequiresExactScopeAndModelDimension(t *testing.T) {
	base := PolicyInput{ScopeType: Organization, OrganizationID: "org_test", Period: Day, Limit: 1, Actor: "operator", Reason: "test"}
	if !validPolicy(base) {
		t.Fatal("valid policy rejected")
	}
	base.Protocol = "openai"
	if validPolicy(base) {
		t.Fatal("partial model dimension accepted")
	}
	base.Operation = "image.generate"
	base.Model = "gpt-image-1"
	if !validPolicy(base) {
		t.Fatal("model policy rejected")
	}
}

func TestExceedsCannotOverflow(t *testing.T) {
	maximum := int64(^uint64(0) >> 1)
	if !exceeds(maximum, maximum-1, 1, 1) || exceeds(maximum, maximum-2, 1, 1) {
		t.Fatal("overflow-safe quota comparison failed")
	}
}
