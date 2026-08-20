// Package chatbilling implements transactional reservation and usage settlement for Chat requests.
package chatbilling

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nativegatewayhq/gateway/internal/billing"
	"github.com/nativegatewayhq/gateway/internal/chatpricing"
	"github.com/nativegatewayhq/gateway/internal/costquota"
	"github.com/nativegatewayhq/gateway/internal/idempotency"
	"github.com/nativegatewayhq/gateway/internal/ledger"
	"github.com/nativegatewayhq/gateway/internal/spendcap"
)

var (
	ErrInvalid  = errors.New("invalid chat charge request")
	ErrConflict = errors.New("chat charge conflict")
	ErrPending  = errors.New("chat charge pending")
	ErrState    = errors.New("invalid chat charge state")
	ErrSnapshot = errors.New("chat response snapshot invalid")
)

type BeginRequest struct {
	RequestID, OrganizationID, ProjectID, APIKeyID, Model, ChannelID, IdempotencyKey string
	Protocol, Operation                                                              string
	DeliveryMode                                                                     string
	Fingerprint                                                                      [32]byte
	MaximumInputTokens, MaximumOutputTokens                                          int64
}
type Charge struct {
	ID, RequestID, OrganizationID, ProjectID, APIKeyID, Model, ChannelID, PriceID, Currency, ReservationID, State, IdempotencyKey string
	Fingerprint                                                                                                                   [32]byte
	MaximumInputTokens, MaximumOutputTokens, EstimatedCost, ReservedSale, CapturedSale                                            int64
	ActualCost                                                                                                                    *int64
	Rates                                                                                                                         chatpricing.Rates
	SnapshotVersion                                                                                                               int16
	Response                                                                                                                      billing.ResponseSnapshot
	Replay                                                                                                                        bool
	DeliveryMode                                                                                                                  string
	StreamCompleted                                                                                                               bool
	Protocol, Operation                                                                                                           string
}
type Estimator interface {
	EstimateInTx(context.Context, pgx.Tx, chatpricing.Request) (chatpricing.Estimate, error)
}
type Wallet interface {
	ReserveInTx(context.Context, pgx.Tx, string, string, string, int64, string) (ledger.Result, error)
	CaptureInTx(context.Context, pgx.Tx, string, int64, string) (ledger.Result, error)
	ReleaseInTx(context.Context, pgx.Tx, string, string) (ledger.Result, error)
}
type Quota interface {
	ReserveInTx(context.Context, pgx.Tx, costquota.ReservationRequest) ([]costquota.Allocation, error)
	CaptureInTx(context.Context, pgx.Tx, string, int64) error
	ReleaseInTx(context.Context, pgx.Tx, string) error
}
type SpendCap interface {
	ReserveInTx(context.Context, pgx.Tx, spendcap.Reservation) ([]spendcap.Allocation, error)
	CaptureInTx(context.Context, pgx.Tx, string, int64) error
	ReleaseInTx(context.Context, pgx.Tx, string) error
}
type Service struct {
	pool                 *pgxpool.Pool
	estimator            Estimator
	wallet               Wallet
	quota                Quota
	spendCap             SpendCap
	entropy              io.Reader
	maximumResponseBytes int64
}

func New(pool *pgxpool.Pool, estimator Estimator, wallet Wallet, maximumResponseBytes int64) (*Service, error) {
	return NewWithControls(pool, estimator, wallet, nil, nil, maximumResponseBytes)
}
func NewWithControls(pool *pgxpool.Pool, estimator Estimator, wallet Wallet, quota Quota, spendCap SpendCap, maximumResponseBytes int64) (*Service, error) {
	if pool == nil || estimator == nil || wallet == nil || maximumResponseBytes < 1 {
		return nil, ErrInvalid
	}
	return &Service{pool: pool, estimator: estimator, wallet: wallet, quota: quota, spendCap: spendCap, entropy: rand.Reader, maximumResponseBytes: maximumResponseBytes}, nil
}
func (s *Service) Begin(ctx context.Context, r BeginRequest) (Charge, error) {
	if r.Operation == "" {
		r.Operation = "chat.completions"
	}
	if r.Protocol == "" {
		r.Protocol = "openai"
	}
	if r.DeliveryMode == "" {
		r.DeliveryMode = "non_stream"
	}
	if !validBegin(r) {
		return Charge{}, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Charge{}, err
	}
	defer tx.Rollback(ctx)
	identity := r.RequestID
	if r.IdempotencyKey != "" {
		identity = r.IdempotencyKey
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, r.OrganizationID+":"+r.Protocol+":"+r.Operation+":"+identity); err != nil {
		return Charge{}, err
	}
	existing, found, err := loadForBegin(ctx, tx, r)
	if err != nil {
		return Charge{}, err
	}
	if found {
		if !sameRequest(existing, r) {
			return Charge{}, ErrConflict
		}
		if existing.State == "RESERVED" || existing.State == "RECONCILING" {
			return Charge{}, ErrPending
		}
		if r.IdempotencyKey == "" || existing.SnapshotVersion != 1 {
			return Charge{}, ErrConflict
		}
		existing.Replay = true
		return existing, nil
	}
	estimate, err := s.estimator.EstimateInTx(ctx, tx, chatpricing.Request{ChannelID: r.ChannelID, Protocol: r.Protocol, Operation: r.Operation, Model: r.Model, MaximumInputTokens: r.MaximumInputTokens, MaximumOutputTokens: r.MaximumOutputTokens})
	if err != nil {
		return Charge{}, err
	}
	id, err := s.id("chc_")
	if err != nil {
		return Charge{}, err
	}
	operationIdentity := r.RequestID
	if r.IdempotencyKey != "" {
		sum := sha256.Sum256([]byte(r.IdempotencyKey))
		operationIdentity = "idem_" + hex.EncodeToString(sum[:])
	}
	operationScope := r.Operation
	if r.Protocol != "openai" {
		operationScope = r.Protocol + ":" + r.Operation
	}
	reservation, err := s.wallet.ReserveInTx(ctx, tx, r.OrganizationID, r.ProjectID, operationScope+":"+operationIdentity, estimate.MaximumSale, operationScope+":reserve:"+operationIdentity)
	if err != nil {
		return Charge{}, err
	}
	charge := Charge{ID: id, Protocol: r.Protocol, Operation: r.Operation, RequestID: r.RequestID, OrganizationID: r.OrganizationID, ProjectID: r.ProjectID, APIKeyID: r.APIKeyID, Model: r.Model, ChannelID: r.ChannelID, PriceID: estimate.Price.ID, Currency: estimate.Price.Currency, ReservationID: reservation.Reservation.ID, State: "RESERVED", IdempotencyKey: r.IdempotencyKey, Fingerprint: r.Fingerprint, MaximumInputTokens: r.MaximumInputTokens, MaximumOutputTokens: r.MaximumOutputTokens, EstimatedCost: estimate.EstimatedCost, ReservedSale: estimate.MaximumSale, Rates: estimate.Price.Rates, DeliveryMode: r.DeliveryMode}
	var key, fingerprint any
	if r.IdempotencyKey != "" {
		key = r.IdempotencyKey
		fingerprint = r.Fingerprint[:]
	}
	_, err = tx.Exec(ctx, `INSERT INTO chat_request_charges(id,request_id,organization_id,project_id,api_key_id,protocol,operation,model,channel_id,price_id,maximum_input_tokens,maximum_output_tokens,currency,estimated_cost,reserved_sale,reservation_id,state,idempotency_key,request_fingerprint,delivery_mode) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,'RESERVED',$17,$18,$19)`, charge.ID, charge.RequestID, charge.OrganizationID, charge.ProjectID, charge.APIKeyID, charge.Protocol, charge.Operation, charge.Model, charge.ChannelID, charge.PriceID, charge.MaximumInputTokens, charge.MaximumOutputTokens, charge.Currency, charge.EstimatedCost, charge.ReservedSale, charge.ReservationID, key, fingerprint, charge.DeliveryMode)
	if err != nil {
		return Charge{}, err
	}
	if s.quota != nil {
		_, err = s.quota.ReserveInTx(ctx, tx, costquota.ReservationRequest{ChargeID: charge.ID, OrganizationID: charge.OrganizationID, ProjectID: charge.ProjectID, APIKeyID: charge.APIKeyID, Protocol: charge.Protocol, Operation: charge.Operation, Model: charge.Model, Currency: charge.Currency, Amount: charge.ReservedSale})
		if err != nil {
			return Charge{}, err
		}
	}
	if s.spendCap != nil {
		_, err = s.spendCap.ReserveInTx(ctx, tx, spendcap.Reservation{ChargeID: charge.ID, ChannelID: charge.ChannelID, Currency: charge.Currency, EstimatedCost: charge.EstimatedCost})
		if err != nil {
			return Charge{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return Charge{}, err
	}
	return charge, nil
}
func (s *Service) Replay(ctx context.Context, r BeginRequest) (Charge, bool, error) {
	if r.Operation == "" {
		r.Operation = "chat.completions"
	}
	if r.Protocol == "" {
		r.Protocol = "openai"
	}
	if r.DeliveryMode == "" {
		r.DeliveryMode = "non_stream"
	}
	if !validBegin(r) || r.IdempotencyKey == "" {
		return Charge{}, false, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return Charge{}, false, err
	}
	defer tx.Rollback(ctx)
	charge, found, err := loadForBegin(ctx, tx, r)
	if err != nil || !found {
		return Charge{}, false, err
	}
	if !sameRequest(charge, r) {
		return Charge{}, false, ErrConflict
	}
	if charge.State == "RESERVED" || charge.State == "RECONCILING" {
		return Charge{}, false, ErrPending
	}
	if charge.DeliveryMode == "stream" {
		return Charge{}, false, ErrConflict
	}
	if charge.SnapshotVersion != 1 {
		return Charge{}, false, ErrSnapshot
	}
	charge.Replay = true
	return charge, true, nil
}

// CompleteStreamUsage settles a streaming charge without retaining its SSE transcript.
func (s *Service) CompleteStreamUsage(ctx context.Context, id string, usage chatpricing.Usage, terminalDigest [32]byte) (Charge, error) {
	if terminalDigest == ([32]byte{}) || !validUsage(usage) {
		return Charge{}, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Charge{}, err
	}
	defer tx.Rollback(ctx)
	charge, found, err := loadID(ctx, tx, id, true)
	if err != nil || !found {
		if err == nil {
			err = ErrState
		}
		return Charge{}, err
	}
	if !((charge.Protocol == "openai" && (charge.Operation == "chat.completions" || charge.Operation == "responses.create")) || (charge.Protocol == "gemini" && charge.Operation == "chat.completions")) || charge.DeliveryMode != "stream" || usage.PromptTokens > charge.MaximumInputTokens || usage.CompletionTokens > charge.MaximumOutputTokens {
		return Charge{}, ErrInvalid
	}
	amounts, err := chatpricing.Calculate(charge.Rates, usage)
	if err != nil || amounts.Sale > charge.ReservedSale || amounts.Cost > charge.EstimatedCost {
		return Charge{}, ErrInvalid
	}
	if charge.State == "CAPTURED" {
		if charge.StreamCompleted && charge.CapturedSale == amounts.Sale && charge.ActualCost != nil && *charge.ActualCost == amounts.Cost && sameStreamEvidence(ctx, tx, id, usage, terminalDigest) {
			return charge, nil
		}
		return Charge{}, ErrConflict
	}
	if charge.State != "RESERVED" && charge.State != "RECONCILING" {
		return Charge{}, ErrState
	}
	operationKey := "chat-stream-capture:" + id
	if charge.Operation == "responses.create" {
		operationKey = "responses.create:stream:capture:" + id
	} else if charge.Protocol == "gemini" {
		operationKey = "gemini:chat.completions:stream:capture:" + id
	}
	if _, err = s.wallet.CaptureInTx(ctx, tx, charge.ReservationID, amounts.Sale, operationKey); err != nil {
		return Charge{}, err
	}
	if s.quota != nil {
		if err = s.quota.CaptureInTx(ctx, tx, id, amounts.Sale); err != nil {
			return Charge{}, err
		}
	}
	if s.spendCap != nil {
		if err = s.spendCap.CaptureInTx(ctx, tx, id, amounts.Cost); err != nil {
			return Charge{}, err
		}
	}
	schemaVersion := "openai-chat-usage-v1"
	if charge.Operation == "responses.create" {
		schemaVersion = "openai-responses-stream-usage-v1"
	} else if charge.Protocol == "gemini" {
		schemaVersion = "gemini-stream-usage-v1"
	}
	_, err = tx.Exec(ctx, `INSERT INTO chat_usage_evidence(charge_id,prompt_tokens,cached_input_tokens,cache_write_tokens,completion_tokens,tool_use_prompt_tokens,thoughts_tokens,schema_version,body_sha256,delivery_mode,terminal_event_sha256) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'stream',$9) ON CONFLICT(charge_id) DO NOTHING`, id, usage.PromptTokens, usage.CachedInputTokens, usage.CacheWriteTokens, usage.CompletionTokens, usage.ToolUsePromptTokens, usage.ThoughtsTokens, schemaVersion, terminalDigest[:])
	if err != nil {
		return Charge{}, err
	}
	_, err = tx.Exec(ctx, `UPDATE chat_request_charges SET state='CAPTURED',actual_cost=$2,captured_sale=$3,stream_completed=true,updated_at=now() WHERE id=$1`, id, amounts.Cost, amounts.Sale)
	if err != nil {
		return Charge{}, err
	}
	charge.State, charge.ActualCost, charge.CapturedSale, charge.StreamCompleted = "CAPTURED", &amounts.Cost, amounts.Sale, true
	if err = tx.Commit(ctx); err != nil {
		return Charge{}, err
	}
	return charge, nil
}

func sameStreamEvidence(ctx context.Context, tx pgx.Tx, id string, usage chatpricing.Usage, digest [32]byte) bool {
	var prompt, cached, cacheWrite, completion, toolUse, thoughts int64
	var storedDigest []byte
	err := tx.QueryRow(ctx, `SELECT prompt_tokens,cached_input_tokens,cache_write_tokens,completion_tokens,tool_use_prompt_tokens,thoughts_tokens,terminal_event_sha256 FROM chat_usage_evidence WHERE charge_id=$1 AND delivery_mode='stream'`, id).Scan(&prompt, &cached, &cacheWrite, &completion, &toolUse, &thoughts, &storedDigest)
	return err == nil && prompt == usage.PromptTokens && cached == usage.CachedInputTokens && cacheWrite == usage.CacheWriteTokens && completion == usage.CompletionTokens && toolUse == usage.ToolUsePromptTokens && thoughts == usage.ThoughtsTokens && bytes.Equal(storedDigest, digest[:])
}
func (s *Service) CompleteUsage(ctx context.Context, id string, usage chatpricing.Usage, snapshot billing.ResponseSnapshot) (Charge, error) {
	if !validUsage(usage) {
		return Charge{}, ErrInvalid
	}
	canonical, headersJSON, digest, err := s.snapshot(snapshot)
	if err != nil {
		return Charge{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Charge{}, err
	}
	defer tx.Rollback(ctx)
	charge, found, err := loadID(ctx, tx, id, true)
	if err != nil || !found {
		if err == nil {
			err = ErrState
		}
		return Charge{}, err
	}
	if usage.PromptTokens > charge.MaximumInputTokens || usage.CompletionTokens > charge.MaximumOutputTokens {
		return Charge{}, ErrInvalid
	}
	if charge.DeliveryMode != "non_stream" {
		return Charge{}, ErrInvalid
	}
	amounts, err := chatpricing.Calculate(charge.Rates, usage)
	if err != nil || amounts.Sale > charge.ReservedSale || amounts.Cost > charge.EstimatedCost {
		return Charge{}, ErrInvalid
	}
	if charge.State == "CAPTURED" {
		if charge.CapturedSale == amounts.Sale && charge.ActualCost != nil && *charge.ActualCost == amounts.Cost && sameSnapshot(charge.Response, canonical) {
			return charge, nil
		}
		return Charge{}, ErrConflict
	}
	if charge.State != "RESERVED" && charge.State != "RECONCILING" {
		return Charge{}, ErrState
	}
	operationScope := charge.Operation
	if charge.Protocol != "openai" {
		operationScope = charge.Protocol + ":" + charge.Operation
	}
	if _, err = s.wallet.CaptureInTx(ctx, tx, charge.ReservationID, amounts.Sale, operationScope+":capture:"+id); err != nil {
		return Charge{}, err
	}
	if s.quota != nil {
		if err = s.quota.CaptureInTx(ctx, tx, id, amounts.Sale); err != nil {
			return Charge{}, err
		}
	}
	if s.spendCap != nil {
		if err = s.spendCap.CaptureInTx(ctx, tx, id, amounts.Cost); err != nil {
			return Charge{}, err
		}
	}
	schemaVersion := "openai-chat-usage-v1"
	if charge.Operation == "responses.create" {
		schemaVersion = "openai-responses-usage-v1"
	} else if charge.Protocol == "gemini" {
		schemaVersion = "gemini-usage-v1"
	} else if charge.Protocol == "anthropic" {
		schemaVersion = "anthropic-usage-v1"
	}
	_, err = tx.Exec(ctx, `INSERT INTO chat_usage_evidence(charge_id,prompt_tokens,cached_input_tokens,cache_write_tokens,completion_tokens,tool_use_prompt_tokens,thoughts_tokens,schema_version,body_sha256) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT(charge_id) DO NOTHING`, id, usage.PromptTokens, usage.CachedInputTokens, usage.CacheWriteTokens, usage.CompletionTokens, usage.ToolUsePromptTokens, usage.ThoughtsTokens, schemaVersion, digest[:])
	if err != nil {
		return Charge{}, err
	}
	charge.ActualCost = &amounts.Cost
	charge.CapturedSale = amounts.Sale
	charge.State = "CAPTURED"
	charge.SnapshotVersion = 1
	charge.Response = canonical
	_, err = tx.Exec(ctx, `UPDATE chat_request_charges SET state='CAPTURED',actual_cost=$2,captured_sale=$3,response_snapshot_version=1,response_status=$4,response_headers=$5::text::jsonb,response_body=$6,response_body_sha256=$7,response_completed_at=now(),updated_at=now() WHERE id=$1`, id, amounts.Cost, amounts.Sale, canonical.Status, string(headersJSON), canonical.Body, digest[:])
	if err != nil {
		return Charge{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Charge{}, err
	}
	return charge, nil
}
func (s *Service) Release(ctx context.Context, id string, snapshot billing.ResponseSnapshot) (Charge, error) {
	canonical, headersJSON, digest, err := s.snapshot(snapshot)
	if err != nil {
		return Charge{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Charge{}, err
	}
	defer tx.Rollback(ctx)
	charge, found, err := loadID(ctx, tx, id, true)
	if err != nil || !found {
		if err == nil {
			err = ErrState
		}
		return Charge{}, err
	}
	if charge.State == "RELEASED" {
		if sameSnapshot(charge.Response, canonical) {
			return charge, nil
		}
		return Charge{}, ErrConflict
	}
	if charge.State != "RESERVED" && charge.State != "RECONCILING" {
		return Charge{}, ErrState
	}
	operationScope := charge.Operation
	if charge.Protocol != "openai" {
		operationScope = charge.Protocol + ":" + charge.Operation
	}
	if _, err = s.wallet.ReleaseInTx(ctx, tx, charge.ReservationID, operationScope+":release:"+id); err != nil {
		return Charge{}, err
	}
	if s.quota != nil {
		if err = s.quota.ReleaseInTx(ctx, tx, id); err != nil {
			return Charge{}, err
		}
	}
	if s.spendCap != nil {
		if err = s.spendCap.ReleaseInTx(ctx, tx, id); err != nil {
			return Charge{}, err
		}
	}
	charge.State = "RELEASED"
	charge.SnapshotVersion = 1
	charge.Response = canonical
	_, err = tx.Exec(ctx, `UPDATE chat_request_charges SET state='RELEASED',response_snapshot_version=1,response_status=$2,response_headers=$3::text::jsonb,response_body=$4,response_body_sha256=$5,response_completed_at=now(),updated_at=now() WHERE id=$1`, id, canonical.Status, string(headersJSON), canonical.Body, digest[:])
	if err != nil {
		return Charge{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Charge{}, err
	}
	return charge, nil
}
func (s *Service) MarkReconciling(ctx context.Context, id, reason string, snapshot *billing.ResponseSnapshot) error {
	return s.markReconciling(ctx, id, reason, snapshot, nil)
}
func (s *Service) MarkReconcilingUsage(ctx context.Context, id, reason string, snapshot *billing.ResponseSnapshot, usage chatpricing.Usage) error {
	if !validUsage(usage) {
		return ErrInvalid
	}
	return s.markReconciling(ctx, id, reason, snapshot, &usage)
}
func (s *Service) MarkStreamReconcilingUsage(ctx context.Context, id string, usage chatpricing.Usage, terminalDigest [32]byte) error {
	if terminalDigest == ([32]byte{}) || !validUsage(usage) {
		return ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	result, err := tx.Exec(ctx, `UPDATE chat_request_charges SET state='RECONCILING',updated_at=now() WHERE id=$1 AND delivery_mode='stream' AND state IN ('RESERVED','RECONCILING')`, id)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return ErrState
	}
	_, err = tx.Exec(ctx, `INSERT INTO chat_charge_reconciliations(charge_id,reason,prompt_tokens,cached_input_tokens,cache_write_tokens,completion_tokens,tool_use_prompt_tokens,thoughts_tokens,terminal_category,terminal_event_sha256) VALUES($1,'settlement_failed',$2,$3,$4,$5,$6,$7,'complete',$8) ON CONFLICT(charge_id) DO NOTHING`, id, usage.PromptTokens, usage.CachedInputTokens, usage.CacheWriteTokens, usage.CompletionTokens, usage.ToolUsePromptTokens, usage.ThoughtsTokens, terminalDigest[:])
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Service) MarkStreamReconciling(ctx context.Context, id, reason, disconnectSide, terminalCategory string) error {
	if !validID(id, "chc_") || !validReason(reason) || !validStreamMetadata(disconnectSide, terminalCategory) {
		return ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	result, err := tx.Exec(ctx, `UPDATE chat_request_charges SET state='RECONCILING',updated_at=now() WHERE id=$1 AND delivery_mode='stream' AND state IN ('RESERVED','RECONCILING')`, id)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return ErrState
	}
	_, err = tx.Exec(ctx, `INSERT INTO chat_charge_reconciliations(charge_id,reason,disconnect_side,terminal_category) VALUES($1,$2,NULLIF($3,''),$4) ON CONFLICT(charge_id) DO NOTHING`, id, reason, disconnectSide, terminalCategory)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Service) markReconciling(ctx context.Context, id, reason string, snapshot *billing.ResponseSnapshot, usage *chatpricing.Usage) error {
	if !validID(id, "chc_") || !validReason(reason) {
		return ErrInvalid
	}
	var status, headers, body, digest any
	if snapshot != nil {
		canonical, headersJSON, sum, err := s.snapshot(*snapshot)
		if err != nil {
			return err
		}
		status = canonical.Status
		headers = string(headersJSON)
		body = canonical.Body
		digest = sum[:]
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	result, err := tx.Exec(ctx, `UPDATE chat_request_charges SET state='RECONCILING',updated_at=now() WHERE id=$1 AND state IN ('RESERVED','RECONCILING')`, id)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return ErrState
	}
	var prompt, cached, cacheWrite, completion any
	if usage != nil {
		prompt, cached, cacheWrite, completion = usage.PromptTokens, usage.CachedInputTokens, usage.CacheWriteTokens, usage.CompletionTokens
	}
	var tool, thoughts any
	if usage != nil {
		tool, thoughts = usage.ToolUsePromptTokens, usage.ThoughtsTokens
	}
	_, err = tx.Exec(ctx, `INSERT INTO chat_charge_reconciliations(charge_id,reason,response_status,response_headers,response_body,response_body_sha256,prompt_tokens,cached_input_tokens,cache_write_tokens,completion_tokens,tool_use_prompt_tokens,thoughts_tokens) VALUES($1,$2,$3,$4::text::jsonb,$5,$6,$7,$8,$9,$10,$11,$12) ON CONFLICT(charge_id) DO NOTHING`, id, reason, status, headers, body, digest, prompt, cached, cacheWrite, completion, tool, thoughts)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Service) snapshot(v billing.ResponseSnapshot) (billing.ResponseSnapshot, []byte, [32]byte, error) {
	if v.Status < 100 || v.Status > 599 || int64(len(v.Body)) > s.maximumResponseBytes {
		return billing.ResponseSnapshot{}, nil, [32]byte{}, ErrSnapshot
	}
	c := billing.ResponseSnapshot{Status: v.Status, Headers: map[string][]string{}, Body: append([]byte(nil), v.Body...)}
	for k, values := range v.Headers {
		key := strings.ToLower(strings.TrimSpace(k))
		if key != "content-type" && key != "retry-after" && key != "request-id" {
			continue
		}
		c.Headers[key] = append([]string(nil), values...)
	}
	h, err := json.Marshal(c.Headers)
	if err != nil {
		return billing.ResponseSnapshot{}, nil, [32]byte{}, ErrSnapshot
	}
	return c, h, sha256.Sum256(c.Body), nil
}

const chargeSelect = `SELECT c.id,c.protocol,c.operation,c.request_id,c.organization_id,c.project_id,c.api_key_id,c.model,c.channel_id,c.price_id,c.currency,c.reservation_id,c.state,c.idempotency_key,c.request_fingerprint,c.maximum_input_tokens,c.maximum_output_tokens,c.estimated_cost,c.reserved_sale,c.actual_cost,c.captured_sale,c.response_snapshot_version,c.response_status,c.response_headers,c.response_body,c.delivery_mode,c.stream_completed,p.input_cost_per_million,p.input_sale_per_million,p.cached_input_cost_per_million,p.cached_input_sale_per_million,p.cache_write_cost_per_million,p.cache_write_sale_per_million,p.output_cost_per_million,p.output_sale_per_million FROM chat_request_charges c JOIN chat_token_prices p ON p.id=c.price_id`

func scan(row pgx.Row) (Charge, bool, error) {
	var c Charge
	var key *string
	var fp, headers, body []byte
	var status *int
	err := row.Scan(&c.ID, &c.Protocol, &c.Operation, &c.RequestID, &c.OrganizationID, &c.ProjectID, &c.APIKeyID, &c.Model, &c.ChannelID, &c.PriceID, &c.Currency, &c.ReservationID, &c.State, &key, &fp, &c.MaximumInputTokens, &c.MaximumOutputTokens, &c.EstimatedCost, &c.ReservedSale, &c.ActualCost, &c.CapturedSale, &c.SnapshotVersion, &status, &headers, &body, &c.DeliveryMode, &c.StreamCompleted, &c.Rates.InputCost, &c.Rates.InputSale, &c.Rates.CachedInputCost, &c.Rates.CachedInputSale, &c.Rates.CacheWriteCost, &c.Rates.CacheWriteSale, &c.Rates.OutputCost, &c.Rates.OutputSale)
	if errors.Is(err, pgx.ErrNoRows) {
		return Charge{}, false, nil
	}
	if err != nil {
		return Charge{}, false, err
	}
	if key != nil {
		c.IdempotencyKey = *key
	}
	copy(c.Fingerprint[:], fp)
	if c.SnapshotVersion == 1 && status != nil {
		c.Response.Status = *status
		c.Response.Body = body
		if json.Unmarshal(headers, &c.Response.Headers) != nil {
			return Charge{}, false, ErrSnapshot
		}
	}
	return c, true, nil
}
func loadForBegin(ctx context.Context, tx pgx.Tx, r BeginRequest) (Charge, bool, error) {
	if r.IdempotencyKey != "" {
		return scan(tx.QueryRow(ctx, chargeSelect+` WHERE c.organization_id=$1 AND c.protocol=$2 AND c.operation=$3 AND c.idempotency_key=$4`, r.OrganizationID, r.Protocol, r.Operation, r.IdempotencyKey))
	}
	return scan(tx.QueryRow(ctx, chargeSelect+` WHERE c.organization_id=$1 AND c.protocol=$2 AND c.operation=$3 AND c.request_id=$4`, r.OrganizationID, r.Protocol, r.Operation, r.RequestID))
}
func loadID(ctx context.Context, tx pgx.Tx, id string, lock bool) (Charge, bool, error) {
	q := chargeSelect + ` WHERE c.id=$1`
	if lock {
		q += ` FOR UPDATE OF c`
	}
	return scan(tx.QueryRow(ctx, q, id))
}
func validBegin(r BeginRequest) bool {
	has := r.Fingerprint != ([32]byte{})
	return ((r.Protocol == "openai" && (r.Operation == "chat.completions" || r.Operation == "responses.create")) || (r.Protocol == "gemini" && r.Operation == "chat.completions") || (r.Protocol == "anthropic" && r.Operation == "messages.create")) && validPrefixed(r.OrganizationID, "org_") && validPrefixed(r.ProjectID, "project_") && validPrefixed(r.APIKeyID, "key_") && r.RequestID != "" && len(r.RequestID) <= 128 && r.Model != "" && len(r.Model) <= 200 && strings.TrimSpace(r.Model) == r.Model && validID(r.ChannelID, "channel_") && r.MaximumInputTokens > 0 && r.MaximumOutputTokens > 0 && (r.DeliveryMode == "non_stream" || r.DeliveryMode == "stream") && ((r.IdempotencyKey == "" && !has) || (idempotency.Valid(r.IdempotencyKey) && has))
}
func sameRequest(c Charge, r BeginRequest) bool {
	identity := c.RequestID == r.RequestID
	if r.IdempotencyKey != "" {
		identity = c.IdempotencyKey == r.IdempotencyKey && bytes.Equal(c.Fingerprint[:], r.Fingerprint[:])
	}
	return identity && c.Protocol == r.Protocol && c.Operation == r.Operation && c.OrganizationID == r.OrganizationID && c.ProjectID == r.ProjectID && c.APIKeyID == r.APIKeyID && c.Model == r.Model && c.ChannelID == r.ChannelID && c.MaximumInputTokens == r.MaximumInputTokens && c.MaximumOutputTokens == r.MaximumOutputTokens && c.DeliveryMode == r.DeliveryMode
}
func sameSnapshot(a, b billing.ResponseSnapshot) bool {
	return a.Status == b.Status && bytes.Equal(a.Body, b.Body)
}
func validReason(v string) bool {
	switch v {
	case "executor_timeout", "executor_connection_lost", "response_unavailable", "usage_invalid", "settlement_failed", "provider_panic", "client_disconnect", "stream_protocol_invalid", "stream_usage_missing", "stream_write_failed":
		return true
	}
	return false
}
func validUsage(usage chatpricing.Usage) bool {
	return usage.PromptTokens >= 0 && usage.CachedInputTokens >= 0 && usage.CacheWriteTokens >= 0 && usage.CachedInputTokens <= usage.PromptTokens-usage.CacheWriteTokens && usage.CompletionTokens >= 0 && usage.ToolUsePromptTokens >= 0 && usage.ToolUsePromptTokens <= usage.PromptTokens && usage.ThoughtsTokens >= 0 && usage.ThoughtsTokens <= usage.CompletionTokens
}
func validStreamMetadata(side, category string) bool {
	if side != "" && side != "client" && side != "provider" {
		return false
	}
	switch category {
	case "complete", "missing_usage", "invalid_usage", "missing_done", "missing_terminal", "write_failed", "provider_error", "client_disconnect", "response_failed", "response_incomplete", "error_event":
		return true
	}
	return false
}
func validID(v, p string) bool {
	if !strings.HasPrefix(v, p) || len(v) != len(p)+32 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(v, p))
	return err == nil
}
func validPrefixed(v, p string) bool {
	return strings.HasPrefix(v, p) && len(v) > len(p) && len(v) <= 200 && strings.TrimSpace(v) == v
}
func (s *Service) id(p string) (string, error) {
	v := make([]byte, 16)
	if _, err := io.ReadFull(s.entropy, v); err != nil {
		return "", err
	}
	return p + hex.EncodeToString(v), nil
}
