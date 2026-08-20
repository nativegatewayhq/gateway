// Package billing coordinates image price estimates with Wallet settlement.
package billing

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nativegatewayhq/gateway/internal/idempotency"
	"github.com/nativegatewayhq/gateway/internal/ledger"
	"github.com/nativegatewayhq/gateway/internal/pricing"
)

var (
	ErrInvalidRequest   = errors.New("invalid billable request")
	ErrRequestConflict  = errors.New("billable request conflict")
	ErrRequestPending   = errors.New("billable request already pending")
	ErrAlreadySettled   = errors.New("billable request already settled")
	ErrInvalidState     = errors.New("invalid charge state")
	ErrSnapshotCorrupt  = errors.New("response snapshot corrupt")
	ErrResponseTooLarge = errors.New("response snapshot too large")
)

type BeginRequest struct {
	RequestID          string
	OrganizationID     string
	ProjectID          string
	Protocol           string
	Operation          string
	Model              string
	ChannelID          string
	Quantity           int64
	Size               string
	Quality            string
	IdempotencyKey     string
	RequestFingerprint [32]byte
}

type ResponseSnapshot struct {
	Status  int
	Headers map[string][]string
	Body    []byte
}

type Outcome string
type Reason string

const (
	KnownSuccess Outcome = "KNOWN_SUCCESS"
	KnownFailure Outcome = "KNOWN_FAILURE"
	Unknown      Outcome = "UNKNOWN"

	ResponseUnavailable Reason = "response_unavailable"
	SettlementFailed    Reason = "settlement_failed"
	ExecutorTimeout     Reason = "executor_timeout"
	ExecutorConnection  Reason = "executor_connection_lost"
	ProviderPanic       Reason = "provider_panic"
)

type Observation struct {
	Outcome  Outcome
	Reason   Reason
	Snapshot ResponseSnapshot
}

type Charge struct {
	ID                 string
	RequestID          string
	OrganizationID     string
	ProjectID          string
	Protocol           string
	Operation          string
	Model              string
	ChannelID          string
	PriceID            string
	Quantity           int64
	Size               string
	Quality            string
	Currency           string
	EstimatedCost      int64
	ReservedSale       int64
	ActualCost         *int64
	CapturedSale       int64
	ReservationID      string
	State              string
	IdempotencyKey     string
	RequestFingerprint [32]byte
	SnapshotVersion    int16
	Response           ResponseSnapshot
	ResponseSHA256     [32]byte
	Replay             bool
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
	pool             *pgxpool.Pool
	estimator        Estimator
	wallet           Wallet
	entropy          io.Reader
	maxResponseBytes int64
}

func NewService(pool *pgxpool.Pool, estimator Estimator, wallet Wallet) (*Service, error) {
	return NewServiceWithLimit(pool, estimator, wallet, 32*1024*1024)
}

func NewServiceWithLimit(pool *pgxpool.Pool, estimator Estimator, wallet Wallet, maxResponseBytes int64) (*Service, error) {
	if pool == nil || estimator == nil || wallet == nil || maxResponseBytes < 1 || maxResponseBytes > 256*1024*1024 {
		return nil, ErrInvalidRequest
	}
	return &Service{pool: pool, estimator: estimator, wallet: wallet, entropy: rand.Reader, maxResponseBytes: maxResponseBytes}, nil
}

func (service *Service) MaximumResponseBytes() int64 { return service.maxResponseBytes }

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
	identity := request.RequestID
	if request.IdempotencyKey != "" {
		identity = "idempotency:" + request.IdempotencyKey
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1 || ':' || $2, 0))`, request.OrganizationID, "image-charge:"+identity); err != nil {
		return Charge{}, err
	}
	existing, found, err := loadForBegin(ctx, tx, request)
	if err != nil {
		return Charge{}, err
	}
	if found {
		if !sameRequest(existing, request) {
			return Charge{}, ErrRequestConflict
		}
		if existing.State == "RESERVED" || existing.State == "RECONCILING" || existing.State == "RESERVING" {
			return Charge{}, ErrRequestPending
		}
		if request.IdempotencyKey == "" {
			return Charge{}, ErrAlreadySettled
		}
		if existing.SnapshotVersion != 1 || !validStoredSnapshot(existing) {
			return Charge{}, ErrSnapshotCorrupt
		}
		existing.Replay = true
		return existing, nil
	}
	estimate, err := service.estimator.EstimateInTx(ctx, tx, pricing.Request{Protocol: request.Protocol, Operation: request.Operation, Model: request.Model, ChannelID: request.ChannelID, Quantity: request.Quantity, Size: request.Size, Quality: request.Quality})
	if err != nil {
		return Charge{}, err
	}
	operationIdentity := request.RequestID
	if request.IdempotencyKey != "" {
		digest := sha256.Sum256([]byte(request.IdempotencyKey))
		operationIdentity = "idem_" + hex.EncodeToString(digest[:])
	}
	reservation, err := service.wallet.ReserveInTx(ctx, tx, request.OrganizationID, request.ProjectID, "image:"+operationIdentity, estimate.MaximumSale, "image-reserve:"+operationIdentity)
	if err != nil {
		return Charge{}, err
	}
	id, err := service.id("charge_")
	if err != nil {
		return Charge{}, err
	}
	charge := Charge{ID: id, RequestID: request.RequestID, OrganizationID: request.OrganizationID, ProjectID: request.ProjectID, Protocol: request.Protocol, Operation: request.Operation, Model: request.Model, ChannelID: estimate.ChannelID, PriceID: estimate.PriceID, Quantity: request.Quantity, Size: request.Size, Quality: request.Quality, Currency: estimate.Currency, EstimatedCost: estimate.EstimatedCost, ReservedSale: estimate.MaximumSale, ReservationID: reservation.Reservation.ID, State: "RESERVED", IdempotencyKey: request.IdempotencyKey, RequestFingerprint: request.RequestFingerprint}
	var storedKey, storedFingerprint any
	if request.IdempotencyKey != "" {
		storedKey = request.IdempotencyKey
		storedFingerprint = request.RequestFingerprint[:]
	}
	_, err = tx.Exec(ctx, `INSERT INTO image_request_charges(id,request_id,organization_id,project_id,protocol,operation,model,channel_id,price_id,quantity,size,quality,currency,estimated_cost,reserved_sale,reservation_id,state,idempotency_key,request_fingerprint)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)`, charge.ID, charge.RequestID, charge.OrganizationID, charge.ProjectID, charge.Protocol, charge.Operation, charge.Model, charge.ChannelID, charge.PriceID, charge.Quantity, charge.Size, charge.Quality, charge.Currency, charge.EstimatedCost, charge.ReservedSale, charge.ReservationID, charge.State, storedKey, storedFingerprint)
	if err != nil {
		return Charge{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Charge{}, err
	}
	return charge, nil
}

func (service *Service) Capture(ctx context.Context, chargeID string) (Charge, error) {
	return service.Complete(ctx, chargeID, true, ResponseSnapshot{Status: 204, Headers: map[string][]string{}, Body: []byte{}})
}

func (service *Service) Release(ctx context.Context, chargeID string) (Charge, error) {
	return service.Complete(ctx, chargeID, false, ResponseSnapshot{Status: 204, Headers: map[string][]string{}, Body: []byte{}})
}

func (service *Service) Complete(ctx context.Context, chargeID string, success bool, snapshot ResponseSnapshot) (Charge, error) {
	if !validID(chargeID, "charge_") {
		return Charge{}, ErrInvalidRequest
	}
	canonical, headersJSON, bodyDigest, err := service.prepareSnapshot(snapshot)
	if err != nil {
		return Charge{}, err
	}
	target := "RELEASED"
	if success {
		target = "CAPTURED"
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
		if charge.SnapshotVersion == 1 && sameSnapshot(charge.Response, canonical) {
			return charge, nil
		}
		return Charge{}, ErrRequestConflict
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
	charge.SnapshotVersion = 1
	charge.Response = canonical
	charge.ResponseSHA256 = bodyDigest
	_, err = tx.Exec(ctx, `UPDATE image_request_charges SET state=$2,actual_cost=$3,captured_sale=$4,response_snapshot_version=1,response_status=$5,response_headers=$6::text::jsonb,response_body=$7,response_body_sha256=$8,response_completed_at=now(),updated_at=now() WHERE id=$1`, charge.ID, charge.State, charge.ActualCost, charge.CapturedSale, canonical.Status, string(headersJSON), canonical.Body, bodyDigest[:])
	if err != nil {
		return Charge{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Charge{}, err
	}
	return charge, nil
}

func (service *Service) MarkReconciling(ctx context.Context, chargeID string, observation Observation) error {
	if !validID(chargeID, "charge_") {
		return ErrInvalidRequest
	}
	var canonical ResponseSnapshot
	var headersJSON []byte
	var bodyDigest [32]byte
	var err error
	if observation.Outcome == KnownSuccess || observation.Outcome == KnownFailure {
		if observation.Reason != ResponseUnavailable && observation.Reason != SettlementFailed {
			return ErrInvalidRequest
		}
		canonical, headersJSON, bodyDigest, err = service.prepareSnapshot(observation.Snapshot)
		if err != nil {
			return err
		}
	} else if observation.Outcome != Unknown || (observation.Reason != ExecutorTimeout && observation.Reason != ExecutorConnection && observation.Reason != ProviderPanic) {
		return ErrInvalidRequest
	}
	tx, err := service.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	charge, found, err := loadByID(ctx, tx, chargeID, true)
	if err != nil {
		return err
	}
	if !found {
		return ErrInvalidState
	}
	if charge.State == "CAPTURED" || charge.State == "RELEASED" {
		return nil
	}
	if charge.State != "RESERVED" && charge.State != "RECONCILING" {
		return ErrInvalidState
	}
	if _, err := tx.Exec(ctx, `UPDATE image_request_charges SET state='RECONCILING',updated_at=now() WHERE id=$1`, chargeID); err != nil {
		return err
	}
	var status, headers, body, digest any
	if observation.Outcome != Unknown {
		status = canonical.Status
		headers = string(headersJSON)
		body = canonical.Body
		digest = bodyDigest[:]
	}
	result, err := tx.Exec(ctx, `INSERT INTO image_charge_reconciliations(charge_id,outcome,reason,response_status,response_headers,response_body,response_body_sha256)
		VALUES($1,$2,$3,$4,$5::text::jsonb,$6,$7) ON CONFLICT(charge_id) DO NOTHING`, chargeID, observation.Outcome, observation.Reason, status, headers, body, digest)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		matching, err := reconciliationMatches(ctx, tx, chargeID, observation, canonical, bodyDigest)
		if err != nil {
			return err
		}
		if !matching {
			return ErrRequestConflict
		}
	}
	return tx.Commit(ctx)
}

func reconciliationMatches(ctx context.Context, tx pgx.Tx, chargeID string, observation Observation, canonical ResponseSnapshot, digest [32]byte) (bool, error) {
	var outcome Outcome
	var reason Reason
	var status *int
	var headersJSON, body, storedDigest []byte
	err := tx.QueryRow(ctx, `SELECT outcome,reason,response_status,response_headers,response_body,response_body_sha256 FROM image_charge_reconciliations WHERE charge_id=$1`, chargeID).Scan(&outcome, &reason, &status, &headersJSON, &body, &storedDigest)
	if err != nil {
		return false, err
	}
	if outcome != observation.Outcome || reason != observation.Reason {
		return false, nil
	}
	if outcome == Unknown {
		return true, nil
	}
	var headers map[string][]string
	if err := json.Unmarshal(headersJSON, &headers); err != nil || status == nil {
		return false, ErrSnapshotCorrupt
	}
	return *status == canonical.Status && bytes.Equal(body, canonical.Body) && bytes.Equal(storedDigest, digest[:]) && reflect.DeepEqual(headers, canonical.Headers), nil
}

func loadByRequest(ctx context.Context, tx pgx.Tx, organizationID, requestID string, lock bool) (Charge, bool, error) {
	query := chargeSelect + ` WHERE organization_id=$1 AND request_id=$2`
	if lock {
		query += ` FOR UPDATE`
	}
	return scanCharge(tx.QueryRow(ctx, query, organizationID, requestID))
}

func loadByIdempotencyKey(ctx context.Context, tx pgx.Tx, organizationID, key string) (Charge, bool, error) {
	return scanCharge(tx.QueryRow(ctx, chargeSelect+` WHERE organization_id=$1 AND idempotency_key=$2`, organizationID, key))
}

func loadForBegin(ctx context.Context, tx pgx.Tx, request BeginRequest) (Charge, bool, error) {
	if request.IdempotencyKey != "" {
		return loadByIdempotencyKey(ctx, tx, request.OrganizationID, request.IdempotencyKey)
	}
	return loadByRequest(ctx, tx, request.OrganizationID, request.RequestID, false)
}

func loadByID(ctx context.Context, tx pgx.Tx, id string, lock bool) (Charge, bool, error) {
	query := chargeSelect + ` WHERE id=$1`
	if lock {
		query += ` FOR UPDATE`
	}
	return scanCharge(tx.QueryRow(ctx, query, id))
}

const chargeSelect = `SELECT id,request_id,organization_id,project_id,protocol,operation,model,channel_id,price_id,quantity,size,quality,currency,estimated_cost,reserved_sale,actual_cost,captured_sale,reservation_id,state,idempotency_key,request_fingerprint,response_snapshot_version,response_status,response_headers,response_body,response_body_sha256 FROM image_request_charges`

func scanCharge(row pgx.Row) (Charge, bool, error) {
	var charge Charge
	var key *string
	var fingerprint, headersJSON, body, bodySHA []byte
	var responseStatus *int
	err := row.Scan(&charge.ID, &charge.RequestID, &charge.OrganizationID, &charge.ProjectID, &charge.Protocol, &charge.Operation, &charge.Model, &charge.ChannelID, &charge.PriceID, &charge.Quantity, &charge.Size, &charge.Quality, &charge.Currency, &charge.EstimatedCost, &charge.ReservedSale, &charge.ActualCost, &charge.CapturedSale, &charge.ReservationID, &charge.State, &key, &fingerprint, &charge.SnapshotVersion, &responseStatus, &headersJSON, &body, &bodySHA)
	if errors.Is(err, pgx.ErrNoRows) {
		return Charge{}, false, nil
	}
	if err == nil {
		if key != nil {
			charge.IdempotencyKey = *key
		}
		copy(charge.RequestFingerprint[:], fingerprint)
		if charge.SnapshotVersion == 1 && responseStatus != nil {
			charge.Response.Status = *responseStatus
			charge.Response.Body = append([]byte(nil), body...)
			if unmarshalErr := json.Unmarshal(headersJSON, &charge.Response.Headers); unmarshalErr != nil {
				return Charge{}, false, ErrSnapshotCorrupt
			}
			copy(charge.ResponseSHA256[:], bodySHA)
		}
	}
	return charge, err == nil, err
}

func sameRequest(charge Charge, request BeginRequest) bool {
	requestIdentityMatches := charge.RequestID == request.RequestID
	if request.IdempotencyKey != "" {
		requestIdentityMatches = charge.IdempotencyKey == request.IdempotencyKey && bytes.Equal(charge.RequestFingerprint[:], request.RequestFingerprint[:])
	}
	return requestIdentityMatches && charge.OrganizationID == request.OrganizationID && charge.ProjectID == request.ProjectID && charge.Protocol == request.Protocol && charge.Operation == request.Operation && charge.Model == request.Model && charge.ChannelID == request.ChannelID && charge.Quantity == request.Quantity && charge.Size == request.Size && charge.Quality == request.Quality
}

func validBeginRequest(request BeginRequest) bool {
	hasFingerprint := request.RequestFingerprint != ([32]byte{})
	validIdempotency := (request.IdempotencyKey == "" && !hasFingerprint) || (idempotency.Valid(request.IdempotencyKey) && hasFingerprint)
	return validIdempotency && validPrefixed(request.OrganizationID, "org_", 200) && validPrefixed(request.ProjectID, "project_", 200) && validText(request.RequestID, 128) && request.Protocol == "openai" && (request.Operation == "image.generate" || request.Operation == "image.edit") && validText(request.Model, 200) && validID(request.ChannelID, "channel_") && request.Quantity >= 1 && request.Quantity <= 10 && validText(request.Size, 80) && validText(request.Quality, 80)
}

func (service *Service) prepareSnapshot(snapshot ResponseSnapshot) (ResponseSnapshot, []byte, [32]byte, error) {
	if snapshot.Status < 100 || snapshot.Status > 599 {
		return ResponseSnapshot{}, nil, [32]byte{}, ErrInvalidRequest
	}
	if int64(len(snapshot.Body)) > service.maxResponseBytes {
		return ResponseSnapshot{}, nil, [32]byte{}, ErrResponseTooLarge
	}
	body := make([]byte, len(snapshot.Body))
	copy(body, snapshot.Body)
	canonical := ResponseSnapshot{Status: snapshot.Status, Headers: map[string][]string{}, Body: body}
	for key, values := range snapshot.Headers {
		canonicalKey := httpCanonicalHeader(key)
		if canonicalKey == "" {
			continue
		}
		for _, value := range values {
			if !validHeaderValue(value) {
				return ResponseSnapshot{}, nil, [32]byte{}, ErrInvalidRequest
			}
			canonical.Headers[canonicalKey] = append(canonical.Headers[canonicalKey], value)
		}
	}
	headersJSON, err := json.Marshal(canonical.Headers)
	if err != nil {
		return ResponseSnapshot{}, nil, [32]byte{}, ErrInvalidRequest
	}
	return canonical, headersJSON, sha256.Sum256(canonical.Body), nil
}

func httpCanonicalHeader(value string) string {
	switch strings.ToLower(value) {
	case "content-type":
		return "Content-Type"
	case "retry-after":
		return "Retry-After"
	default:
		return ""
	}
}

func validHeaderValue(value string) bool {
	if len(value) > 4096 {
		return false
	}
	for _, character := range value {
		if character == '\r' || character == '\n' || character == 0 {
			return false
		}
	}
	return true
}

func validStoredSnapshot(charge Charge) bool {
	return charge.SnapshotVersion == 1 && charge.Response.Status >= 100 && charge.Response.Status <= 599 && sha256.Sum256(charge.Response.Body) == charge.ResponseSHA256
}

func sameSnapshot(left, right ResponseSnapshot) bool {
	return left.Status == right.Status && bytes.Equal(left.Body, right.Body) && reflect.DeepEqual(left.Headers, right.Headers)
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
