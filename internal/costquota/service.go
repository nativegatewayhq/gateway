// Package costquota enforces hierarchical sale-cost budgets in Billing transactions.
package costquota

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nativegatewayhq/gateway/internal/ledger"
)

var (
	ErrInvalidPolicy = errors.New("invalid cost quota policy")
	ErrNotFound      = errors.New("cost quota policy not found")
	ErrExceeded      = errors.New("cost quota exceeded")
	ErrConflict      = errors.New("cost quota settlement conflict")
)

type ScopeType string
type Period string

const (
	Organization ScopeType = "organization"
	Project      ScopeType = "project"
	APIKey       ScopeType = "api_key"
	Day          Period    = "day"
	Month        Period    = "month"
)

type PolicyInput struct {
	ScopeType      ScopeType
	OrganizationID string
	ProjectID      string
	APIKeyID       string
	Protocol       string
	Operation      string
	Model          string
	Period         Period
	Limit          int64
	Actor          string
	Reason         string
}

type Policy struct {
	ID, OrganizationID, ProjectID, APIKeyID, Protocol, Operation, Model string
	ScopeType                                                           ScopeType
	Period                                                              Period
	Version, Limit                                                      int64
	Status                                                              string
}

type ReservationRequest struct {
	ChargeID, OrganizationID, ProjectID, APIKeyID string
	Protocol, Operation, Model, Currency          string
	Amount                                        int64
}

type Allocation struct {
	PolicyID                     string
	PolicyVersion, Limit, Amount int64
	ScopeType                    ScopeType
	PeriodStart, PeriodEnd       time.Time
}

type Usage struct {
	PolicyID    string
	PeriodStart time.Time
	PeriodEnd   time.Time
	Reserved    int64
	Captured    int64
	Limit       int64
	Currency    string
}

type LimitError struct {
	ScopeType ScopeType
	Period    Period
	ResetAt   time.Time
	ProjectID string
	APIKeyID  string
}

func (err *LimitError) Error() string { return ErrExceeded.Error() }
func (err *LimitError) Unwrap() error { return ErrExceeded }

type Store struct {
	pool    *pgxpool.Pool
	entropy io.Reader
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool, entropy: rand.Reader} }

func ValidatePolicy(input PolicyInput) error {
	if !validPolicy(input) {
		return ErrInvalidPolicy
	}
	return nil
}

func ValidateDisable(policyID, actor, reason string) error {
	if !validID(policyID, "quota_") || !validText(actor, 200) || !validText(reason, 500) {
		return ErrInvalidPolicy
	}
	return nil
}

func (store *Store) SetPolicy(ctx context.Context, input PolicyInput) (Policy, error) {
	if store == nil || store.pool == nil || ValidatePolicy(input) != nil {
		return Policy{}, ErrInvalidPolicy
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return Policy{}, err
	}
	defer tx.Rollback(ctx)
	if err := validateOwnership(ctx, tx, input); err != nil {
		return Policy{}, err
	}
	dimension := fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s|%s", input.ScopeType, input.OrganizationID, input.ProjectID, input.APIKeyID, input.Protocol, input.Operation, input.Model, input.Period)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "cost-quota:"+dimension); err != nil {
		return Policy{}, err
	}
	policy, found, err := findActive(ctx, tx, input)
	if err != nil {
		return Policy{}, err
	}
	action := "create"
	if found {
		action = "update"
		policy.Version++
		policy.Limit = input.Limit
		if _, err := tx.Exec(ctx, `UPDATE cost_quota_policies SET version=$2,limit_amount=$3,updated_at=now() WHERE id=$1`, policy.ID, policy.Version, policy.Limit); err != nil {
			return Policy{}, err
		}
	} else {
		id, err := store.id("quota_")
		if err != nil {
			return Policy{}, err
		}
		policy = Policy{ID: id, ScopeType: input.ScopeType, OrganizationID: input.OrganizationID, ProjectID: nullable(input.ProjectID), APIKeyID: nullable(input.APIKeyID), Protocol: nullable(input.Protocol), Operation: nullable(input.Operation), Model: nullable(input.Model), Period: input.Period, Version: 1, Limit: input.Limit, Status: "active"}
		_, err = tx.Exec(ctx, `INSERT INTO cost_quota_policies(id,scope_type,organization_id,project_id,api_key_id,protocol,operation,model,period,currency,limit_amount) VALUES($1,$2,$3,NULLIF($4,''),NULLIF($5,''),NULLIF($6,''),NULLIF($7,''),NULLIF($8,''),$9,$10,$11)`, policy.ID, policy.ScopeType, policy.OrganizationID, policy.ProjectID, policy.APIKeyID, policy.Protocol, policy.Operation, policy.Model, policy.Period, ledger.Currency, policy.Limit)
		if err != nil {
			return Policy{}, err
		}
	}
	eventID, err := store.id("qevt_")
	if err != nil {
		return Policy{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO cost_quota_policy_events(id,policy_id,version,action,actor,reason,limit_amount) VALUES($1,$2,$3,$4,$5,$6,$7)`, eventID, policy.ID, policy.Version, action, input.Actor, input.Reason, input.Limit); err != nil {
		return Policy{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Policy{}, err
	}
	return policy, nil
}

func (store *Store) DisablePolicy(ctx context.Context, policyID, actor, reason string) error {
	if store == nil || store.pool == nil || ValidateDisable(policyID, actor, reason) != nil {
		return ErrInvalidPolicy
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var version, limit int64
	err = tx.QueryRow(ctx, `SELECT version,limit_amount FROM cost_quota_policies WHERE id=$1 AND status='active' FOR UPDATE`, policyID).Scan(&version, &limit)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	version++
	if _, err := tx.Exec(ctx, `UPDATE cost_quota_policies SET version=$2,status='disabled',updated_at=now() WHERE id=$1`, policyID, version); err != nil {
		return err
	}
	eventID, err := store.id("qevt_")
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO cost_quota_policy_events(id,policy_id,version,action,actor,reason,limit_amount) VALUES($1,$2,$3,'disable',$4,$5,$6)`, eventID, policyID, version, actor, reason, limit); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Usage returns the current UTC bucket without creating usage as a read side effect.
func (store *Store) Usage(ctx context.Context, policyID string, at time.Time) (Usage, error) {
	if store == nil || store.pool == nil || !validID(policyID, "quota_") {
		return Usage{}, ErrInvalidPolicy
	}
	var period Period
	var limit int64
	var currency string
	if err := store.pool.QueryRow(ctx, `SELECT period,limit_amount,currency FROM cost_quota_policies WHERE id=$1`, policyID).Scan(&period, &limit, &currency); errors.Is(err, pgx.ErrNoRows) {
		return Usage{}, ErrNotFound
	} else if err != nil {
		return Usage{}, err
	}
	if at.IsZero() {
		if err := store.pool.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&at); err != nil {
			return Usage{}, err
		}
	}
	start, end := bounds(at, period)
	usage := Usage{PolicyID: policyID, PeriodStart: start, PeriodEnd: end, Limit: limit, Currency: currency}
	err := store.pool.QueryRow(ctx, `SELECT reserved,captured FROM cost_quota_buckets WHERE policy_id=$1 AND period_start=$2`, policyID, start).Scan(&usage.Reserved, &usage.Captured)
	if errors.Is(err, pgx.ErrNoRows) {
		return usage, nil
	}
	return usage, err
}

func (store *Store) ReserveInTx(ctx context.Context, tx pgx.Tx, request ReservationRequest) ([]Allocation, error) {
	if tx == nil || !validReservation(request) {
		return nil, ErrInvalidPolicy
	}
	rows, err := tx.Query(ctx, `SELECT id,version,scope_type,period,limit_amount FROM cost_quota_policies
		WHERE status='active' AND organization_id=$1
		AND ((scope_type='organization') OR (scope_type='project' AND project_id=$2) OR (scope_type='api_key' AND project_id=$2 AND api_key_id=$3))
		AND ((protocol IS NULL AND operation IS NULL AND model IS NULL) OR (protocol=$4 AND operation=$5 AND model=$6))
		ORDER BY id FOR SHARE`, request.OrganizationID, request.ProjectID, nullable(request.APIKeyID), request.Protocol, request.Operation, request.Model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type selected struct {
		id      string
		version int64
		scope   ScopeType
		period  Period
		limit   int64
	}
	var policies []selected
	for rows.Next() {
		var policy selected
		if err := rows.Scan(&policy.id, &policy.version, &policy.scope, &policy.period, &policy.limit); err != nil {
			return nil, err
		}
		policies = append(policies, policy)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(policies) == 0 {
		return nil, nil
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&now); err != nil {
		return nil, err
	}
	allocations := make([]Allocation, 0, len(policies))
	var exceeded *LimitError
	for _, policy := range policies {
		start, end := bounds(now, policy.period)
		if _, err := tx.Exec(ctx, `INSERT INTO cost_quota_buckets(policy_id,period_start,period_end,currency) VALUES($1,$2,$3,$4) ON CONFLICT(policy_id,period_start) DO NOTHING`, policy.id, start, end, request.Currency); err != nil {
			return nil, err
		}
		var reserved, captured int64
		if err := tx.QueryRow(ctx, `SELECT reserved,captured FROM cost_quota_buckets WHERE policy_id=$1 AND period_start=$2 FOR UPDATE`, policy.id, start).Scan(&reserved, &captured); err != nil {
			return nil, err
		}
		if exceeds(policy.limit, captured, reserved, request.Amount) && (exceeded == nil || end.Before(exceeded.ResetAt)) {
			exceeded = &LimitError{ScopeType: policy.scope, Period: policy.period, ResetAt: end, ProjectID: request.ProjectID, APIKeyID: request.APIKeyID}
		}
		allocations = append(allocations, Allocation{PolicyID: policy.id, PolicyVersion: policy.version, ScopeType: policy.scope, PeriodStart: start, PeriodEnd: end, Limit: policy.limit, Amount: request.Amount})
	}
	if exceeded != nil {
		return nil, exceeded
	}
	for _, allocation := range allocations {
		if _, err := tx.Exec(ctx, `UPDATE cost_quota_buckets SET reserved=reserved+$3,updated_at=now() WHERE policy_id=$1 AND period_start=$2`, allocation.PolicyID, allocation.PeriodStart, allocation.Amount); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO cost_quota_allocations(charge_id,policy_id,policy_version,period_start,period_end,limit_snapshot,reserved_amount) VALUES($1,$2,$3,$4,$5,$6,$7)`, request.ChargeID, allocation.PolicyID, allocation.PolicyVersion, allocation.PeriodStart, allocation.PeriodEnd, allocation.Limit, allocation.Amount); err != nil {
			return nil, err
		}
	}
	return allocations, nil
}

func (store *Store) CaptureInTx(ctx context.Context, tx pgx.Tx, chargeID string, actual int64) error {
	return store.finish(ctx, tx, chargeID, actual, "captured")
}

func (store *Store) ReleaseInTx(ctx context.Context, tx pgx.Tx, chargeID string) error {
	return store.finish(ctx, tx, chargeID, 0, "released")
}

func (store *Store) finish(ctx context.Context, tx pgx.Tx, chargeID string, actual int64, target string) error {
	if tx == nil || !validChargeID(chargeID) || actual < 0 || (target != "captured" && target != "released") {
		return ErrInvalidPolicy
	}
	rows, err := tx.Query(ctx, `SELECT policy_id,period_start,reserved_amount,state,captured_amount FROM cost_quota_allocations WHERE charge_id=$1 ORDER BY policy_id FOR UPDATE`, chargeID)
	if err != nil {
		return err
	}
	type row struct {
		policyID string
		start    time.Time
		reserved int64
		state    string
		captured int64
	}
	var allocations []row
	for rows.Next() {
		var allocation row
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
			if _, err := tx.Exec(ctx, `UPDATE cost_quota_buckets SET reserved=reserved-$3,captured=captured+$4,updated_at=now() WHERE policy_id=$1 AND period_start=$2`, allocation.policyID, allocation.start, allocation.reserved, actual); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE cost_quota_allocations SET state='captured',captured_amount=$2,updated_at=now() WHERE charge_id=$1 AND policy_id=$3`, chargeID, actual, allocation.policyID); err != nil {
				return err
			}
		} else {
			if _, err := tx.Exec(ctx, `UPDATE cost_quota_buckets SET reserved=reserved-$3,updated_at=now() WHERE policy_id=$1 AND period_start=$2`, allocation.policyID, allocation.start, allocation.reserved); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE cost_quota_allocations SET state='released',updated_at=now() WHERE charge_id=$1 AND policy_id=$2`, chargeID, allocation.policyID); err != nil {
				return err
			}
		}
	}
	return nil
}

func findActive(ctx context.Context, tx pgx.Tx, input PolicyInput) (Policy, bool, error) {
	var policy Policy
	err := tx.QueryRow(ctx, `SELECT id,version,scope_type,organization_id,COALESCE(project_id,''),COALESCE(api_key_id,''),COALESCE(protocol,''),COALESCE(operation,''),COALESCE(model,''),period,limit_amount,status FROM cost_quota_policies WHERE status='active' AND scope_type=$1 AND organization_id=$2 AND project_id IS NOT DISTINCT FROM NULLIF($3,'') AND api_key_id IS NOT DISTINCT FROM NULLIF($4,'') AND protocol IS NOT DISTINCT FROM NULLIF($5,'') AND operation IS NOT DISTINCT FROM NULLIF($6,'') AND model IS NOT DISTINCT FROM NULLIF($7,'') AND period=$8 FOR UPDATE`, input.ScopeType, input.OrganizationID, input.ProjectID, input.APIKeyID, input.Protocol, input.Operation, input.Model, input.Period).Scan(&policy.ID, &policy.Version, &policy.ScopeType, &policy.OrganizationID, &policy.ProjectID, &policy.APIKeyID, &policy.Protocol, &policy.Operation, &policy.Model, &policy.Period, &policy.Limit, &policy.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return Policy{}, false, nil
	}
	return policy, err == nil, err
}

func validateOwnership(ctx context.Context, tx pgx.Tx, input PolicyInput) error {
	var valid bool
	switch input.ScopeType {
	case Organization:
		err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM organizations WHERE id=$1 AND status='active')`, input.OrganizationID).Scan(&valid)
		if err != nil {
			return err
		}
	case Project:
		err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM projects WHERE id=$1 AND organization_id=$2 AND status='active')`, input.ProjectID, input.OrganizationID).Scan(&valid)
		if err != nil {
			return err
		}
	case APIKey:
		err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM service_api_keys k JOIN projects p ON p.id=k.project_id WHERE k.id=$1 AND p.id=$2 AND p.organization_id=$3 AND k.status='active' AND p.status='active')`, input.APIKeyID, input.ProjectID, input.OrganizationID).Scan(&valid)
		if err != nil {
			return err
		}
	}
	if !valid {
		return ErrInvalidPolicy
	}
	return nil
}

func validPolicy(input PolicyInput) bool {
	if input.Limit <= 0 || (input.Period != Day && input.Period != Month) || !validText(input.Actor, 200) || !validText(input.Reason, 500) || !strings.HasPrefix(input.OrganizationID, "org_") {
		return false
	}
	if input.ScopeType == Organization && (input.ProjectID != "" || input.APIKeyID != "") {
		return false
	}
	if input.ScopeType == Project && (!strings.HasPrefix(input.ProjectID, "project_") || input.APIKeyID != "") {
		return false
	}
	if input.ScopeType == APIKey && (!strings.HasPrefix(input.ProjectID, "project_") || !strings.HasPrefix(input.APIKeyID, "key_")) {
		return false
	}
	if input.ScopeType != Organization && input.ScopeType != Project && input.ScopeType != APIKey {
		return false
	}
	return validDimension(input.Protocol, input.Operation, input.Model)
}

func validReservation(request ReservationRequest) bool {
	return validChargeID(request.ChargeID) && strings.HasPrefix(request.OrganizationID, "org_") && strings.HasPrefix(request.ProjectID, "project_") && request.Amount > 0 && request.Currency == ledger.Currency && validDimension(request.Protocol, request.Operation, request.Model) && request.Protocol != ""
}

func exceeds(limit, captured, reserved, amount int64) bool {
	return limit <= 0 || captured < 0 || reserved < 0 || amount <= 0 || captured > limit || reserved > limit-captured || amount > limit-captured-reserved
}

func validDimension(protocol, operation, model string) bool {
	if protocol == "" && operation == "" && model == "" {
		return true
	}
	return ((protocol == "openai" && (operation == "image.generate" || operation == "image.edit" || operation == "chat.completions" || operation == "responses.create")) || ((protocol == "gemini" || protocol == "replicate" || protocol == "fal") && operation == "image.generate")) && validText(model, 200)
}

func validChargeID(value string) bool { return validID(value, "charge_") || validID(value, "chc_") }

func bounds(value time.Time, period Period) (time.Time, time.Time) {
	value = value.UTC()
	start := time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, time.UTC)
	if period == Month {
		start = time.Date(value.Year(), value.Month(), 1, 0, 0, 0, 0, time.UTC)
		return start, start.AddDate(0, 1, 0)
	}
	return start, start.AddDate(0, 0, 1)
}

func nullable(value string) string { return value }
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
func (store *Store) id(prefix string) (string, error) {
	value := make([]byte, 16)
	if _, err := io.ReadFull(store.entropy, value); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(value), nil
}
