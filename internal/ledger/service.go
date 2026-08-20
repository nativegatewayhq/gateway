// Package ledger implements organization wallet and append-only ledger commands.
package ledger

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const Currency = "USD_TICKS"

var (
	ErrInvalidAmount       = errors.New("invalid amount")
	ErrInvalidIdentifier   = errors.New("invalid identifier")
	ErrInsufficientFunds   = errors.New("insufficient funds")
	ErrInvalidTransition   = errors.New("invalid wallet transition")
	ErrIdempotencyConflict = errors.New("idempotency conflict")
	ErrTenantUnavailable   = errors.New("tenant unavailable")
)

type Balance struct {
	Available int64
	Reserved  int64
	Currency  string
}
type Reservation struct {
	ID, OrganizationID, ProjectID, RequestID, State string
	Maximum, Captured, Refunded                     int64
}
type Result struct {
	Balance     Balance
	Reservation Reservation
}
type Service struct {
	pool    *pgxpool.Pool
	entropy io.Reader
}

func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool, entropy: rand.Reader} }
func newService(pool *pgxpool.Pool, entropy io.Reader) *Service {
	return &Service{pool: pool, entropy: entropy}
}

func (service *Service) Deposit(ctx context.Context, organizationID string, amount int64, operationKey string) (Balance, error) {
	if amount <= 0 {
		return Balance{}, ErrInvalidAmount
	}
	if !valid(organizationID, "org_") || !validKey(operationKey) {
		return Balance{}, ErrInvalidIdentifier
	}
	tx, err := service.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Balance{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockOperationKey(ctx, tx, organizationID, operationKey); err != nil {
		return Balance{}, err
	}
	if existing, found, err := loadOperation(ctx, tx, organizationID, operationKey, "deposit", amount, ""); err != nil {
		return Balance{}, err
	} else if found {
		return existing.Balance, nil
	}
	resultBalance := Balance{Currency: Currency}
	result, err := tx.Exec(ctx, `INSERT INTO organization_wallets(organization_id,currency,available)
		SELECT id,$2,$3 FROM organizations WHERE id=$1 AND status='active' ON CONFLICT(organization_id,currency) DO NOTHING`, organizationID, Currency, amount)
	if err != nil {
		return Balance{}, classifyAmount(err)
	}
	if result.RowsAffected() == 0 {
		if err := tx.QueryRow(ctx, `UPDATE organization_wallets w SET available=w.available+$2,version=version+1,updated_at=now()
			FROM organizations o WHERE w.organization_id=$1 AND w.currency=$3 AND o.id=w.organization_id AND o.status='active'
			RETURNING w.available,w.reserved`, organizationID, amount, Currency).Scan(&resultBalance.Available, &resultBalance.Reserved); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return Balance{}, ErrTenantUnavailable
			}
			return Balance{}, classifyAmount(err)
		}
	} else {
		resultBalance = Balance{Available: amount, Currency: Currency}
	}
	resultBalance.Currency = Currency
	ids, err := service.ids("wop_", "led_")
	if err != nil {
		return Balance{}, err
	}
	operationID, entryID := ids[0], ids[1]
	if err := insertOperation(ctx, tx, operationID, organizationID, operationKey, "deposit", amount, "", resultBalance, Reservation{}); err != nil {
		return Balance{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger_entries(id,operation_id,organization_id,entry_type,currency,delta_available,delta_reserved) VALUES($1,$2,$3,'deposit',$4,$5,0)`, entryID, operationID, organizationID, Currency, amount); err != nil {
		return Balance{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Balance{}, err
	}
	return resultBalance, nil
}

func (service *Service) Reserve(ctx context.Context, organizationID, projectID, requestID string, maximum int64, operationKey string) (Result, error) {
	if maximum <= 0 {
		return Result{}, ErrInvalidAmount
	}
	if !valid(organizationID, "org_") || !valid(projectID, "project_") || !validKey(requestID) || !validKey(operationKey) {
		return Result{}, ErrInvalidIdentifier
	}
	tx, err := service.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Result{}, err
	}
	defer tx.Rollback(ctx)
	result, err := service.ReserveInTx(ctx, tx, organizationID, projectID, requestID, maximum, operationKey)
	if err != nil {
		return Result{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Result{}, err
	}
	return result, nil
}

// ReserveInTx applies Reserve using a caller-owned transaction. The caller
// must commit or roll back and must not use this method concurrently on a Tx.
func (service *Service) ReserveInTx(ctx context.Context, tx pgx.Tx, organizationID, projectID, requestID string, maximum int64, operationKey string) (Result, error) {
	if maximum <= 0 {
		return Result{}, ErrInvalidAmount
	}
	if tx == nil || !valid(organizationID, "org_") || !valid(projectID, "project_") || !validKey(requestID) || !validKey(operationKey) {
		return Result{}, ErrInvalidIdentifier
	}
	if err := lockOperationKey(ctx, tx, organizationID, operationKey); err != nil {
		return Result{}, err
	}
	var available, reserved int64
	err := tx.QueryRow(ctx, `SELECT w.available,w.reserved FROM organization_wallets w JOIN organizations o ON o.id=w.organization_id JOIN projects p ON p.organization_id=o.id
		WHERE w.organization_id=$1 AND w.currency=$2 AND p.id=$3 AND o.status='active' AND p.status='active' FOR UPDATE OF w`, organizationID, Currency, projectID).Scan(&available, &reserved)
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{}, ErrTenantUnavailable
	}
	if err != nil {
		return Result{}, err
	}
	if existing, found, err := loadOperation(ctx, tx, organizationID, operationKey, "reserve", maximum, ""); err != nil {
		return Result{}, err
	} else if found {
		return existing, nil
	}
	if available < maximum {
		return Result{}, ErrInsufficientFunds
	}
	ids, err := service.ids("res_", "wop_", "led_")
	if err != nil {
		return Result{}, err
	}
	reservationID, operationID, entryID := ids[0], ids[1], ids[2]
	reservation := Reservation{ID: reservationID, OrganizationID: organizationID, ProjectID: projectID, RequestID: requestID, State: "pending", Maximum: maximum}
	balance := Balance{Available: available - maximum, Reserved: reserved + maximum, Currency: Currency}
	if _, err := tx.Exec(ctx, `INSERT INTO wallet_reservations(id,organization_id,project_id,request_id,currency,maximum) VALUES($1,$2,$3,$4,$5,$6)`, reservationID, organizationID, projectID, requestID, Currency, maximum); err != nil {
		return Result{}, classifyUnique(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE organization_wallets SET available=$2,reserved=$3,version=version+1,updated_at=now() WHERE organization_id=$1 AND currency=$4`, organizationID, balance.Available, balance.Reserved, Currency); err != nil {
		return Result{}, err
	}
	if err := insertOperation(ctx, tx, operationID, organizationID, operationKey, "reserve", maximum, reservationID, balance, reservation); err != nil {
		return Result{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger_entries(id,operation_id,organization_id,project_id,reservation_id,entry_type,currency,delta_available,delta_reserved) VALUES($1,$2,$3,$4,$5,'reserve',$6,$7,$8)`, entryID, operationID, organizationID, projectID, reservationID, Currency, -maximum, maximum); err != nil {
		return Result{}, err
	}
	return Result{balance, reservation}, nil
}

func (service *Service) Capture(ctx context.Context, reservationID string, actual int64, operationKey string) (Result, error) {
	return service.finish(ctx, reservationID, actual, operationKey, "capture")
}
func (service *Service) Release(ctx context.Context, reservationID, operationKey string) (Result, error) {
	return service.finish(ctx, reservationID, 0, operationKey, "release")
}

func (service *Service) finish(ctx context.Context, reservationID string, amount int64, operationKey, kind string) (Result, error) {
	if amount < 0 {
		return Result{}, ErrInvalidAmount
	}
	if !valid(reservationID, "res_") || !validKey(operationKey) || (kind != "capture" && kind != "release") {
		return Result{}, ErrInvalidIdentifier
	}
	tx, err := service.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Result{}, err
	}
	defer tx.Rollback(ctx)
	result, err := service.finishInTx(ctx, tx, reservationID, amount, operationKey, kind)
	if err != nil {
		return Result{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Result{}, err
	}
	return result, nil
}

func (service *Service) CaptureInTx(ctx context.Context, tx pgx.Tx, reservationID string, actual int64, operationKey string) (Result, error) {
	return service.finishInTx(ctx, tx, reservationID, actual, operationKey, "capture")
}

func (service *Service) ReleaseInTx(ctx context.Context, tx pgx.Tx, reservationID, operationKey string) (Result, error) {
	return service.finishInTx(ctx, tx, reservationID, 0, operationKey, "release")
}

func (service *Service) finishInTx(ctx context.Context, tx pgx.Tx, reservationID string, amount int64, operationKey, kind string) (Result, error) {
	if amount < 0 {
		return Result{}, ErrInvalidAmount
	}
	if tx == nil || !valid(reservationID, "res_") || !validKey(operationKey) || (kind != "capture" && kind != "release") {
		return Result{}, ErrInvalidIdentifier
	}
	reservation, err := lockReservation(ctx, tx, reservationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{}, ErrInvalidTransition
	}
	if err != nil {
		return Result{}, err
	}
	if err := lockOperationKey(ctx, tx, reservation.OrganizationID, operationKey); err != nil {
		return Result{}, err
	}
	if existing, found, err := loadOperation(ctx, tx, reservation.OrganizationID, operationKey, kind, amount, reservationID); err != nil {
		return Result{}, err
	} else if found {
		return existing, nil
	}
	if reservation.State != "pending" || (kind == "capture" && amount > reservation.Maximum) {
		return Result{}, ErrInvalidTransition
	}
	var available, reserved int64
	if err := tx.QueryRow(ctx, `SELECT available,reserved FROM organization_wallets WHERE organization_id=$1 AND currency=$2 FOR UPDATE`, reservation.OrganizationID, Currency).Scan(&available, &reserved); err != nil {
		return Result{}, err
	}
	release := reservation.Maximum
	if kind == "capture" {
		reservation.Captured = amount
		release -= amount
		reservation.State = "captured"
	} else {
		reservation.State = "released"
	}
	balance := Balance{Available: available + release, Reserved: reserved - reservation.Maximum, Currency: Currency}
	if balance.Reserved < 0 {
		return Result{}, ErrInvalidTransition
	}
	if _, err := tx.Exec(ctx, `UPDATE organization_wallets SET available=$2,reserved=$3,version=version+1,updated_at=now() WHERE organization_id=$1 AND currency=$4`, reservation.OrganizationID, balance.Available, balance.Reserved, Currency); err != nil {
		return Result{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE wallet_reservations SET state=$2,captured=$3,updated_at=now() WHERE id=$1`, reservation.ID, reservation.State, reservation.Captured); err != nil {
		return Result{}, err
	}
	operationID, err := service.id("wop_")
	if err != nil {
		return Result{}, err
	}
	if err := insertOperation(ctx, tx, operationID, reservation.OrganizationID, operationKey, kind, amount, reservation.ID, balance, reservation); err != nil {
		return Result{}, err
	}
	if amount > 0 {
		if err := service.entry(ctx, tx, operationID, reservation, "capture", 0, -amount); err != nil {
			return Result{}, err
		}
	}
	if release > 0 {
		if err := service.entry(ctx, tx, operationID, reservation, "release", release, -release); err != nil {
			return Result{}, err
		}
	}
	return Result{balance, reservation}, nil
}

func (service *Service) Refund(ctx context.Context, reservationID string, amount int64, operationKey string) (Result, error) {
	if amount <= 0 {
		return Result{}, ErrInvalidAmount
	}
	if !valid(reservationID, "res_") || !validKey(operationKey) {
		return Result{}, ErrInvalidIdentifier
	}
	tx, err := service.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Result{}, err
	}
	defer tx.Rollback(ctx)
	reservation, err := lockReservation(ctx, tx, reservationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{}, ErrInvalidTransition
	}
	if err != nil {
		return Result{}, err
	}
	if err := lockOperationKey(ctx, tx, reservation.OrganizationID, operationKey); err != nil {
		return Result{}, err
	}
	if existing, found, err := loadOperation(ctx, tx, reservation.OrganizationID, operationKey, "refund", amount, reservationID); err != nil {
		return Result{}, err
	} else if found {
		return existing, nil
	}
	if reservation.State != "captured" || amount > reservation.Captured-reservation.Refunded {
		return Result{}, ErrInvalidTransition
	}
	var balance Balance
	balance.Currency = Currency
	if err := tx.QueryRow(ctx, `UPDATE organization_wallets SET available=available+$2,version=version+1,updated_at=now() WHERE organization_id=$1 AND currency=$3 RETURNING available,reserved`, reservation.OrganizationID, amount, Currency).Scan(&balance.Available, &balance.Reserved); err != nil {
		return Result{}, classifyAmount(err)
	}
	reservation.Refunded += amount
	if _, err := tx.Exec(ctx, `UPDATE wallet_reservations SET refunded=$2,updated_at=now() WHERE id=$1`, reservation.ID, reservation.Refunded); err != nil {
		return Result{}, err
	}
	operationID, err := service.id("wop_")
	if err != nil {
		return Result{}, err
	}
	if err := insertOperation(ctx, tx, operationID, reservation.OrganizationID, operationKey, "refund", amount, reservation.ID, balance, reservation); err != nil {
		return Result{}, err
	}
	if err := service.entry(ctx, tx, operationID, reservation, "refund", amount, 0); err != nil {
		return Result{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Result{}, err
	}
	return Result{balance, reservation}, nil
}

func lockReservation(ctx context.Context, tx pgx.Tx, id string) (Reservation, error) {
	var r Reservation
	err := tx.QueryRow(ctx, `SELECT id,organization_id,project_id,request_id,state,maximum,captured,refunded FROM wallet_reservations WHERE id=$1 FOR UPDATE`, id).Scan(&r.ID, &r.OrganizationID, &r.ProjectID, &r.RequestID, &r.State, &r.Maximum, &r.Captured, &r.Refunded)
	return r, err
}

func lockOperationKey(ctx context.Context, tx pgx.Tx, organizationID, operationKey string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1 || ':' || $2, 0))`, organizationID, operationKey)
	return err
}
func loadOperation(ctx context.Context, tx pgx.Tx, org, key, kind string, amount int64, reservationID string) (Result, bool, error) {
	var storedKind string
	var storedAmount, available, reserved, maximum, captured, refunded int64
	var rid, state, projectID, requestID *string
	err := tx.QueryRow(ctx, `SELECT kind,amount,reservation_id,result_available,result_reserved,result_state,result_project_id,result_request_id,result_maximum,result_captured,result_refunded FROM wallet_operations WHERE organization_id=$1 AND operation_key=$2`, org, key).Scan(&storedKind, &storedAmount, &rid, &available, &reserved, &state, &projectID, &requestID, &maximum, &captured, &refunded)
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, false, err
	}
	if storedKind != kind || storedAmount != amount || (reservationID != "" && (rid == nil || *rid != reservationID)) {
		return Result{}, false, ErrIdempotencyConflict
	}
	return Result{Balance: Balance{Available: available, Reserved: reserved, Currency: Currency}, Reservation: Reservation{ID: value(rid), OrganizationID: org, ProjectID: value(projectID), RequestID: value(requestID), State: value(state), Maximum: maximum, Captured: captured, Refunded: refunded}}, true, nil
}
func value(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
func insertOperation(ctx context.Context, tx pgx.Tx, id, org, key, kind string, amount int64, reservationID string, b Balance, r Reservation) error {
	var rid, state, projectID, requestID any
	if reservationID != "" {
		rid = reservationID
		state = r.State
		projectID = r.ProjectID
		requestID = r.RequestID
	}
	_, err := tx.Exec(ctx, `INSERT INTO wallet_operations(id,organization_id,operation_key,kind,amount,reservation_id,result_available,result_reserved,result_state,result_project_id,result_request_id,result_maximum,result_captured,result_refunded) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`, id, org, key, kind, amount, rid, b.Available, b.Reserved, state, projectID, requestID, r.Maximum, r.Captured, r.Refunded)
	return err
}
func (service *Service) entry(ctx context.Context, tx pgx.Tx, operationID string, r Reservation, kind string, available, reserved int64) error {
	id, err := service.id("led_")
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO ledger_entries(id,operation_id,organization_id,project_id,reservation_id,entry_type,currency,delta_available,delta_reserved) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, id, operationID, r.OrganizationID, r.ProjectID, r.ID, kind, Currency, available, reserved)
	return err
}
func (service *Service) id(prefix string) (string, error) {
	ids, err := service.ids(prefix)
	if err != nil {
		return "", err
	}
	return ids[0], nil
}
func (service *Service) ids(prefixes ...string) ([]string, error) {
	result := make([]string, len(prefixes))
	for i, prefix := range prefixes {
		value := make([]byte, 16)
		if _, err := io.ReadFull(service.entropy, value); err != nil {
			return nil, fmt.Errorf("generate ledger id: %w", err)
		}
		result[i] = prefix + hex.EncodeToString(value)
	}
	return result, nil
}
func valid(value, prefix string) bool {
	return strings.HasPrefix(value, prefix) && len(value) <= 200 && strings.TrimSpace(value) == value
}
func validKey(value string) bool {
	return value != "" && len(value) <= 200 && strings.TrimSpace(value) == value
}
func classifyUnique(err error) error {
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) && postgresError.Code == "23505" {
		return ErrIdempotencyConflict
	}
	return err
}

func classifyAmount(err error) error {
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) && (postgresError.Code == "22003" || postgresError.Code == "23514") {
		return ErrInvalidAmount
	}
	return err
}
