//go:build integration

package audioreconciliation

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nativegatewayhq/gateway/internal/audiobilling"
	"github.com/nativegatewayhq/gateway/internal/audiopricing"
	"github.com/nativegatewayhq/gateway/internal/costquota"
	"github.com/nativegatewayhq/gateway/internal/database"
	"github.com/nativegatewayhq/gateway/internal/ledger"
	"github.com/nativegatewayhq/gateway/internal/spendcap"
)

func reconciliationFixture(t *testing.T) (*Worker, *audiobilling.TranscriptionService, *pgxpool.Pool) {
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
	schema := fmt.Sprintf("transcription_reconciliation_%d", time.Now().UnixNano())
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
	_, err = pool.Exec(ctx, `INSERT INTO organizations(id,name,slug) VALUES('org_audio_reconcile','Audio Reconcile','audio-reconcile'); INSERT INTO projects(id,organization_id,name,slug) VALUES('project_audio_reconcile','org_audio_reconcile','Audio Reconcile','audio-reconcile'); INSERT INTO service_api_keys(id,name,key_digest,key_prefix,project_id) VALUES('key_audio_reconcile','Audio Reconcile',decode(repeat('55',32),'hex'),'ngw_audio_reconcile','project_audio_reconcile')`)
	if err != nil {
		t.Fatal(err)
	}
	wallet := ledger.NewService(pool)
	if _, err = wallet.Deposit(ctx, "org_audio_reconcile", 10_000, "audio-reconciliation-fixture"); err != nil {
		t.Fatal(err)
	}
	prices, _ := audiopricing.New(pool, 0)
	_, err = prices.PublishTranscription(ctx, audiopricing.TranscriptionPrice{ChannelID: "channel_00000000000000000000000000000001", Model: "gpt-4o-transcribe", Strategy: audiopricing.TranscriptionTokenStrategy, CostInputPerMillion: 1_000_000, CostOutputPerMillion: 1_000_000, SaleInputPerMillion: 2_000_000, SaleOutputPerMillion: 2_000_000, MaximumInputTokens: 5, MaximumOutputTokens: 5, EffectiveFrom: time.Now().Add(-time.Hour)}, "reconciliation-price")
	if err != nil {
		t.Fatal(err)
	}
	service, err := audiobilling.NewTranscriptionWithControls(pool, prices, wallet, costquota.NewStore(pool), spendcap.NewStore(pool))
	if err != nil {
		t.Fatal(err)
	}
	worker, err := New(pool, service, "test-worker", time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	return worker, service, pool
}

func reconciliationBegin(key string) audiobilling.TranscriptionBeginRequest {
	return audiobilling.TranscriptionBeginRequest{RequestID: "request-" + key, OrganizationID: "org_audio_reconcile", ProjectID: "project_audio_reconcile", APIKeyID: "key_audio_reconcile", Model: "gpt-4o-transcribe", ChannelID: "channel_00000000000000000000000000000001", IdempotencyKey: key, Fingerprint: [32]byte{3}}
}

func reconciliationEvidence() audiobilling.TranscriptionEvidence {
	return audiobilling.TranscriptionEvidence{SchemaVersion: "openai-transcription-token-json-v1", Usage: audiopricing.TranscriptionUsage{Type: audiopricing.TranscriptionTokens, InputTokens: 2, AudioInputTokens: 2, OutputTokens: 1, TotalTokens: 3}, Status: 200, Headers: map[string][]string{"Content-Type": {"application/json"}}, SHA256: [32]byte{4}}
}

func TestWorkerSettlesCompleteEvidenceAndManualizesUnknownOutcome(t *testing.T) {
	worker, service, pool := reconciliationFixture(t)
	ctx := context.Background()
	recoverable, err := service.Begin(ctx, reconciliationBegin("recoverable"))
	if err != nil {
		t.Fatal(err)
	}
	evidence := reconciliationEvidence()
	if err = service.MarkReconciling(ctx, recoverable.ID, "settlement_failed", &evidence); err != nil {
		t.Fatal(err)
	}
	processed, err := worker.RunOne(ctx)
	if err != nil || !processed {
		t.Fatalf("processed=%t err=%v", processed, err)
	}
	var chargeState, taskState string
	if err = pool.QueryRow(ctx, `SELECT c.state,r.state FROM audio_transcription_charges c JOIN audio_transcription_reconciliations r ON r.charge_id=c.id WHERE c.id=$1`, recoverable.ID).Scan(&chargeState, &taskState); err != nil || chargeState != "CAPTURED" || taskState != "RESOLVED" {
		t.Fatalf("charge=%s task=%s err=%v", chargeState, taskState, err)
	}
	unknown, err := service.Begin(ctx, reconciliationBegin("unknown"))
	if err != nil {
		t.Fatal(err)
	}
	if err = service.MarkReconciling(ctx, unknown.ID, "executor_uncertain", nil); err != nil {
		t.Fatal(err)
	}
	processed, err = worker.RunOne(ctx)
	if err != nil || !processed {
		t.Fatalf("processed=%t err=%v", processed, err)
	}
	if err = pool.QueryRow(ctx, `SELECT state FROM audio_transcription_reconciliations WHERE charge_id=$1`, unknown.ID).Scan(&taskState); err != nil || taskState != "MANUAL_REVIEW" {
		t.Fatalf("task=%s err=%v", taskState, err)
	}
	orphaned, err := service.Begin(ctx, reconciliationBegin("orphaned"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE audio_transcription_charges SET updated_at=now()-interval '16 minutes' WHERE id=$1`, orphaned.ID); err != nil {
		t.Fatal(err)
	}
	processed, err = worker.RunOne(ctx)
	if err != nil || !processed {
		t.Fatalf("processed=%t err=%v", processed, err)
	}
	if err = pool.QueryRow(ctx, `SELECT state FROM audio_transcription_reconciliations WHERE charge_id=$1`, orphaned.ID).Scan(&taskState); err != nil || taskState != "MANUAL_REVIEW" {
		t.Fatalf("orphan task=%s err=%v", taskState, err)
	}
	overbound, err := service.Begin(ctx, reconciliationBegin("overbound"))
	if err != nil {
		t.Fatal(err)
	}
	overboundEvidence := reconciliationEvidence()
	overboundEvidence.Usage = audiopricing.TranscriptionUsage{Type: audiopricing.TranscriptionTokens, InputTokens: 6, AudioInputTokens: 6, OutputTokens: 1, TotalTokens: 7}
	if err = service.MarkReconciling(ctx, overbound.ID, "usage_upper_bound_exceeded", &overboundEvidence); err != nil {
		t.Fatal(err)
	}
	processed, err = worker.RunOne(ctx)
	if err != nil || !processed {
		t.Fatalf("processed=%t err=%v", processed, err)
	}
	if err = pool.QueryRow(ctx, `SELECT state FROM audio_transcription_reconciliations WHERE charge_id=$1`, overbound.ID).Scan(&taskState); err != nil || taskState != "MANUAL_REVIEW" {
		t.Fatalf("overbound task=%s err=%v", taskState, err)
	}
}

func translationReconciliationFixture(t *testing.T) (*TranslationWorker, *audiobilling.TranslationService, *pgxpool.Pool) {
	_, _, pool := reconciliationFixture(t)
	prices, _ := audiopricing.New(pool, 0)
	_, err := prices.PublishTranslation(context.Background(), audiopricing.TranslationPrice{ChannelID: "channel_00000000000000000000000000000001", Model: "whisper-1", CostPerMinute: 60, SalePerMinute: 120, MaximumDurationMilliseconds: 600_000, EffectiveFrom: time.Now().Add(-time.Hour)}, "translation-reconciliation-price")
	if err != nil {
		t.Fatal(err)
	}
	service, err := audiobilling.NewTranslationWithControls(pool, prices, ledger.NewService(pool), costquota.NewStore(pool), spendcap.NewStore(pool))
	if err != nil {
		t.Fatal(err)
	}
	worker, err := NewTranslation(pool, service, "translation-test-worker", time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	return worker, service, pool
}

func translationReconciliationBegin(key string) audiobilling.TranslationBeginRequest {
	return audiobilling.TranslationBeginRequest{RequestID: "translation-" + key, OrganizationID: "org_audio_reconcile", ProjectID: "project_audio_reconcile", APIKeyID: "key_audio_reconcile", Model: "whisper-1", ChannelID: "channel_00000000000000000000000000000001", IdempotencyKey: key, Fingerprint: [32]byte{6}}
}

func TestTranslationWorkerSettlesEvidenceAndManualizesUnknownOrOverbound(t *testing.T) {
	worker, service, pool := translationReconciliationFixture(t)
	ctx := context.Background()
	recoverable, err := service.Begin(ctx, translationReconciliationBegin("recoverable"))
	if err != nil {
		t.Fatal(err)
	}
	evidence := audiobilling.TranslationEvidence{SchemaVersion: "openai-translation-duration-json-v1", DurationMilliseconds: 60_001, Status: 200, Headers: map[string][]string{"Content-Type": {"application/json"}}, SHA256: [32]byte{7}}
	if err = service.MarkReconciling(ctx, recoverable.ID, "settlement_failed", &evidence); err != nil {
		t.Fatal(err)
	}
	if processed, runErr := worker.RunOne(ctx); runErr != nil || !processed {
		t.Fatalf("processed=%t err=%v", processed, runErr)
	}
	var chargeState, taskState string
	if err = pool.QueryRow(ctx, `SELECT c.state,r.state FROM audio_translation_charges c JOIN audio_translation_reconciliations r ON r.charge_id=c.id WHERE c.id=$1`, recoverable.ID).Scan(&chargeState, &taskState); err != nil || chargeState != "CAPTURED" || taskState != "RESOLVED" {
		t.Fatalf("charge=%s task=%s err=%v", chargeState, taskState, err)
	}
	unknown, err := service.Begin(ctx, translationReconciliationBegin("unknown"))
	if err != nil {
		t.Fatal(err)
	}
	if err = service.MarkReconciling(ctx, unknown.ID, "executor_uncertain", nil); err != nil {
		t.Fatal(err)
	}
	if processed, runErr := worker.RunOne(ctx); runErr != nil || !processed {
		t.Fatalf("processed=%t err=%v", processed, runErr)
	}
	if err = pool.QueryRow(ctx, `SELECT state FROM audio_translation_reconciliations WHERE charge_id=$1`, unknown.ID).Scan(&taskState); err != nil || taskState != "MANUAL_REVIEW" {
		t.Fatalf("unknown task=%s err=%v", taskState, err)
	}
	overbound, err := service.Begin(ctx, translationReconciliationBegin("overbound"))
	if err != nil {
		t.Fatal(err)
	}
	overboundEvidence := evidence
	overboundEvidence.DurationMilliseconds = overbound.MaximumDurationMilliseconds + 1
	overboundEvidence.SHA256 = [32]byte{8}
	if err = service.MarkReconciling(ctx, overbound.ID, "duration_upper_bound_exceeded", &overboundEvidence); err != nil {
		t.Fatal(err)
	}
	if processed, runErr := worker.RunOne(ctx); runErr != nil || !processed {
		t.Fatalf("processed=%t err=%v", processed, runErr)
	}
	if err = pool.QueryRow(ctx, `SELECT state FROM audio_translation_reconciliations WHERE charge_id=$1`, overbound.ID).Scan(&taskState); err != nil || taskState != "MANUAL_REVIEW" {
		t.Fatalf("overbound task=%s err=%v", taskState, err)
	}
}
