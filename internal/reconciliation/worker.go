// Package reconciliation resolves durable image charge observations.
package reconciliation

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nativegatewayhq/gateway/internal/billing"
	"github.com/nativegatewayhq/gateway/internal/imagestorage"
)

var ErrInvalidConfig = errors.New("invalid reconciliation configuration")

type Config struct {
	Interval    time.Duration
	Lease       time.Duration
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
	BatchSize   int
	MaxAttempts int
}

type Completer interface {
	Complete(context.Context, string, bool, billing.ResponseSnapshot) (billing.Charge, error)
}

type RunResult struct {
	Claimed  int
	Resolved int
	Retried  int
	Manual   int
}

type Worker struct {
	pool      *pgxpool.Pool
	completer Completer
	config    Config
	owner     string
	now       func() time.Time
	results   ResultManager
}

type ResultManager interface {
	Transform(context.Context, imagestorage.TransformInput) ([]byte, error)
}

type task struct {
	ChargeID                                 string
	Outcome                                  billing.Outcome
	Reason                                   billing.Reason
	Snapshot                                 billing.ResponseSnapshot
	BodyHash                                 [32]byte
	Attempt                                  int
	RequestID, Protocol, Provider, ChannelID string
}

func New(pool *pgxpool.Pool, completer Completer, config Config) (*Worker, error) {
	return newWorker(pool, completer, config, rand.Reader, time.Now)
}

func NewWithResultManager(pool *pgxpool.Pool, completer Completer, config Config, results ResultManager) (*Worker, error) {
	worker, err := newWorker(pool, completer, config, rand.Reader, time.Now)
	if err != nil {
		return nil, err
	}
	worker.results = results
	return worker, nil
}

func newWorker(pool *pgxpool.Pool, completer Completer, config Config, entropy io.Reader, now func() time.Time) (*Worker, error) {
	if pool == nil || completer == nil || entropy == nil || now == nil || config.Interval <= 0 || config.Lease <= 0 || config.BaseBackoff <= 0 || config.MaxBackoff < config.BaseBackoff || config.BatchSize < 1 || config.BatchSize > 100 || config.MaxAttempts < 1 || config.MaxAttempts > 100 {
		return nil, ErrInvalidConfig
	}
	value := make([]byte, 16)
	if _, err := io.ReadFull(entropy, value); err != nil {
		return nil, fmt.Errorf("generate reconciliation owner: %w", err)
	}
	return &Worker{pool: pool, completer: completer, config: config, owner: "worker_" + hex.EncodeToString(value), now: now}, nil
}

func (worker *Worker) Run(ctx context.Context, onError func(error)) {
	ticker := time.NewTicker(worker.config.Interval)
	defer ticker.Stop()
	for {
		if _, err := worker.RunOnce(ctx); err != nil && onError != nil && !errors.Is(err, context.Canceled) {
			onError(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (worker *Worker) RunOnce(ctx context.Context) (RunResult, error) {
	tasks, err := worker.claim(ctx, worker.now().UTC())
	if err != nil {
		return RunResult{}, err
	}
	result := RunResult{Claimed: len(tasks)}
	for _, task := range tasks {
		if task.Outcome == billing.Unknown {
			manual, err := worker.retry(ctx, task, "provider_outcome_unknown")
			if err != nil {
				return result, err
			}
			if manual {
				result.Manual++
			} else {
				result.Retried++
			}
			continue
		}
		if sha256.Sum256(task.Snapshot.Body) != task.BodyHash {
			if err := worker.manual(ctx, task, "snapshot_corrupt"); err != nil {
				return result, err
			}
			result.Manual++
			continue
		}
		if task.Reason == billing.StorageFailed {
			if worker.results == nil {
				manual, retryErr := worker.retry(ctx, task, "storage_manager_unavailable")
				if retryErr != nil {
					return result, retryErr
				}
				if manual {
					result.Manual++
				} else {
					result.Retried++
				}
				continue
			}
			managedBody, transformErr := worker.results.Transform(ctx, imagestorage.TransformInput{Protocol: task.Protocol, Provider: task.Provider, ChannelID: task.ChannelID, RequestID: task.RequestID, ChargeID: task.ChargeID, Body: task.Snapshot.Body})
			if transformErr != nil {
				manual, retryErr := worker.retry(ctx, task, "storage_retry_failed")
				if retryErr != nil {
					return result, retryErr
				}
				if manual {
					result.Manual++
				} else {
					result.Retried++
				}
				continue
			}
			task.Snapshot.Body = managedBody
		}
		_, completeErr := worker.completer.Complete(ctx, task.ChargeID, task.Outcome == billing.KnownSuccess, task.Snapshot)
		if completeErr != nil {
			manual, err := worker.retry(ctx, task, "settlement_retry_failed")
			if err != nil {
				return result, err
			}
			if manual {
				result.Manual++
			} else {
				result.Retried++
			}
			continue
		}
		if err := worker.resolve(ctx, task); err != nil {
			return result, err
		}
		result.Resolved++
	}
	return result, nil
}

func (worker *Worker) claim(ctx context.Context, at time.Time) ([]task, error) {
	tx, err := worker.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `WITH candidates AS (
		SELECT charge_id FROM image_charge_reconciliations
		WHERE (state='PENDING' AND next_attempt_at <= $1) OR (state='LEASED' AND lease_until <= $1)
		ORDER BY next_attempt_at,charge_id FOR UPDATE SKIP LOCKED LIMIT $2
	) UPDATE image_charge_reconciliations reconciliation
	SET state='LEASED',lease_owner=$3,lease_until=$4,attempt_count=attempt_count+1,updated_at=$1
	FROM candidates,image_request_charges charge,provider_channels channel WHERE reconciliation.charge_id=candidates.charge_id AND charge.id=reconciliation.charge_id AND channel.id=charge.channel_id
	RETURNING reconciliation.charge_id,reconciliation.outcome,reconciliation.reason,reconciliation.response_status,reconciliation.response_headers,reconciliation.response_body,reconciliation.response_body_sha256,reconciliation.attempt_count,charge.request_id,charge.protocol,channel.provider,charge.channel_id`, at, worker.config.BatchSize, worker.owner, at.Add(worker.config.Lease))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tasks []task
	for rows.Next() {
		var item task
		var status *int
		var headersJSON, body, bodyHash []byte
		if err := rows.Scan(&item.ChargeID, &item.Outcome, &item.Reason, &status, &headersJSON, &body, &bodyHash, &item.Attempt, &item.RequestID, &item.Protocol, &item.Provider, &item.ChannelID); err != nil {
			return nil, err
		}
		if item.Outcome != billing.Unknown {
			if status == nil || json.Unmarshal(headersJSON, &item.Snapshot.Headers) != nil || len(bodyHash) != 32 {
				item.BodyHash = [32]byte{}
			} else {
				item.Snapshot.Status = *status
				item.Snapshot.Body = append([]byte(nil), body...)
				copy(item.BodyHash[:], bodyHash)
			}
		}
		tasks = append(tasks, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return tasks, nil
}

func (worker *Worker) resolve(ctx context.Context, task task) error {
	result, err := worker.pool.Exec(ctx, `UPDATE image_charge_reconciliations SET state='RESOLVED',lease_owner=NULL,lease_until=NULL,resolved_at=now(),last_error_category=NULL,updated_at=now() WHERE charge_id=$1 AND state='LEASED' AND lease_owner=$2`, task.ChargeID, worker.owner)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return errors.New("reconciliation lease lost")
	}
	return nil
}

func (worker *Worker) retry(ctx context.Context, task task, category string) (bool, error) {
	if task.Attempt >= worker.config.MaxAttempts {
		return true, worker.manual(ctx, task, category)
	}
	delay := backoff(worker.config.BaseBackoff, worker.config.MaxBackoff, task.Attempt)
	at := worker.now().UTC()
	result, err := worker.pool.Exec(ctx, `UPDATE image_charge_reconciliations SET state='PENDING',lease_owner=NULL,lease_until=NULL,next_attempt_at=$3,last_error_category=$4,updated_at=$2 WHERE charge_id=$1 AND state='LEASED' AND lease_owner=$5`, task.ChargeID, at, at.Add(delay), category, worker.owner)
	if err != nil {
		return false, err
	}
	if result.RowsAffected() != 1 {
		return false, errors.New("reconciliation lease lost")
	}
	return false, nil
}

func (worker *Worker) manual(ctx context.Context, task task, category string) error {
	result, err := worker.pool.Exec(ctx, `UPDATE image_charge_reconciliations SET state='MANUAL_REVIEW',lease_owner=NULL,lease_until=NULL,last_error_category=$3,updated_at=$2 WHERE charge_id=$1 AND state='LEASED' AND lease_owner=$4`, task.ChargeID, worker.now().UTC(), category, worker.owner)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return errors.New("reconciliation lease lost")
	}
	return nil
}

func backoff(base, maximum time.Duration, attempt int) time.Duration {
	result := base
	for index := 1; index < attempt && result < maximum; index++ {
		if result > maximum/2 {
			return maximum
		}
		result *= 2
	}
	if result > maximum {
		return maximum
	}
	return result
}
