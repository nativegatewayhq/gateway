//go:build integration

package main

import (
	"bytes"
	"context"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/nativegatewayhq/gateway/internal/database"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRunCreatesAndDisablesSpendCap(t *testing.T) {
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	admin, err := database.Open(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := fmt.Sprintf("spend_cli_%d", time.Now().UnixNano())
	if _, err := admin.Exec(context.Background(), "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	separator := "?"
	if strings.Contains(base, "?") {
		separator = "&"
	}
	url := base + separator + "search_path=" + schema
	lookup := func(key string) string {
		if key == "GATEWAY_DATABASE_URL" {
			return url
		}
		return ""
	}
	var stdout, stderr bytes.Buffer
	arguments := []string{"-channel-id", "channel_00000000000000000000000000000001", "-period", "day", "-limit", "1000", "-actor", "integration", "-reason", "CLI test"}
	if code := run(arguments, &stdout, &stderr, lookup); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	id := strings.TrimSpace(stdout.String())
	if !strings.HasPrefix(id, "spcap_") {
		t.Fatalf("id=%q", id)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"-action", "disable", "-policy-id", id, "-actor", "integration", "-reason", "cleanup"}, &stdout, &stderr, lookup); code != 0 {
		t.Fatalf("disable=%d stderr=%s", code, stderr.String())
	}
	pool, err := database.Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var status string
	var events int
	if err := pool.QueryRow(context.Background(), `SELECT status FROM provider_channel_spend_policies WHERE id=$1`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM provider_channel_spend_policy_events WHERE policy_id=$1`, id).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if status != "disabled" || events != 2 {
		t.Fatalf("status=%s events=%d", status, events)
	}
}
