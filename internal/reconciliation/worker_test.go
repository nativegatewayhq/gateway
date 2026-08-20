package reconciliation

import (
	"testing"
	"time"
)

func TestBackoffDoublesAndCaps(t *testing.T) {
	base, maximum := time.Second, 5*time.Second
	for _, test := range []struct {
		attempt int
		want    time.Duration
	}{{1, time.Second}, {2, 2 * time.Second}, {3, 4 * time.Second}, {4, 5 * time.Second}, {20, 5 * time.Second}} {
		if got := backoff(base, maximum, test.attempt); got != test.want {
			t.Fatalf("attempt=%d got=%v want=%v", test.attempt, got, test.want)
		}
	}
}
