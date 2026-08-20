// Package replicate implements the Replicate-native Prediction facade.
package replicate

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nativegatewayhq/gateway/internal/apikey"
	"github.com/nativegatewayhq/gateway/internal/billing"
	"github.com/nativegatewayhq/gateway/internal/idempotency"
	"github.com/nativegatewayhq/gateway/internal/jobs"
	"github.com/nativegatewayhq/gateway/internal/networkauth"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	"github.com/nativegatewayhq/gateway/internal/ratelimit"
	"github.com/nativegatewayhq/gateway/internal/requestid"
	imageoperation "github.com/nativegatewayhq/gateway/operations/image"
	joboperation "github.com/nativegatewayhq/gateway/operations/job"
	providerreplicate "github.com/nativegatewayhq/gateway/providers/replicate"
)

type Authenticator interface {
	Authenticate(context.Context, string) (apikey.Principal, error)
}
type ModelRegistry interface {
	Candidates(string, string, imageoperation.Operation, imageoperation.MediaType) ([]imageoperation.RoutingDecision, error)
}
type JobService interface {
	Submit(context.Context, jobs.CreateRequest, any) (joboperation.Job, error)
	Get(context.Context, joboperation.Owner, string) (joboperation.Job, error)
	Cancel(context.Context, joboperation.Owner, string) (joboperation.Job, error)
}
type Billing interface {
	Begin(context.Context, billing.BeginRequest) (billing.Charge, error)
}
type Availability interface {
	ConfiguredChannel(context.Context, string, providercredentials.ProviderID) bool
}

type Handler struct {
	logger           *slog.Logger
	authenticator    Authenticator
	models           ModelRegistry
	jobs             JobService
	billing          Billing
	availability     Availability
	maximumBodyBytes int64
	publicBase       string
}

func NewHandler(logger *slog.Logger, authenticator Authenticator, models ModelRegistry, service JobService, chargeBilling Billing, availability Availability, maximumBodyBytes int64, publicBase string) *Handler {
	return &Handler{logger: logger, authenticator: authenticator, models: models, jobs: service, billing: chargeBilling, availability: availability, maximumBodyBytes: maximumBodyBytes, publicBase: strings.TrimSuffix(publicBase, "/")}
}

func (handler *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if handler == nil || handler.authenticator == nil || handler.models == nil || handler.jobs == nil || handler.maximumBodyBytes < 1 {
		writeDetail(writer, http.StatusServiceUnavailable, "service unavailable")
		return
	}
	principal, ok := handler.authenticate(writer, request)
	if !ok {
		return
	}
	path := strings.TrimPrefix(request.URL.Path, "/v1/predictions")
	switch {
	case path == "" || path == "/":
		if request.Method != http.MethodPost {
			methodNotAllowed(writer, http.MethodPost)
			return
		}
		handler.create(writer, request, principal)
	case strings.HasSuffix(path, "/cancel"):
		if request.Method != http.MethodPost {
			methodNotAllowed(writer, http.MethodPost)
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/"), "/cancel")
		handler.cancel(writer, request, principal, id)
	default:
		if request.Method != http.MethodGet {
			methodNotAllowed(writer, http.MethodGet)
			return
		}
		id := strings.TrimPrefix(path, "/")
		if strings.Contains(id, "/") {
			writeDetail(writer, http.StatusNotFound, "Not found")
			return
		}
		handler.get(writer, request, principal, id)
	}
}

func (handler *Handler) create(writer http.ResponseWriter, request *http.Request, principal apikey.Principal) {
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeDetail(writer, http.StatusBadRequest, "Content-Type must be application/json")
		return
	}
	if request.ContentLength > handler.maximumBodyBytes {
		writeDetail(writer, http.StatusRequestEntityTooLarge, "Request body is too large")
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, handler.maximumBodyBytes+1))
	if err != nil || int64(len(body)) > handler.maximumBodyBytes {
		writeDetail(writer, http.StatusRequestEntityTooLarge, "Request body is too large")
		return
	}
	version, err := validateCreate(body)
	if err != nil {
		writeDetail(writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if !principal.AuthorizeModel("replicate", "image.generate", version) {
		writeDetail(writer, http.StatusForbidden, "You are not permitted to use this model")
		return
	}
	candidates, err := handler.models.Candidates("replicate", version, imageoperation.Generate, imageoperation.JSON)
	if err != nil || len(candidates) == 0 {
		writeDetail(writer, http.StatusUnprocessableEntity, "The specified version is not available")
		return
	}
	route := candidates[0]
	if route.Provider != providercredentials.Replicate || (handler.availability != nil && !handler.availability.ConfiguredChannel(request.Context(), route.ChannelID, route.Provider)) {
		writeDetail(writer, http.StatusServiceUnavailable, "Prediction provider is unavailable")
		return
	}
	key := strings.TrimSpace(request.Header.Get("Idempotency-Key"))
	if key != "" && !idempotency.Valid(key) {
		writeDetail(writer, http.StatusBadRequest, "Invalid Idempotency-Key")
		return
	}
	prefer, wait, err := parsePrefer(request.Header.Get("Prefer"))
	if err != nil {
		writeDetail(writer, http.StatusBadRequest, "Invalid Prefer header")
		return
	}
	cancelAfter, err := parseCancelAfter(request.Header.Get("Cancel-After"))
	if err != nil {
		writeDetail(writer, http.StatusBadRequest, "Invalid Cancel-After header")
		return
	}
	var fingerprint [32]byte
	if key != "" {
		fingerprint = sha256.Sum256(body)
	}
	chargeID := ""
	if handler.billing != nil {
		charge, err := handler.billing.Begin(request.Context(), billing.BeginRequest{RequestID: requestid.FromContext(request.Context()), OrganizationID: principal.OrganizationID, ProjectID: principal.ProjectID, APIKeyID: principal.APIKeyID, Protocol: "replicate", Operation: "image.generate", Model: version, ChannelID: route.ChannelID, Quantity: 1, Size: "default", Quality: "default", IdempotencyKey: key, RequestFingerprint: fingerprint})
		if err != nil {
			handler.billingError(writer, err)
			return
		}
		chargeID = charge.ID
	}
	owner := joboperation.Owner{OrganizationID: principal.OrganizationID, ProjectID: principal.ProjectID, APIKeyID: principal.APIKeyID}
	created, err := handler.jobs.Submit(request.Context(), jobs.CreateRequest{RequestID: requestid.FromContext(request.Context()), Owner: owner, Protocol: "replicate", Operation: "image.generate", Model: version, Provider: string(route.Provider), ChannelID: route.ChannelID, ChargeID: chargeID, IdempotencyKey: key, Fingerprint: fingerprint}, providerreplicate.SubmitPayload{Body: body, Prefer: prefer, CancelAfter: cancelAfter})
	if err != nil {
		writeDetail(writer, http.StatusServiceUnavailable, "Prediction could not be submitted")
		return
	}
	if wait > 0 && !created.Status.Terminal() {
		created = handler.wait(request.Context(), owner, created.ID, wait)
	}
	handler.writeJob(writer, created, true)
}

func (handler *Handler) get(writer http.ResponseWriter, request *http.Request, principal apikey.Principal, id string) {
	owner := joboperation.Owner{OrganizationID: principal.OrganizationID, ProjectID: principal.ProjectID, APIKeyID: principal.APIKeyID}
	value, err := handler.jobs.Get(request.Context(), owner, id)
	if errors.Is(err, joboperation.ErrNotFound) || errors.Is(err, joboperation.ErrInvalid) {
		writeDetail(writer, http.StatusNotFound, "Prediction not found")
		return
	}
	if err != nil {
		writeDetail(writer, http.StatusServiceUnavailable, "Prediction is unavailable")
		return
	}
	if !principal.AuthorizeModel("replicate", "image.generate", value.Model) {
		writeDetail(writer, http.StatusNotFound, "Prediction not found")
		return
	}
	handler.writeJob(writer, value, false)
}
func (handler *Handler) cancel(writer http.ResponseWriter, request *http.Request, principal apikey.Principal, id string) {
	owner := joboperation.Owner{OrganizationID: principal.OrganizationID, ProjectID: principal.ProjectID, APIKeyID: principal.APIKeyID}
	value, err := handler.jobs.Get(request.Context(), owner, id)
	if errors.Is(err, joboperation.ErrNotFound) || errors.Is(err, joboperation.ErrInvalid) {
		writeDetail(writer, http.StatusNotFound, "Prediction not found")
		return
	}
	if err != nil {
		writeDetail(writer, http.StatusServiceUnavailable, "Prediction is unavailable")
		return
	}
	if !principal.AuthorizeModel("replicate", "image.generate", value.Model) {
		writeDetail(writer, http.StatusNotFound, "Prediction not found")
		return
	}
	value, err = handler.jobs.Cancel(request.Context(), owner, id)
	if err != nil {
		writeDetail(writer, http.StatusServiceUnavailable, "Prediction cancellation is uncertain")
		return
	}
	handler.writeJob(writer, value, false)
}

func (handler *Handler) writeJob(writer http.ResponseWriter, value joboperation.Job, create bool) {
	status := http.StatusOK
	if create {
		status = http.StatusCreated
	}
	if len(value.Snapshot.Body) > 0 && json.Valid(value.Snapshot.Body) {
		if create && value.Snapshot.Status >= 400 {
			status = value.Snapshot.Status
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(status)
		_, _ = writer.Write(value.Snapshot.Body)
		return
	}
	nativeStatus := "starting"
	switch value.Status {
	case joboperation.Processing, joboperation.Reconciling:
		nativeStatus = "processing"
	case joboperation.Succeeded:
		nativeStatus = "succeeded"
	case joboperation.Failed:
		nativeStatus = "failed"
	case joboperation.Canceled:
		nativeStatus = "canceled"
	}
	base := handler.publicBase
	response := map[string]any{"id": value.ID, "status": nativeStatus, "urls": map[string]string{"get": base + "/v1/predictions/" + value.ID, "cancel": base + "/v1/predictions/" + value.ID + "/cancel"}}
	writeJSON(writer, status, response)
}
func (handler *Handler) wait(ctx context.Context, owner joboperation.Owner, id string, duration time.Duration) joboperation.Job {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	current, _ := handler.jobs.Get(ctx, owner, id)
	for !current.Status.Terminal() {
		select {
		case <-ctx.Done():
			return current
		case <-timer.C:
			return current
		case <-ticker.C:
			next, err := handler.jobs.Get(ctx, owner, id)
			if err != nil {
				return current
			}
			current = next
		}
	}
	return current
}

func validateCreate(body []byte) (string, error) {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(body, &envelope) != nil {
		return "", errors.New("Invalid JSON")
	}
	for _, field := range []string{"webhook", "webhook_events_filter"} {
		if _, exists := envelope[field]; exists {
			return "", errors.New("Webhooks are not supported")
		}
	}
	var version string
	if json.Unmarshal(envelope["version"], &version) != nil || version == "" || len(version) > 200 || strings.TrimSpace(version) != version {
		return "", errors.New("version is required")
	}
	var input map[string]json.RawMessage
	if json.Unmarshal(envelope["input"], &input) != nil {
		return "", errors.New("input must be an object")
	}
	return version, nil
}
func parsePrefer(value string) (string, time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "respond-async" {
		return value, 0, nil
	}
	if value == "wait" {
		return value, 60 * time.Second, nil
	}
	if !strings.HasPrefix(value, "wait=") {
		return "", 0, errors.New("invalid")
	}
	seconds, err := strconv.Atoi(strings.TrimPrefix(value, "wait="))
	if err != nil || seconds < 1 || seconds > 60 {
		return "", 0, errors.New("invalid")
	}
	return value, time.Duration(seconds) * time.Second, nil
}
func parseCancelAfter(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds < 5 || seconds > 86400 {
			return "", errors.New("invalid")
		}
		return value, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration < 5*time.Second || duration > 24*time.Hour {
		return "", errors.New("invalid")
	}
	return value, nil
}

func (handler *Handler) authenticate(writer http.ResponseWriter, request *http.Request) (apikey.Principal, bool) {
	raw, err := apikey.Extract(request)
	if err != nil {
		writeDetail(writer, http.StatusUnauthorized, "Unauthenticated")
		return apikey.Principal{}, false
	}
	principal, err := handler.authenticator.Authenticate(request.Context(), raw)
	if err == nil {
		return principal, true
	}
	var denied *networkauth.DeniedError
	if errors.As(err, &denied) {
		writeDetail(writer, http.StatusForbidden, "This API key is not permitted from this network")
		return apikey.Principal{}, false
	}
	var limited *ratelimit.LimitError
	if errors.As(err, &limited) {
		writer.Header().Set("Retry-After", strconv.Itoa(max(1, int(limited.Decision.RetryAfter/time.Second))))
		writeDetail(writer, http.StatusTooManyRequests, "Request was throttled")
		return apikey.Principal{}, false
	}
	if errors.Is(err, ratelimit.ErrUnavailable) || errors.Is(err, apikey.ErrUnavailable) {
		writeDetail(writer, http.StatusServiceUnavailable, "Authentication is unavailable")
		return apikey.Principal{}, false
	}
	writeDetail(writer, http.StatusUnauthorized, "Unauthenticated")
	return apikey.Principal{}, false
}
func (handler *Handler) billingError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, billing.ErrRequestConflict):
		writeDetail(writer, http.StatusConflict, "Idempotency-Key conflicts with an existing prediction")
	case errors.Is(err, billing.ErrRequestPending):
		writeDetail(writer, http.StatusConflict, "Prediction is already pending")
	default:
		writeDetail(writer, http.StatusPaymentRequired, "Prediction billing is unavailable")
	}
}
func methodNotAllowed(writer http.ResponseWriter, method string) {
	writer.Header().Set("Allow", method)
	writeDetail(writer, http.StatusMethodNotAllowed, "Method not allowed")
}
func writeDetail(writer http.ResponseWriter, status int, detail string) {
	writeJSON(writer, status, map[string]string{"detail": detail})
}
func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
