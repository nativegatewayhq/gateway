package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunRejectsInvalidArgumentsBeforeReadingDatabase(t *testing.T) {
	tests := [][]string{
		{"-action", "other"},
		{"-action", "stage", "-channel-id", "bad", "-provider", "openai"},
		{"-action", "activate", "-actor", "ops", "-reason", "rotate", "-operation-key", "op"},
		{"-action", "list"},
	}
	for _, arguments := range tests {
		var stdout, stderr bytes.Buffer
		code := run(arguments, strings.NewReader("stdin-secret"), &stdout, &stderr, func(string) (string, bool) { return "", false })
		if code != 2 || stdout.Len() != 0 || strings.Contains(stderr.String(), "stdin-secret") {
			t.Fatalf("arguments=%v code=%d stdout=%q stderr=%q", arguments, code, stdout.String(), stderr.String())
		}
	}
}

func TestRunDoesNotEchoSecretOnMissingConfiguration(t *testing.T) {
	secret := "cli-provider-secret"
	var stdout, stderr bytes.Buffer
	code := run([]string{"-action", "stage", "-channel-id", "channel_00000000000000000000000000000001", "-provider", "openai", "-actor", "ops", "-reason", "rotate", "-operation-key", "op"}, strings.NewReader(secret), &stdout, &stderr, func(string) (string, bool) { return "", false })
	if code != 1 || strings.Contains(stdout.String()+stderr.String(), secret) {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}
