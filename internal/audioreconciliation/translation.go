package audioreconciliation

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nativegatewayhq/gateway/internal/audiobilling"
)

type TranslationSettler interface {
	Complete(context.Context, string, audiobilling.TranslationEvidence) (audiobilling.TranslationCharge, error)
}

type TranslationWorker struct {
	pool        *pgxpool.Pool
	settler     TranslationSettler
	owner       string
	lease       time.Duration
	maxAttempts int
}

func NewTranslation(pool *pgxpool.Pool, settler TranslationSettler, owner string, lease time.Duration, maximumAttempts int) (*TranslationWorker, error) {
	if pool == nil || settler == nil || owner == "" || lease <= 0 || maximumAttempts < 1 {
		return nil, ErrInvalid
	}
	return &TranslationWorker{pool: pool, settler: settler, owner: owner, lease: lease, maxAttempts: maximumAttempts}, nil
}

func (worker *TranslationWorker) RunOne(ctx context.Context) (bool, error) {
	if _, err := worker.pool.Exec(ctx, `INSERT INTO audio_translation_reconciliations(charge_id,reason) SELECT id,'orphaned_reserved' FROM audio_translation_charges WHERE state='RESERVED' AND updated_at<now()-interval '15 minutes' ON CONFLICT(charge_id) DO NOTHING`); err != nil {
		return false, err
	}
	tx, err := worker.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var id string
	var schema *string
	var duration *int64
	var status *int
	var headersJSON, digest []byte
	err = tx.QueryRow(ctx, `SELECT r.charge_id,e.schema_version,e.duration_milliseconds,e.response_status,e.response_headers::text,e.response_sha256 FROM audio_translation_reconciliations r LEFT JOIN audio_translation_duration_evidence e ON e.charge_id=r.charge_id WHERE (r.state='PENDING' AND r.next_attempt_at<=now()) OR (r.state='LEASED' AND r.lease_until<=now()) ORDER BY r.next_attempt_at,r.charge_id FOR UPDATE OF r SKIP LOCKED LIMIT 1`).Scan(&id, &schema, &duration, &status, &headersJSON, &digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err = tx.Exec(ctx, `UPDATE audio_translation_reconciliations SET state='LEASED',lease_owner=$2,lease_until=now()+$3::interval,attempt_count=attempt_count+1,updated_at=now() WHERE charge_id=$1`, id, worker.owner, worker.lease.String()); err != nil {
		return false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	if schema == nil || duration == nil || status == nil || len(digest) != 32 {
		return true, worker.retry(ctx, id, "provider_outcome_requires_manual_review")
	}
	headers := map[string][]string{}
	if len(headersJSON) > 0 && string(headersJSON) != "null" {
		if err = json.Unmarshal(headersJSON, &headers); err != nil {
			return true, worker.finish(ctx, id, false, "evidence_headers_invalid")
		}
	}
	var sha [32]byte
	copy(sha[:], digest)
	evidence := audiobilling.TranslationEvidence{SchemaVersion: *schema, DurationMilliseconds: *duration, Status: *status, Headers: headers, SHA256: sha}
	if _, settleErr := worker.settler.Complete(ctx, id, evidence); settleErr == nil {
		return true, worker.finish(ctx, id, true, "")
	}
	return true, worker.retry(ctx, id, "settlement_retry_failed")
}

func (worker *TranslationWorker) finish(ctx context.Context, id string, resolved bool, category string) error {
	state, resolvedAt := "MANUAL_REVIEW", "NULL"
	if resolved {
		state, resolvedAt = "RESOLVED", "now()"
	}
	_, err := worker.pool.Exec(ctx, `UPDATE audio_translation_reconciliations SET state=$2,lease_owner=NULL,lease_until=NULL,last_error_category=NULLIF($3,''),resolved_at=`+resolvedAt+`,updated_at=now() WHERE charge_id=$1 AND state='LEASED' AND lease_owner=$4`, id, state, category, worker.owner)
	return err
}

func (worker *TranslationWorker) retry(ctx context.Context, id, category string) error {
	_, err := worker.pool.Exec(ctx, `UPDATE audio_translation_reconciliations SET state=CASE WHEN attempt_count >= $3 THEN 'MANUAL_REVIEW' ELSE 'PENDING' END,lease_owner=NULL,lease_until=NULL,last_error_category=$2,next_attempt_at=now()+interval '30 seconds',updated_at=now() WHERE charge_id=$1 AND state='LEASED' AND lease_owner=$4`, id, category, worker.maxAttempts, worker.owner)
	return err
}
