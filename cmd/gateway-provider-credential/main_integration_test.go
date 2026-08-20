//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nativegatewayhq/gateway/internal/database"
)

func TestRunCredentialLifecycleWithoutSecretOutput(t *testing.T) {
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	admin, err := database.Open(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := fmt.Sprintf("credential_cli_%d", time.Now().UnixNano())
	if _, err := admin.Exec(context.Background(), "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	separator := "?"
	if strings.Contains(base, "?") {
		separator = "&"
	}
	url := base + separator + "search_path=" + schema
	values := map[string]string{
		"GATEWAY_DATABASE_URL":                       url,
		"GATEWAY_PROVIDER_CREDENTIAL_CURRENT_KEY_ID": "integration",
		"GATEWAY_PROVIDER_CREDENTIAL_KEY_IDS":        "integration",
		"GATEWAY_PROVIDER_CREDENTIAL_KEY_0":          base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)),
	}
	lookup := func(key string) (string, bool) { value, ok := values[key]; return value, ok }
	secret := "cli-integration-provider-secret"
	var stdout, stderr bytes.Buffer
	stageArgs := []string{"-action", "stage", "-channel-id", "channel_00000000000000000000000000000001", "-provider", "openai", "-actor", "integration", "-reason", "rotation", "-operation-key", "cli-stage"}
	if code := run(stageArgs, strings.NewReader(secret+"\n"), &stdout, &stderr, lookup); code != 0 {
		t.Fatalf("stage=%d stderr=%s", code, stderr.String())
	}
	if strings.Contains(stdout.String()+stderr.String(), secret) {
		t.Fatal("stage output leaked secret")
	}
	fields := strings.Fields(stdout.String())
	if len(fields) != 5 || !strings.HasPrefix(fields[0], "pcred_") || fields[4] != "staged" {
		t.Fatalf("stage output=%q", stdout.String())
	}
	id := fields[0]
	stdout.Reset()
	stderr.Reset()
	activateArgs := []string{"-action", "activate", "-credential-id", id, "-actor", "integration", "-reason", "rotation", "-operation-key", "cli-activate"}
	if code := run(activateArgs, strings.NewReader("unused"), &stdout, &stderr, lookup); code != 0 || !strings.Contains(stdout.String(), "active") {
		t.Fatalf("activate=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"-action", "list", "-channel-id", "channel_00000000000000000000000000000001"}, strings.NewReader("unused"), &stdout, &stderr, lookup); code != 0 || !strings.Contains(stdout.String(), id) || strings.Contains(stdout.String()+stderr.String(), secret) {
		t.Fatalf("list=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
}
