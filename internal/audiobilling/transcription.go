package audiobilling

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nativegatewayhq/gateway/internal/audiopricing"
	"github.com/nativegatewayhq/gateway/internal/costquota"
	"github.com/nativegatewayhq/gateway/internal/idempotency"
	"github.com/nativegatewayhq/gateway/internal/spendcap"
)

type TranscriptionBeginRequest struct {
	RequestID, OrganizationID, ProjectID, APIKeyID, Model, ChannelID, IdempotencyKey string
	Fingerprint                                                                      [32]byte
}

type TranscriptionCharge struct {
	ID, RequestID, OrganizationID, ProjectID, APIKeyID, Model, ChannelID, PriceID, Strategy, Currency, ReservationID, State, IdempotencyKey string
	Fingerprint                                                                                                                             [32]byte
	MaximumInputTokens, MaximumOutputTokens, MaximumDurationMilliseconds                                                                    int64
	EstimatedCost, ReservedSale, CapturedSale                                                                                               int64
	ActualCost                                                                                                                              *int64
}

type TranscriptionEvidence struct {
	SchemaVersion string
	Usage         audiopricing.TranscriptionUsage
	Status        int
	Headers       map[string][]string
	SHA256        [32]byte
}

type TranscriptionEstimator interface {
	EstimateTranscriptionInTx(context.Context, pgx.Tx, audiopricing.TranscriptionPriceRequest) (audiopricing.TranscriptionEstimate, error)
	CalculateTranscriptionInTx(context.Context, pgx.Tx, string, audiopricing.TranscriptionUsage) (audiopricing.TranscriptionActual, error)
}

type TranscriptionService struct {
	pool      *pgxpool.Pool
	estimator TranscriptionEstimator
	wallet    Wallet
	quota     Quota
	spend     SpendCap
	entropy   io.Reader
}

func NewTranscriptionWithControls(pool *pgxpool.Pool, estimator TranscriptionEstimator, wallet Wallet, quota Quota, spend SpendCap) (*TranscriptionService, error) {
	if pool == nil || estimator == nil || wallet == nil {
		return nil, ErrInvalid
	}
	return &TranscriptionService{pool: pool, estimator: estimator, wallet: wallet, quota: quota, spend: spend, entropy: rand.Reader}, nil
}

func (s *TranscriptionService) Begin(ctx context.Context, r TranscriptionBeginRequest) (TranscriptionCharge, error) {
	if !validTranscriptionBegin(r) {
		return TranscriptionCharge{}, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return TranscriptionCharge{}, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, r.OrganizationID+":audio.transcription:"+r.IdempotencyKey); err != nil {
		return TranscriptionCharge{}, err
	}
	if existing, found, loadErr := loadTranscription(ctx, tx, r.OrganizationID, r.IdempotencyKey, false); loadErr != nil {
		return TranscriptionCharge{}, loadErr
	} else if found {
		if !sameTranscriptionRequest(existing, r) {
			return TranscriptionCharge{}, ErrConflict
		}
		if existing.State == "RESERVED" || existing.State == "RECONCILING" {
			return TranscriptionCharge{}, ErrPending
		}
		return TranscriptionCharge{}, ErrConflict
	}
	estimate, err := s.estimator.EstimateTranscriptionInTx(ctx, tx, audiopricing.TranscriptionPriceRequest{ChannelID: r.ChannelID, Protocol: "openai", Operation: "audio.transcription", Model: r.Model})
	if err != nil {
		return TranscriptionCharge{}, err
	}
	id, err := s.id()
	if err != nil {
		return TranscriptionCharge{}, err
	}
	reservation, err := s.wallet.ReserveInTx(ctx, tx, r.OrganizationID, r.ProjectID, "audio.transcription:"+r.IdempotencyKey, estimate.MaximumSale, "audio.transcription:reserve:"+r.IdempotencyKey)
	if err != nil {
		return TranscriptionCharge{}, err
	}
	p := estimate.Price
	c := TranscriptionCharge{ID: id, RequestID: r.RequestID, OrganizationID: r.OrganizationID, ProjectID: r.ProjectID, APIKeyID: r.APIKeyID, Model: r.Model, ChannelID: r.ChannelID, PriceID: p.ID, Strategy: p.Strategy, Currency: p.Currency, ReservationID: reservation.Reservation.ID, State: "RESERVED", IdempotencyKey: r.IdempotencyKey, Fingerprint: r.Fingerprint, MaximumInputTokens: p.MaximumInputTokens, MaximumOutputTokens: p.MaximumOutputTokens, MaximumDurationMilliseconds: p.MaximumDurationMilliseconds, EstimatedCost: estimate.MaximumCost, ReservedSale: estimate.MaximumSale}
	_, err = tx.Exec(ctx, `INSERT INTO audio_transcription_charges(id,request_id,organization_id,project_id,api_key_id,protocol,operation,model,channel_id,price_id,strategy,maximum_input_tokens,maximum_output_tokens,maximum_duration_milliseconds,currency,estimated_cost,reserved_sale,reservation_id,state,idempotency_key,request_fingerprint) VALUES($1,$2,$3,$4,$5,'openai','audio.transcription',$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,'RESERVED',$17,$18)`, c.ID, c.RequestID, c.OrganizationID, c.ProjectID, c.APIKeyID, c.Model, c.ChannelID, c.PriceID, c.Strategy, c.MaximumInputTokens, c.MaximumOutputTokens, c.MaximumDurationMilliseconds, c.Currency, c.EstimatedCost, c.ReservedSale, c.ReservationID, c.IdempotencyKey, c.Fingerprint[:])
	if err != nil {
		return TranscriptionCharge{}, err
	}
	if s.quota != nil {
		if _, err = s.quota.ReserveInTx(ctx, tx, costquota.ReservationRequest{ChargeID: c.ID, OrganizationID: c.OrganizationID, ProjectID: c.ProjectID, APIKeyID: c.APIKeyID, Protocol: "openai", Operation: "audio.transcription", Model: c.Model, Currency: c.Currency, Amount: c.ReservedSale}); err != nil {
			return TranscriptionCharge{}, err
		}
	}
	if s.spend != nil {
		if _, err = s.spend.ReserveInTx(ctx, tx, spendcap.Reservation{ChargeID: c.ID, ChannelID: c.ChannelID, Currency: c.Currency, EstimatedCost: c.EstimatedCost}); err != nil {
			return TranscriptionCharge{}, err
		}
	}
	if _, err = tx.Exec(ctx, `INSERT INTO audio_transcription_charge_events(charge_id,event_type) VALUES($1,'RESERVED')`, c.ID); err != nil {
		return TranscriptionCharge{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return TranscriptionCharge{}, err
	}
	return c, nil
}

func (s *TranscriptionService) Complete(ctx context.Context, id string, evidence TranscriptionEvidence) (TranscriptionCharge, error) {
	headers, err := validateTranscriptionEvidence(evidence)
	if !validID(id, "atc_") || err != nil {
		return TranscriptionCharge{}, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return TranscriptionCharge{}, err
	}
	defer tx.Rollback(ctx)
	c, found, err := loadTranscriptionID(ctx, tx, id, true)
	if err != nil || !found {
		if err == nil {
			err = ErrState
		}
		return TranscriptionCharge{}, err
	}
	if c.State == "CAPTURED" {
		stored, evidenceErr := loadTranscriptionEvidence(ctx, tx, id)
		if evidenceErr == nil && sameTranscriptionEvidence(stored, evidence) {
			return c, nil
		}
		return TranscriptionCharge{}, ErrConflict
	}
	if c.State != "RESERVED" && c.State != "RECONCILING" {
		return TranscriptionCharge{}, ErrState
	}
	actual, err := s.estimator.CalculateTranscriptionInTx(ctx, tx, c.PriceID, evidence.Usage)
	if err != nil || actual.Cost > c.EstimatedCost || actual.Sale > c.ReservedSale {
		return TranscriptionCharge{}, ErrInvalid
	}
	if _, err = s.wallet.CaptureInTx(ctx, tx, c.ReservationID, actual.Sale, "audio.transcription:capture:"+id); err != nil {
		return TranscriptionCharge{}, err
	}
	if s.quota != nil {
		if err = s.quota.CaptureInTx(ctx, tx, id, actual.Sale); err != nil {
			return TranscriptionCharge{}, err
		}
	}
	if s.spend != nil {
		if err = s.spend.CaptureInTx(ctx, tx, id, actual.Cost); err != nil {
			return TranscriptionCharge{}, err
		}
	}
	if err = insertTranscriptionEvidence(ctx, tx, id, evidence, headers); err != nil {
		return TranscriptionCharge{}, err
	}
	_, err = tx.Exec(ctx, `UPDATE audio_transcription_charges SET state='CAPTURED',actual_cost=$2,captured_sale=$3,completed_at=now(),updated_at=now() WHERE id=$1`, id, actual.Cost, actual.Sale)
	if err != nil {
		return TranscriptionCharge{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO audio_transcription_charge_events(charge_id,event_type) VALUES($1,'CAPTURED')`, id); err != nil {
		return TranscriptionCharge{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return TranscriptionCharge{}, err
	}
	c.State, c.CapturedSale, c.ActualCost = "CAPTURED", actual.Sale, &actual.Cost
	return c, nil
}

func (s *TranscriptionService) Release(ctx context.Context, id, reason string) (TranscriptionCharge, error) {
	if !validID(id, "atc_") || !validText(reason, 200) {
		return TranscriptionCharge{}, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return TranscriptionCharge{}, err
	}
	defer tx.Rollback(ctx)
	c, found, err := loadTranscriptionID(ctx, tx, id, true)
	if err != nil || !found {
		if err == nil {
			err = ErrState
		}
		return TranscriptionCharge{}, err
	}
	if c.State == "RELEASED" {
		return c, nil
	}
	if c.State != "RESERVED" && c.State != "RECONCILING" {
		return TranscriptionCharge{}, ErrState
	}
	if _, err = s.wallet.ReleaseInTx(ctx, tx, c.ReservationID, "audio.transcription:release:"+id); err != nil {
		return TranscriptionCharge{}, err
	}
	if s.quota != nil {
		if err = s.quota.ReleaseInTx(ctx, tx, id); err != nil {
			return TranscriptionCharge{}, err
		}
	}
	if s.spend != nil {
		if err = s.spend.ReleaseInTx(ctx, tx, id); err != nil {
			return TranscriptionCharge{}, err
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE audio_transcription_charges SET state='RELEASED',updated_at=now() WHERE id=$1`, id); err != nil {
		return TranscriptionCharge{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO audio_transcription_charge_events(charge_id,event_type,reason) VALUES($1,'RELEASED',$2)`, id, reason); err != nil {
		return TranscriptionCharge{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return TranscriptionCharge{}, err
	}
	c.State = "RELEASED"
	return c, nil
}

func (s *TranscriptionService) MarkReconciling(ctx context.Context, id, reason string, evidence *TranscriptionEvidence) error {
	if !validID(id, "atc_") || !validText(reason, 200) {
		return ErrInvalid
	}
	var headers map[string][]string
	var err error
	if evidence != nil {
		headers, err = validateTranscriptionEvidence(*evidence)
		if err != nil {
			return err
		}
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	result, err := tx.Exec(ctx, `UPDATE audio_transcription_charges SET state='RECONCILING',updated_at=now() WHERE id=$1 AND state IN('RESERVED','RECONCILING')`, id)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return ErrState
	}
	if evidence != nil {
		if err = insertTranscriptionEvidence(ctx, tx, id, *evidence, headers); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, `INSERT INTO audio_transcription_reconciliations(charge_id,reason) VALUES($1,$2) ON CONFLICT(charge_id) DO UPDATE SET reason=EXCLUDED.reason,updated_at=now()`, id, reason); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO audio_transcription_charge_events(charge_id,event_type,reason) VALUES($1,'RECONCILING',$2)`, id, reason); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func validateTranscriptionEvidence(e TranscriptionEvidence) (map[string][]string, error) {
	if e.Status < 200 || e.Status > 299 || e.SHA256 == ([32]byte{}) || !validText(e.SchemaVersion, 100) {
		return nil, ErrInvalid
	}
	validSchema := false
	for _, schema := range []string{"openai-transcription-token-json-v1", "openai-transcription-duration-json-v1", "openai-transcription-token-sse-v1", "openai-transcription-duration-sse-v1"} {
		if e.SchemaVersion == schema {
			validSchema = true
		}
	}
	if !validSchema || (e.Usage.Type == audiopricing.TranscriptionTokens) != (e.SchemaVersion == "openai-transcription-token-json-v1" || e.SchemaVersion == "openai-transcription-token-sse-v1") {
		return nil, ErrInvalid
	}
	return safeHeaders(e.Headers)
}

func insertTranscriptionEvidence(ctx context.Context, tx pgx.Tx, id string, e TranscriptionEvidence, headers map[string][]string) error {
	h, _ := json.Marshal(headers)
	result, err := tx.Exec(ctx, `INSERT INTO audio_transcription_usage_evidence(charge_id,schema_version,usage_type,input_tokens,audio_input_tokens,text_input_tokens,output_tokens,total_tokens,duration_milliseconds,response_status,response_headers,response_sha256) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::text::jsonb,$12) ON CONFLICT(charge_id) DO NOTHING`, id, e.SchemaVersion, e.Usage.Type, e.Usage.InputTokens, e.Usage.AudioInputTokens, e.Usage.TextInputTokens, e.Usage.OutputTokens, e.Usage.TotalTokens, e.Usage.DurationMilliseconds, e.Status, string(h), e.SHA256[:])
	if err != nil || result.RowsAffected() == 1 {
		return err
	}
	existing, err := loadTranscriptionEvidence(ctx, tx, id)
	if err != nil {
		return err
	}
	if !sameTranscriptionEvidence(existing, e) {
		return ErrConflict
	}
	return nil
}

func loadTranscriptionEvidence(ctx context.Context, tx pgx.Tx, id string) (TranscriptionEvidence, error) {
	var e TranscriptionEvidence
	var headers []byte
	var digest []byte
	err := tx.QueryRow(ctx, `SELECT schema_version,usage_type,input_tokens,audio_input_tokens,text_input_tokens,output_tokens,total_tokens,duration_milliseconds,response_status,response_headers,response_sha256 FROM audio_transcription_usage_evidence WHERE charge_id=$1`, id).Scan(&e.SchemaVersion, &e.Usage.Type, &e.Usage.InputTokens, &e.Usage.AudioInputTokens, &e.Usage.TextInputTokens, &e.Usage.OutputTokens, &e.Usage.TotalTokens, &e.Usage.DurationMilliseconds, &e.Status, &headers, &digest)
	if err != nil {
		return TranscriptionEvidence{}, err
	}
	_ = json.Unmarshal(headers, &e.Headers)
	copy(e.SHA256[:], digest)
	return e, nil
}

func sameTranscriptionEvidence(a, b TranscriptionEvidence) bool {
	ah, _ := safeHeaders(a.Headers)
	bh, _ := safeHeaders(b.Headers)
	aj, _ := json.Marshal(ah)
	bj, _ := json.Marshal(bh)
	return a.SchemaVersion == b.SchemaVersion && a.Usage == b.Usage && a.Status == b.Status && a.SHA256 == b.SHA256 && bytes.Equal(aj, bj)
}

const transcriptionChargeSelect = `SELECT id,request_id,organization_id,project_id,api_key_id,model,channel_id,price_id,strategy,currency,reservation_id,state,idempotency_key,request_fingerprint,maximum_input_tokens,maximum_output_tokens,maximum_duration_milliseconds,estimated_cost,reserved_sale,actual_cost,captured_sale FROM audio_transcription_charges`

func loadTranscription(ctx context.Context, tx pgx.Tx, org, key string, lock bool) (TranscriptionCharge, bool, error) {
	query := transcriptionChargeSelect + ` WHERE organization_id=$1 AND idempotency_key=$2`
	if lock {
		query += ` FOR UPDATE`
	}
	return scanTranscriptionCharge(tx.QueryRow(ctx, query, org, key))
}

func loadTranscriptionID(ctx context.Context, tx pgx.Tx, id string, lock bool) (TranscriptionCharge, bool, error) {
	query := transcriptionChargeSelect + ` WHERE id=$1`
	if lock {
		query += ` FOR UPDATE`
	}
	return scanTranscriptionCharge(tx.QueryRow(ctx, query, id))
}

func scanTranscriptionCharge(row pgx.Row) (TranscriptionCharge, bool, error) {
	var c TranscriptionCharge
	var fingerprint []byte
	err := row.Scan(&c.ID, &c.RequestID, &c.OrganizationID, &c.ProjectID, &c.APIKeyID, &c.Model, &c.ChannelID, &c.PriceID, &c.Strategy, &c.Currency, &c.ReservationID, &c.State, &c.IdempotencyKey, &fingerprint, &c.MaximumInputTokens, &c.MaximumOutputTokens, &c.MaximumDurationMilliseconds, &c.EstimatedCost, &c.ReservedSale, &c.ActualCost, &c.CapturedSale)
	if errors.Is(err, pgx.ErrNoRows) {
		return TranscriptionCharge{}, false, nil
	}
	if err != nil {
		return TranscriptionCharge{}, false, err
	}
	copy(c.Fingerprint[:], fingerprint)
	return c, true, nil
}

func validTranscriptionBegin(r TranscriptionBeginRequest) bool {
	return validPrefixed(r.OrganizationID, "org_") && validPrefixed(r.ProjectID, "project_") && validPrefixed(r.APIKeyID, "key_") && validID(r.ChannelID, "channel_") && validText(r.RequestID, 128) && validText(r.Model, 200) && idempotency.Valid(r.IdempotencyKey) && r.Fingerprint != ([32]byte{})
}

func sameTranscriptionRequest(c TranscriptionCharge, r TranscriptionBeginRequest) bool {
	return c.OrganizationID == r.OrganizationID && c.ProjectID == r.ProjectID && c.APIKeyID == r.APIKeyID && c.Model == r.Model && c.ChannelID == r.ChannelID && c.IdempotencyKey == r.IdempotencyKey && bytes.Equal(c.Fingerprint[:], r.Fingerprint[:])
}

func (s *TranscriptionService) id() (string, error) {
	value := make([]byte, 16)
	if _, err := io.ReadFull(s.entropy, value); err != nil {
		return "", err
	}
	return "atc_" + hex.EncodeToString(value), nil
}
