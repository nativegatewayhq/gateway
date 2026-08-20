// Package billing coordinates image price estimates with Wallet settlement.
package billing

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nativegatewayhq/gateway/internal/ledger"
	"github.com/nativegatewayhq/gateway/internal/pricing"
)

var (
	ErrInvalidRequest  = errors.New("invalid billable request")
	ErrRequestConflict = errors.New("billable request conflict")
	ErrRequestPending  = errors.New("billable request already pending")
	ErrAlreadySettled  = errors.New("billable request already settled")
	ErrInvalidState    = errors.New("invalid charge state")
)

type BeginRequest struct {
	RequestID      string
	OrganizationID string
	ProjectID      string
	Protocol       string
	Operation      string
	Model          string
	ChannelID      string
	Quantity       int64
	Size           string
	Quality        string
}

type Charge struct {
	ID             string
	RequestID      string
	OrganizationID string
	ProjectID      string
	Protocol       string
	Operation      string
	Model          string
	ChannelID      string
	PriceID        string
	Quantity       int64
	Size           string
	Quality        string
	Currency       string
	EstimatedCost  int64
	ReservedSale   int64
	ActualCost     *int64
	CapturedSale   int64
	ReservationID  string
	State          string
}

type Estimator interface {
	EstimateInTx(context.Context, pgx.Tx, pricing.Request) (pricing.Estimate, error)
}

type Wallet interface {
	ReserveInTx(context.Context, pgx.Tx, string, string, string, int64, string) (ledger.Result, error)
	CaptureInTx(context.Context, pgx.Tx, string, int64, string) (ledger.Result, error)
	ReleaseInTx(context.Context, pgx.Tx, string, string) (ledger.Result, error)
}

type Service struct {
	pool      *pgxpool.Pool
	estimator Estimator
	wallet    Wallet
	entropy   io.Reader
}

func NewService(pool *pgxpool.Pool, estimator Estimator, wallet Wallet) (*Service, error) {
	if pool == nil || estimator == nil || wallet == nil {
		return nil, ErrInvalidRequest
	}
	return &Service{pool: pool, estimator: estimator, wallet: wallet, entropy: rand.Reader}, nil
}

func (service *Service) Begin(ctx context.Context, request BeginRequest) (Charge, error) {
	request.Size = defaultDimension(request.Size)
	request.Quality = defaultDimension(request.Quality)
	if !validBeginRequest(request) {
		return Charge{}, ErrInvalidRequest
	}
	tx, err := service.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Charge{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1 || ':' || $2, 0))`, request.OrganizationID, "image-charge:"+request.RequestID); err != nil {
		return Charge{}, err
	}
	if existing, found, err := loadByRequest(ctx, tx, request.OrganizationID, request.RequestID, false); err != nil {
		return Charge{}, err
	} else if found {
		if !sameRequest(existing, request) {
			return Charge{}, ErrRequestConflict
		}
		if existing.State == "RESERVED" || existing.State == "RECONCILING" || existing.State == "RESERVING" {
			return Charge{}, ErrRequestPending
		}
		return Charge{}, ErrAlreadySettled
	}
	estimate, err := service.estimator.EstimateInTx(ctx, tx, pricing.Request{Protocol: request.Protocol, Operation: request.Operation, Model: request.Model, ChannelID: request.ChannelID, Quantity: request.Quantity, Size: request.Size, Quality: request.Quality})
	if err != nil {
		return Charge{}, err
	}
	reservation, err := service.wallet.ReserveInTx(ctx, tx, request.OrganizationID, request.ProjectID, "image:"+request.RequestID, estimate.MaximumSale, "image-reserve:"+request.RequestID)
	if err != nil {
		return Charge{}, err
	}
	id, err := service.id("charge_")
	if err != nil {
		return Charge{}, err
	}
	charge := Charge{ID: id, RequestID: request.RequestID, OrganizationID: request.OrganizationID, ProjectID: request.ProjectID, Protocol: request.Protocol, Operation: request.Operation, Model: request.Model, ChannelID: estimate.ChannelID, PriceID: estimate.PriceID, Quantity: request.Quantity, Size: request.Size, Quality: request.Quality, Currency: estimate.Currency, EstimatedCost: estimate.EstimatedCost, ReservedSale: estimate.MaximumSale, ReservationID: reservation.Reservation.ID, State: "RESERVED"}
	_, err = tx.Exec(ctx, `INSERT INTO image_request_charges(id,request_id,organization_id,project_id,protocol,operation,model,channel_id,price_id,quantity,size,quality,currency,estimated_cost,reserved_sale,reservation_id,state)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`, charge.ID, charge.RequestID, charge.OrganizationID, charge.ProjectID, charge.Protocol, charge.Operation, charge.Model, charge.ChannelID, charge.PriceID, charge.Quantity, charge.Size, charge.Quality, charge.Currency, charge.EstimatedCost, charge.ReservedSale, charge.ReservationID, charge.State)
	if err != nil {
		return Charge{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Charge{}, err
	}
	return charge, nil
}

func (service *Service) Capture(ctx context.Context, chargeID string) (Charge, error) {
	return service.settle(ctx, chargeID, "CAPTURED")
}

func (service *Service) Release(ctx context.Context, chargeID string) (Charge, error) {
	return service.settle(ctx, chargeID, "RELEASED")
}

func (service *Service) settle(ctx context.Context, chargeID, target string) (Charge, error) {
	if !validID(chargeID, "charge_") || (target != "CAPTURED" && target != "RELEASED") {
		return Charge{}, ErrInvalidRequest
	}
	tx, err := service.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Charge{}, err
	}
	defer tx.Rollback(ctx)
	charge, found, err := loadByID(ctx, tx, chargeID, true)
	if err != nil {
		return Charge{}, err
	}
	if !found {
		return Charge{}, ErrInvalidState
	}
	if charge.State == target {
		return charge, nil
	}
	if charge.State != "RESERVED" && charge.State != "RECONCILING" {
		return Charge{}, ErrInvalidState
	}
	if target == "CAPTURED" {
		if _, err := service.wallet.CaptureInTx(ctx, tx, charge.ReservationID, charge.ReservedSale, "image-capture:"+charge.ID); err != nil {
			return Charge{}, err
		}
		actualCost := charge.EstimatedCost
		charge.ActualCost = &actualCost
		charge.CapturedSale = charge.ReservedSale
	} else {
		if _, err := service.wallet.ReleaseInTx(ctx, tx, charge.ReservationID, "image-release:"+charge.ID); err != nil {
			return Charge{}, err
		}
		charge.ActualCost = nil
		charge.CapturedSale = 0
	}
	charge.State = target
	_, err = tx.Exec(ctx, `UPDATE image_request_charges SET state=$2,actual_cost=$3,captured_sale=$4,updated_at=now() WHERE id=$1`, charge.ID, charge.State, charge.ActualCost, charge.CapturedSale)
	if err != nil {
		return Charge{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Charge{}, err
	}
	return charge, nil
}

func (service *Service) MarkReconciling(ctx context.Context, chargeID string) error {
	if !validID(chargeID, "charge_") {
		return ErrInvalidRequest
	}
	result, err := service.pool.Exec(ctx, `UPDATE image_request_charges SET state='RECONCILING',updated_at=now() WHERE id=$1 AND state IN ('RESERVED','RECONCILING')`, chargeID)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return ErrInvalidState
	}
	return nil
}

func loadByRequest(ctx context.Context, tx pgx.Tx, organizationID, requestID string, lock bool) (Charge, bool, error) {
	query := chargeSelect + ` WHERE organization_id=$1 AND request_id=$2`
	if lock {
		query += ` FOR UPDATE`
	}
	return scanCharge(tx.QueryRow(ctx, query, organizationID, requestID))
}

func loadByID(ctx context.Context, tx pgx.Tx, id string, lock bool) (Charge, bool, error) {
	query := chargeSelect + ` WHERE id=$1`
	if lock {
		query += ` FOR UPDATE`
	}
	return scanCharge(tx.QueryRow(ctx, query, id))
}

const chargeSelect = `SELECT id,request_id,organization_id,project_id,protocol,operation,model,channel_id,price_id,quantity,size,quality,currency,estimated_cost,reserved_sale,actual_cost,captured_sale,reservation_id,state FROM image_request_charges`

func scanCharge(row pgx.Row) (Charge, bool, error) {
	var charge Charge
	err := row.Scan(&charge.ID, &charge.RequestID, &charge.OrganizationID, &charge.ProjectID, &charge.Protocol, &charge.Operation, &charge.Model, &charge.ChannelID, &charge.PriceID, &charge.Quantity, &charge.Size, &charge.Quality, &charge.Currency, &charge.EstimatedCost, &charge.ReservedSale, &charge.ActualCost, &charge.CapturedSale, &charge.ReservationID, &charge.State)
	if errors.Is(err, pgx.ErrNoRows) {
		return Charge{}, false, nil
	}
	return charge, err == nil, err
}

func sameRequest(charge Charge, request BeginRequest) bool {
	return charge.OrganizationID == request.OrganizationID && charge.ProjectID == request.ProjectID && charge.RequestID == request.RequestID && charge.Protocol == request.Protocol && charge.Operation == request.Operation && charge.Model == request.Model && charge.ChannelID == request.ChannelID && charge.Quantity == request.Quantity && charge.Size == request.Size && charge.Quality == request.Quality
}

func validBeginRequest(request BeginRequest) bool {
	return validPrefixed(request.OrganizationID, "org_", 200) && validPrefixed(request.ProjectID, "project_", 200) && validText(request.RequestID, 128) && request.Protocol == "openai" && (request.Operation == "image.generate" || request.Operation == "image.edit") && validText(request.Model, 200) && validID(request.ChannelID, "channel_") && request.Quantity >= 1 && request.Quantity <= 10 && validText(request.Size, 80) && validText(request.Quality, 80)
}

func validPrefixed(value, prefix string, maximum int) bool {
	return strings.HasPrefix(value, prefix) && validText(value, maximum)
}

func validText(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && strings.TrimSpace(value) == value
}

func validID(value, prefix string) bool {
	if len(value) != len(prefix)+32 || !strings.HasPrefix(value, prefix) {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, prefix))
	return err == nil
}

func defaultDimension(value string) string {
	if value == "" {
		return pricing.DefaultDimension
	}
	return value
}

func (service *Service) id(prefix string) (string, error) {
	value := make([]byte, 16)
	if _, err := io.ReadFull(service.entropy, value); err != nil {
		return "", fmt.Errorf("generate billing id: %w", err)
	}
	return prefix + hex.EncodeToString(value), nil
}
