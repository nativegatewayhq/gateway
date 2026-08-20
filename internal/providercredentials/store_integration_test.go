//go:build integration

package providercredentials

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nativegatewayhq/gateway/internal/database"
)

func credentialPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	ctx := context.Background()
	admin, err := database.Open(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("provider_credentials_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	separator := "?"
	if strings.Contains(base, "?") {
		separator = "&"
	}
	pool, err := database.Open(ctx, base+separator+"search_path="+schema)
	if err != nil {
		admin.Close()
		t.Fatal(err)
	}
	if err := database.Migrate(ctx, pool); err != nil {
		pool.Close()
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		admin.Close()
	})
	return pool
}

func TestCredentialLifecycleEncryptionRotationAndAudit(t *testing.T) {
	pool := credentialPool(t)
	store := NewStore(pool, testKeyring(t))
	ctx := context.Background()
	channel := "channel_00000000000000000000000000000001"
	if _, err := store.Stage(ctx, StageRequest{ChannelID: channel, Provider: XAI, Plaintext: []byte("wrong-provider-secret"), Actor: "integration", Reason: "scope", OperationKey: "wrong-provider"}); !errors.Is(err, ErrScopeMismatch) {
		t.Fatalf("channel/provider scope=%v", err)
	}
	firstSecret := []byte("first-upstream-secret")
	first, err := store.Stage(ctx, StageRequest{ChannelID: channel, Provider: OpenAI, Plaintext: firstSecret, Actor: "integration", Reason: "initial", OperationKey: "stage-first"})
	if err != nil {
		t.Fatal(err)
	}
	if first.State != Staged || first.Version != 1 {
		t.Fatalf("first=%+v", first)
	}
	replayed, err := store.Stage(ctx, StageRequest{ChannelID: channel, Provider: OpenAI, Plaintext: firstSecret, Actor: "ignored-on-replay", Reason: "ignored", OperationKey: "stage-first"})
	if err != nil || replayed.ID != first.ID {
		t.Fatalf("replay=%+v err=%v", replayed, err)
	}
	if _, err := store.Stage(ctx, StageRequest{ChannelID: channel, Provider: OpenAI, Plaintext: []byte("different-secret"), Actor: "integration", Reason: "conflict", OperationKey: "stage-first"}); !errors.Is(err, ErrLifecycleConflict) {
		t.Fatalf("stage conflict=%v", err)
	}
	if _, err := store.Activate(ctx, LifecycleRequest{CredentialID: first.ID, Actor: "integration", Reason: "activate", OperationKey: "activate-first"}); err != nil {
		t.Fatal(err)
	}
	credential, err := store.Resolve(ctx, channel, OpenAI)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(credential.value, firstSecret) {
		t.Fatal("resolved credential mismatch")
	}
	credential.Destroy()

	secondSecret := []byte("second-upstream-secret")
	second, err := store.Stage(ctx, StageRequest{ChannelID: channel, Provider: OpenAI, Plaintext: secondSecret, Actor: "integration", Reason: "rotate", OperationKey: "stage-second"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Activate(ctx, LifecycleRequest{CredentialID: second.ID, Actor: "integration", Reason: "rotate", OperationKey: "activate-second"}); err != nil {
		t.Fatal(err)
	}
	items, err := store.List(ctx, channel)
	if err != nil || len(items) != 2 || items[0].State != Retired || items[1].State != Active {
		t.Fatalf("items=%+v err=%v", items, err)
	}
	credential, err = store.Resolve(ctx, channel, OpenAI)
	if err != nil || !bytes.Equal(credential.value, secondSecret) {
		t.Fatalf("rotated credential=%v err=%v", credential, err)
	}
	credential.Destroy()

	events, err := store.Events(ctx, second.ID)
	if err != nil || len(events) != 2 || events[0].Action != "stage" || events[1].Action != "activate" {
		t.Fatalf("events=%+v err=%v", events, err)
	}
	previousEvents, err := store.Events(ctx, first.ID)
	if err != nil || len(previousEvents) != 3 || previousEvents[2].Action != "retire" || previousEvents[2].OperationKey != "activate-second" {
		t.Fatalf("previous events=%+v err=%v", previousEvents, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE provider_credential_events SET reason='tampered' WHERE credential_id=$1`, second.ID); err == nil {
		t.Fatal("append-only audit update succeeded")
	}
	if _, err := pool.Exec(ctx, `UPDATE provider_credentials SET ciphertext=decode(repeat('00',17),'hex') WHERE id=$1`, second.ID); err == nil {
		t.Fatal("encrypted credential mutation succeeded")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM provider_credential_lifecycle_operations WHERE operation_key='stage-second'`); err == nil {
		t.Fatal("lifecycle operation deletion succeeded")
	}
	var plaintextMatches int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM provider_credentials WHERE position(encode(convert_to($1,'UTF8'),'hex') in encode(ciphertext,'hex')) > 0`, "upstream-secret").Scan(&plaintextMatches); err != nil || plaintextMatches != 0 {
		t.Fatalf("ciphertext plaintext matches=%d err=%v", plaintextMatches, err)
	}
	var plaintextColumns int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_name='provider_credentials' AND column_name IN ('credential','plaintext','secret','api_key')`).Scan(&plaintextColumns); err != nil || plaintextColumns != 0 {
		t.Fatalf("plaintext columns=%d err=%v", plaintextColumns, err)
	}
}

func TestCredentialScopeLegacyFallbackAndMasterKeyRotation(t *testing.T) {
	pool := credentialPool(t)
	oldRing, err := NewMasterKeyring("old", map[string][]byte{"old": bytes.Repeat([]byte{3}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	oldStore := NewStore(pool, oldRing)
	ctx := context.Background()
	channel := "channel_00000000000000000000000000000002"
	staged, err := oldStore.Stage(ctx, StageRequest{ChannelID: channel, Provider: XAI, Plaintext: []byte("database-xai-secret"), Actor: "integration", Reason: "old key", OperationKey: "old-stage"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := oldStore.Activate(ctx, LifecycleRequest{CredentialID: staged.ID, Actor: "integration", Reason: "old key", OperationKey: "old-activate"}); err != nil {
		t.Fatal(err)
	}
	wrongRing, err := NewMasterKeyring("old", map[string][]byte{"old": bytes.Repeat([]byte{8}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(pool, wrongRing).Resolve(ctx, channel, XAI); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("database-only/wrong-key resolve=%v", err)
	}
	rotatedRing, err := NewMasterKeyring("new", map[string][]byte{"old": bytes.Repeat([]byte{3}, 32), "new": bytes.Repeat([]byte{4}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := Load(func(key string) (string, bool) {
		if key == "GATEWAY_XAI_API_KEY" {
			return "legacy-xai-secret", true
		}
		if key == "GATEWAY_GOOGLE_API_KEY" {
			return "legacy-google-secret", true
		}
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := NewControlPlane(legacy, NewStore(pool, rotatedRing))
	if err != nil {
		t.Fatal(err)
	}
	failClosedRegistry, err := NewControlPlane(legacy, NewStore(pool, wrongRing))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := failClosedRegistry.Resolve(ctx, channel, XAI); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("decrypt failure used legacy fallback: %v", err)
	}
	canceledContext, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := registry.Resolve(canceledContext, channel, XAI); !errors.Is(err, ErrCredentialStoreUnavailable) {
		t.Fatalf("store failure used legacy fallback: %v", err)
	}
	rotatedStore := NewStore(pool, rotatedRing)
	replayed, err := rotatedStore.Stage(ctx, StageRequest{ChannelID: channel, Provider: XAI, Plaintext: []byte("database-xai-secret"), Actor: "integration", Reason: "replay after key rotation", OperationKey: "old-stage"})
	if err != nil || replayed.ID != staged.ID || replayed.State != Staged {
		t.Fatalf("master-key rotation replay=%+v err=%v", replayed, err)
	}
	credential, err := registry.Resolve(ctx, channel, XAI)
	if err != nil || string(credential.value) != "database-xai-secret" {
		t.Fatalf("database precedence credential=%v err=%v", credential, err)
	}
	credential.Destroy()
	googleChannel, _ := LegacyChannel(Google)
	credential, err = registry.Resolve(ctx, googleChannel, Google)
	if err != nil || string(credential.value) != "legacy-google-secret" {
		t.Fatalf("legacy fallback credential=%v err=%v", credential, err)
	}
	credential.Destroy()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.x.ai/v1/images/generations?key=inbound", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer inbound-service-key")
	outbound, err := PrepareOutboundChannel(request, channel, XAI, registry)
	if err != nil || outbound.Header.Get("Authorization") != "Bearer database-xai-secret" || outbound.URL.Query().Get("key") != "" {
		t.Fatalf("outbound authorization/query err=%v query=%v", err, outbound.URL.Query())
	}
	if _, err := registry.Resolve(ctx, channel, OpenAI); !errors.Is(err, ErrScopeMismatch) {
		t.Fatalf("provider scope=%v", err)
	}
	if _, err := registry.Resolve(ctx, "channel_00000000000000000000000000000009", XAI); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("nonlegacy fallback=%v", err)
	}
	if _, err := rotatedStore.Retire(ctx, LifecycleRequest{CredentialID: staged.ID, Actor: "integration", Reason: "rollback", OperationKey: "retire-database-xai"}); err != nil {
		t.Fatal(err)
	}
	credential, err = registry.Resolve(ctx, channel, XAI)
	if err != nil || string(credential.value) != "legacy-xai-secret" {
		t.Fatalf("retired fallback credential=%v err=%v", credential, err)
	}
	credential.Destroy()
}

func TestConcurrentActivationLeavesExactlyOneActiveVersion(t *testing.T) {
	pool := credentialPool(t)
	store := NewStore(pool, testKeyring(t))
	ctx := context.Background()
	channel := "channel_00000000000000000000000000000003"
	first, err := store.Stage(ctx, StageRequest{ChannelID: channel, Provider: Google, Plaintext: []byte("concurrent-first-secret"), Actor: "integration", Reason: "concurrency", OperationKey: "concurrent-stage-1"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Stage(ctx, StageRequest{ChannelID: channel, Provider: Google, Plaintext: []byte("concurrent-second-secret"), Actor: "integration", Reason: "concurrency", OperationKey: "concurrent-stage-2"})
	if err != nil {
		t.Fatal(err)
	}
	requests := []LifecycleRequest{
		{CredentialID: first.ID, Actor: "integration", Reason: "concurrency", OperationKey: "concurrent-activate-1"},
		{CredentialID: second.ID, Actor: "integration", Reason: "concurrency", OperationKey: "concurrent-activate-2"},
	}
	var wait sync.WaitGroup
	errorsFound := make(chan error, len(requests))
	for _, request := range requests {
		request := request
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, activateErr := store.Activate(ctx, request)
			errorsFound <- activateErr
		}()
	}
	wait.Wait()
	close(errorsFound)
	for activateErr := range errorsFound {
		if activateErr != nil {
			t.Fatalf("concurrent activation=%v", activateErr)
		}
	}
	items, err := store.List(ctx, channel)
	if err != nil {
		t.Fatal(err)
	}
	active := 0
	for _, item := range items {
		if item.State == Active {
			active++
		}
	}
	if active != 1 {
		t.Fatalf("active=%d items=%+v", active, items)
	}
}

func TestConcurrentResolveDuringRotationUsesOneCompleteVersion(t *testing.T) {
	pool := credentialPool(t)
	store := NewStore(pool, testKeyring(t))
	ctx := context.Background()
	channel := "channel_00000000000000000000000000000001"
	first, err := store.Stage(ctx, StageRequest{ChannelID: channel, Provider: OpenAI, Plaintext: []byte("rotation-old-secret"), Actor: "integration", Reason: "rotation", OperationKey: "resolve-stage-old"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Activate(ctx, LifecycleRequest{CredentialID: first.ID, Actor: "integration", Reason: "rotation", OperationKey: "resolve-activate-old"}); err != nil {
		t.Fatal(err)
	}
	second, err := store.Stage(ctx, StageRequest{ChannelID: channel, Provider: OpenAI, Plaintext: []byte("rotation-new-secret"), Actor: "integration", Reason: "rotation", OperationKey: "resolve-stage-new"})
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan string, 24)
	var wait sync.WaitGroup
	for index := 0; index < 24; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			credential, resolveErr := store.Resolve(ctx, channel, OpenAI)
			if resolveErr != nil {
				results <- "error"
				return
			}
			results <- string(credential.value)
			credential.Destroy()
		}()
	}
	close(start)
	if _, err := store.Activate(ctx, LifecycleRequest{CredentialID: second.ID, Actor: "integration", Reason: "rotation", OperationKey: "resolve-activate-new"}); err != nil {
		t.Fatal(err)
	}
	wait.Wait()
	close(results)
	for value := range results {
		if value != "rotation-old-secret" && value != "rotation-new-secret" {
			t.Fatalf("partial or unavailable credential=%q", value)
		}
	}
	for index := 0; index < 3; index++ {
		credential, err := store.Resolve(ctx, channel, OpenAI)
		if err != nil || string(credential.value) != "rotation-new-secret" {
			t.Fatalf("post-commit resolve=%v err=%v", credential, err)
		}
		credential.Destroy()
	}
}
