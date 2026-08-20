// Command gateway-spend-cap manages Provider channel cost caps.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/nativegatewayhq/gateway/internal/database"
	"github.com/nativegatewayhq/gateway/internal/spendcap"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, os.Getenv)) }

func run(arguments []string, stdout, stderr io.Writer, getenv func(string) string) int {
	flags := flag.NewFlagSet("gateway-spend-cap", flag.ContinueOnError)
	flags.SetOutput(stderr)
	action := flags.String("action", "set", "set or disable")
	policyID := flags.String("policy-id", "", "policy ID for disable")
	channelID := flags.String("channel-id", "", "Provider channel ID")
	period := flags.String("period", "", "day or month")
	limit := flags.String("limit", "", "positive USD_TICKS cost amount")
	actor := flags.String("actor", "", "operator identity")
	reason := flags.String("reason", "", "audit reason")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if *action != "set" && *action != "disable" {
		_, _ = fmt.Fprintln(stderr, "action must be set or disable")
		return 2
	}
	if *actor == "" || *reason == "" {
		_, _ = fmt.Fprintln(stderr, "actor and reason are required")
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
	input := spendcap.PolicyInput{ChannelID: *channelID, Period: spendcap.Period(*period), Limit: amount, Actor: *actor, Reason: *reason}
	if (*action == "set" && spendcap.ValidatePolicy(input) != nil) || (*action == "disable" && spendcap.ValidateDisable(*policyID, *actor, *reason) != nil) {
		_, _ = fmt.Fprintln(stderr, "spend cap arguments are invalid")
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
	store := spendcap.NewStore(pool)
	if *action == "disable" {
		if err := store.DisablePolicy(ctx, *policyID, *actor, *reason); err != nil {
			_, _ = fmt.Fprintln(stderr, "spend cap disable failed")
			return 1
		}
		_, _ = fmt.Fprintln(stdout, *policyID)
		return 0
	}
	policy, err := store.SetPolicy(ctx, input)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "spend cap creation failed")
		return 1
	}
	_, _ = fmt.Fprintln(stdout, policy.ID)
	return 0
}
