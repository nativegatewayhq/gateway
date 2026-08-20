// Command gateway-quota manages cost quota policies for operators.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/nativegatewayhq/gateway/internal/costquota"
	"github.com/nativegatewayhq/gateway/internal/database"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, os.Getenv)) }

func run(arguments []string, stdout, stderr io.Writer, getenv func(string) string) int {
	flags := flag.NewFlagSet("gateway-quota", flag.ContinueOnError)
	flags.SetOutput(stderr)
	action := flags.String("action", "set", "set or disable")
	policyID := flags.String("policy-id", "", "policy ID for disable")
	scope := flags.String("scope", "", "organization, project, or api_key")
	organizationID := flags.String("organization-id", "", "organization ID")
	projectID := flags.String("project-id", "", "project ID")
	apiKeyID := flags.String("api-key-id", "", "API key ID")
	protocol := flags.String("protocol", "", "optional native protocol")
	operation := flags.String("operation", "", "optional operation")
	model := flags.String("model", "", "optional logical model")
	period := flags.String("period", "", "day or month")
	limit := flags.String("limit", "", "positive USD_TICKS amount")
	actor := flags.String("actor", "", "operator identity")
	reason := flags.String("reason", "", "audit reason")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if *action != "set" && *action != "disable" {
		_, _ = fmt.Fprintln(stderr, "action must be set or disable")
		return 2
	}
	var amount int64
	var err error
	if *action == "set" {
		amount, err = strconv.ParseInt(*limit, 10, 64)
		if err != nil || amount <= 0 {
			_, _ = fmt.Fprintln(stderr, "limit must be a positive integer")
			return 2
		}
	}
	if *actor == "" || *reason == "" {
		_, _ = fmt.Fprintln(stderr, "actor and reason are required")
		return 2
	}
	if *action == "disable" && *policyID == "" {
		_, _ = fmt.Fprintln(stderr, "policy-id is required for disable")
		return 2
	}
	input := costquota.PolicyInput{ScopeType: costquota.ScopeType(*scope), OrganizationID: *organizationID, ProjectID: *projectID, APIKeyID: *apiKeyID, Protocol: *protocol, Operation: *operation, Model: *model, Period: costquota.Period(*period), Limit: amount, Actor: *actor, Reason: *reason}
	if (*action == "set" && costquota.ValidatePolicy(input) != nil) || (*action == "disable" && costquota.ValidateDisable(*policyID, *actor, *reason) != nil) {
		_, _ = fmt.Fprintln(stderr, "quota policy arguments are invalid")
		return 2
	}
	url := getenv("GATEWAY_DATABASE_URL")
	if url == "" {
		_, _ = fmt.Fprintln(stderr, "GATEWAY_DATABASE_URL is required")
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := database.Open(ctx, url)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "database unavailable")
		return 1
	}
	defer pool.Close()
	if err := database.Migrate(ctx, pool); err != nil {
		_, _ = fmt.Fprintln(stderr, "database migration failed")
		return 1
	}
	store := costquota.NewStore(pool)
	if *action == "disable" {
		if err := store.DisablePolicy(ctx, *policyID, *actor, *reason); err != nil {
			_, _ = fmt.Fprintln(stderr, "quota policy disable failed")
			return 1
		}
		_, _ = fmt.Fprintln(stdout, *policyID)
		return 0
	}
	policy, err := store.SetPolicy(ctx, input)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "quota policy creation failed")
		return 1
	}
	_, _ = fmt.Fprintln(stdout, policy.ID)
	return 0
}
