//go:build integration

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"strings"
	"testing"

	"github.com/nativegatewayhq/gateway/internal/database"
)

func TestRunStoresOnlyDigestAndPrintsKeyOnce(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	var stdout, stderr bytes.Buffer
	entropy := bytes.NewReader(bytes.Repeat([]byte{3}, 48))
	code := run([]string{"-name", "cli integration", "-requests-per-minute", "120", "-burst", "10", "-allow-model", "openai:image.generate:gpt-image-1", "-allow-model", "gemini:image.generate:gemini-image"}, &stdout, &stderr, func(key string) string {
		if key == "GATEWAY_DATABASE_URL" {
			return url
		}
		return ""
	}, entropy)
	if code != 0 {
		t.Fatalf("run()=%d stderr=%s", code, stderr.String())
	}
	raw := strings.TrimSpace(stdout.String())
	if raw == "" || strings.Count(stdout.String(), raw) != 1 || strings.Contains(stderr.String(), raw) {
		t.Fatalf("unsafe output stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "120 requests/minute, burst 10") {
		t.Fatalf("policy summary=%q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "allowlist (2 permissions)") {
		t.Fatalf("permission summary=%q", stderr.String())
	}
	digest := sha256.Sum256([]byte(raw))
	ctx := context.Background()
	pool, err := database.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	defer pool.Exec(ctx, `DELETE FROM service_api_keys WHERE key_digest=$1`, digest[:])
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM service_api_keys WHERE key_digest=$1 AND name='cli integration' AND project_id='project_legacy' AND requests_per_minute=120 AND burst=10`, digest[:]).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("stored digest rows=%d", count)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM service_api_key_model_permissions p JOIN service_api_keys k ON k.id=p.api_key_id WHERE k.key_digest=$1`, digest[:]).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("permission rows=%d", count)
	}
	var leaked bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM service_api_keys WHERE id LIKE '%' || $1 || '%' OR name LIKE '%' || $1 || '%' OR key_prefix LIKE '%' || $1 || '%')`, raw).Scan(&leaked); err != nil {
		t.Fatal(err)
	}
	if leaked {
		t.Fatal("plaintext key persisted")
	}
}
