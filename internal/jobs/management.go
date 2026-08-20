package jobs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	joboperation "github.com/nativegatewayhq/gateway/operations/job"
)

type ManagementFilter struct {
	Protocol, Status, SettlementState, Model string
}

type ModelAccess struct{ Protocol, Operation, Model string }

type ManagementListRequest struct {
	Owner           joboperation.Owner
	Filter          ManagementFilter
	AllowAllModels  bool
	AllowedModels   []ModelAccess
	Limit           int
	BeforeCreatedAt time.Time
	BeforeID        string
}

type ManagementBilling struct {
	Currency     string `json:"currency"`
	ReservedSale int64  `json:"reserved_sale"`
	CapturedSale int64  `json:"captured_sale"`
	State        string `json:"state"`
}

type ManagementUsage struct {
	Mode                 string `json:"mode"`
	EstimatedQuantity    int64  `json:"estimated_quantity"`
	ActualQuantity       *int64 `json:"actual_quantity,omitempty"`
	Unit                 string `json:"unit"`
	ReconciliationReason string `json:"reconciliation_reason,omitempty"`
}

type ManagementJob struct {
	ID              string             `json:"id"`
	Protocol        string             `json:"protocol"`
	Operation       string             `json:"operation"`
	Model           string             `json:"model"`
	Status          string             `json:"status"`
	SettlementState string             `json:"settlement_state"`
	FailureCategory string             `json:"failure_category,omitempty"`
	ResultAvailable bool               `json:"result_available"`
	Usage           ManagementUsage    `json:"usage"`
	Billing         *ManagementBilling `json:"billing,omitempty"`
	CreatedAt       time.Time          `json:"created_at"`
	UpdatedAt       time.Time          `json:"updated_at"`
	CompletedAt     *time.Time         `json:"completed_at,omitempty"`
}

type ManagementEvent struct {
	Version    int64     `json:"version"`
	Type       string    `json:"type"`
	FromStatus string    `json:"from_status,omitempty"`
	ToStatus   string    `json:"to_status"`
	Source     string    `json:"source"`
	Category   string    `json:"category,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

type ManagementDetail struct {
	Job       ManagementJob
	Events    []ManagementEvent
	Truncated bool
}

func (repository *Repository) ListManagement(ctx context.Context, request ManagementListRequest) ([]ManagementJob, bool, error) {
	if !validManagementRequest(request) {
		return nil, false, joboperation.ErrInvalid
	}
	if !request.AllowAllModels && len(request.AllowedModels) == 0 {
		return []ManagementJob{}, false, nil
	}
	args := []any{request.Owner.OrganizationID, request.Owner.ProjectID, request.Owner.APIKeyID}
	where := []string{"job.organization_id=$1", "job.project_id=$2", "job.api_key_id=$3"}
	add := func(value any) string { args = append(args, value); return fmt.Sprintf("$%d", len(args)) }
	if request.Filter.Protocol != "" {
		where = append(where, "job.protocol="+add(request.Filter.Protocol))
	}
	if request.Filter.Status != "" {
		where = append(where, "job.status="+add(request.Filter.Status))
	}
	if request.Filter.SettlementState != "" {
		where = append(where, "job.settlement_state="+add(request.Filter.SettlementState))
	}
	if request.Filter.Model != "" {
		where = append(where, "job.model="+add(request.Filter.Model))
	}
	if !request.AllowAllModels {
		allowed := make([]string, 0, len(request.AllowedModels))
		for _, model := range request.AllowedModels {
			allowed = append(allowed, "(job.protocol="+add(model.Protocol)+" AND job.operation="+add(model.Operation)+" AND job.model="+add(model.Model)+")")
		}
		where = append(where, "("+strings.Join(allowed, " OR ")+")")
	}
	if !request.BeforeCreatedAt.IsZero() {
		where = append(where, "(job.created_at,job.id)<("+add(request.BeforeCreatedAt.UTC())+","+add(request.BeforeID)+")")
	}
	args = append(args, request.Limit+1)
	query := managementSelect + " WHERE " + strings.Join(where, " AND ") + fmt.Sprintf(" ORDER BY job.created_at DESC,job.id DESC LIMIT $%d", len(args))
	rows, err := repository.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	items := make([]ManagementJob, 0, request.Limit+1)
	for rows.Next() {
		item, err := scanManagementJob(rows)
		if err != nil {
			return nil, false, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	hasMore := len(items) > request.Limit
	if hasMore {
		items = items[:request.Limit]
	}
	return items, hasMore, nil
}

func (repository *Repository) GetManagement(ctx context.Context, owner joboperation.Owner, id string, allow func(string, string, string) bool) (ManagementDetail, error) {
	if !validOwner(owner) || !joboperation.ValidID(id) || allow == nil {
		return ManagementDetail{}, joboperation.ErrInvalid
	}
	item, err := scanManagementJob(repository.pool.QueryRow(ctx, managementSelect+` WHERE job.id=$1 AND job.organization_id=$2 AND job.project_id=$3 AND job.api_key_id=$4`, id, owner.OrganizationID, owner.ProjectID, owner.APIKeyID))
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !allow(item.Protocol, item.Operation, item.Model)) {
		return ManagementDetail{}, joboperation.ErrNotFound
	}
	if err != nil {
		return ManagementDetail{}, err
	}
	rows, err := repository.pool.Query(ctx, `SELECT version,event_type,COALESCE(from_status,''),to_status,source,COALESCE(category,''),created_at FROM async_job_events WHERE job_id=$1 ORDER BY version LIMIT 257`, id)
	if err != nil {
		return ManagementDetail{}, err
	}
	defer rows.Close()
	events := make([]ManagementEvent, 0, 256)
	truncated := false
	for rows.Next() {
		if len(events) == 256 {
			truncated = true
			break
		}
		var event ManagementEvent
		if err := rows.Scan(&event.Version, &event.Type, &event.FromStatus, &event.ToStatus, &event.Source, &event.Category, &event.CreatedAt); err != nil {
			return ManagementDetail{}, err
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return ManagementDetail{}, err
	}
	return ManagementDetail{Job: item, Events: events, Truncated: truncated}, nil
}

const managementSelect = `SELECT job.id,job.protocol,job.operation,job.model,job.status,job.settlement_state,COALESCE(job.failure_category,''),job.response_body IS NOT NULL,job.created_at,job.updated_at,job.completed_at,
    job.usage_unit,job.estimated_quantity,usage.quantity,usage.reconciliation_reason,
    charge.currency,charge.reserved_sale,charge.captured_sale,charge.state
    FROM async_jobs job
    LEFT JOIN async_job_usage_evidence usage ON usage.job_id=job.id
    LEFT JOIN image_request_charges charge ON charge.id=job.charge_id`

type managementScanner interface{ Scan(...any) error }

func scanManagementJob(row managementScanner) (ManagementJob, error) {
	var item ManagementJob
	var completed *time.Time
	var usageUnit, usageReason, currency, chargeState *string
	var estimated, actual, reserved, captured *int64
	if err := row.Scan(&item.ID, &item.Protocol, &item.Operation, &item.Model, &item.Status, &item.SettlementState, &item.FailureCategory, &item.ResultAvailable, &item.CreatedAt, &item.UpdatedAt, &completed, &usageUnit, &estimated, &actual, &usageReason, &currency, &reserved, &captured, &chargeState); err != nil {
		return item, err
	}
	item.CompletedAt = completed
	if estimated == nil {
		item.Usage = ManagementUsage{Mode: "legacy", EstimatedQuantity: 1, Unit: "image"}
	} else {
		mode := "verified"
		if usageReason != nil && *usageReason != "" {
			mode = "manual"
		}
		item.Usage = ManagementUsage{Mode: mode, EstimatedQuantity: *estimated, Unit: *usageUnit, ActualQuantity: actual}
		if usageReason != nil {
			item.Usage.ReconciliationReason = *usageReason
		}
	}
	if currency != nil {
		item.Billing = &ManagementBilling{Currency: *currency, ReservedSale: *reserved, CapturedSale: *captured, State: *chargeState}
	}
	return item, nil
}

func validManagementRequest(request ManagementListRequest) bool {
	if !validOwner(request.Owner) || request.Limit < 1 || request.Limit > 100 || (!request.BeforeCreatedAt.IsZero() && !joboperation.ValidID(request.BeforeID)) || (request.BeforeCreatedAt.IsZero() && request.BeforeID != "") {
		return false
	}
	if request.Filter.Protocol != "" && request.Filter.Protocol != "replicate" && request.Filter.Protocol != "fal" {
		return false
	}
	if request.Filter.Status != "" && !validManagementStatus(request.Filter.Status) {
		return false
	}
	if request.Filter.SettlementState != "" && request.Filter.SettlementState != "NONE" && request.Filter.SettlementState != "PENDING" && request.Filter.SettlementState != "SETTLED" && request.Filter.SettlementState != "MANUAL_REVIEW" {
		return false
	}
	if len(request.Filter.Model) > 200 || strings.TrimSpace(request.Filter.Model) != request.Filter.Model {
		return false
	}
	for _, access := range request.AllowedModels {
		if (access.Protocol != "replicate" && access.Protocol != "fal") || access.Operation != "image.generate" || access.Model == "" || len(access.Model) > 200 {
			return false
		}
	}
	return true
}

func validManagementStatus(value string) bool {
	switch joboperation.Status(value) {
	case joboperation.Pending, joboperation.Queued, joboperation.Processing, joboperation.Succeeded, joboperation.Failed, joboperation.Canceled, joboperation.Reconciling:
		return true
	}
	return false
}
