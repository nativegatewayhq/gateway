package main

import (
	"bytes"
	"testing"
)

func TestRunRejectsInvalidArgumentsBeforeDatabase(t *testing.T) {
	for _, arguments := range [][]string{{"-action", "other", "-actor", "ops", "-reason", "test"}, {"-channel-id", "bad", "-period", "day", "-limit", "1", "-actor", "ops", "-reason", "test"}, {"-action", "disable", "-actor", "ops", "-reason", "test"}} {
		var stdout, stderr bytes.Buffer
		if code := run(arguments, &stdout, &stderr, func(string) string { return "" }); code != 2 || stdout.Len() != 0 {
			t.Fatalf("arguments=%v code=%d stdout=%q stderr=%q", arguments, code, stdout.String(), stderr.String())
		}
	}
}
