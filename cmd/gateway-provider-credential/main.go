// Command gateway-provider-credential manages encrypted Provider credentials.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/nativegatewayhq/gateway/internal/database"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
)

func main() { os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.LookupEnv)) }

func run(arguments []string, stdin io.Reader, stdout, stderr io.Writer, lookup providercredentials.LookupEnv) int {
	flags := flag.NewFlagSet("gateway-provider-credential", flag.ContinueOnError)
	flags.SetOutput(stderr)
	action := flags.String("action", "stage", "stage, activate, retire, or list")
	credentialID := flags.String("credential-id", "", "credential ID")
	channelID := flags.String("channel-id", "", "Provider channel ID")
	providerName := flags.String("provider", "", "google, openai, or xai")
	actor := flags.String("actor", "", "operator identity")
	reason := flags.String("reason", "", "audit reason")
	operationKey := flags.String("operation-key", "", "idempotent lifecycle operation key")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if *action != "stage" && *action != "activate" && *action != "retire" && *action != "list" {
		_, _ = fmt.Fprintln(stderr, "action must be stage, activate, retire, or list")
		return 2
	}
	if *action == "list" {
		if !providercredentials.ValidChannelID(*channelID) {
			_, _ = fmt.Fprintln(stderr, "channel-id is required")
			return 2
		}
	} else if *actor == "" || *reason == "" || *operationKey == "" {
		_, _ = fmt.Fprintln(stderr, "actor, reason, and operation-key are required")
		return 2
	}
	if (*action == "activate" || *action == "retire") && *credentialID == "" {
		_, _ = fmt.Fprintln(stderr, "credential-id is required")
		return 2
	}
	var plaintext []byte
	var provider providercredentials.ProviderID
	if *action == "stage" {
		var err error
		provider, err = providercredentials.ParseProviderID(*providerName)
		if err != nil || *channelID == "" {
			_, _ = fmt.Fprintln(stderr, "valid channel-id and provider are required")
			return 2
		}
		plaintext, err = io.ReadAll(io.LimitReader(stdin, 4098))
		if err != nil || len(plaintext) == 0 || len(plaintext) > 4097 {
			_, _ = fmt.Fprintln(stderr, "credential stdin is invalid")
			return 2
		}
		plaintext = bytes.TrimSuffix(plaintext, []byte("\n"))
		defer zero(plaintext)
		if providercredentials.ValidateStageRequest(providercredentials.StageRequest{ChannelID: *channelID, Provider: provider, Plaintext: plaintext, Actor: *actor, Reason: *reason, OperationKey: *operationKey}) != nil {
			_, _ = fmt.Fprintln(stderr, "provider credential stage arguments are invalid")
			return 2
		}
	} else if *action == "activate" || *action == "retire" {
		if providercredentials.ValidateLifecycleRequest(providercredentials.LifecycleRequest{CredentialID: *credentialID, Actor: *actor, Reason: *reason, OperationKey: *operationKey}) != nil {
			_, _ = fmt.Fprintln(stderr, "provider credential lifecycle arguments are invalid")
			return 2
		}
	}
	databaseURL, configured := lookup("GATEWAY_DATABASE_URL")
	if !configured || databaseURL == "" {
		_, _ = fmt.Fprintln(stderr, "GATEWAY_DATABASE_URL is required")
		return 1
	}
	keyring, err := providercredentials.LoadMasterKeyring(lookup)
	if err != nil || keyring == nil {
		_, _ = fmt.Fprintln(stderr, "provider credential key configuration is required")
		return 1
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
	store := providercredentials.NewStore(pool, keyring)
	var metadata providercredentials.Metadata
	switch *action {
	case "stage":
		metadata, err = store.Stage(ctx, providercredentials.StageRequest{ChannelID: *channelID, Provider: provider, Plaintext: plaintext, Actor: *actor, Reason: *reason, OperationKey: *operationKey})
	case "activate":
		metadata, err = store.Activate(ctx, providercredentials.LifecycleRequest{CredentialID: *credentialID, Actor: *actor, Reason: *reason, OperationKey: *operationKey})
	case "retire":
		metadata, err = store.Retire(ctx, providercredentials.LifecycleRequest{CredentialID: *credentialID, Actor: *actor, Reason: *reason, OperationKey: *operationKey})
	case "list":
		items, listErr := store.List(ctx, *channelID)
		if listErr != nil {
			err = listErr
			break
		}
		for _, item := range items {
			_, _ = fmt.Fprintf(stdout, "%s\t%s\t%s\t%d\t%s\n", item.ID, item.ChannelID, item.Provider, item.Version, item.State)
		}
		return 0
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "provider credential lifecycle operation failed")
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "%s\t%s\t%s\t%d\t%s\n", metadata.ID, metadata.ChannelID, metadata.Provider, metadata.Version, metadata.State)
	return 0
}

func zero(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
