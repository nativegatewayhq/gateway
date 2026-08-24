//go:build integration

package audiobilling

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nativegatewayhq/gateway/internal/audiopricing"
	"github.com/nativegatewayhq/gateway/internal/costquota"
	"github.com/nativegatewayhq/gateway/internal/database"
	"github.com/nativegatewayhq/gateway/internal/ledger"
	"github.com/nativegatewayhq/gateway/internal/spendcap"
)

func transcriptionFixture(t *testing.T) (*TranscriptionService, *pgxpool.Pool) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	ctx := context.Background()
	admin, err := database.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("transcription_billing_%d", time.Now().UnixNano())
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	config, _ := pgxpool.ParseConfig(url)
	config.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	if err = database.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		admin.Close()
	})
	_, err = pool.Exec(ctx, `INSERT INTO organizations(id,name,slug) VALUES('org_transcription','Transcription','transcription'); INSERT INTO projects(id,organization_id,name,slug) VALUES('project_transcription','org_transcription','Transcription','transcription'); INSERT INTO service_api_keys(id,name,key_digest,key_prefix,project_id) VALUES('key_transcription','Transcription',decode(repeat('44',32),'hex'),'ngw_transcription','project_transcription')`)
	if err != nil {
		t.Fatal(err)
	}
	wallet := ledger.NewService(pool)
	if _, err = wallet.Deposit(ctx, "org_transcription", 10_000, "transcription-fixture"); err != nil {
		t.Fatal(err)
	}
	prices, _ := audiopricing.New(pool, 0)
	_, err = prices.PublishTranscription(ctx, audiopricing.TranscriptionPrice{ChannelID: "channel_00000000000000000000000000000001", Model: "gpt-4o-transcribe", Strategy: audiopricing.TranscriptionTokenStrategy, CostInputPerMillion: 1_000_000, CostOutputPerMillion: 1_000_000, SaleInputPerMillion: 2_000_000, SaleOutputPerMillion: 2_000_000, MaximumInputTokens: 5, MaximumOutputTokens: 5, EffectiveFrom: time.Now().Add(-time.Hour)}, "transcription-price")
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewTranscriptionWithControls(pool, prices, wallet, costquota.NewStore(pool), spendcap.NewStore(pool))
	if err != nil {
		t.Fatal(err)
	}
	return service, pool
}

func transcriptionBegin(key string) TranscriptionBeginRequest {
	return TranscriptionBeginRequest{RequestID: "request-" + key, OrganizationID: "org_transcription", ProjectID: "project_transcription", APIKeyID: "key_transcription", Model: "gpt-4o-transcribe", ChannelID: "channel_00000000000000000000000000000001", IdempotencyKey: key, Fingerprint: [32]byte{7}}
}

func transcriptionEvidence() TranscriptionEvidence {
	return TranscriptionEvidence{SchemaVersion: "openai-transcription-token-json-v1", Usage: audiopricing.TranscriptionUsage{Type: audiopricing.TranscriptionTokens, InputTokens: 2, AudioInputTokens: 2, OutputTokens: 1, TotalTokens: 3}, Status: 200, Headers: map[string][]string{"Content-Type": {"application/json"}, "Set-Cookie": {"secret"}}, SHA256: [32]byte{9}}
}

func TestTranscriptionConcurrentBeginAndActualSettlementAreExactlyOnce(t *testing.T) {
	service, pool := transcriptionFixture(t)
	ctx := context.Background()
	var wait sync.WaitGroup
	charges := make(chan TranscriptionCharge, 2)
	errs := make(chan error, 2)
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			charge, err := service.Begin(ctx, transcriptionBegin("same-key"))
			charges <- charge
			errs <- err
		}()
	}
	wait.Wait()
	close(charges)
	close(errs)
	var charge TranscriptionCharge
	success, pending := 0, 0
	for candidate := range charges {
		if candidate.ID != "" {
			charge = candidate
		}
	}
	for err := range errs {
		if err == nil {
			success++
		} else if errors.Is(err, ErrPending) {
			pending++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || pending != 1 || charge.ReservedSale != 20 {
		t.Fatalf("success=%d pending=%d charge=%+v", success, pending, charge)
	}
	settled, err := service.Complete(ctx, charge.ID, transcriptionEvidence())
	if err != nil || settled.CapturedSale != 6 || settled.ActualCost == nil || *settled.ActualCost != 3 {
		t.Fatalf("settled=%+v err=%v", settled, err)
	}
	if _, err = service.Complete(ctx, charge.ID, transcriptionEvidence()); err != nil {
		t.Fatal(err)
	}
	var available, reserved int64
	if err = pool.QueryRow(ctx, `SELECT available,reserved FROM organization_wallets WHERE organization_id='org_transcription'`).Scan(&available, &reserved); err != nil || available != 9994 || reserved != 0 {
		t.Fatalf("wallet=%d/%d err=%v", available, reserved, err)
	}
	var captures, evidenceRows int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM wallet_operations WHERE operation_key=$1`, "audio.transcription:capture:"+charge.ID).Scan(&captures)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM audio_transcription_usage_evidence WHERE charge_id=$1 AND response_headers ? 'content-type' AND NOT(response_headers ? 'set-cookie')`, charge.ID).Scan(&evidenceRows)
	if captures != 1 || evidenceRows != 1 {
		t.Fatalf("captures=%d evidence=%d", captures, evidenceRows)
	}
}

func TestTranscriptionReleaseAndUncertainOutcomePreserveCorrectFunds(t *testing.T) {
	service, pool := transcriptionFixture(t)
	ctx := context.Background()
	released, err := service.Begin(ctx, transcriptionBegin("release-key"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.Release(ctx, released.ID, "provider_non_2xx"); err != nil {
		t.Fatal(err)
	}
	uncertain, err := service.Begin(ctx, transcriptionBegin("uncertain-key"))
	if err != nil {
		t.Fatal(err)
	}
	if err = service.MarkReconciling(ctx, uncertain.ID, "usage_missing", nil); err != nil {
		t.Fatal(err)
	}
	var available, reserved int64
	if err = pool.QueryRow(ctx, `SELECT available,reserved FROM organization_wallets WHERE organization_id='org_transcription'`).Scan(&available, &reserved); err != nil || available != 9980 || reserved != 20 {
		t.Fatalf("wallet=%d/%d err=%v", available, reserved, err)
	}
}
