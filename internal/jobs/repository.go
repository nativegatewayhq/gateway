// Package jobs persists protocol-neutral asynchronous jobs and leases.
package jobs

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	joboperation "github.com/nativegatewayhq/gateway/operations/job"
)

const defaultMaximumResultBytes = 32 * 1024 * 1024

type CreateRequest struct {
	RequestID, Protocol, Operation, Model string
	Owner                                 joboperation.Owner
	Provider, ChannelID, ChargeID         string
	IdempotencyKey                        string
	Fingerprint                           [32]byte
}

type ProviderAttempt struct {
	JobID, Provider, ChannelID, ProviderJobID string
	AttemptNo, PollCount                      int
	State, LeaseOwner, LeaseToken             string
	LeaseUntil, NextPollAt                    time.Time
}

type Lease struct{ ProviderAttempt }

type Repository struct {
	pool             *pgxpool.Pool
	entropy          io.Reader
	now              func() time.Time
	maximumBodyBytes int64
}

func NewRepository(pool *pgxpool.Pool, maximumBodyBytes int64) (*Repository, error) {
	if pool == nil || maximumBodyBytes < 1 || maximumBodyBytes > 256*1024*1024 {
		return nil, joboperation.ErrInvalid
	}
	return &Repository{pool: pool, entropy: rand.Reader, now: time.Now, maximumBodyBytes: maximumBodyBytes}, nil
}

func NewDefaultRepository(pool *pgxpool.Pool) (*Repository, error) {
	return NewRepository(pool, defaultMaximumResultBytes)
}

func (repository *Repository) Create(ctx context.Context, request CreateRequest) (joboperation.Job, bool, error) {
	if !validCreate(request) {
		return joboperation.Job{}, false, joboperation.ErrInvalid
	}
	tx, err := repository.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return joboperation.Job{}, false, err
	}
	defer tx.Rollback(ctx)
	identity := request.RequestID
	if request.IdempotencyKey != "" {
		identity = "idempotency:" + request.IdempotencyKey
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1 || ':' || $2,0))`, request.Owner.OrganizationID, "async-job:"+identity); err != nil {
		return joboperation.Job{}, false, err
	}
	existing, found, err := loadByIdentity(ctx, tx, request.Owner.OrganizationID, request.RequestID, request.IdempotencyKey, true)
	if err != nil {
		return joboperation.Job{}, false, err
	}
	if found {
		if !sameCreate(existing, request) {
			return joboperation.Job{}, false, joboperation.ErrConflict
		}
		return existing, true, tx.Commit(ctx)
	}
	id, err := repository.id("job_")
	if err != nil {
		return joboperation.Job{}, false, err
	}
	var key, fingerprint, charge any
	if request.IdempotencyKey != "" {
		key, fingerprint = request.IdempotencyKey, request.Fingerprint[:]
	}
	if request.ChargeID != "" {
		charge = request.ChargeID
	}
	_, err = tx.Exec(ctx, `INSERT INTO async_jobs(id,request_id,organization_id,project_id,api_key_id,protocol,operation,model,provider,channel_id,charge_id,idempotency_key,request_fingerprint,status)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,'PENDING')`, id, request.RequestID, request.Owner.OrganizationID, request.Owner.ProjectID, request.Owner.APIKeyID, request.Protocol, request.Operation, request.Model, request.Provider, request.ChannelID, charge, key, fingerprint)
	if err != nil {
		return joboperation.Job{}, false, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO async_job_events(job_id,version,event_type,to_status,source) VALUES($1,1,'CREATED','PENDING','api')`, id)
	if err != nil {
		return joboperation.Job{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return joboperation.Job{}, false, err
	}
	created := repository.now().UTC()
	return joboperation.Job{ID: id, RequestID: request.RequestID, Protocol: request.Protocol, Operation: request.Operation, Model: request.Model, Owner: request.Owner, Provider: request.Provider, ChannelID: request.ChannelID, ChargeID: request.ChargeID, IdempotencyKey: request.IdempotencyKey, Fingerprint: request.Fingerprint, Status: joboperation.Pending, SettlementState: "NONE", Version: 1, CreatedAt: created, UpdatedAt: created}, false, nil
}

func (repository *Repository) Get(ctx context.Context, owner joboperation.Owner, id string) (joboperation.Job, error) {
	if !joboperation.ValidID(id) || !validOwner(owner) {
		return joboperation.Job{}, joboperation.ErrInvalid
	}
	result, found, err := scanJob(repository.pool.QueryRow(ctx, jobSelect+` WHERE id=$1 AND organization_id=$2 AND project_id=$3 AND api_key_id=$4`, id, owner.OrganizationID, owner.ProjectID, owner.APIKeyID))
	if err != nil {
		return joboperation.Job{}, err
	}
	if !found {
		return joboperation.Job{}, joboperation.ErrNotFound
	}
	return result, nil
}

func (repository *Repository) BeginSubmit(ctx context.Context, owner joboperation.Owner, id string) (ProviderAttempt, error) {
	tx, err := repository.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return ProviderAttempt{}, err
	}
	defer tx.Rollback(ctx)
	current, found, err := loadOwned(ctx, tx, owner, id, true)
	if err != nil {
		return ProviderAttempt{}, err
	}
	if !found {
		return ProviderAttempt{}, joboperation.ErrNotFound
	}
	if current.Status != joboperation.Pending {
		return ProviderAttempt{}, joboperation.ErrConflict
	}
	result, err := tx.Exec(ctx, `INSERT INTO async_job_provider_attempts(job_id,attempt_no,provider,channel_id,state) VALUES($1,1,$2,$3,'SUBMITTING') ON CONFLICT DO NOTHING`, id, current.Provider, current.ChannelID)
	if err != nil {
		return ProviderAttempt{}, err
	}
	if result.RowsAffected() != 1 {
		return ProviderAttempt{}, joboperation.ErrConflict
	}
	if err := appendEvent(ctx, tx, id, current.Version, current.Status, current.Status, "SUBMIT_STARTED", "submit", ""); err != nil {
		return ProviderAttempt{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ProviderAttempt{}, err
	}
	return ProviderAttempt{JobID: id, AttemptNo: 1, Provider: current.Provider, ChannelID: current.ChannelID, State: "SUBMITTING"}, nil
}

func (repository *Repository) ConfirmSubmit(ctx context.Context, owner joboperation.Owner, id, providerJobID string, status joboperation.Status, nextPollAt time.Time) (joboperation.Job, error) {
	if strings.TrimSpace(providerJobID) == "" || len(providerJobID) > 500 || (status != joboperation.Queued && status != joboperation.Processing) || nextPollAt.IsZero() {
		return joboperation.Job{}, joboperation.ErrInvalid
	}
	return repository.finishSubmit(ctx, owner, id, providerJobID, status, nextPollAt.UTC(), false)
}

func (repository *Repository) MarkSubmitUnknown(ctx context.Context, owner joboperation.Owner, id string, nextPollAt time.Time) (joboperation.Job, error) {
	if nextPollAt.IsZero() {
		return joboperation.Job{}, joboperation.ErrInvalid
	}
	return repository.finishSubmit(ctx, owner, id, "", joboperation.Reconciling, nextPollAt.UTC(), true)
}

func (repository *Repository) finishSubmit(ctx context.Context, owner joboperation.Owner, id, providerJobID string, status joboperation.Status, nextPollAt time.Time, unknown bool) (joboperation.Job, error) {
	tx, err := repository.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return joboperation.Job{}, err
	}
	defer tx.Rollback(ctx)
	current, found, err := loadOwned(ctx, tx, owner, id, true)
	if err != nil {
		return joboperation.Job{}, err
	}
	if !found {
		return joboperation.Job{}, joboperation.ErrNotFound
	}
	if current.Status != joboperation.Pending {
		return joboperation.Job{}, joboperation.ErrConflict
	}
	state, event := "SUBMITTED", "SUBMIT_CONFIRMED"
	var providerID any = providerJobID
	if unknown {
		state, event, providerID = "RECONCILING", "SUBMIT_UNKNOWN", nil
	}
	result, err := tx.Exec(ctx, `UPDATE async_job_provider_attempts SET provider_job_id=$3,state=$4,next_poll_at=$5,updated_at=now() WHERE job_id=$1 AND attempt_no=1 AND state='SUBMITTING' AND provider=$2`, id, current.Provider, providerID, state, nextPollAt)
	if err != nil {
		return joboperation.Job{}, err
	}
	if result.RowsAffected() != 1 {
		return joboperation.Job{}, joboperation.ErrConflict
	}
	if err := updateJobStatus(ctx, tx, current, status, joboperation.Observation{Status: status}, "submit", event); err != nil {
		return joboperation.Job{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return joboperation.Job{}, err
	}
	return repository.Get(ctx, owner, id)
}

func (repository *Repository) ClaimDue(ctx context.Context, owner string, at time.Time, lease time.Duration, limit int) ([]Lease, error) {
	if owner == "" || len(owner) > 128 || lease <= 0 || limit < 1 || limit > 100 {
		return nil, joboperation.ErrInvalid
	}
	token, err := repository.id("lease_")
	if err != nil {
		return nil, err
	}
	tx, err := repository.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `WITH candidates AS (
		SELECT attempt.job_id,attempt.attempt_no FROM async_job_provider_attempts attempt JOIN async_jobs job ON job.id=attempt.job_id
		WHERE job.status NOT IN ('SUCCEEDED','FAILED','CANCELED') AND attempt.state IN ('SUBMITTED','RECONCILING')
		AND ((attempt.lease_until IS NULL AND attempt.next_poll_at<=$1) OR attempt.lease_until<=$1)
		ORDER BY attempt.next_poll_at,attempt.job_id FOR UPDATE SKIP LOCKED LIMIT $2
	) UPDATE async_job_provider_attempts attempt SET lease_owner=$3,lease_token=$4,lease_until=$5,poll_count=poll_count+1,updated_at=$1
	FROM candidates WHERE attempt.job_id=candidates.job_id AND attempt.attempt_no=candidates.attempt_no
	RETURNING attempt.job_id,attempt.attempt_no,attempt.provider,attempt.channel_id,attempt.provider_job_id,attempt.state,attempt.lease_owner,attempt.lease_token,attempt.lease_until,attempt.poll_count,attempt.next_poll_at`, at.UTC(), limit, owner, token, at.UTC().Add(lease))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var leases []Lease
	for rows.Next() {
		var item Lease
		var providerJobID *string
		if err := rows.Scan(&item.JobID, &item.AttemptNo, &item.Provider, &item.ChannelID, &providerJobID, &item.State, &item.LeaseOwner, &item.LeaseToken, &item.LeaseUntil, &item.PollCount, &item.NextPollAt); err != nil {
			return nil, err
		}
		if providerJobID != nil {
			item.ProviderJobID = *providerJobID
		}
		leases = append(leases, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return leases, nil
}

func (repository *Repository) ApplyObservation(ctx context.Context, lease Lease, observation joboperation.Observation, source string) (joboperation.Job, error) {
	if source != "poll" && source != "webhook" && source != "cancel" && source != "reconciliation" {
		return joboperation.Job{}, joboperation.ErrInvalid
	}
	tx, err := repository.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return joboperation.Job{}, err
	}
	defer tx.Rollback(ctx)
	current, found, err := loadByID(ctx, tx, lease.JobID, true)
	if err != nil {
		return joboperation.Job{}, err
	}
	if !found {
		return joboperation.Job{}, joboperation.ErrNotFound
	}
	if current.Status.Terminal() {
		if joboperation.SameTerminal(current, observation) {
			return current, tx.Commit(ctx)
		}
		return joboperation.Job{}, joboperation.ErrConflict
	}
	if err := joboperation.ValidateObservation(current.Status, observation, repository.maximumBodyBytes); err != nil {
		return joboperation.Job{}, err
	}
	if lease.LeaseToken != "" {
		var valid bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM async_job_provider_attempts WHERE job_id=$1 AND attempt_no=$2 AND lease_owner=$3 AND lease_token=$4 AND lease_until>now())`, lease.JobID, lease.AttemptNo, lease.LeaseOwner, lease.LeaseToken).Scan(&valid)
		if err != nil {
			return joboperation.Job{}, err
		}
		if !valid {
			return joboperation.Job{}, joboperation.ErrLeaseLost
		}
	}
	if err := updateJobStatus(ctx, tx, current, observation.Status, observation, source, "OBSERVED"); err != nil {
		return joboperation.Job{}, err
	}
	state := "SUBMITTED"
	if observation.Status == joboperation.Reconciling {
		state = "RECONCILING"
	}
	if observation.Status.Terminal() {
		state = "TERMINAL"
	}
	_, err = tx.Exec(ctx, `UPDATE async_job_provider_attempts SET state=$3,lease_owner=NULL,lease_token=NULL,lease_until=NULL,updated_at=now() WHERE job_id=$1 AND attempt_no=$2`, lease.JobID, lease.AttemptNo, state)
	if err != nil {
		return joboperation.Job{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return joboperation.Job{}, err
	}
	return repository.getUnowned(ctx, lease.JobID)
}

func (repository *Repository) Reschedule(ctx context.Context, lease Lease, next time.Time, category string) error {
	if next.IsZero() || !validCategory(category) {
		return joboperation.ErrInvalid
	}
	result, err := repository.pool.Exec(ctx, `UPDATE async_job_provider_attempts SET lease_owner=NULL,lease_token=NULL,lease_until=NULL,next_poll_at=$5,last_error_category=$6,updated_at=now() WHERE job_id=$1 AND attempt_no=$2 AND lease_owner=$3 AND lease_token=$4`, lease.JobID, lease.AttemptNo, lease.LeaseOwner, lease.LeaseToken, next.UTC(), category)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return joboperation.ErrLeaseLost
	}
	return nil
}

func (repository *Repository) id(prefix string) (string, error) {
	value := make([]byte, 16)
	if _, err := io.ReadFull(repository.entropy, value); err != nil {
		return "", fmt.Errorf("generate async job identifier: %w", err)
	}
	return prefix + hex.EncodeToString(value), nil
}

func validOwner(owner joboperation.Owner) bool {
	return owner.OrganizationID != "" && owner.ProjectID != "" && owner.APIKeyID != "" && len(owner.OrganizationID) <= 128 && len(owner.ProjectID) <= 128 && len(owner.APIKeyID) <= 200
}
func validCreate(request CreateRequest) bool {
	return validOwner(request.Owner) && request.RequestID != "" && len(request.RequestID) <= 128 && request.Protocol != "" && request.Protocol == strings.ToLower(request.Protocol) && len(request.Protocol) <= 40 && request.Operation != "" && request.Operation == strings.ToLower(request.Operation) && len(request.Operation) <= 80 && strings.TrimSpace(request.Model) == request.Model && request.Model != "" && len(request.Model) <= 200 && request.Provider != "" && request.Provider == strings.ToLower(request.Provider) && len(request.Provider) <= 40 && request.ChannelID != "" && len(request.IdempotencyKey) <= 256 && (request.IdempotencyKey == "" || request.Fingerprint != ([32]byte{}))
}
func validCategory(value string) bool {
	return value != "" && len(value) <= 80 && !strings.ContainsAny(value, "\r\n")
}

func sameCreate(existing joboperation.Job, request CreateRequest) bool {
	return existing.Owner == request.Owner && existing.Protocol == request.Protocol && existing.Operation == request.Operation && existing.Model == request.Model && existing.Provider == request.Provider && existing.ChannelID == request.ChannelID && existing.ChargeID == request.ChargeID && existing.IdempotencyKey == request.IdempotencyKey && (request.IdempotencyKey == "" || existing.Fingerprint == request.Fingerprint)
}

const jobSelect = `SELECT id,request_id,organization_id,project_id,api_key_id,protocol,operation,model,provider,channel_id,charge_id,idempotency_key,request_fingerprint,status,settlement_state,version,failure_category,response_status,response_headers,response_body,response_body_sha256,created_at,updated_at,completed_at FROM async_jobs`

func scanJob(row pgx.Row) (joboperation.Job, bool, error) {
	var item joboperation.Job
	var charge, key, category *string
	var fingerprint, headers, body, digest []byte
	var status *int
	err := row.Scan(&item.ID, &item.RequestID, &item.Owner.OrganizationID, &item.Owner.ProjectID, &item.Owner.APIKeyID, &item.Protocol, &item.Operation, &item.Model, &item.Provider, &item.ChannelID, &charge, &key, &fingerprint, &item.Status, &item.SettlementState, &item.Version, &category, &status, &headers, &body, &digest, &item.CreatedAt, &item.UpdatedAt, &item.CompletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return item, false, nil
	}
	if err != nil {
		return item, false, err
	}
	if charge != nil {
		item.ChargeID = *charge
	}
	if key != nil {
		item.IdempotencyKey = *key
	}
	if category != nil {
		item.FailureCategory = *category
	}
	copy(item.Fingerprint[:], fingerprint)
	if status != nil {
		item.Snapshot.Status = *status
		item.Snapshot.Body = append([]byte(nil), body...)
		if err := json.Unmarshal(headers, &item.Snapshot.Headers); err != nil {
			return item, false, joboperation.ErrInvalid
		}
		copy(item.Snapshot.SHA256[:], digest)
	}
	return item, true, nil
}
func loadByID(ctx context.Context, tx pgx.Tx, id string, lock bool) (joboperation.Job, bool, error) {
	query := jobSelect + ` WHERE id=$1`
	if lock {
		query += ` FOR UPDATE`
	}
	return scanJob(tx.QueryRow(ctx, query, id))
}
func loadOwned(ctx context.Context, tx pgx.Tx, owner joboperation.Owner, id string, lock bool) (joboperation.Job, bool, error) {
	if !validOwner(owner) || !joboperation.ValidID(id) {
		return joboperation.Job{}, false, joboperation.ErrInvalid
	}
	query := jobSelect + ` WHERE id=$1 AND organization_id=$2 AND project_id=$3 AND api_key_id=$4`
	if lock {
		query += ` FOR UPDATE`
	}
	return scanJob(tx.QueryRow(ctx, query, id, owner.OrganizationID, owner.ProjectID, owner.APIKeyID))
}
func loadByIdentity(ctx context.Context, tx pgx.Tx, organizationID, requestID, key string, lock bool) (joboperation.Job, bool, error) {
	query := jobSelect + ` WHERE organization_id=$1 AND request_id=$2`
	args := []any{organizationID, requestID}
	if key != "" {
		query = jobSelect + ` WHERE organization_id=$1 AND idempotency_key=$2`
		args = []any{organizationID, key}
	}
	if lock {
		query += ` FOR UPDATE`
	}
	return scanJob(tx.QueryRow(ctx, query, args...))
}
func (repository *Repository) getUnowned(ctx context.Context, id string) (joboperation.Job, error) {
	item, found, err := scanJob(repository.pool.QueryRow(ctx, jobSelect+` WHERE id=$1`, id))
	if err != nil {
		return item, err
	}
	if !found {
		return item, joboperation.ErrNotFound
	}
	return item, nil
}

func appendEvent(ctx context.Context, tx pgx.Tx, id string, version int64, from, to joboperation.Status, event, source, category string) error {
	_, err := tx.Exec(ctx, `INSERT INTO async_job_events(job_id,version,event_type,from_status,to_status,source,category) VALUES($1,$2,$3,$4,$5,$6,$7)`, id, version+1, event, string(from), string(to), source, nullable(category))
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE async_jobs SET version=version+1,updated_at=now() WHERE id=$1 AND version=$2`, id, version)
	return err
}
func updateJobStatus(ctx context.Context, tx pgx.Tx, current joboperation.Job, status joboperation.Status, observation joboperation.Observation, source, event string) error {
	if !joboperation.CanTransition(current.Status, status) {
		return joboperation.ErrInvalidState
	}
	var responseStatus, headers, body, digest, completed, category, settlement any
	if status == joboperation.Succeeded || status == joboperation.Failed {
		canonical := observation.Snapshot
		canonical.SHA256 = sha256.Sum256(canonical.Body)
		encoded, err := json.Marshal(canonical.Headers)
		if err != nil {
			return joboperation.ErrInvalid
		}
		responseStatus, headers, body, digest = canonical.Status, string(encoded), canonical.Body, canonical.SHA256[:]
	}
	if observation.FailureCategory != "" {
		category = observation.FailureCategory
	}
	if status.Terminal() {
		completed = time.Now().UTC()
		settlement = "PENDING"
	} else {
		settlement = "NONE"
	}
	result, err := tx.Exec(ctx, `UPDATE async_jobs SET status=$3,settlement_state=$4,failure_category=$5,response_status=$6,response_headers=$7::text::jsonb,response_body=$8,response_body_sha256=$9,completed_at=$10,version=version+1,updated_at=now() WHERE id=$1 AND version=$2`, current.ID, current.Version, status, settlement, category, responseStatus, headers, body, digest, completed)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return joboperation.ErrConflict
	}
	_, err = tx.Exec(ctx, `INSERT INTO async_job_events(job_id,version,event_type,from_status,to_status,source,category) VALUES($1,$2,$3,$4,$5,$6,$7)`, current.ID, current.Version+1, event, current.Status, status, source, category)
	return err
}
func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}
