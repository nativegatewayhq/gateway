// Package jobs persists protocol-neutral asynchronous jobs and leases.
package jobs

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
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

var (
	ErrWebhookRejected = errors.New("webhook rejected")
	ErrWebhookNotReady = errors.New("webhook binding not ready")
)

type CreateRequest struct {
	RequestID, Protocol, Operation, Model string
	Owner                                 joboperation.Owner
	Provider, ChannelID, ChargeID         string
	IdempotencyKey                        string
	Fingerprint                           [32]byte
	EstimatedUsage                        *joboperation.Usage
}

type ProviderAttempt struct {
	JobID, Model, Provider, ChannelID, ProviderJobID string
	AttemptNo, PollCount                             int
	State, LeaseOwner, LeaseToken                    string
	LeaseUntil, NextPollAt                           time.Time
}

type Lease struct{ ProviderAttempt }

type WebhookBinding struct {
	JobID, Provider, ChannelID, Token string
	ExpiresAt                         time.Time
}

type WebhookObservation struct {
	JobID, Provider, DeliveryID, Token, ProviderJobID string
	Observation                                       joboperation.Observation
	CallbackSecret                                    []byte
}

type SettlementLease struct {
	Job          joboperation.Job
	Owner, Token string
	Until        time.Time
	Attempt      int
	ActualUsage  *joboperation.Usage
	UsageReason  string
}

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

func (repository *Repository) Ready(ctx context.Context) error { return repository.pool.Ping(ctx) }

func (repository *Repository) CreateWebhookBinding(ctx context.Context, jobID, provider, channelID string, callbackSecret []byte, ttl time.Duration) (WebhookBinding, error) {
	if !joboperation.ValidID(jobID) || !validWebhookProvider(provider) || channelID == "" || len(callbackSecret) != 32 || ttl <= 0 || ttl > 30*24*time.Hour {
		return WebhookBinding{}, joboperation.ErrInvalid
	}
	token, err := repository.id("whk_")
	if err != nil {
		return WebhookBinding{}, err
	}
	digest := webhookTokenDigest(callbackSecret, token)
	expires := repository.now().UTC().Add(ttl)
	result, err := repository.pool.Exec(ctx, `INSERT INTO async_job_webhook_bindings(job_id,provider,channel_id,token_digest,expires_at)
		SELECT id,$2,$3,$4,$5 FROM async_jobs WHERE id=$1 AND provider=$2 AND channel_id=$3 ON CONFLICT DO NOTHING`, jobID, provider, channelID, digest[:], expires)
	if err != nil {
		return WebhookBinding{}, err
	}
	if result.RowsAffected() != 1 {
		return WebhookBinding{}, joboperation.ErrConflict
	}
	return WebhookBinding{JobID: jobID, Provider: provider, ChannelID: channelID, Token: token, ExpiresAt: expires}, nil
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
	var usageDimension, usageUnit, estimatedQuantity, extractorVersion, resultExtractorVersion any
	if request.EstimatedUsage != nil {
		usageDimension, usageUnit, estimatedQuantity, extractorVersion, resultExtractorVersion = request.EstimatedUsage.Dimension, request.EstimatedUsage.Unit, request.EstimatedUsage.Quantity, request.EstimatedUsage.ExtractorVersion, request.EstimatedUsage.ResultExtractorVersion
	}
	_, err = tx.Exec(ctx, `INSERT INTO async_jobs(id,request_id,organization_id,project_id,api_key_id,protocol,operation,model,provider,channel_id,charge_id,idempotency_key,request_fingerprint,status,usage_dimension,usage_unit,estimated_quantity,usage_extractor_version,usage_result_extractor_version)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,'PENDING',$14,$15,$16,$17,$18)`, id, request.RequestID, request.Owner.OrganizationID, request.Owner.ProjectID, request.Owner.APIKeyID, request.Protocol, request.Operation, request.Model, request.Provider, request.ChannelID, charge, key, fingerprint, usageDimension, usageUnit, estimatedQuantity, extractorVersion, resultExtractorVersion)
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
	return joboperation.Job{ID: id, RequestID: request.RequestID, Protocol: request.Protocol, Operation: request.Operation, Model: request.Model, Owner: request.Owner, Provider: request.Provider, ChannelID: request.ChannelID, ChargeID: request.ChargeID, IdempotencyKey: request.IdempotencyKey, Fingerprint: request.Fingerprint, Status: joboperation.Pending, SettlementState: "NONE", EstimatedUsage: cloneUsage(request.EstimatedUsage), Version: 1, CreatedAt: created, UpdatedAt: created}, false, nil
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

func (repository *Repository) BeginSubmit(ctx context.Context, owner joboperation.Owner, id, leaseOwner string, lease time.Duration) (ProviderAttempt, error) {
	if leaseOwner == "" || len(leaseOwner) > 128 || lease <= 0 {
		return ProviderAttempt{}, joboperation.ErrInvalid
	}
	token, err := repository.id("lease_")
	if err != nil {
		return ProviderAttempt{}, err
	}
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
	until := repository.now().UTC().Add(lease)
	result, err := tx.Exec(ctx, `INSERT INTO async_job_provider_attempts(job_id,attempt_no,provider,channel_id,state,lease_owner,lease_token,lease_until) VALUES($1,1,$2,$3,'SUBMITTING',$4,$5,$6) ON CONFLICT DO NOTHING`, id, current.Provider, current.ChannelID, leaseOwner, token, until)
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
	return ProviderAttempt{JobID: id, Model: current.Model, AttemptNo: 1, Provider: current.Provider, ChannelID: current.ChannelID, State: "SUBMITTING", LeaseOwner: leaseOwner, LeaseToken: token, LeaseUntil: until}, nil
}

func (repository *Repository) ConfirmSubmit(ctx context.Context, owner joboperation.Owner, attempt ProviderAttempt, providerJobID string, status joboperation.Status, nextPollAt time.Time) (joboperation.Job, error) {
	if strings.TrimSpace(providerJobID) == "" || len(providerJobID) > 500 || (status != joboperation.Queued && status != joboperation.Processing) || nextPollAt.IsZero() {
		return joboperation.Job{}, joboperation.ErrInvalid
	}
	return repository.finishSubmit(ctx, owner, attempt, providerJobID, status, nextPollAt.UTC(), false)
}

func (repository *Repository) MarkSubmitUnknown(ctx context.Context, owner joboperation.Owner, attempt ProviderAttempt, nextPollAt time.Time) (joboperation.Job, error) {
	if nextPollAt.IsZero() {
		return joboperation.Job{}, joboperation.ErrInvalid
	}
	return repository.finishSubmit(ctx, owner, attempt, "", joboperation.Reconciling, nextPollAt.UTC(), true)
}

func (repository *Repository) finishSubmit(ctx context.Context, owner joboperation.Owner, attempt ProviderAttempt, providerJobID string, status joboperation.Status, nextPollAt time.Time, unknown bool) (joboperation.Job, error) {
	tx, err := repository.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return joboperation.Job{}, err
	}
	defer tx.Rollback(ctx)
	current, found, err := loadOwned(ctx, tx, owner, attempt.JobID, true)
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
	result, err := tx.Exec(ctx, `UPDATE async_job_provider_attempts SET provider_job_id=$3,state=$4,next_poll_at=$5,lease_owner=NULL,lease_token=NULL,lease_until=NULL,updated_at=now() WHERE job_id=$1 AND attempt_no=1 AND state='SUBMITTING' AND provider=$2 AND lease_owner=$6 AND lease_token=$7`, attempt.JobID, current.Provider, providerID, state, nextPollAt, attempt.LeaseOwner, attempt.LeaseToken)
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
	return repository.Get(ctx, owner, attempt.JobID)
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
		SELECT attempt.job_id,attempt.attempt_no,job.model FROM async_job_provider_attempts attempt JOIN async_jobs job ON job.id=attempt.job_id
		WHERE job.status NOT IN ('SUCCEEDED','FAILED','CANCELED') AND attempt.state IN ('SUBMITTING','SUBMITTED','RECONCILING')
		AND ((attempt.lease_until IS NULL AND attempt.next_poll_at<=$1) OR attempt.lease_until<=$1)
		ORDER BY attempt.next_poll_at,attempt.job_id FOR UPDATE SKIP LOCKED LIMIT $2
	) UPDATE async_job_provider_attempts attempt SET lease_owner=$3,lease_token=$4,lease_until=$5,poll_count=poll_count+1,updated_at=$1
	FROM candidates WHERE attempt.job_id=candidates.job_id AND attempt.attempt_no=candidates.attempt_no
		RETURNING attempt.job_id,candidates.model,attempt.attempt_no,attempt.provider,attempt.channel_id,attempt.provider_job_id,attempt.state,attempt.lease_owner,attempt.lease_token,attempt.lease_until,attempt.poll_count,attempt.next_poll_at`, at.UTC(), limit, owner, token, at.UTC().Add(lease))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var leases []Lease
	for rows.Next() {
		var item Lease
		var providerJobID *string
		if err := rows.Scan(&item.JobID, &item.Model, &item.AttemptNo, &item.Provider, &item.ChannelID, &providerJobID, &item.State, &item.LeaseOwner, &item.LeaseToken, &item.LeaseUntil, &item.PollCount, &item.NextPollAt); err != nil {
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

func (repository *Repository) ApplyObservation(ctx context.Context, lease Lease, observation joboperation.Observation, source string, nextPollAt time.Time) (joboperation.Job, error) {
	if source != "submit" && source != "poll" && source != "webhook" && source != "cancel" && source != "reconciliation" {
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
	if current.Status.Terminal() && current.EstimatedUsage != nil && current.ActualUsage == nil {
		if err := hydrateActualUsage(ctx, tx, &current); err != nil {
			return joboperation.Job{}, err
		}
	}
	if current.Status.Terminal() {
		if joboperation.SameTerminal(current, observation) && sameActualUsage(current, observation) {
			return current, tx.Commit(ctx)
		}
		return joboperation.Job{}, joboperation.ErrConflict
	}
	if err := joboperation.ValidateObservation(current.Status, observation, repository.maximumBodyBytes); err != nil {
		return joboperation.Job{}, err
	}
	if lease.ProviderJobID == "" && observation.ProviderJobID == "" && !observation.Status.Terminal() && observation.Status != joboperation.Reconciling {
		return joboperation.Job{}, joboperation.ErrInvalid
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
	providerJobID := lease.ProviderJobID
	if observation.ProviderJobID != "" {
		providerJobID = observation.ProviderJobID
	}
	if providerJobID == "" && state == "SUBMITTED" {
		state = "RECONCILING"
	}
	if !observation.Status.Terminal() && nextPollAt.IsZero() {
		return joboperation.Job{}, joboperation.ErrInvalid
	}
	_, err = tx.Exec(ctx, `UPDATE async_job_provider_attempts SET state=$3,provider_job_id=COALESCE($5,provider_job_id),lease_owner=NULL,lease_token=NULL,lease_until=NULL,next_poll_at=$4,updated_at=now() WHERE job_id=$1 AND attempt_no=$2`, lease.JobID, lease.AttemptNo, state, nextPollAt.UTC(), nullable(providerJobID))
	if err != nil {
		return joboperation.Job{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return joboperation.Job{}, err
	}
	return repository.getUnowned(ctx, lease.JobID)
}

// ApplyWebhook durably records one verified Provider delivery and its terminal
// observation in the same transaction. Signature verification belongs at the
// HTTP boundary; this method enforces the independent callback capability and
// Provider identity binding before mutating the Job or settlement intent.
func (repository *Repository) ApplyWebhook(ctx context.Context, request WebhookObservation) (joboperation.Job, bool, error) {
	if !joboperation.ValidID(request.JobID) || !validWebhookProvider(request.Provider) || request.DeliveryID == "" || len(request.DeliveryID) > 200 || request.Token == "" || len(request.CallbackSecret) != 32 || len(request.ProviderJobID) > 500 || !request.Observation.Status.Terminal() {
		return joboperation.Job{}, false, joboperation.ErrInvalid
	}
	if request.Observation.ProviderJobID != "" && request.Observation.ProviderJobID != request.ProviderJobID {
		return joboperation.Job{}, false, fmt.Errorf("%w: observation identity", ErrWebhookRejected)
	}
	tx, err := repository.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return joboperation.Job{}, false, err
	}
	defer tx.Rollback(ctx)

	var channelID string
	var digest []byte
	var expiresAt time.Time
	var disabledAt *time.Time
	err = tx.QueryRow(ctx, `SELECT channel_id,token_digest,expires_at,disabled_at FROM async_job_webhook_bindings WHERE job_id=$1 AND provider=$2 FOR UPDATE`, request.JobID, request.Provider).Scan(&channelID, &digest, &expiresAt, &disabledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return joboperation.Job{}, false, fmt.Errorf("%w: binding missing", ErrWebhookRejected)
	}
	if err != nil {
		return joboperation.Job{}, false, err
	}
	presented := webhookTokenDigest(request.CallbackSecret, request.Token)
	if disabledAt != nil || !repository.now().UTC().Before(expiresAt) || len(digest) != sha256.Size || subtle.ConstantTimeCompare(digest, presented) != 1 {
		return joboperation.Job{}, false, fmt.Errorf("%w: capability", ErrWebhookRejected)
	}

	current, found, err := loadByID(ctx, tx, request.JobID, true)
	if err != nil {
		return joboperation.Job{}, false, err
	}
	if !found || current.Provider != request.Provider || current.ChannelID != channelID {
		return joboperation.Job{}, false, fmt.Errorf("%w: job binding", ErrWebhookRejected)
	}
	if current.Status.Terminal() && current.EstimatedUsage != nil && current.ActualUsage == nil {
		if err := hydrateActualUsage(ctx, tx, &current); err != nil {
			return joboperation.Job{}, false, err
		}
	}
	var attempt ProviderAttempt
	var storedProviderJobID *string
	err = tx.QueryRow(ctx, `SELECT job_id,attempt_no,provider,channel_id,provider_job_id,state FROM async_job_provider_attempts WHERE job_id=$1 AND attempt_no=1 FOR UPDATE`, request.JobID).Scan(&attempt.JobID, &attempt.AttemptNo, &attempt.Provider, &attempt.ChannelID, &storedProviderJobID, &attempt.State)
	if errors.Is(err, pgx.ErrNoRows) {
		return joboperation.Job{}, false, fmt.Errorf("%w: attempt missing", ErrWebhookRejected)
	}
	if err != nil {
		return joboperation.Job{}, false, err
	}
	if storedProviderJobID == nil && attempt.State == "SUBMITTING" {
		return joboperation.Job{}, false, ErrWebhookNotReady
	}
	if storedProviderJobID == nil || *storedProviderJobID != request.ProviderJobID || attempt.Provider != request.Provider || attempt.ChannelID != channelID {
		return joboperation.Job{}, false, fmt.Errorf("%w: provider identity", ErrWebhookRejected)
	}
	attempt.ProviderJobID = *storedProviderJobID

	var deliveredJobID, deliveredStatus string
	err = tx.QueryRow(ctx, `SELECT job_id,terminal_status FROM async_job_webhook_deliveries WHERE provider=$1 AND delivery_id=$2`, request.Provider, request.DeliveryID).Scan(&deliveredJobID, &deliveredStatus)
	if err == nil {
		if deliveredJobID != request.JobID || deliveredStatus != string(request.Observation.Status) || !joboperation.SameTerminal(current, request.Observation) || !sameActualUsage(current, request.Observation) {
			return joboperation.Job{}, false, fmt.Errorf("%w: delivery collision", ErrWebhookRejected)
		}
		if err := tx.Commit(ctx); err != nil {
			return joboperation.Job{}, false, err
		}
		return current, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return joboperation.Job{}, false, err
	}

	if current.Status.Terminal() {
		if !joboperation.SameTerminal(current, request.Observation) || !sameActualUsage(current, request.Observation) {
			return joboperation.Job{}, false, joboperation.ErrConflict
		}
	} else {
		if err := joboperation.ValidateObservation(current.Status, request.Observation, repository.maximumBodyBytes); err != nil {
			return joboperation.Job{}, false, err
		}
		if err := updateJobStatus(ctx, tx, current, request.Observation.Status, request.Observation, "webhook", "OBSERVED"); err != nil {
			return joboperation.Job{}, false, err
		}
		result, err := tx.Exec(ctx, `UPDATE async_job_provider_attempts SET state='TERMINAL',lease_owner=NULL,lease_token=NULL,lease_until=NULL,next_poll_at='infinity',updated_at=now() WHERE job_id=$1 AND attempt_no=1 AND provider=$2 AND channel_id=$3 AND provider_job_id=$4`, request.JobID, request.Provider, channelID, request.ProviderJobID)
		if err != nil {
			return joboperation.Job{}, false, err
		}
		if result.RowsAffected() != 1 {
			return joboperation.Job{}, false, fmt.Errorf("%w: attempt changed", ErrWebhookRejected)
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO async_job_webhook_deliveries(provider,delivery_id,job_id,terminal_status) VALUES($1,$2,$3,$4)`, request.Provider, request.DeliveryID, request.JobID, request.Observation.Status)
	if err != nil {
		return joboperation.Job{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return joboperation.Job{}, false, err
	}
	result, err := repository.getUnowned(ctx, request.JobID)
	return result, false, err
}

func webhookTokenDigest(secret []byte, token string) []byte {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(token))
	return mac.Sum(nil)
}

func validWebhookProvider(provider string) bool { return provider == "replicate" || provider == "fal" }

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

func (repository *Repository) Heartbeat(ctx context.Context, lease Lease, until time.Time) (Lease, error) {
	if until.IsZero() || until.Before(repository.now().UTC()) {
		return Lease{}, joboperation.ErrInvalid
	}
	result, err := repository.pool.Exec(ctx, `UPDATE async_job_provider_attempts SET lease_until=$5,updated_at=now() WHERE job_id=$1 AND attempt_no=$2 AND lease_owner=$3 AND lease_token=$4 AND lease_until>now()`, lease.JobID, lease.AttemptNo, lease.LeaseOwner, lease.LeaseToken, until.UTC())
	if err != nil {
		return Lease{}, err
	}
	if result.RowsAffected() != 1 {
		return Lease{}, joboperation.ErrLeaseLost
	}
	lease.LeaseUntil = until.UTC()
	return lease, nil
}

func (repository *Repository) ClaimCancel(ctx context.Context, owner joboperation.Owner, id, leaseOwner string, lease time.Duration) (joboperation.Job, Lease, bool, error) {
	if leaseOwner == "" || len(leaseOwner) > 128 || lease <= 0 {
		return joboperation.Job{}, Lease{}, false, joboperation.ErrInvalid
	}
	token, err := repository.id("lease_")
	if err != nil {
		return joboperation.Job{}, Lease{}, false, err
	}
	tx, err := repository.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return joboperation.Job{}, Lease{}, false, err
	}
	defer tx.Rollback(ctx)
	current, found, err := loadOwned(ctx, tx, owner, id, true)
	if err != nil {
		return joboperation.Job{}, Lease{}, false, err
	}
	if !found {
		return joboperation.Job{}, Lease{}, false, joboperation.ErrNotFound
	}
	if current.Status.Terminal() {
		return current, Lease{}, false, tx.Commit(ctx)
	}
	var attempt ProviderAttempt
	attempt.Model = current.Model
	var providerID *string
	err = tx.QueryRow(ctx, `SELECT job_id,attempt_no,provider,channel_id,provider_job_id,state,poll_count,next_poll_at FROM async_job_provider_attempts WHERE job_id=$1 AND attempt_no=1 FOR UPDATE`, id).Scan(&attempt.JobID, &attempt.AttemptNo, &attempt.Provider, &attempt.ChannelID, &providerID, &attempt.State, &attempt.PollCount, &attempt.NextPollAt)
	if errors.Is(err, pgx.ErrNoRows) || attempt.State == "SUBMITTING" || providerID == nil {
		return current, Lease{}, false, tx.Commit(ctx)
	}
	if err != nil {
		return joboperation.Job{}, Lease{}, false, err
	}
	var already bool
	if err := tx.QueryRow(ctx, `SELECT cancel_requested_at IS NOT NULL FROM async_job_provider_attempts WHERE job_id=$1 AND attempt_no=1`, id).Scan(&already); err != nil {
		return joboperation.Job{}, Lease{}, false, err
	}
	if already {
		return current, Lease{}, false, tx.Commit(ctx)
	}
	attempt.ProviderJobID = *providerID
	attempt.LeaseOwner = leaseOwner
	attempt.LeaseToken = token
	attempt.LeaseUntil = repository.now().UTC().Add(lease)
	_, err = tx.Exec(ctx, `UPDATE async_job_provider_attempts SET cancel_requested_at=now(),lease_owner=$2,lease_token=$3,lease_until=$4,updated_at=now() WHERE job_id=$1`, id, leaseOwner, token, attempt.LeaseUntil)
	if err != nil {
		return joboperation.Job{}, Lease{}, false, err
	}
	if err := appendEvent(ctx, tx, id, current.Version, current.Status, current.Status, "CANCEL_REQUESTED", "cancel", ""); err != nil {
		return joboperation.Job{}, Lease{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return joboperation.Job{}, Lease{}, false, err
	}
	return current, Lease{ProviderAttempt: attempt}, true, nil
}

func (repository *Repository) MarkPollManual(ctx context.Context, lease Lease, category string) error {
	if !validCategory(category) {
		return joboperation.ErrInvalid
	}
	tx, err := repository.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	current, found, err := loadByID(ctx, tx, lease.JobID, true)
	if err != nil {
		return err
	}
	if !found {
		return joboperation.ErrNotFound
	}
	if current.Status.Terminal() {
		return tx.Commit(ctx)
	}
	if lease.LeaseToken != "" {
		var valid bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM async_job_provider_attempts WHERE job_id=$1 AND attempt_no=$2 AND lease_owner=$3 AND lease_token=$4)`, lease.JobID, lease.AttemptNo, lease.LeaseOwner, lease.LeaseToken).Scan(&valid); err != nil {
			return err
		}
		if !valid {
			return joboperation.ErrLeaseLost
		}
	}
	if err := updateJobStatus(ctx, tx, current, joboperation.Reconciling, joboperation.Observation{Status: joboperation.Reconciling, FailureCategory: category}, "worker", "MANUAL_REVIEW"); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE async_job_provider_attempts SET state='RECONCILING',lease_owner=NULL,lease_token=NULL,lease_until=NULL,last_error_category=$3,next_poll_at='infinity',updated_at=now() WHERE job_id=$1 AND attempt_no=$2`, lease.JobID, lease.AttemptNo, category)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (repository *Repository) ClaimSettlements(ctx context.Context, owner string, at time.Time, lease time.Duration, limit int) ([]SettlementLease, error) {
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
	rows, err := tx.Query(ctx, `WITH candidates AS (SELECT id FROM async_jobs WHERE settlement_state='PENDING' AND ((settlement_lease_until IS NULL AND settlement_next_attempt_at<=$1) OR settlement_lease_until<=$1) ORDER BY settlement_next_attempt_at,id FOR UPDATE SKIP LOCKED LIMIT $2) UPDATE async_jobs job SET settlement_lease_owner=$3,settlement_lease_token=$4,settlement_lease_until=$5,settlement_attempt_count=settlement_attempt_count+1,updated_at=$1 FROM candidates WHERE job.id=candidates.id RETURNING job.id,job.settlement_attempt_count`, at.UTC(), limit, owner, token, at.UTC().Add(lease))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type claimed struct {
		id      string
		attempt int
	}
	var claimedRows []claimed
	for rows.Next() {
		var item claimed
		if err := rows.Scan(&item.id, &item.attempt); err != nil {
			return nil, err
		}
		claimedRows = append(claimedRows, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	result := make([]SettlementLease, 0, len(claimedRows))
	for _, claimed := range claimedRows {
		item, err := repository.getUnowned(ctx, claimed.id)
		if err != nil {
			return nil, err
		}
		result = append(result, SettlementLease{Job: item, Owner: owner, Token: token, Until: at.UTC().Add(lease), Attempt: claimed.attempt})
	}
	return result, nil
}

func (repository *Repository) MarkSettled(ctx context.Context, lease SettlementLease) error {
	return repository.finishSettlement(ctx, lease, "SETTLED", "")
}
func (repository *Repository) MarkSettlementManual(ctx context.Context, lease SettlementLease, category string) error {
	if !validCategory(category) {
		return joboperation.ErrInvalid
	}
	return repository.finishSettlement(ctx, lease, "MANUAL_REVIEW", category)
}
func (repository *Repository) finishSettlement(ctx context.Context, lease SettlementLease, state, category string) error {
	tx, err := repository.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	current, found, err := loadByID(ctx, tx, lease.Job.ID, true)
	if err != nil {
		return err
	}
	if !found {
		return joboperation.ErrNotFound
	}
	if current.SettlementState == state {
		return tx.Commit(ctx)
	}
	if current.SettlementState != "PENDING" {
		return joboperation.ErrConflict
	}
	result, err := tx.Exec(ctx, `UPDATE async_jobs SET settlement_state=$5,settlement_lease_owner=NULL,settlement_lease_token=NULL,settlement_lease_until=NULL,settlement_last_error_category=$6,version=version+1,updated_at=now() WHERE id=$1 AND settlement_state='PENDING' AND settlement_lease_owner=$2 AND settlement_lease_token=$3 AND settlement_lease_until>now() AND version=$4`, current.ID, lease.Owner, lease.Token, current.Version, state, nullable(category))
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return joboperation.ErrLeaseLost
	}
	_, err = tx.Exec(ctx, `INSERT INTO async_job_events(job_id,version,event_type,from_status,to_status,source,category) VALUES($1,$2,$3,$4,$4,'worker',$5)`, current.ID, current.Version+1, map[bool]string{true: "SETTLED", false: "MANUAL_REVIEW"}[state == "SETTLED"], current.Status, nullable(category))
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (repository *Repository) RescheduleSettlement(ctx context.Context, lease SettlementLease, next time.Time, category string) error {
	if next.IsZero() || !validCategory(category) {
		return joboperation.ErrInvalid
	}
	result, err := repository.pool.Exec(ctx, `UPDATE async_jobs SET settlement_lease_owner=NULL,settlement_lease_token=NULL,settlement_lease_until=NULL,settlement_next_attempt_at=$4,settlement_last_error_category=$5,updated_at=now() WHERE id=$1 AND settlement_state='PENDING' AND settlement_lease_owner=$2 AND settlement_lease_token=$3`, lease.Job.ID, lease.Owner, lease.Token, next.UTC(), category)
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
	return validOwner(request.Owner) && request.RequestID != "" && len(request.RequestID) <= 128 && request.Protocol != "" && request.Protocol == strings.ToLower(request.Protocol) && len(request.Protocol) <= 40 && request.Operation != "" && request.Operation == strings.ToLower(request.Operation) && len(request.Operation) <= 80 && strings.TrimSpace(request.Model) == request.Model && request.Model != "" && len(request.Model) <= 200 && request.Provider != "" && request.Provider == strings.ToLower(request.Provider) && len(request.Provider) <= 40 && request.ChannelID != "" && len(request.IdempotencyKey) <= 256 && (request.IdempotencyKey == "" || request.Fingerprint != ([32]byte{})) && (request.EstimatedUsage == nil || joboperation.ValidEstimatedUsage(*request.EstimatedUsage))
}
func validCategory(value string) bool {
	return joboperation.ValidFailureCategory(value)
}

func sameCreate(existing joboperation.Job, request CreateRequest) bool {
	return existing.Owner == request.Owner && existing.Protocol == request.Protocol && existing.Operation == request.Operation && existing.Model == request.Model && existing.Provider == request.Provider && existing.ChannelID == request.ChannelID && existing.ChargeID == request.ChargeID && existing.IdempotencyKey == request.IdempotencyKey && sameUsage(existing.EstimatedUsage, request.EstimatedUsage) && (request.IdempotencyKey == "" || existing.Fingerprint == request.Fingerprint)
}

const jobSelect = `SELECT id,request_id,organization_id,project_id,api_key_id,protocol,operation,model,provider,channel_id,charge_id,idempotency_key,request_fingerprint,status,settlement_state,version,failure_category,response_status,response_headers,response_body,response_body_sha256,created_at,updated_at,completed_at,usage_dimension,usage_unit,estimated_quantity,usage_extractor_version,usage_result_extractor_version,
    (SELECT dimension FROM async_job_usage_evidence evidence WHERE evidence.job_id=async_jobs.id),
    (SELECT unit FROM async_job_usage_evidence evidence WHERE evidence.job_id=async_jobs.id),
    (SELECT quantity FROM async_job_usage_evidence evidence WHERE evidence.job_id=async_jobs.id),
    (SELECT provenance FROM async_job_usage_evidence evidence WHERE evidence.job_id=async_jobs.id),
    (SELECT extractor_version FROM async_job_usage_evidence evidence WHERE evidence.job_id=async_jobs.id),
    (SELECT reconciliation_reason FROM async_job_usage_evidence evidence WHERE evidence.job_id=async_jobs.id)
    FROM async_jobs`

func scanJob(row pgx.Row) (joboperation.Job, bool, error) {
	var item joboperation.Job
	var charge, key, category, estimateDimension, estimateUnit, estimateExtractor, estimateResultExtractor, actualDimension, actualUnit, actualProvenance, actualExtractor, usageReason *string
	var estimatedQuantity, actualQuantity *int64
	var fingerprint, headers, body, digest []byte
	var status *int
	err := row.Scan(&item.ID, &item.RequestID, &item.Owner.OrganizationID, &item.Owner.ProjectID, &item.Owner.APIKeyID, &item.Protocol, &item.Operation, &item.Model, &item.Provider, &item.ChannelID, &charge, &key, &fingerprint, &item.Status, &item.SettlementState, &item.Version, &category, &status, &headers, &body, &digest, &item.CreatedAt, &item.UpdatedAt, &item.CompletedAt, &estimateDimension, &estimateUnit, &estimatedQuantity, &estimateExtractor, &estimateResultExtractor, &actualDimension, &actualUnit, &actualQuantity, &actualProvenance, &actualExtractor, &usageReason)
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
	if estimatedQuantity != nil {
		item.EstimatedUsage = &joboperation.Usage{Dimension: *estimateDimension, Unit: *estimateUnit, Quantity: *estimatedQuantity, Provenance: "request", ExtractorVersion: *estimateExtractor, ResultExtractorVersion: *estimateResultExtractor}
	}
	if actualQuantity != nil {
		item.ActualUsage = &joboperation.Usage{Dimension: *actualDimension, Unit: *actualUnit, Quantity: *actualQuantity, Provenance: *actualProvenance, ExtractorVersion: *actualExtractor}
	}
	if usageReason != nil {
		item.UsageReconciliationReason = *usageReason
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
	if observation.Usage != nil && observation.Usage.Provenance != source {
		return joboperation.ErrInvalid
	}
	var responseStatus, headers, body, digest, completed, category, settlement any
	var observationDigest [32]byte
	if observation.Snapshot.Status != 0 {
		canonical := observation.Snapshot
		if err := joboperation.ValidateSnapshot(canonical, 256*1024*1024); err != nil {
			return err
		}
		canonical.SHA256 = sha256.Sum256(canonical.Body)
		encoded, err := json.Marshal(canonical.Headers)
		if err != nil {
			return joboperation.ErrInvalid
		}
		responseStatus, headers, body, digest = canonical.Status, string(encoded), canonical.Body, canonical.SHA256[:]
		observationDigest = canonical.SHA256
	}
	if observation.FailureCategory != "" {
		category = observation.FailureCategory
	}
	if status.Terminal() {
		completed = time.Now().UTC()
		settlement = "PENDING"
		if current.EstimatedUsage != nil {
			reason := usageReason(current, observation)
			if reason != "" {
				settlement = "MANUAL_REVIEW"
			}
			if observation.Usage != nil {
				if observationDigest == ([32]byte{}) {
					observationDigest = sha256.Sum256(nil)
				}
				_, err := tx.Exec(ctx, `INSERT INTO async_job_usage_evidence(job_id,charge_id,provider,source,dimension,unit,quantity,provenance,extractor_version,observation_sha256,reconciliation_reason) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, current.ID, nullable(current.ChargeID), current.Provider, source, observation.Usage.Dimension, observation.Usage.Unit, observation.Usage.Quantity, observation.Usage.Provenance, observation.Usage.ExtractorVersion, observationDigest[:], nullable(reason))
				if err != nil {
					return err
				}
			}
		}
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

func usageReason(current joboperation.Job, observation joboperation.Observation) string {
	if current.EstimatedUsage == nil || !observation.Status.Terminal() {
		return ""
	}
	if observation.Status == joboperation.Succeeded {
		if observation.Usage == nil || observation.Usage.Quantity == 0 {
			return "usage_unknown"
		}
		if observation.Usage.Dimension != current.EstimatedUsage.Dimension || observation.Usage.Unit != current.EstimatedUsage.Unit {
			return "usage_identity_mismatch"
		}
		if observation.Usage.ExtractorVersion != current.EstimatedUsage.ResultExtractorVersion {
			return "usage_identity_mismatch"
		}
		if observation.Usage.Quantity > current.EstimatedUsage.Quantity {
			return "usage_exceeds_estimate"
		}
		return ""
	}
	if observation.Usage != nil && observation.Usage.Quantity > 0 {
		return "partial_terminal_conflict"
	}
	return ""
}

func cloneUsage(value *joboperation.Usage) *joboperation.Usage {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func sameUsage(left, right *joboperation.Usage) bool {
	return (left == nil && right == nil) || (left != nil && right != nil && *left == *right)
}

func sameActualUsage(current joboperation.Job, observation joboperation.Observation) bool {
	if current.EstimatedUsage == nil {
		return true
	}
	if current.ActualUsage == nil || observation.Usage == nil {
		return current.ActualUsage == nil && observation.Usage == nil
	}
	return current.ActualUsage.Dimension == observation.Usage.Dimension && current.ActualUsage.Unit == observation.Usage.Unit && current.ActualUsage.Quantity == observation.Usage.Quantity && current.ActualUsage.ExtractorVersion == observation.Usage.ExtractorVersion
}

func hydrateActualUsage(ctx context.Context, tx pgx.Tx, current *joboperation.Job) error {
	var usage joboperation.Usage
	var reason *string
	err := tx.QueryRow(ctx, `SELECT dimension,unit,quantity,provenance,extractor_version,reconciliation_reason FROM async_job_usage_evidence WHERE job_id=$1`, current.ID).Scan(&usage.Dimension, &usage.Unit, &usage.Quantity, &usage.Provenance, &usage.ExtractorVersion, &reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	current.ActualUsage = &usage
	if reason != nil {
		current.UsageReconciliationReason = *reason
	}
	return nil
}
func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}
