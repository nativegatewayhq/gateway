// Package spendcap enforces Provider channel upstream-cost budgets.
package spendcap

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nativegatewayhq/gateway/internal/ledger"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
)

var (
	ErrInvalid  = errors.New("invalid provider spend cap")
	ErrNotFound = errors.New("provider spend cap not found")
	ErrExceeded = errors.New("provider spend cap exceeded")
	ErrConflict = errors.New("provider spend settlement conflict")
)

type Period string

const (
	Day   Period = "day"
	Month Period = "month"
)

type PolicyInput struct {
	ChannelID     string
	Period        Period
	Limit         int64
	Actor, Reason string
}
type Policy struct {
	ID, ChannelID, Status string
	Period                Period
	Version, Limit        int64
}
type Reservation struct {
	ChargeID, ChannelID, Currency string
	EstimatedCost                 int64
}
type Allocation struct {
	PolicyID, ChannelID          string
	Provider                     providercredentials.ProviderID
	Period                       Period
	PolicyVersion, Limit, Amount int64
	PeriodStart, PeriodEnd       time.Time
}
type Usage struct {
	PolicyID                  string
	PeriodStart, PeriodEnd    time.Time
	Reserved, Captured, Limit int64
	Currency                  string
}
type LimitError struct {
	ChannelID string
	Provider  providercredentials.ProviderID
	Period    Period
	ResetAt   time.Time
}

func (err *LimitError) Error() string { return ErrExceeded.Error() }
func (err *LimitError) Unwrap() error { return ErrExceeded }

type Store struct {
	pool    *pgxpool.Pool
	entropy io.Reader
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool, entropy: rand.Reader} }

func ValidatePolicy(input PolicyInput) error {
	if !validID(input.ChannelID, "channel_") || (input.Period != Day && input.Period != Month) || input.Limit <= 0 || !validText(input.Actor, 200) || !validText(input.Reason, 500) {
		return ErrInvalid
	}
	return nil
}
func ValidateDisable(id, actor, reason string) error {
	if !validID(id, "spcap_") || !validText(actor, 200) || !validText(reason, 500) {
		return ErrInvalid
	}
	return nil
}

func (store *Store) SetPolicy(ctx context.Context, input PolicyInput) (Policy, error) {
	if store == nil || store.pool == nil || ValidatePolicy(input) != nil {
		return Policy{}, ErrInvalid
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return Policy{}, err
	}
	defer tx.Rollback(ctx)
	var active bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM provider_channels WHERE id=$1 AND status='active')`, input.ChannelID).Scan(&active); err != nil {
		return Policy{}, err
	}
	if !active {
		return Policy{}, ErrInvalid
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "provider-spend:"+input.ChannelID+":"+string(input.Period)); err != nil {
		return Policy{}, err
	}
	var policy Policy
	err = tx.QueryRow(ctx, `SELECT id,channel_id,period,version,limit_amount,status FROM provider_channel_spend_policies WHERE channel_id=$1 AND period=$2 AND status='active' FOR UPDATE`, input.ChannelID, input.Period).Scan(&policy.ID, &policy.ChannelID, &policy.Period, &policy.Version, &policy.Limit, &policy.Status)
	action := "create"
	if errors.Is(err, pgx.ErrNoRows) {
		policyID, idErr := store.id("spcap_")
		if idErr != nil {
			return Policy{}, idErr
		}
		policy = Policy{ID: policyID, ChannelID: input.ChannelID, Period: input.Period, Version: 1, Limit: input.Limit, Status: "active"}
		if _, err := tx.Exec(ctx, `INSERT INTO provider_channel_spend_policies(id,channel_id,period,currency,limit_amount) VALUES($1,$2,$3,$4,$5)`, policy.ID, policy.ChannelID, policy.Period, ledger.Currency, policy.Limit); err != nil {
			return Policy{}, err
		}
	} else if err != nil {
		return Policy{}, err
	} else {
		action = "update"
		policy.Version++
		policy.Limit = input.Limit
		if _, err := tx.Exec(ctx, `UPDATE provider_channel_spend_policies SET version=$2,limit_amount=$3,updated_at=now() WHERE id=$1`, policy.ID, policy.Version, policy.Limit); err != nil {
			return Policy{}, err
		}
	}
	eventID, err := store.id("spevt_")
	if err != nil {
		return Policy{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO provider_channel_spend_policy_events(id,policy_id,version,action,actor,reason,limit_amount) VALUES($1,$2,$3,$4,$5,$6,$7)`, eventID, policy.ID, policy.Version, action, input.Actor, input.Reason, input.Limit); err != nil {
		return Policy{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Policy{}, err
	}
	return policy, nil
}

func (store *Store) DisablePolicy(ctx context.Context, id, actor, reason string) error {
	if store == nil || store.pool == nil || ValidateDisable(id, actor, reason) != nil {
		return ErrInvalid
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var version, limit int64
	err = tx.QueryRow(ctx, `SELECT version,limit_amount FROM provider_channel_spend_policies WHERE id=$1 AND status='active' FOR UPDATE`, id).Scan(&version, &limit)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	version++
	if _, err := tx.Exec(ctx, `UPDATE provider_channel_spend_policies SET version=$2,status='disabled',updated_at=now() WHERE id=$1`, id, version); err != nil {
		return err
	}
	eventID, err := store.id("spevt_")
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO provider_channel_spend_policy_events(id,policy_id,version,action,actor,reason,limit_amount) VALUES($1,$2,$3,'disable',$4,$5,$6)`, eventID, id, version, actor, reason, limit); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (store *Store) ReserveInTx(ctx context.Context, tx pgx.Tx, request Reservation) ([]Allocation, error) {
	if tx == nil || !validChargeID(request.ChargeID) || !validID(request.ChannelID, "channel_") || request.Currency != ledger.Currency || request.EstimatedCost < 0 {
		return nil, ErrInvalid
	}
	rows, err := tx.Query(ctx, `SELECT p.id,p.version,p.channel_id,p.period,p.limit_amount,c.provider FROM provider_channel_spend_policies p JOIN provider_channels c ON c.id=p.channel_id WHERE p.channel_id=$1 AND p.status='active' ORDER BY p.id FOR SHARE`, request.ChannelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var allocations []Allocation
	for rows.Next() {
		var allocation Allocation
		if err := rows.Scan(&allocation.PolicyID, &allocation.PolicyVersion, &allocation.ChannelID, &allocation.Period, &allocation.Limit, &allocation.Provider); err != nil {
			return nil, err
		}
		allocations = append(allocations, allocation)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(allocations) == 0 || request.EstimatedCost == 0 {
		return nil, nil
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&now); err != nil {
		return nil, err
	}
	var exceeded *LimitError
	for index := range allocations {
		allocation := &allocations[index]
		allocation.PeriodStart, allocation.PeriodEnd = bounds(now, allocation.Period)
		allocation.Amount = request.EstimatedCost
		if _, err := tx.Exec(ctx, `INSERT INTO provider_channel_spend_buckets(policy_id,period_start,period_end,currency) VALUES($1,$2,$3,$4) ON CONFLICT(policy_id,period_start) DO NOTHING`, allocation.PolicyID, allocation.PeriodStart, allocation.PeriodEnd, request.Currency); err != nil {
			return nil, err
		}
		var reserved, captured int64
		if err := tx.QueryRow(ctx, `SELECT reserved,captured FROM provider_channel_spend_buckets WHERE policy_id=$1 AND period_start=$2 FOR UPDATE`, allocation.PolicyID, allocation.PeriodStart).Scan(&reserved, &captured); err != nil {
			return nil, err
		}
		if exceeds(allocation.Limit, captured, reserved, allocation.Amount) && (exceeded == nil || allocation.PeriodEnd.Before(exceeded.ResetAt)) {
			exceeded = &LimitError{ChannelID: allocation.ChannelID, Provider: allocation.Provider, Period: allocation.Period, ResetAt: allocation.PeriodEnd}
		}
	}
	if exceeded != nil {
		return nil, exceeded
	}
	for _, allocation := range allocations {
		if _, err := tx.Exec(ctx, `UPDATE provider_channel_spend_buckets SET reserved=reserved+$3,updated_at=now() WHERE policy_id=$1 AND period_start=$2`, allocation.PolicyID, allocation.PeriodStart, allocation.Amount); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO provider_channel_spend_allocations(charge_id,policy_id,policy_version,period_start,period_end,limit_snapshot,reserved_cost) VALUES($1,$2,$3,$4,$5,$6,$7)`, request.ChargeID, allocation.PolicyID, allocation.PolicyVersion, allocation.PeriodStart, allocation.PeriodEnd, allocation.Limit, allocation.Amount); err != nil {
			return nil, err
		}
	}
	return allocations, nil
}

func (store *Store) CaptureInTx(ctx context.Context, tx pgx.Tx, chargeID string, actual int64) error {
	if actual == 0 {
		return store.finish(ctx, tx, chargeID, 0, "released")
	}
	return store.finish(ctx, tx, chargeID, actual, "captured")
}
func (store *Store) ReleaseInTx(ctx context.Context, tx pgx.Tx, chargeID string) error {
	return store.finish(ctx, tx, chargeID, 0, "released")
}
func (store *Store) finish(ctx context.Context, tx pgx.Tx, chargeID string, actual int64, target string) error {
	if tx == nil || !validChargeID(chargeID) || actual < 0 || (target != "captured" && target != "released") {
		return ErrInvalid
	}
	rows, err := tx.Query(ctx, `SELECT policy_id,period_start,reserved_cost,state,captured_cost FROM provider_channel_spend_allocations WHERE charge_id=$1 ORDER BY policy_id FOR UPDATE`, chargeID)
	if err != nil {
		return err
	}
	type stored struct {
		policyID, state    string
		start              time.Time
		reserved, captured int64
	}
	var allocations []stored
	for rows.Next() {
		var allocation stored
		if err := rows.Scan(&allocation.policyID, &allocation.start, &allocation.reserved, &allocation.state, &allocation.captured); err != nil {
			rows.Close()
			return err
		}
		allocations = append(allocations, allocation)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, allocation := range allocations {
		if allocation.state == target && (target != "captured" || allocation.captured == actual) {
			continue
		}
		if allocation.state != "reserved" || actual > allocation.reserved {
			return ErrConflict
		}
		if target == "captured" {
			if actual <= 0 {
				return ErrInvalid
			}
			if _, err := tx.Exec(ctx, `UPDATE provider_channel_spend_buckets SET reserved=reserved-$3,captured=captured+$4,updated_at=now() WHERE policy_id=$1 AND period_start=$2`, allocation.policyID, allocation.start, allocation.reserved, actual); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE provider_channel_spend_allocations SET state='captured',captured_cost=$2,updated_at=now() WHERE charge_id=$1 AND policy_id=$3`, chargeID, actual, allocation.policyID); err != nil {
				return err
			}
		} else {
			if _, err := tx.Exec(ctx, `UPDATE provider_channel_spend_buckets SET reserved=reserved-$3,updated_at=now() WHERE policy_id=$1 AND period_start=$2`, allocation.policyID, allocation.start, allocation.reserved); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE provider_channel_spend_allocations SET state='released',updated_at=now() WHERE charge_id=$1 AND policy_id=$2`, chargeID, allocation.policyID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (store *Store) Usage(ctx context.Context, id string, at time.Time) (Usage, error) {
	if store == nil || store.pool == nil || !validID(id, "spcap_") {
		return Usage{}, ErrInvalid
	}
	var period Period
	var usage Usage
	usage.PolicyID = id
	if err := store.pool.QueryRow(ctx, `SELECT period,limit_amount,currency FROM provider_channel_spend_policies WHERE id=$1`, id).Scan(&period, &usage.Limit, &usage.Currency); errors.Is(err, pgx.ErrNoRows) {
		return Usage{}, ErrNotFound
	} else if err != nil {
		return Usage{}, err
	}
	if at.IsZero() {
		if err := store.pool.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&at); err != nil {
			return Usage{}, err
		}
	}
	usage.PeriodStart, usage.PeriodEnd = bounds(at, period)
	err := store.pool.QueryRow(ctx, `SELECT reserved,captured FROM provider_channel_spend_buckets WHERE policy_id=$1 AND period_start=$2`, id, usage.PeriodStart).Scan(&usage.Reserved, &usage.Captured)
	if errors.Is(err, pgx.ErrNoRows) {
		return usage, nil
	}
	return usage, err
}

func bounds(value time.Time, period Period) (time.Time, time.Time) {
	value = value.UTC()
	start := time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, time.UTC)
	if period == Month {
		start = time.Date(value.Year(), value.Month(), 1, 0, 0, 0, 0, time.UTC)
		return start, start.AddDate(0, 1, 0)
	}
	return start, start.AddDate(0, 0, 1)
}
func exceeds(limit, captured, reserved, amount int64) bool {
	return limit <= 0 || captured < 0 || reserved < 0 || amount < 0 || captured > limit || reserved > limit-captured || amount > limit-captured-reserved
}
func validText(value string, max int) bool {
	return value != "" && len(value) <= max && strings.TrimSpace(value) == value
}
func validID(value, prefix string) bool {
	if len(value) != len(prefix)+32 || !strings.HasPrefix(value, prefix) {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, prefix))
	return err == nil
}
func validChargeID(value string) bool {
	return validID(value, "charge_") || validID(value, "chc_") || validID(value, "asc_")
}
func (store *Store) id(prefix string) (string, error) {
	value := make([]byte, 16)
	if _, err := io.ReadFull(store.entropy, value); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(value), nil
}
