package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunValidateOnly(t *testing.T) {
	args := []string{
		"-channel-id", "channel_00000000000000000000000000000007",
		"-model", "gateway-video",
		"-task-kind", "text_to_video",
		"-quality", "ratio=1280:720;audio=false",
		"-credits-per-second-micros", "5000000",
		"-credit-cost", "10000",
		"-credit-sale", "12500",
		"-effective-from", "2026-08-22T00:00:00Z",
		"-publication-key", "runway-v1",
		"-validate-only",
	}
	var stdout, stderr bytes.Buffer
	if code := run(args, &stdout, &stderr, func(string) string { return "" }); code != 0 || stdout.String() != "valid\n" || stderr.Len() != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	args[11] = "13000"
	stdout.Reset()
	stderr.Reset()
	if code := run(args, &stdout, &stderr, func(string) string { return "" }); code != 2 || !strings.Contains(stderr.String(), "invalid") {
		t.Fatalf("invalid code=%d stderr=%q", code, stderr.String())
	}
}
