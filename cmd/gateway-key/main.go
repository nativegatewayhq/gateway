// Command gateway-key provisions a service API key. The plaintext key is
// printed once to stdout and is never persisted by the Gateway.
package main

import (
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/nativegatewayhq/gateway/internal/apikey"
	"github.com/nativegatewayhq/gateway/internal/database"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, os.Getenv, rand.Reader)) }

type permissionFlags []apikey.ModelPermission

func (values *permissionFlags) String() string { return fmt.Sprintf("%d permissions", len(*values)) }
func (values *permissionFlags) Set(value string) error {
	parts := strings.SplitN(value, ":", 3)
	if len(parts) != 3 {
		return fmt.Errorf("allow-model must be protocol:operation:model")
	}
	*values = append(*values, apikey.ModelPermission{Protocol: parts[0], Operation: parts[1], Model: parts[2]})
	return nil
}

func run(arguments []string, stdout, stderr io.Writer, getenv func(string) string, entropy io.Reader) int {
	flags := flag.NewFlagSet("gateway-key", flag.ContinueOnError)
	flags.SetOutput(stderr)
	name := flags.String("name", "", "non-secret key name")
	expires := flags.String("expires-at", "", "optional RFC3339 expiration")
	projectID := flags.String("project-id", "project_legacy", "owning active project ID")
	rpm := flags.Int64("requests-per-minute", 0, "optional per-key requests per minute")
	burst := flags.Int64("burst", 0, "optional token bucket burst")
	var permissions permissionFlags
	flags.Var(&permissions, "allow-model", "repeatable protocol:operation:logical-model permission")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	databaseURL := getenv("GATEWAY_DATABASE_URL")
	if databaseURL == "" {
		_, _ = fmt.Fprintln(stderr, "GATEWAY_DATABASE_URL is required")
		return 1
	}
	var expiresAt *time.Time
	if *expires != "" {
		parsed, err := time.Parse(time.RFC3339, *expires)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "expires-at must be RFC3339")
			return 2
		}
		expiresAt = &parsed
	}
	record, raw, err := apikey.GenerateForProjectWithPolicies(entropy, *name, *projectID, expiresAt, apikey.RateLimitPolicy{RequestsPerMinute: *rpm, Burst: *burst}, permissions)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := database.Open(ctx, databaseURL)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "database unavailable")
		return 1
	}
	defer pool.Close()
	if err := database.Migrate(ctx, pool); err != nil {
		_, _ = fmt.Fprintln(stderr, "database migration failed")
		return 1
	}
	if err := apikey.NewPostgresStore(pool).Create(ctx, record); err != nil {
		_, _ = fmt.Fprintln(stderr, "API key creation failed")
		return 1
	}
	if record.RateLimit.Enabled() {
		_, _ = fmt.Fprintf(stderr, "rate limit: %d requests/minute, burst %d\n", record.RateLimit.RequestsPerMinute, record.RateLimit.Burst)
	} else {
		_, _ = fmt.Fprintln(stderr, "rate limit: unlimited")
	}
	if record.ModelAccessMode == apikey.ModelAccessAllowlist {
		_, _ = fmt.Fprintf(stderr, "model access: allowlist (%d permissions)\n", len(record.ModelPermissions))
	} else {
		_, _ = fmt.Fprintln(stderr, "model access: all")
	}
	_, _ = fmt.Fprintln(stdout, raw)
	return 0
}
