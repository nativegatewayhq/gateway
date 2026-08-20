// Package fal implements the fal-native model-scoped Queue facade.
package fal

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
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
	providerfal "github.com/nativegatewayhq/gateway/providers/fal"
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

type route struct{ model, id, action string }

func (handler *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if handler == nil || handler.authenticator == nil || handler.models == nil || handler.jobs == nil || handler.maximumBodyBytes < 1 {
		writeError(writer, http.StatusServiceUnavailable, "Queue service unavailable")
		return
	}
	if request.URL.Path == "/fal/proxy" {
		if !translateSDKProxy(request) {
			writeError(writer, http.StatusBadRequest, "Invalid fal target")
			return
		}
	}
	parsed, ok := parseRoute(request)
	if !ok {
		writeError(writer, http.StatusNotFound, "Not found")
		return
	}
	principal, ok := handler.authenticate(writer, request)
	if !ok {
		return
	}
	switch parsed.action {
	case "submit":
		if request.Method != http.MethodPost {
			methodNotAllowed(writer, http.MethodPost)
			return
		}
		handler.submit(writer, request, principal, parsed.model)
	case "status":
		if request.Method != http.MethodGet {
			methodNotAllowed(writer, http.MethodGet)
			return
		}
		handler.status(writer, request, principal, parsed)
	case "result":
		if request.Method != http.MethodGet {
			methodNotAllowed(writer, http.MethodGet)
			return
		}
		handler.result(writer, request, principal, parsed)
	case "cancel":
		if request.Method != http.MethodPut {
			methodNotAllowed(writer, http.MethodPut)
			return
		}
		handler.cancel(writer, request, principal, parsed)
	}
}

func (handler *Handler) submit(writer http.ResponseWriter, request *http.Request, principal apikey.Principal, model string) {
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" || request.URL.RawQuery != "" {
		writeError(writer, http.StatusBadRequest, "Invalid queue request")
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, handler.maximumBodyBytes+1))
	if err != nil || len(body) == 0 || int64(len(body)) > handler.maximumBodyBytes || !validBody(body) {
		writeError(writer, http.StatusUnprocessableEntity, "Invalid input")
		return
	}
	if !principal.AuthorizeModel("fal", "image.generate", model) {
		writeError(writer, http.StatusForbidden, "Model is not allowed")
		return
	}
	candidates, err := handler.models.Candidates("fal", model, imageoperation.Generate, imageoperation.JSON)
	if err != nil || len(candidates) == 0 {
		writeError(writer, http.StatusNotFound, "Model not found")
		return
	}
	selected := candidates[0]
	usage, err := requestedOutputUsage(body, selected.Usage)
	if err != nil {
		writeError(writer, http.StatusUnprocessableEntity, "num_images is not supported for this model")
		return
	}
	if selected.Provider != providercredentials.Fal || (handler.availability != nil && !handler.availability.ConfiguredChannel(request.Context(), selected.ChannelID, selected.Provider)) {
		writeError(writer, http.StatusServiceUnavailable, "Queue provider unavailable")
		return
	}
	key := strings.TrimSpace(request.Header.Get("Idempotency-Key"))
	if key != "" && !idempotency.Valid(key) {
		writeError(writer, http.StatusBadRequest, "Invalid Idempotency-Key")
		return
	}
	var fingerprint [32]byte
	if key != "" {
		fingerprint = sha256.Sum256(append([]byte(model+"\x00"), body...))
	}
	chargeID := ""
	if handler.billing != nil {
		charge, beginErr := handler.billing.Begin(request.Context(), billing.BeginRequest{RequestID: requestid.FromContext(request.Context()), OrganizationID: principal.OrganizationID, ProjectID: principal.ProjectID, APIKeyID: principal.APIKeyID, Protocol: "fal", Operation: "image.generate", Model: model, ChannelID: selected.ChannelID, Quantity: usage.Quantity, Size: "default", Quality: "default", IdempotencyKey: key, RequestFingerprint: fingerprint})
		if beginErr != nil {
			handler.billingError(writer, beginErr)
			return
		}
		chargeID = charge.ID
	}
	owner := joboperation.Owner{OrganizationID: principal.OrganizationID, ProjectID: principal.ProjectID, APIKeyID: principal.APIKeyID}
	value, err := handler.jobs.Submit(request.Context(), jobs.CreateRequest{RequestID: requestid.FromContext(request.Context()), Owner: owner, Protocol: "fal", Operation: "image.generate", Model: model, Provider: string(selected.Provider), ChannelID: selected.ChannelID, ChargeID: chargeID, IdempotencyKey: key, Fingerprint: fingerprint, EstimatedUsage: usage}, providerfal.SubmitPayload{Body: body})
	if err != nil {
		writeError(writer, http.StatusServiceUnavailable, "Queue request could not be submitted")
		return
	}
	handler.writeSubmit(writer, value)
}

func (handler *Handler) status(writer http.ResponseWriter, request *http.Request, principal apikey.Principal, parsed route) {
	if !validStatusQuery(request.URL.Query()) {
		writeError(writer, http.StatusBadRequest, "Invalid status query")
		return
	}
	value, ok := handler.ownedJob(writer, request, principal, parsed)
	if !ok {
		return
	}
	writer.Header().Set("X-Fal-Request-Id", value.ID)
	if !value.Status.Terminal() && len(value.Snapshot.Body) > 0 && json.Valid(value.Snapshot.Body) {
		writeJSONBytes(writer, http.StatusOK, value.Snapshot.Body)
		return
	}
	response := map[string]any{"status": "IN_QUEUE", "request_id": value.ID, "queue_position": 0}
	switch value.Status {
	case joboperation.Processing, joboperation.Reconciling:
		response = map[string]any{"status": "IN_PROGRESS", "request_id": value.ID, "logs": nil}
	case joboperation.Succeeded:
		response = map[string]any{"status": "COMPLETED", "request_id": value.ID, "logs": nil, "metrics": map[string]any{}}
	case joboperation.Failed:
		response = map[string]any{"status": "COMPLETED", "request_id": value.ID, "logs": nil, "metrics": map[string]any{}, "error": "Queue request failed", "error_type": value.FailureCategory}
	case joboperation.Canceled:
		response = map[string]any{"status": "COMPLETED", "request_id": value.ID, "logs": nil, "metrics": map[string]any{}, "error": "Queue request canceled", "error_type": "canceled"}
	}
	writeJSON(writer, http.StatusOK, response)
}

func (handler *Handler) result(writer http.ResponseWriter, request *http.Request, principal apikey.Principal, parsed route) {
	if request.URL.RawQuery != "" {
		writeError(writer, http.StatusBadRequest, "Invalid result query")
		return
	}
	value, ok := handler.ownedJob(writer, request, principal, parsed)
	if !ok {
		return
	}
	writer.Header().Set("X-Fal-Request-Id", value.ID)
	if value.Status != joboperation.Succeeded || len(value.Snapshot.Body) == 0 || !json.Valid(value.Snapshot.Body) {
		writeError(writer, http.StatusConflict, "Request is not completed")
		return
	}
	writeJSONBytes(writer, http.StatusOK, value.Snapshot.Body)
}

func (handler *Handler) cancel(writer http.ResponseWriter, request *http.Request, principal apikey.Principal, parsed route) {
	if request.URL.RawQuery != "" {
		writeError(writer, http.StatusBadRequest, "Invalid cancel query")
		return
	}
	current, ok := handler.ownedJob(writer, request, principal, parsed)
	if !ok {
		return
	}
	writer.Header().Set("X-Fal-Request-Id", current.ID)
	owner := current.Owner
	value, err := handler.jobs.Cancel(request.Context(), owner, current.ID)
	if err != nil {
		writeError(writer, http.StatusServiceUnavailable, "Cancellation is uncertain")
		return
	}
	status := "CANCELING"
	if value.Status == joboperation.Canceled {
		status = "CANCELED"
	}
	writeJSON(writer, http.StatusOK, map[string]any{"request_id": value.ID, "status": status})
}

func (handler *Handler) ownedJob(writer http.ResponseWriter, request *http.Request, principal apikey.Principal, parsed route) (joboperation.Job, bool) {
	owner := joboperation.Owner{OrganizationID: principal.OrganizationID, ProjectID: principal.ProjectID, APIKeyID: principal.APIKeyID}
	value, err := handler.jobs.Get(request.Context(), owner, parsed.id)
	if err != nil || value.Protocol != "fal" || value.Model != parsed.model || !principal.AuthorizeModel("fal", "image.generate", value.Model) {
		writeError(writer, http.StatusNotFound, "Request not found")
		return joboperation.Job{}, false
	}
	return value, true
}

func (handler *Handler) writeSubmit(writer http.ResponseWriter, value joboperation.Job) {
	writer.Header().Set("X-Fal-Request-Id", value.ID)
	prefix := handler.publicBase + "/" + value.Model + "/requests/" + value.ID
	writeJSON(writer, http.StatusOK, map[string]any{"request_id": value.ID, "status_url": prefix + "/status", "response_url": prefix, "cancel_url": prefix + "/cancel"})
}

func (handler *Handler) authenticate(writer http.ResponseWriter, request *http.Request) (apikey.Principal, bool) {
	raw, err := apikey.Extract(request)
	if err != nil {
		writeError(writer, http.StatusUnauthorized, "Invalid credentials")
		return apikey.Principal{}, false
	}
	principal, err := handler.authenticator.Authenticate(request.Context(), raw)
	if err == nil {
		return principal, true
	}
	var denied *networkauth.DeniedError
	var limited *ratelimit.LimitError
	switch {
	case errors.As(err, &denied):
		writeError(writer, http.StatusForbidden, "Source is not allowed")
	case errors.As(err, &limited):
		writer.Header().Set("Retry-After", strconv.Itoa(max(1, int(limited.Decision.RetryAfter/time.Second))))
		writeError(writer, http.StatusTooManyRequests, "Rate limit exceeded")
	case errors.Is(err, ratelimit.ErrUnavailable) || errors.Is(err, apikey.ErrUnavailable):
		writeError(writer, http.StatusServiceUnavailable, "Authentication unavailable")
	default:
		writeError(writer, http.StatusUnauthorized, "Invalid credentials")
	}
	return apikey.Principal{}, false
}

func (handler *Handler) billingError(writer http.ResponseWriter, err error) {
	status, detail := http.StatusServiceUnavailable, "Billing unavailable"
	if errors.Is(err, billing.ErrRequestConflict) {
		status, detail = http.StatusConflict, "Idempotency conflict"
	}
	writeError(writer, status, detail)
}

func parseRoute(request *http.Request) (route, bool) {
	escaped := strings.ToLower(request.URL.EscapedPath())
	if strings.Contains(escaped, "%2f") || strings.Contains(escaped, "%5c") || strings.Contains(request.URL.Path, "//") || !strings.HasPrefix(request.URL.Path, "/") {
		return route{}, false
	}
	path := strings.TrimPrefix(request.URL.Path, "/")
	marker := "/requests/"
	index := strings.LastIndex(path, marker)
	if index < 0 {
		if validModel(path) {
			return route{model: path, action: "submit"}, true
		}
		return route{}, false
	}
	model, suffix := path[:index], path[index+len(marker):]
	if !validModel(model) {
		return route{}, false
	}
	parts := strings.Split(suffix, "/")
	if len(parts) < 1 || !joboperation.ValidID(parts[0]) {
		return route{}, false
	}
	if len(parts) == 1 {
		return route{model: model, id: parts[0], action: "result"}, true
	}
	if len(parts) == 2 && (parts[1] == "status" || parts[1] == "cancel") {
		return route{model: model, id: parts[0], action: parts[1]}, true
	}
	return route{}, false
}

func validModel(model string) bool {
	parts := strings.Split(model, "/")
	if len(parts) < 2 || len(model) > 200 {
		return false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return false
		}
		for _, character := range part {
			if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || strings.ContainsRune("._-", character) {
				continue
			}
			return false
		}
	}
	return true
}

func validBody(body []byte) bool {
	var value map[string]json.RawMessage
	if json.Unmarshal(body, &value) != nil || value == nil {
		return false
	}
	for key := range value {
		if strings.Contains(strings.ToLower(key), "webhook") {
			return false
		}
	}
	return true
}

func requestedOutputUsage(body []byte, capability imageoperation.UsageCapability) (*joboperation.Usage, error) {
	if capability.Dimension != "output" || capability.Unit != "image" || capability.RequestExtractor != "fal-input-num_images-v1" || capability.DefaultQuantity < 1 || capability.MaximumQuantity < capability.DefaultQuantity {
		return nil, errors.New("usage capability unavailable")
	}
	var value map[string]json.RawMessage
	if json.Unmarshal(body, &value) != nil || value == nil {
		return nil, errors.New("invalid input")
	}
	quantity := capability.DefaultQuantity
	if raw, exists := value["num_images"]; exists {
		if json.Unmarshal(raw, &quantity) != nil || quantity < 1 || quantity > capability.MaximumQuantity {
			return nil, errors.New("invalid output quantity")
		}
	}
	return &joboperation.Usage{Dimension: capability.Dimension, Unit: capability.Unit, Quantity: quantity, Provenance: "request", ExtractorVersion: capability.RequestExtractor, ResultExtractorVersion: capability.ResultExtractor}, nil
}

func validStatusQuery(values map[string][]string) bool {
	for key, entries := range values {
		if key != "logs" || len(entries) != 1 || (entries[0] != "true" && entries[0] != "false" && entries[0] != "1" && entries[0] != "0") {
			return false
		}
	}
	return true
}

func translateSDKProxy(request *http.Request) bool {
	values := request.Header.Values("x-fal-target-url")
	if len(values) != 1 {
		return false
	}
	target, err := url.Parse(values[0])
	if err != nil || target.Scheme != "https" || !strings.EqualFold(target.Host, "queue.fal.run") || target.User != nil || target.Fragment != "" || target.Path == "" {
		return false
	}
	request.Header.Del("x-fal-target-url")
	request.URL.Path = target.Path
	request.URL.RawPath = target.RawPath
	request.URL.RawQuery = target.RawQuery
	return true
}

func methodNotAllowed(writer http.ResponseWriter, allowed string) {
	writer.Header().Set("Allow", allowed)
	writeError(writer, http.StatusMethodNotAllowed, "Method not allowed")
}

func writeError(writer http.ResponseWriter, status int, detail string) {
	writeJSON(writer, status, map[string]any{"detail": detail})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeJSONBytes(writer http.ResponseWriter, status int, body []byte) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_, _ = writer.Write(body)
}
