package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunRejectsInvalidRateLimitPolicyBeforeDatabase(t *testing.T) {
	for _, arguments := range [][]string{
		{"-name", "test", "-requests-per-minute", "60"},
		{"-name", "test", "-burst", "1"},
		{"-name", "test", "-requests-per-minute", "10", "-burst", "11"},
	} {
		var stdout, stderr bytes.Buffer
		code := run(arguments, &stdout, &stderr, func(string) string { return "postgres://secret" }, bytes.NewReader(make([]byte, 64)))
		if code != 2 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "rate limit policy is invalid") {
			t.Fatalf("args=%v code=%d stdout=%q stderr=%q", arguments, code, stdout.String(), stderr.String())
		}
	}
}
