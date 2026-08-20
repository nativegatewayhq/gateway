// Package chatreconciliation durably resolves recoverable Chat billing outcomes.
package chatreconciliation

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nativegatewayhq/gateway/internal/billing"
	"github.com/nativegatewayhq/gateway/internal/chatbilling"
	"github.com/nativegatewayhq/gateway/internal/chatpricing"
)

var ErrInvalid = errors.New("invalid chat reconciliation configuration")

type Settler interface {
	CompleteUsage(context.Context, string, chatpricing.Usage, billing.ResponseSnapshot) (chatbilling.Charge, error)
	CompleteStreamUsage(context.Context, string, chatpricing.Usage, [32]byte) (chatbilling.Charge, error)
}

type Worker struct {
	pool        *pgxpool.Pool
	settler     Settler
	owner       string
	lease       time.Duration
	maxAttempts int
}

func New(pool *pgxpool.Pool, settler Settler, owner string, lease time.Duration, maxAttempts int) (*Worker, error) {
	if pool == nil || settler == nil || owner == "" || lease <= 0 || maxAttempts < 1 {
		return nil, ErrInvalid
	}
	return &Worker{pool: pool, settler: settler, owner: owner, lease: lease, maxAttempts: maxAttempts}, nil
}

// RunOne claims at most one task. Unknown Provider outcomes converge to manual
// review; only tasks carrying a complete response and validated usage retry settlement.
func (w *Worker) RunOne(ctx context.Context) (bool, error) {
	tx, err := w.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var id, reason string
	var status *int
	var headersJSON, body []byte
	var prompt, cached, cacheWrite, completion, toolUse, thoughts *int64
	var terminalDigest []byte
	err = tx.QueryRow(ctx, `SELECT charge_id,reason,response_status,response_headers::text,response_body,prompt_tokens,cached_input_tokens,cache_write_tokens,completion_tokens,tool_use_prompt_tokens,thoughts_tokens,terminal_event_sha256 FROM chat_charge_reconciliations WHERE (state='PENDING' AND next_attempt_at<=now()) OR (state='LEASED' AND lease_until<=now()) ORDER BY next_attempt_at,charge_id FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&id, &reason, &status, &headersJSON, &body, &prompt, &cached, &cacheWrite, &completion, &toolUse, &thoughts, &terminalDigest)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_, err = tx.Exec(ctx, `UPDATE chat_charge_reconciliations SET state='LEASED',lease_owner=$2,lease_until=now()+$3::interval,attempt_count=attempt_count+1,updated_at=now() WHERE charge_id=$1`, id, w.owner, w.lease.String())
	if err != nil {
		return false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}

	recoverable := reason == "settlement_failed" && prompt != nil && cached != nil && completion != nil && (status != nil || len(terminalDigest) == 32)
	if !recoverable {
		return true, w.retry(ctx, id, "provider_outcome_requires_manual_review")
	}
	usage := chatpricing.Usage{PromptTokens: *prompt, CachedInputTokens: *cached, CompletionTokens: *completion}
	if cacheWrite != nil {
		usage.CacheWriteTokens = *cacheWrite
	}
	if toolUse != nil {
		usage.ToolUsePromptTokens = *toolUse
	}
	if thoughts != nil {
		usage.ThoughtsTokens = *thoughts
	}
	if len(terminalDigest) == 32 {
		var digest [32]byte
		copy(digest[:], terminalDigest)
		_, settleErr := w.settler.CompleteStreamUsage(ctx, id, usage, digest)
		if settleErr == nil {
			return true, w.finish(ctx, id, true, "")
		}
		return true, w.retry(ctx, id, "settlement_retry_failed")
	}
	headers := map[string][]string{}
	if len(headersJSON) > 0 && string(headersJSON) != "null" {
		if err := json.Unmarshal(headersJSON, &headers); err != nil {
			return true, w.finish(ctx, id, false, "snapshot_headers_invalid")
		}
	}
	_, settleErr := w.settler.CompleteUsage(ctx, id, usage, billing.ResponseSnapshot{Status: *status, Headers: headers, Body: body})
	if settleErr == nil {
		return true, w.finish(ctx, id, true, "")
	}
	return true, w.retry(ctx, id, "settlement_retry_failed")
}

func (w *Worker) finish(ctx context.Context, id string, resolved bool, category string) error {
	state := "MANUAL_REVIEW"
	resolvedAt := "NULL"
	if resolved {
		state, resolvedAt = "RESOLVED", "now()"
	}
	_, err := w.pool.Exec(ctx, `UPDATE chat_charge_reconciliations SET state=$2,lease_owner=NULL,lease_until=NULL,last_error_category=NULLIF($3,''),resolved_at=`+resolvedAt+`,updated_at=now() WHERE charge_id=$1 AND state='LEASED' AND lease_owner=$4`, id, state, category, w.owner)
	return err
}
func (w *Worker) retry(ctx context.Context, id, category string) error {
	_, err := w.pool.Exec(ctx, `UPDATE chat_charge_reconciliations SET state=CASE WHEN attempt_count >= $3 THEN 'MANUAL_REVIEW' ELSE 'PENDING' END,lease_owner=NULL,lease_until=NULL,last_error_category=$2,next_attempt_at=now()+interval '30 seconds',updated_at=now() WHERE charge_id=$1 AND state='LEASED' AND lease_owner=$4`, id, category, w.maxAttempts, w.owner)
	return err
}
