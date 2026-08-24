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

type TranslationBeginRequest struct {
	RequestID, OrganizationID, ProjectID, APIKeyID, Model, ChannelID, IdempotencyKey string
	Fingerprint                                                                      [32]byte
}

type TranslationCharge struct {
	ID, RequestID, OrganizationID, ProjectID, APIKeyID, Model, ChannelID, PriceID, Strategy, Currency, ReservationID, State, IdempotencyKey string
	Fingerprint                                                                                                                             [32]byte
	MaximumDurationMilliseconds                                                                                                             int64
	EstimatedCost, ReservedSale, CapturedSale                                                                                               int64
	ActualCost                                                                                                                              *int64
}

type TranslationEvidence struct {
	SchemaVersion        string
	DurationMilliseconds int64
	Status               int
	Headers              map[string][]string
	SHA256               [32]byte
}

type TranslationEstimator interface {
	EstimateTranslationInTx(context.Context, pgx.Tx, audiopricing.TranslationPriceRequest) (audiopricing.TranslationEstimate, error)
	CalculateTranslationInTx(context.Context, pgx.Tx, string, int64) (audiopricing.TranslationActual, error)
}

type TranslationService struct {
	pool      *pgxpool.Pool
	estimator TranslationEstimator
	wallet    Wallet
	quota     Quota
	spend     SpendCap
	entropy   io.Reader
}

func NewTranslationWithControls(pool *pgxpool.Pool, estimator TranslationEstimator, wallet Wallet, quota Quota, spend SpendCap) (*TranslationService, error) {
	if pool == nil || estimator == nil || wallet == nil {
		return nil, ErrInvalid
	}
	return &TranslationService{pool: pool, estimator: estimator, wallet: wallet, quota: quota, spend: spend, entropy: rand.Reader}, nil
}

func (service *TranslationService) Begin(ctx context.Context, request TranslationBeginRequest) (TranslationCharge, error) {
	if !validTranslationBegin(request) {
		return TranslationCharge{}, ErrInvalid
	}
	tx, err := service.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return TranslationCharge{}, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, request.OrganizationID+":audio.translation:"+request.IdempotencyKey); err != nil {
		return TranslationCharge{}, err
	}
	if existing, found, loadErr := loadTranslation(ctx, tx, request.OrganizationID, request.IdempotencyKey, false); loadErr != nil {
		return TranslationCharge{}, loadErr
	} else if found {
		if !sameTranslationRequest(existing, request) {
			return TranslationCharge{}, ErrConflict
		}
		if existing.State == "RESERVED" || existing.State == "RECONCILING" {
			return TranslationCharge{}, ErrPending
		}
		return TranslationCharge{}, ErrConflict
	}
	estimate, err := service.estimator.EstimateTranslationInTx(ctx, tx, audiopricing.TranslationPriceRequest{ChannelID: request.ChannelID, Model: request.Model})
	if err != nil {
		return TranslationCharge{}, err
	}
	id, err := service.id()
	if err != nil {
		return TranslationCharge{}, err
	}
	reservation, err := service.wallet.ReserveInTx(ctx, tx, request.OrganizationID, request.ProjectID, "audio.translation:"+request.IdempotencyKey, estimate.MaximumSale, "audio.translation:reserve:"+request.IdempotencyKey)
	if err != nil {
		return TranslationCharge{}, err
	}
	price := estimate.Price
	charge := TranslationCharge{ID: id, RequestID: request.RequestID, OrganizationID: request.OrganizationID, ProjectID: request.ProjectID, APIKeyID: request.APIKeyID, Model: request.Model, ChannelID: request.ChannelID, PriceID: price.ID, Strategy: price.Strategy, Currency: price.Currency, ReservationID: reservation.Reservation.ID, State: "RESERVED", IdempotencyKey: request.IdempotencyKey, Fingerprint: request.Fingerprint, MaximumDurationMilliseconds: price.MaximumDurationMilliseconds, EstimatedCost: estimate.MaximumCost, ReservedSale: estimate.MaximumSale}
	_, err = tx.Exec(ctx, `INSERT INTO audio_translation_charges(id,request_id,organization_id,project_id,api_key_id,protocol,operation,model,channel_id,price_id,strategy,maximum_duration_milliseconds,currency,estimated_cost,reserved_sale,reservation_id,state,idempotency_key,request_fingerprint) VALUES($1,$2,$3,$4,$5,'openai','audio.translation',$6,$7,$8,$9,$10,$11,$12,$13,$14,'RESERVED',$15,$16)`, charge.ID, charge.RequestID, charge.OrganizationID, charge.ProjectID, charge.APIKeyID, charge.Model, charge.ChannelID, charge.PriceID, charge.Strategy, charge.MaximumDurationMilliseconds, charge.Currency, charge.EstimatedCost, charge.ReservedSale, charge.ReservationID, charge.IdempotencyKey, charge.Fingerprint[:])
	if err != nil {
		return TranslationCharge{}, err
	}
	if service.quota != nil {
		if _, err = service.quota.ReserveInTx(ctx, tx, costquota.ReservationRequest{ChargeID: charge.ID, OrganizationID: charge.OrganizationID, ProjectID: charge.ProjectID, APIKeyID: charge.APIKeyID, Protocol: "openai", Operation: "audio.translation", Model: charge.Model, Currency: charge.Currency, Amount: charge.ReservedSale}); err != nil {
			return TranslationCharge{}, err
		}
	}
	if service.spend != nil {
		if _, err = service.spend.ReserveInTx(ctx, tx, spendcap.Reservation{ChargeID: charge.ID, ChannelID: charge.ChannelID, Currency: charge.Currency, EstimatedCost: charge.EstimatedCost}); err != nil {
			return TranslationCharge{}, err
		}
	}
	if _, err = tx.Exec(ctx, `INSERT INTO audio_translation_charge_events(charge_id,event_type) VALUES($1,'RESERVED')`, charge.ID); err != nil {
		return TranslationCharge{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return TranslationCharge{}, err
	}
	return charge, nil
}

func (service *TranslationService) Complete(ctx context.Context, id string, evidence TranslationEvidence) (TranslationCharge, error) {
	headers, err := validateTranslationEvidence(evidence)
	if !validID(id, "altc_") || err != nil {
		return TranslationCharge{}, ErrInvalid
	}
	tx, err := service.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return TranslationCharge{}, err
	}
	defer tx.Rollback(ctx)
	charge, found, err := loadTranslationID(ctx, tx, id, true)
	if err != nil || !found {
		if err == nil {
			err = ErrState
		}
		return TranslationCharge{}, err
	}
	if charge.State == "CAPTURED" {
		stored, evidenceErr := loadTranslationEvidence(ctx, tx, id)
		if evidenceErr == nil && sameTranslationEvidence(stored, evidence) {
			return charge, nil
		}
		return TranslationCharge{}, ErrConflict
	}
	if charge.State != "RESERVED" && charge.State != "RECONCILING" {
		return TranslationCharge{}, ErrState
	}
	actual, err := service.estimator.CalculateTranslationInTx(ctx, tx, charge.PriceID, evidence.DurationMilliseconds)
	if err != nil || actual.Cost > charge.EstimatedCost || actual.Sale > charge.ReservedSale {
		return TranslationCharge{}, ErrInvalid
	}
	if _, err = service.wallet.CaptureInTx(ctx, tx, charge.ReservationID, actual.Sale, "audio.translation:capture:"+id); err != nil {
		return TranslationCharge{}, err
	}
	if service.quota != nil {
		if err = service.quota.CaptureInTx(ctx, tx, id, actual.Sale); err != nil {
			return TranslationCharge{}, err
		}
	}
	if service.spend != nil {
		if err = service.spend.CaptureInTx(ctx, tx, id, actual.Cost); err != nil {
			return TranslationCharge{}, err
		}
	}
	if err = insertTranslationEvidence(ctx, tx, id, evidence, headers); err != nil {
		return TranslationCharge{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE audio_translation_charges SET state='CAPTURED',actual_cost=$2,captured_sale=$3,completed_at=now(),updated_at=now() WHERE id=$1`, id, actual.Cost, actual.Sale); err != nil {
		return TranslationCharge{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO audio_translation_charge_events(charge_id,event_type) VALUES($1,'CAPTURED')`, id); err != nil {
		return TranslationCharge{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return TranslationCharge{}, err
	}
	charge.State, charge.CapturedSale, charge.ActualCost = "CAPTURED", actual.Sale, &actual.Cost
	return charge, nil
}

func (service *TranslationService) Release(ctx context.Context, id, reason string) (TranslationCharge, error) {
	if !validID(id, "altc_") || !validText(reason, 200) {
		return TranslationCharge{}, ErrInvalid
	}
	tx, err := service.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return TranslationCharge{}, err
	}
	defer tx.Rollback(ctx)
	charge, found, err := loadTranslationID(ctx, tx, id, true)
	if err != nil || !found {
		if err == nil {
			err = ErrState
		}
		return TranslationCharge{}, err
	}
	if charge.State == "RELEASED" {
		return charge, nil
	}
	if charge.State != "RESERVED" && charge.State != "RECONCILING" {
		return TranslationCharge{}, ErrState
	}
	if _, err = service.wallet.ReleaseInTx(ctx, tx, charge.ReservationID, "audio.translation:release:"+id); err != nil {
		return TranslationCharge{}, err
	}
	if service.quota != nil {
		if err = service.quota.ReleaseInTx(ctx, tx, id); err != nil {
			return TranslationCharge{}, err
		}
	}
	if service.spend != nil {
		if err = service.spend.ReleaseInTx(ctx, tx, id); err != nil {
			return TranslationCharge{}, err
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE audio_translation_charges SET state='RELEASED',updated_at=now() WHERE id=$1`, id); err != nil {
		return TranslationCharge{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO audio_translation_charge_events(charge_id,event_type,reason) VALUES($1,'RELEASED',$2)`, id, reason); err != nil {
		return TranslationCharge{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return TranslationCharge{}, err
	}
	charge.State = "RELEASED"
	return charge, nil
}

func (service *TranslationService) MarkReconciling(ctx context.Context, id, reason string, evidence *TranslationEvidence) error {
	if !validID(id, "altc_") || !validText(reason, 200) {
		return ErrInvalid
	}
	var headers map[string][]string
	var err error
	if evidence != nil {
		headers, err = validateTranslationEvidence(*evidence)
		if err != nil {
			return err
		}
	}
	tx, err := service.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	result, err := tx.Exec(ctx, `UPDATE audio_translation_charges SET state='RECONCILING',updated_at=now() WHERE id=$1 AND state IN('RESERVED','RECONCILING')`, id)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return ErrState
	}
	if evidence != nil {
		if err = insertTranslationEvidence(ctx, tx, id, *evidence, headers); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, `INSERT INTO audio_translation_reconciliations(charge_id,reason) VALUES($1,$2) ON CONFLICT(charge_id) DO UPDATE SET reason=EXCLUDED.reason,updated_at=now()`, id, reason); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO audio_translation_charge_events(charge_id,event_type,reason) VALUES($1,'RECONCILING',$2)`, id, reason); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func validateTranslationEvidence(evidence TranslationEvidence) (map[string][]string, error) {
	if evidence.SchemaVersion != "openai-translation-duration-json-v1" || evidence.DurationMilliseconds < 1 || evidence.Status < 200 || evidence.Status > 299 || evidence.SHA256 == ([32]byte{}) {
		return nil, ErrInvalid
	}
	return safeHeaders(evidence.Headers)
}

func insertTranslationEvidence(ctx context.Context, tx pgx.Tx, id string, evidence TranslationEvidence, headers map[string][]string) error {
	encoded, _ := json.Marshal(headers)
	result, err := tx.Exec(ctx, `INSERT INTO audio_translation_duration_evidence(charge_id,schema_version,duration_milliseconds,response_status,response_headers,response_sha256) VALUES($1,$2,$3,$4,$5::text::jsonb,$6) ON CONFLICT(charge_id) DO NOTHING`, id, evidence.SchemaVersion, evidence.DurationMilliseconds, evidence.Status, string(encoded), evidence.SHA256[:])
	if err != nil || result.RowsAffected() == 1 {
		return err
	}
	existing, err := loadTranslationEvidence(ctx, tx, id)
	if err != nil {
		return err
	}
	if !sameTranslationEvidence(existing, evidence) {
		return ErrConflict
	}
	return nil
}

func loadTranslationEvidence(ctx context.Context, tx pgx.Tx, id string) (TranslationEvidence, error) {
	var evidence TranslationEvidence
	var headers, digest []byte
	err := tx.QueryRow(ctx, `SELECT schema_version,duration_milliseconds,response_status,response_headers,response_sha256 FROM audio_translation_duration_evidence WHERE charge_id=$1`, id).Scan(&evidence.SchemaVersion, &evidence.DurationMilliseconds, &evidence.Status, &headers, &digest)
	if err != nil {
		return TranslationEvidence{}, err
	}
	_ = json.Unmarshal(headers, &evidence.Headers)
	copy(evidence.SHA256[:], digest)
	return evidence, nil
}

func sameTranslationEvidence(a, b TranslationEvidence) bool {
	aHeaders, _ := safeHeaders(a.Headers)
	bHeaders, _ := safeHeaders(b.Headers)
	aJSON, _ := json.Marshal(aHeaders)
	bJSON, _ := json.Marshal(bHeaders)
	return a.SchemaVersion == b.SchemaVersion && a.DurationMilliseconds == b.DurationMilliseconds && a.Status == b.Status && a.SHA256 == b.SHA256 && bytes.Equal(aJSON, bJSON)
}

const translationChargeSelect = `SELECT id,request_id,organization_id,project_id,api_key_id,model,channel_id,price_id,strategy,currency,reservation_id,state,idempotency_key,request_fingerprint,maximum_duration_milliseconds,estimated_cost,reserved_sale,actual_cost,captured_sale FROM audio_translation_charges`

func loadTranslation(ctx context.Context, tx pgx.Tx, organization, key string, lock bool) (TranslationCharge, bool, error) {
	query := translationChargeSelect + ` WHERE organization_id=$1 AND idempotency_key=$2`
	if lock {
		query += ` FOR UPDATE`
	}
	return scanTranslationCharge(tx.QueryRow(ctx, query, organization, key))
}

func loadTranslationID(ctx context.Context, tx pgx.Tx, id string, lock bool) (TranslationCharge, bool, error) {
	query := translationChargeSelect + ` WHERE id=$1`
	if lock {
		query += ` FOR UPDATE`
	}
	return scanTranslationCharge(tx.QueryRow(ctx, query, id))
}

func scanTranslationCharge(row pgx.Row) (TranslationCharge, bool, error) {
	var charge TranslationCharge
	var fingerprint []byte
	err := row.Scan(&charge.ID, &charge.RequestID, &charge.OrganizationID, &charge.ProjectID, &charge.APIKeyID, &charge.Model, &charge.ChannelID, &charge.PriceID, &charge.Strategy, &charge.Currency, &charge.ReservationID, &charge.State, &charge.IdempotencyKey, &fingerprint, &charge.MaximumDurationMilliseconds, &charge.EstimatedCost, &charge.ReservedSale, &charge.ActualCost, &charge.CapturedSale)
	if errors.Is(err, pgx.ErrNoRows) {
		return TranslationCharge{}, false, nil
	}
	if err != nil {
		return TranslationCharge{}, false, err
	}
	copy(charge.Fingerprint[:], fingerprint)
	return charge, true, nil
}

func validTranslationBegin(request TranslationBeginRequest) bool {
	return validPrefixed(request.OrganizationID, "org_") && validPrefixed(request.ProjectID, "project_") && validPrefixed(request.APIKeyID, "key_") && validID(request.ChannelID, "channel_") && validText(request.RequestID, 128) && validText(request.Model, 200) && idempotency.Valid(request.IdempotencyKey) && request.Fingerprint != ([32]byte{})
}

func sameTranslationRequest(charge TranslationCharge, request TranslationBeginRequest) bool {
	return charge.OrganizationID == request.OrganizationID && charge.ProjectID == request.ProjectID && charge.APIKeyID == request.APIKeyID && charge.Model == request.Model && charge.ChannelID == request.ChannelID && charge.IdempotencyKey == request.IdempotencyKey && bytes.Equal(charge.Fingerprint[:], request.Fingerprint[:])
}

func (service *TranslationService) id() (string, error) {
	value := make([]byte, 16)
	if _, err := io.ReadFull(service.entropy, value); err != nil {
		return "", err
	}
	return "altc_" + hex.EncodeToString(value), nil
}
