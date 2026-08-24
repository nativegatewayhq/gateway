// Package audioreconciliation resolves transcription charges that carry
// complete typed usage evidence and sends uncertain Provider outcomes to
// bounded manual review without redispatching audio.
package audioreconciliation

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nativegatewayhq/gateway/internal/audiobilling"
	"github.com/nativegatewayhq/gateway/internal/audiopricing"
)

var ErrInvalid = errors.New("invalid audio reconciliation configuration")

type Settler interface {
	Complete(context.Context, string, audiobilling.TranscriptionEvidence) (audiobilling.TranscriptionCharge, error)
}

type Worker struct {
	pool        *pgxpool.Pool
	settler     Settler
	owner       string
	lease       time.Duration
	maxAttempts int
}

func New(pool *pgxpool.Pool, settler Settler, owner string, lease time.Duration, maximumAttempts int) (*Worker, error) {
	if pool == nil || settler == nil || owner == "" || lease <= 0 || maximumAttempts < 1 {
		return nil, ErrInvalid
	}
	return &Worker{pool: pool, settler: settler, owner: owner, lease: lease, maxAttempts: maximumAttempts}, nil
}

func (w *Worker) RunOne(ctx context.Context) (bool, error) {
	// A process may stop after reservation but before it can classify the
	// Provider outcome. The request timeout is capped at ten minutes, so a
	// fifteen-minute RESERVED charge is no longer an active request.
	if _, err := w.pool.Exec(ctx, `INSERT INTO audio_transcription_reconciliations(charge_id,reason) SELECT id,'orphaned_reserved' FROM audio_transcription_charges WHERE state='RESERVED' AND updated_at<now()-interval '15 minutes' ON CONFLICT(charge_id) DO NOTHING`); err != nil {
		return false, err
	}
	tx, err := w.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var id, reason string
	var schema, usageType *string
	var input, audioInput, textInput, output, total, duration *int64
	var status *int
	var headersJSON, digest []byte
	err = tx.QueryRow(ctx, `SELECT r.charge_id,r.reason,e.schema_version,e.usage_type,e.input_tokens,e.audio_input_tokens,e.text_input_tokens,e.output_tokens,e.total_tokens,e.duration_milliseconds,e.response_status,e.response_headers::text,e.response_sha256 FROM audio_transcription_reconciliations r LEFT JOIN audio_transcription_usage_evidence e ON e.charge_id=r.charge_id WHERE (r.state='PENDING' AND r.next_attempt_at<=now()) OR (r.state='LEASED' AND r.lease_until<=now()) ORDER BY r.next_attempt_at,r.charge_id FOR UPDATE OF r SKIP LOCKED LIMIT 1`).Scan(&id, &reason, &schema, &usageType, &input, &audioInput, &textInput, &output, &total, &duration, &status, &headersJSON, &digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err = tx.Exec(ctx, `UPDATE audio_transcription_reconciliations SET state='LEASED',lease_owner=$2,lease_until=now()+$3::interval,attempt_count=attempt_count+1,updated_at=now() WHERE charge_id=$1`, id, w.owner, w.lease.String()); err != nil {
		return false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	if schema == nil || usageType == nil || input == nil || audioInput == nil || textInput == nil || output == nil || total == nil || duration == nil || status == nil || len(digest) != 32 {
		return true, w.retry(ctx, id, "provider_outcome_requires_manual_review")
	}
	headers := map[string][]string{}
	if len(headersJSON) > 0 && string(headersJSON) != "null" {
		if err = json.Unmarshal(headersJSON, &headers); err != nil {
			return true, w.finish(ctx, id, false, "evidence_headers_invalid")
		}
	}
	var sha [32]byte
	copy(sha[:], digest)
	evidence := audiobilling.TranscriptionEvidence{SchemaVersion: *schema, Usage: audiopricing.TranscriptionUsage{Type: audiopricing.TranscriptionUsageType(*usageType), InputTokens: *input, AudioInputTokens: *audioInput, TextInputTokens: *textInput, OutputTokens: *output, TotalTokens: *total, DurationMilliseconds: *duration}, Status: *status, Headers: headers, SHA256: sha}
	if _, settleErr := w.settler.Complete(ctx, id, evidence); settleErr == nil {
		return true, w.finish(ctx, id, true, "")
	}
	return true, w.retry(ctx, id, "settlement_retry_failed")
}

func (w *Worker) finish(ctx context.Context, id string, resolved bool, category string) error {
	state, resolvedAt := "MANUAL_REVIEW", "NULL"
	if resolved {
		state, resolvedAt = "RESOLVED", "now()"
	}
	_, err := w.pool.Exec(ctx, `UPDATE audio_transcription_reconciliations SET state=$2,lease_owner=NULL,lease_until=NULL,last_error_category=NULLIF($3,''),resolved_at=`+resolvedAt+`,updated_at=now() WHERE charge_id=$1 AND state='LEASED' AND lease_owner=$4`, id, state, category, w.owner)
	return err
}

func (w *Worker) retry(ctx context.Context, id, category string) error {
	_, err := w.pool.Exec(ctx, `UPDATE audio_transcription_reconciliations SET state=CASE WHEN attempt_count >= $3 THEN 'MANUAL_REVIEW' ELSE 'PENDING' END,lease_owner=NULL,lease_until=NULL,last_error_category=$2,next_attempt_at=now()+interval '30 seconds',updated_at=now() WHERE charge_id=$1 AND state='LEASED' AND lease_owner=$4`, id, category, w.maxAttempts, w.owner)
	return err
}
