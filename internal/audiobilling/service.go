// Package audiobilling provides exactly-once Speech and transcription settlement.
package audiobilling

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nativegatewayhq/gateway/internal/audiopricing"
	"github.com/nativegatewayhq/gateway/internal/costquota"
	"github.com/nativegatewayhq/gateway/internal/idempotency"
	"github.com/nativegatewayhq/gateway/internal/ledger"
	"github.com/nativegatewayhq/gateway/internal/spendcap"
)

var (
	ErrInvalid  = errors.New("invalid audio speech charge")
	ErrConflict = errors.New("audio speech charge conflict")
	ErrPending  = errors.New("audio speech charge pending")
	ErrState    = errors.New("invalid audio speech charge state")
)

type BeginRequest struct {
	RequestID, OrganizationID, ProjectID, APIKeyID, Model, ChannelID, IdempotencyKey string
	Fingerprint                                                                      [32]byte
	Quantity                                                                         int64
}
type Charge struct {
	ID, RequestID, OrganizationID, ProjectID, APIKeyID, Model, ChannelID, PriceID, Currency, ReservationID, State, IdempotencyKey string
	Fingerprint                                                                                                                   [32]byte
	Quantity, EstimatedCost, ReservedSale, CapturedSale                                                                           int64
	ActualCost                                                                                                                    *int64
	ResponseStatus                                                                                                                int
	ResponseHeaders                                                                                                               map[string][]string
	ResponseBytes                                                                                                                 int64
	ResponseSHA256                                                                                                                [32]byte
}
type StreamEvidence struct {
	Status  int
	Headers map[string][]string
	Bytes   int64
	SHA256  [32]byte
}
type Estimator interface {
	EstimateInTx(context.Context, pgx.Tx, audiopricing.Request) (audiopricing.Estimate, error)
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
	pool      *pgxpool.Pool
	estimator Estimator
	wallet    Wallet
	quota     Quota
	spend     SpendCap
	entropy   io.Reader
}

func NewWithControls(pool *pgxpool.Pool, e Estimator, w Wallet, q Quota, s SpendCap) (*Service, error) {
	if pool == nil || e == nil || w == nil {
		return nil, ErrInvalid
	}
	return &Service{pool: pool, estimator: e, wallet: w, quota: q, spend: s, entropy: rand.Reader}, nil
}

func (s *Service) Begin(ctx context.Context, r BeginRequest) (Charge, error) {
	if !validBegin(r) {
		return Charge{}, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Charge{}, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, r.OrganizationID+":audio.speech:"+r.IdempotencyKey); err != nil {
		return Charge{}, err
	}
	if c, found, e := load(ctx, tx, r.OrganizationID, r.IdempotencyKey, false); e != nil {
		return Charge{}, e
	} else if found {
		if !sameRequest(c, r) {
			return Charge{}, ErrConflict
		}
		if c.State == "RESERVED" || c.State == "RECONCILING" {
			return Charge{}, ErrPending
		}
		return Charge{}, ErrConflict
	}
	est, err := s.estimator.EstimateInTx(ctx, tx, audiopricing.Request{ChannelID: r.ChannelID, Protocol: "openai", Operation: "audio.speech", Model: r.Model, Quantity: r.Quantity})
	if err != nil {
		return Charge{}, err
	}
	id, err := s.id()
	if err != nil {
		return Charge{}, err
	}
	reservation, err := s.wallet.ReserveInTx(ctx, tx, r.OrganizationID, r.ProjectID, "audio.speech:"+r.IdempotencyKey, est.Sale, "audio.speech:reserve:"+r.IdempotencyKey)
	if err != nil {
		return Charge{}, err
	}
	c := Charge{ID: id, RequestID: r.RequestID, OrganizationID: r.OrganizationID, ProjectID: r.ProjectID, APIKeyID: r.APIKeyID, Model: r.Model, ChannelID: r.ChannelID, PriceID: est.Price.ID, Currency: est.Price.Currency, ReservationID: reservation.Reservation.ID, State: "RESERVED", IdempotencyKey: r.IdempotencyKey, Fingerprint: r.Fingerprint, Quantity: r.Quantity, EstimatedCost: est.Cost, ReservedSale: est.Sale}
	_, err = tx.Exec(ctx, `INSERT INTO audio_speech_charges(id,request_id,organization_id,project_id,api_key_id,protocol,operation,model,channel_id,price_id,quantity,unit,currency,estimated_cost,reserved_sale,reservation_id,state,idempotency_key,request_fingerprint) VALUES($1,$2,$3,$4,$5,'openai','audio.speech',$6,$7,$8,$9,'unicode_scalar',$10,$11,$12,$13,'RESERVED',$14,$15)`, c.ID, c.RequestID, c.OrganizationID, c.ProjectID, c.APIKeyID, c.Model, c.ChannelID, c.PriceID, c.Quantity, c.Currency, c.EstimatedCost, c.ReservedSale, c.ReservationID, c.IdempotencyKey, c.Fingerprint[:])
	if err != nil {
		return Charge{}, err
	}
	if s.quota != nil {
		if _, err = s.quota.ReserveInTx(ctx, tx, costquota.ReservationRequest{ChargeID: c.ID, OrganizationID: c.OrganizationID, ProjectID: c.ProjectID, APIKeyID: c.APIKeyID, Protocol: "openai", Operation: "audio.speech", Model: c.Model, Currency: c.Currency, Amount: c.ReservedSale}); err != nil {
			return Charge{}, err
		}
	}
	if s.spend != nil {
		if _, err = s.spend.ReserveInTx(ctx, tx, spendcap.Reservation{ChargeID: c.ID, ChannelID: c.ChannelID, Currency: c.Currency, EstimatedCost: c.EstimatedCost}); err != nil {
			return Charge{}, err
		}
	}
	if _, err = tx.Exec(ctx, `INSERT INTO audio_speech_charge_events(charge_id,event_type) VALUES($1,'RESERVED')`, c.ID); err != nil {
		return Charge{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Charge{}, err
	}
	return c, nil
}

func (s *Service) Complete(ctx context.Context, id string, e StreamEvidence) (Charge, error) {
	if !validID(id, "asc_") || e.Status < 200 || e.Status > 299 || e.Bytes < 0 || e.SHA256 == ([32]byte{}) {
		return Charge{}, ErrInvalid
	}
	headers, err := safeHeaders(e.Headers)
	if err != nil {
		return Charge{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Charge{}, err
	}
	defer tx.Rollback(ctx)
	c, found, err := loadID(ctx, tx, id, true)
	if err != nil || !found {
		if err == nil {
			err = ErrState
		}
		return Charge{}, err
	}
	if c.State == "CAPTURED" {
		if c.ResponseStatus == e.Status && c.ResponseBytes == e.Bytes && c.ResponseSHA256 == e.SHA256 {
			return c, nil
		}
		return Charge{}, ErrConflict
	}
	if c.State != "RESERVED" && c.State != "RECONCILING" {
		return Charge{}, ErrState
	}
	if _, err = s.wallet.CaptureInTx(ctx, tx, c.ReservationID, c.ReservedSale, "audio.speech:capture:"+id); err != nil {
		return Charge{}, err
	}
	if s.quota != nil {
		if err = s.quota.CaptureInTx(ctx, tx, id, c.ReservedSale); err != nil {
			return Charge{}, err
		}
	}
	if s.spend != nil {
		if err = s.spend.CaptureInTx(ctx, tx, id, c.EstimatedCost); err != nil {
			return Charge{}, err
		}
	}
	hb, _ := json.Marshal(headers)
	_, err = tx.Exec(ctx, `UPDATE audio_speech_charges SET state='CAPTURED',actual_cost=estimated_cost,captured_sale=reserved_sale,response_status=$2,response_headers=$3::text::jsonb,response_bytes=$4,response_sha256=$5,completed_at=now(),updated_at=now() WHERE id=$1`, id, e.Status, string(hb), e.Bytes, e.SHA256[:])
	if err != nil {
		return Charge{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO audio_speech_charge_events(charge_id,event_type) VALUES($1,'CAPTURED')`, id); err != nil {
		return Charge{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Charge{}, err
	}
	c.State = "CAPTURED"
	c.ActualCost = &c.EstimatedCost
	c.CapturedSale = c.ReservedSale
	c.ResponseStatus = e.Status
	c.ResponseHeaders = headers
	c.ResponseBytes = e.Bytes
	c.ResponseSHA256 = e.SHA256
	return c, nil
}

func (s *Service) Release(ctx context.Context, id, reason string) (Charge, error) {
	if !validID(id, "asc_") || !validText(reason, 200) {
		return Charge{}, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Charge{}, err
	}
	defer tx.Rollback(ctx)
	c, found, err := loadID(ctx, tx, id, true)
	if err != nil || !found {
		if err == nil {
			err = ErrState
		}
		return Charge{}, err
	}
	if c.State == "RELEASED" {
		return c, nil
	}
	if c.State != "RESERVED" && c.State != "RECONCILING" {
		return Charge{}, ErrState
	}
	if _, err = s.wallet.ReleaseInTx(ctx, tx, c.ReservationID, "audio.speech:release:"+id); err != nil {
		return Charge{}, err
	}
	if s.quota != nil {
		if err = s.quota.ReleaseInTx(ctx, tx, id); err != nil {
			return Charge{}, err
		}
	}
	if s.spend != nil {
		if err = s.spend.ReleaseInTx(ctx, tx, id); err != nil {
			return Charge{}, err
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE audio_speech_charges SET state='RELEASED',updated_at=now() WHERE id=$1`, id); err != nil {
		return Charge{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO audio_speech_charge_events(charge_id,event_type,reason) VALUES($1,'RELEASED',$2)`, id, reason); err != nil {
		return Charge{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Charge{}, err
	}
	c.State = "RELEASED"
	return c, nil
}
func (s *Service) MarkReconciling(ctx context.Context, id, reason string) error {
	if !validID(id, "asc_") || !validText(reason, 200) {
		return ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	result, err := tx.Exec(ctx, `UPDATE audio_speech_charges SET state='RECONCILING',updated_at=now() WHERE id=$1 AND state IN('RESERVED','RECONCILING')`, id)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return ErrState
	}
	if _, err = tx.Exec(ctx, `INSERT INTO audio_speech_reconciliations(charge_id,reason) VALUES($1,$2) ON CONFLICT(charge_id) DO NOTHING`, id, reason); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO audio_speech_charge_events(charge_id,event_type,reason) VALUES($1,'RECONCILING',$2)`, id, reason); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

const chargeSelect = `SELECT id,request_id,organization_id,project_id,api_key_id,model,channel_id,price_id,currency,reservation_id,state,idempotency_key,request_fingerprint,quantity,estimated_cost,reserved_sale,actual_cost,captured_sale,COALESCE(response_status,0),COALESCE(response_headers,'{}'),COALESCE(response_bytes,0),response_sha256 FROM audio_speech_charges`

func load(ctx context.Context, tx pgx.Tx, org, key string, lock bool) (Charge, bool, error) {
	q := chargeSelect + ` WHERE organization_id=$1 AND idempotency_key=$2`
	if lock {
		q += ` FOR UPDATE`
	}
	return scanCharge(tx.QueryRow(ctx, q, org, key))
}
func loadID(ctx context.Context, tx pgx.Tx, id string, lock bool) (Charge, bool, error) {
	q := chargeSelect + ` WHERE id=$1`
	if lock {
		q += ` FOR UPDATE`
	}
	return scanCharge(tx.QueryRow(ctx, q, id))
}
func scanCharge(row pgx.Row) (Charge, bool, error) {
	var c Charge
	var fp, digest []byte
	var headers []byte
	err := row.Scan(&c.ID, &c.RequestID, &c.OrganizationID, &c.ProjectID, &c.APIKeyID, &c.Model, &c.ChannelID, &c.PriceID, &c.Currency, &c.ReservationID, &c.State, &c.IdempotencyKey, &fp, &c.Quantity, &c.EstimatedCost, &c.ReservedSale, &c.ActualCost, &c.CapturedSale, &c.ResponseStatus, &headers, &c.ResponseBytes, &digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return Charge{}, false, nil
	}
	if err != nil {
		return Charge{}, false, err
	}
	copy(c.Fingerprint[:], fp)
	copy(c.ResponseSHA256[:], digest)
	_ = json.Unmarshal(headers, &c.ResponseHeaders)
	return c, true, nil
}
func validBegin(r BeginRequest) bool {
	return validPrefixed(r.OrganizationID, "org_") && validPrefixed(r.ProjectID, "project_") && validPrefixed(r.APIKeyID, "key_") && validID(r.ChannelID, "channel_") && validText(r.RequestID, 128) && validText(r.Model, 200) && idempotency.Valid(r.IdempotencyKey) && r.Fingerprint != ([32]byte{}) && r.Quantity >= 1 && r.Quantity <= 4096
}
func sameRequest(c Charge, r BeginRequest) bool {
	return c.OrganizationID == r.OrganizationID && c.ProjectID == r.ProjectID && c.APIKeyID == r.APIKeyID && c.Model == r.Model && c.ChannelID == r.ChannelID && c.Quantity == r.Quantity && c.IdempotencyKey == r.IdempotencyKey && bytes.Equal(c.Fingerprint[:], r.Fingerprint[:])
}
func safeHeaders(in map[string][]string) (map[string][]string, error) {
	out := map[string][]string{}
	for k, v := range in {
		canonical := strings.ToLower(k)
		if canonical != "content-type" && canonical != "content-length" && canonical != "content-disposition" {
			continue
		}
		for _, x := range v {
			if len(x) > 1024 || strings.ContainsAny(x, "\r\n") {
				return nil, ErrInvalid
			}
			out[canonical] = append(out[canonical], x)
		}
	}
	return out, nil
}
func validText(v string, n int) bool { return v != "" && len(v) <= n && v == strings.TrimSpace(v) }
func validID(v, p string) bool       { return strings.HasPrefix(v, p) && len(v) == len(p)+32 }
func validPrefixed(v, p string) bool {
	return strings.HasPrefix(v, p) && len(v) > len(p) && len(v) <= 200 && v == strings.TrimSpace(v)
}
func (s *Service) id() (string, error) {
	b := make([]byte, 16)
	if _, err := io.ReadFull(s.entropy, b); err != nil {
		return "", err
	}
	return "asc_" + hex.EncodeToString(b), nil
}
