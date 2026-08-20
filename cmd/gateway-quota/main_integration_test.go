//go:build integration

package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nativegatewayhq/gateway/internal/database"
)

func TestRunCreatesAndDisablesAuditedPolicy(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	admin, err := database.Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := fmt.Sprintf("quota_cli_%d", time.Now().UnixNano())
	if _, err := admin.Exec(context.Background(), "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	separator := "?"
	if strings.Contains(url, "?") {
		separator = "&"
	}
	url += separator + "search_path=" + schema
	lookup := func(key string) string {
		if key == "GATEWAY_DATABASE_URL" {
			return url
		}
		return ""
	}
	var stdout, stderr bytes.Buffer
	arguments := []string{"-scope", "organization", "-organization-id", "org_legacy", "-period", "day", "-limit", "1000", "-actor", "integration", "-reason", "CLI test"}
	if code := run(arguments, &stdout, &stderr, lookup); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	policyID := strings.TrimSpace(stdout.String())
	if !strings.HasPrefix(policyID, "quota_") || strings.Contains(stderr.String(), policyID) {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	pool, err := database.Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"-action", "disable", "-policy-id", policyID, "-actor", "integration", "-reason", "CLI cleanup"}, &stdout, &stderr, lookup); code != 0 {
		t.Fatalf("disable=%d stderr=%s", code, stderr.String())
	}
	var status string
	var events int
	if err := pool.QueryRow(context.Background(), `SELECT status FROM cost_quota_policies WHERE id=$1`, policyID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM cost_quota_policy_events WHERE policy_id=$1`, policyID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if status != "disabled" || events != 2 {
		t.Fatalf("status=%s events=%d", status, events)
	}
}
