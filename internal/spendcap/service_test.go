package spendcap

import (
	"testing"
	"time"
)

func TestBoundsAndOverflow(t *testing.T) {
	value := time.Date(2024, time.February, 29, 23, 0, 0, 0, time.FixedZone("KST", 9*60*60))
	start, end := bounds(value, Month)
	if start.Format(time.RFC3339) != "2024-02-01T00:00:00Z" || end.Format(time.RFC3339) != "2024-03-01T00:00:00Z" {
		t.Fatalf("bounds=%s/%s", start, end)
	}
	maximum := int64(^uint64(0) >> 1)
	if !exceeds(maximum, maximum-1, 1, 1) || exceeds(maximum, maximum-2, 1, 1) {
		t.Fatal("overflow comparison failed")
	}
}

func TestPolicyValidation(t *testing.T) {
	input := PolicyInput{ChannelID: "channel_00000000000000000000000000000001", Period: Day, Limit: 1, Actor: "ops", Reason: "test"}
	if ValidatePolicy(input) != nil {
		t.Fatal("valid policy rejected")
	}
	input.Limit = 0
	if ValidatePolicy(input) == nil {
		t.Fatal("zero limit accepted")
	}
}
