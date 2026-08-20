package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunRejectsInvalidArgumentsBeforeDatabase(t *testing.T) {
	for _, arguments := range [][]string{
		{"-action", "other", "-actor", "ops", "-reason", "test"},
		{"-scope", "organization", "-organization-id", "org_test", "-period", "day", "-limit", "zero", "-actor", "ops", "-reason", "test"},
		{"-action", "disable", "-actor", "ops", "-reason", "test"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(arguments, &stdout, &stderr, func(string) string { return "" }); code != 2 || stdout.Len() != 0 || strings.Contains(stderr.String(), "postgres") {
			t.Fatalf("arguments=%v code=%d stdout=%q stderr=%q", arguments, code, stdout.String(), stderr.String())
		}
	}
}
