// Package runway implements the Runway-native video task facade.
package runway

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/nativegatewayhq/gateway/internal/apikey"
	"github.com/nativegatewayhq/gateway/internal/billing"
	"github.com/nativegatewayhq/gateway/internal/idempotency"
	"github.com/nativegatewayhq/gateway/internal/jobs"
	"github.com/nativegatewayhq/gateway/internal/pricing"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	"github.com/nativegatewayhq/gateway/internal/requestid"
	joboperation "github.com/nativegatewayhq/gateway/operations/job"
	videooperation "github.com/nativegatewayhq/gateway/operations/video"
	providerrunway "github.com/nativegatewayhq/gateway/providers/runway"
)

type Authenticator interface {
	Authenticate(context.Context, string) (apikey.Principal, error)
}
type JobService interface {
	Submit(context.Context, jobs.CreateRequest, any) (joboperation.Job, error)
	Get(context.Context, joboperation.Owner, string) (joboperation.Job, error)
	Cancel(context.Context, joboperation.Owner, string) (joboperation.Job, error)
}
type ModelRegistry interface {
	Resolve(string) (videooperation.Route, error)
}
type Billing interface {
	Begin(context.Context, billing.BeginRequest) (billing.Charge, error)
}
type Uploader interface {
	CreateEphemeralUpload(context.Context, string, []byte) (providerrunway.UploadResponse, error)
}
type AssetAuthorizer interface {
	Bind(context.Context, joboperation.Owner, string, string, time.Time) error
	Authorize(context.Context, joboperation.Owner, string, string) error
}

type Handler struct {
	logger           *slog.Logger
	authenticator    Authenticator
	models           ModelRegistry
	jobs             JobService
	maximumBodyBytes int64
	billingRequired  bool
	billing          Billing
	uploader         Uploader
	assets           AssetAuthorizer
}

func NewHandler(logger *slog.Logger, authenticator Authenticator, models ModelRegistry, service JobService, maximumBodyBytes int64) *Handler {
	return &Handler{logger: logger, authenticator: authenticator, models: models, jobs: service, maximumBodyBytes: maximumBodyBytes}
}

func (handler *Handler) SetBillingRequired(required bool) { handler.billingRequired = required }
func (handler *Handler) SetBilling(service Billing) {
	handler.billing = service
	handler.billingRequired = service != nil
}
func (handler *Handler) SetUploads(uploader Uploader, assets AssetAuthorizer) {
	handler.uploader = uploader
	handler.assets = assets
}

func (handler *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if handler == nil || handler.authenticator == nil || handler.models == nil || handler.jobs == nil || handler.maximumBodyBytes < 1 {
		writeError(writer, http.StatusServiceUnavailable, "service unavailable")
		return
	}
	principal, ok := handler.authenticate(writer, request)
	if !ok {
		return
	}
	if request.Header.Get("X-Runway-Version") != providerrunway.APIVersion {
		writeError(writer, http.StatusBadRequest, "unsupported Runway API version")
		return
	}
	switch {
	case request.Method == http.MethodPost && request.URL.Path == "/v1/uploads":
		handler.createUpload(writer, request, principal)
	case request.Method == http.MethodPost && (request.URL.Path == "/v1/text_to_video" || request.URL.Path == "/v1/image_to_video"):
		handler.create(writer, request, principal)
	case strings.HasPrefix(request.URL.Path, "/v1/tasks/") && request.Method == http.MethodGet:
		handler.get(writer, request, principal, strings.TrimPrefix(request.URL.Path, "/v1/tasks/"))
	case strings.HasPrefix(request.URL.Path, "/v1/tasks/") && request.Method == http.MethodDelete:
		handler.cancel(writer, request, principal, strings.TrimPrefix(request.URL.Path, "/v1/tasks/"))
	default:
		writeError(writer, http.StatusNotFound, "not found")
	}
}

func (handler *Handler) createUpload(writer http.ResponseWriter, request *http.Request, principal apikey.Principal) {
	if handler.uploader == nil || handler.assets == nil {
		writeError(writer, http.StatusServiceUnavailable, "upload service unavailable")
		return
	}
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(writer, http.StatusBadRequest, "Content-Type must be application/json")
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, 4097))
	if err != nil || len(body) > 4096 {
		writeError(writer, http.StatusRequestEntityTooLarge, "request body is too large")
		return
	}
	if err = validateUploadRequest(body); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid upload request")
		return
	}
	channelID, _ := providercredentials.LegacyChannel(providercredentials.Runway)
	response, err := handler.uploader.CreateEphemeralUpload(request.Context(), channelID, body)
	if err != nil {
		writeError(writer, http.StatusServiceUnavailable, "upload bootstrap unavailable")
		return
	}
	if response.Status < 200 || response.Status > 299 {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(response.Status)
		_, _ = writer.Write(response.Body)
		return
	}
	owner := joboperation.Owner{OrganizationID: principal.OrganizationID, ProjectID: principal.ProjectID, APIKeyID: principal.APIKeyID}
	if err = handler.assets.Bind(request.Context(), owner, channelID, response.URI, time.Now().UTC().Add(24*time.Hour)); err != nil {
		writeError(writer, http.StatusServiceUnavailable, "upload binding unavailable")
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(response.Body)
}

func validateUploadRequest(body []byte) error {
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return errors.New("object required")
	}
	seen := map[string]bool{}
	filename, kind := "", ""
	for decoder.More() {
		keyToken, tokenErr := decoder.Token()
		key, ok := keyToken.(string)
		if tokenErr != nil || !ok || seen[key] || (key != "filename" && key != "type") {
			return errors.New("invalid field")
		}
		seen[key] = true
		var value string
		if decoder.Decode(&value) != nil {
			return errors.New("invalid value")
		}
		if key == "filename" {
			filename = value
		} else {
			kind = value
		}
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') || decoder.Decode(&struct{}{}) != io.EOF || kind != "ephemeral" || !validUploadFilename(filename) {
		return errors.New("invalid upload")
	}
	return nil
}

func validUploadFilename(value string) bool {
	if len(value) < 3 || len(value) > 255 || strings.TrimSpace(value) != value || strings.ContainsAny(value, "/\\") || strings.Contains(value, "..") || strings.IndexFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return false
	}
	dot := strings.LastIndexByte(value, '.')
	if dot < 1 || dot == len(value)-1 {
		return false
	}
	extension := strings.ToLower(value[dot+1:])
	switch extension {
	case "jpg", "jpeg", "png", "webp", "gif", "mp4", "mov", "webm", "mp3", "wav", "m4a", "aac", "flac":
		return true
	default:
		return false
	}
}

func (handler *Handler) create(writer http.ResponseWriter, request *http.Request, principal apikey.Principal) {
	if handler.billingRequired && handler.billing == nil {
		writeError(writer, http.StatusServiceUnavailable, "video billing is not configured")
		return
	}
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(writer, http.StatusBadRequest, "Content-Type must be application/json")
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, handler.maximumBodyBytes+1))
	if err != nil || int64(len(body)) > handler.maximumBodyBytes {
		writeError(writer, http.StatusRequestEntityTooLarge, "request body is too large")
		return
	}
	model, outbound, err := validateAndRewrite(body, handler.models, request.URL.Path)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "invalid video generation request")
		return
	}
	if !principal.AuthorizeModel("runway", string(videooperation.Generate), model) {
		writeError(writer, http.StatusForbidden, "model is not permitted")
		return
	}
	route, _ := handler.models.Resolve(model)
	if (request.URL.Path == "/v1/text_to_video" && !route.TextToVideo) || (request.URL.Path == "/v1/image_to_video" && !route.ImageToVideo) {
		writeError(writer, http.StatusBadRequest, "model does not support this video operation")
		return
	}
	owner := joboperation.Owner{OrganizationID: principal.OrganizationID, ProjectID: principal.ProjectID, APIKeyID: principal.APIKeyID}
	for _, uri := range runwayAssetURIs(body) {
		if handler.assets == nil || handler.assets.Authorize(request.Context(), owner, route.ChannelID, uri) != nil {
			writeError(writer, http.StatusForbidden, "Runway upload asset is not permitted")
			return
		}
	}
	key := strings.TrimSpace(request.Header.Get("Idempotency-Key"))
	if key != "" && !idempotency.Valid(key) {
		writeError(writer, http.StatusBadRequest, "invalid Idempotency-Key")
		return
	}
	fingerprint := sha256.Sum256(body)
	chargeID := ""
	var estimatedUsage *joboperation.Usage
	if handler.billingRequired {
		duration, size, quality, extractErr := videoBillingDimensions(body, request.URL.Path)
		if extractErr != nil {
			writeError(writer, http.StatusBadRequest, "billable video dimensions are required")
			return
		}
		charge, beginErr := handler.billing.Begin(request.Context(), billing.BeginRequest{RequestID: requestid.FromContext(request.Context()), OrganizationID: principal.OrganizationID, ProjectID: principal.ProjectID, APIKeyID: principal.APIKeyID, Protocol: "runway", Operation: string(videooperation.Generate), Model: model, ChannelID: route.ChannelID, Quantity: duration, Size: size, Quality: quality, IdempotencyKey: key, RequestFingerprint: fingerprint})
		if beginErr != nil {
			switch {
			case errors.Is(beginErr, billing.ErrRequestConflict):
				writeError(writer, http.StatusConflict, "idempotency conflict")
			case errors.Is(beginErr, billing.ErrRequestPending):
				writeError(writer, http.StatusConflict, "video task is pending")
			default:
				writeError(writer, http.StatusPaymentRequired, "video price or balance is unavailable")
			}
			return
		}
		chargeID = charge.ID
		estimatedUsage = &joboperation.Usage{Dimension: "provider_credit", Unit: "microcredit", Quantity: charge.Quantity, Provenance: "request", ExtractorVersion: "runway-request-cost-v1", ResultExtractorVersion: "runway-task-cost-v1"}
	}
	created, err := handler.jobs.Submit(request.Context(), jobs.CreateRequest{RequestID: requestid.FromContext(request.Context()), Owner: owner, Protocol: "runway", Operation: string(videooperation.Generate), Model: model, Provider: string(route.Provider), ChannelID: route.ChannelID, ChargeID: chargeID, IdempotencyKey: key, Fingerprint: fingerprint, EstimatedUsage: estimatedUsage}, providerrunway.SubmitPayload{Path: request.URL.Path, Body: outbound})
	if errors.Is(err, joboperation.ErrConflict) {
		writeError(writer, http.StatusConflict, "idempotency conflict")
		return
	}
	if err != nil {
		writeError(writer, http.StatusServiceUnavailable, "video task could not be submitted")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"id": created.ID})
}

func videoBillingDimensions(body []byte, path string) (int64, string, string, error) {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(body, &envelope) != nil {
		return 0, "", "", errors.New("invalid json")
	}
	var duration int64
	var ratio string
	if json.Unmarshal(envelope["duration"], &duration) != nil || duration < 1 || duration > 60 {
		return 0, "", "", errors.New("invalid duration")
	}
	if json.Unmarshal(envelope["ratio"], &ratio) != nil || ratio == "" || len(ratio) > 32 {
		return 0, "", "", errors.New("invalid ratio")
	}
	audio := false
	if raw, ok := envelope["audio"]; ok && json.Unmarshal(raw, &audio) != nil {
		return 0, "", "", errors.New("invalid audio")
	}
	kind := strings.TrimPrefix(path, "/v1/")
	quality := "ratio=" + ratio + ";audio=" + strconv.FormatBool(audio)
	if len(kind) > 80 || len(quality) > 80 {
		return 0, "", "", errors.New("invalid dimensions")
	}
	return duration, kind, quality, nil
}

func (handler *Handler) get(writer http.ResponseWriter, request *http.Request, principal apikey.Principal, id string) {
	if strings.Contains(id, "/") {
		writeError(writer, http.StatusNotFound, "task not found")
		return
	}
	owner := joboperation.Owner{OrganizationID: principal.OrganizationID, ProjectID: principal.ProjectID, APIKeyID: principal.APIKeyID}
	value, err := handler.jobs.Get(request.Context(), owner, id)
	if err != nil || value.Protocol != "runway" || !principal.AuthorizeModel("runway", string(videooperation.Generate), value.Model) {
		writeError(writer, http.StatusNotFound, "task not found")
		return
	}
	if len(value.Snapshot.Body) > 0 && json.Valid(value.Snapshot.Body) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write(value.Snapshot.Body)
		return
	}
	response := map[string]any{"id": value.ID, "status": nativeStatus(value.Status), "createdAt": value.CreatedAt.UTC().Format(time.RFC3339Nano)}
	switch value.Status {
	case joboperation.Pending, joboperation.Queued:
		if value.EstimatedUsage != nil {
			response["estimatedCost"] = map[string]any{"credits": creditNumber(value.EstimatedUsage.Quantity)}
		}
	case joboperation.Processing, joboperation.Reconciling:
		if value.EstimatedUsage != nil {
			response["estimatedCost"] = map[string]any{"credits": creditNumber(value.EstimatedUsage.Quantity)}
		}
		response["progress"] = 0
	case joboperation.Canceled:
		if value.ActualUsage != nil {
			response["cost"] = map[string]any{"credits": creditNumber(value.ActualUsage.Quantity)}
		}
	}
	writeJSON(writer, http.StatusOK, response)
}

func creditNumber(micros int64) json.Number {
	whole := micros / pricing.ProviderCreditScale
	fraction := micros % pricing.ProviderCreditScale
	if fraction == 0 {
		return json.Number(strconv.FormatInt(whole, 10))
	}
	return json.Number(strconv.FormatInt(whole, 10) + "." + strings.TrimRight(fmt.Sprintf("%06d", fraction), "0"))
}

func (handler *Handler) cancel(writer http.ResponseWriter, request *http.Request, principal apikey.Principal, id string) {
	if strings.Contains(id, "/") {
		writeError(writer, http.StatusNotFound, "task not found")
		return
	}
	owner := joboperation.Owner{OrganizationID: principal.OrganizationID, ProjectID: principal.ProjectID, APIKeyID: principal.APIKeyID}
	value, err := handler.jobs.Get(request.Context(), owner, id)
	if err != nil || value.Protocol != "runway" || !principal.AuthorizeModel("runway", string(videooperation.Generate), value.Model) {
		writeError(writer, http.StatusNotFound, "task not found")
		return
	}
	if !value.Status.Terminal() {
		if _, err = handler.jobs.Cancel(request.Context(), owner, id); err != nil {
			writeError(writer, http.StatusServiceUnavailable, "task cancellation is uncertain")
			return
		}
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (handler *Handler) authenticate(writer http.ResponseWriter, request *http.Request) (apikey.Principal, bool) {
	authorization := strings.TrimSpace(request.Header.Get("Authorization"))
	if !strings.HasPrefix(authorization, "Bearer ") || strings.TrimSpace(strings.TrimPrefix(authorization, "Bearer ")) == "" {
		writeError(writer, http.StatusUnauthorized, "invalid API key")
		return apikey.Principal{}, false
	}
	principal, err := handler.authenticator.Authenticate(request.Context(), strings.TrimSpace(strings.TrimPrefix(authorization, "Bearer ")))
	if err != nil {
		writeError(writer, http.StatusUnauthorized, "invalid API key")
		return apikey.Principal{}, false
	}
	return principal, true
}

func validateAndRewrite(body []byte, models ModelRegistry, path string) (string, []byte, error) {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(body, &envelope) != nil {
		return "", nil, errors.New("invalid json")
	}
	var model string
	if json.Unmarshal(envelope["model"], &model) != nil || model == "" {
		return "", nil, errors.New("invalid model")
	}
	if path == "/v1/text_to_video" {
		var prompt string
		if json.Unmarshal(envelope["promptText"], &prompt) != nil || strings.TrimSpace(prompt) == "" {
			return "", nil, errors.New("promptText required")
		}
	}
	if path == "/v1/image_to_video" {
		raw, ok := envelope["promptImage"]
		if !ok || !validAssetValue(raw) {
			return "", nil, errors.New("promptImage required")
		}
	}
	route, err := models.Resolve(model)
	if err != nil {
		return "", nil, err
	}
	for _, field := range []string{"promptImage", "referenceImage"} {
		if raw, ok := envelope[field]; ok && !validAssetValue(raw) {
			return "", nil, errors.New("invalid asset")
		}
	}
	outbound, err := rewriteTopLevelModel(body, route.ProviderModel)
	return model, outbound, err
}

func validAssetValue(raw json.RawMessage) bool {
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return validAsset(value)
	}
	var values []string
	if json.Unmarshal(raw, &values) == nil {
		for _, item := range values {
			if !validAsset(item) {
				return false
			}
		}
		return len(values) > 0
	}
	return false
}
func validAsset(value string) bool {
	if strings.HasPrefix(value, "runway://") {
		return len(value) >= 13 && len(value) <= 5000 && strings.TrimSpace(value) == value && strings.IndexFunc(value, func(r rune) bool { return r < 0x21 || r == 0x7f }) == -1
	}
	if strings.HasPrefix(value, "data:image/") {
		prefix, encoded, ok := strings.Cut(value, ",")
		if !ok || !strings.HasSuffix(prefix, ";base64") || (prefix != "data:image/png;base64" && prefix != "data:image/jpeg;base64" && prefix != "data:image/webp;base64") {
			return false
		}
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		return err == nil && len(decoded) > 0 && len(decoded) <= 5*1024*1024
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return false
	}
	_, ipErr := netip.ParseAddr(parsed.Hostname())
	host := strings.ToLower(parsed.Hostname())
	return ipErr != nil && strings.Contains(host, ".") && host != "localhost"
}

func runwayAssetURIs(body []byte) []string {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(body, &envelope) != nil {
		return nil
	}
	var result []string
	for _, field := range []string{"promptImage", "referenceImage"} {
		raw, ok := envelope[field]
		if !ok {
			continue
		}
		var single string
		if json.Unmarshal(raw, &single) == nil {
			if strings.HasPrefix(single, "runway://") {
				result = append(result, single)
			}
			continue
		}
		var multiple []string
		if json.Unmarshal(raw, &multiple) == nil {
			for _, value := range multiple {
				if strings.HasPrefix(value, "runway://") {
					result = append(result, value)
				}
			}
		}
	}
	return result
}

func rewriteTopLevelModel(body []byte, providerModel string) ([]byte, error) {
	encoded, err := json.Marshal(providerModel)
	if err != nil {
		return nil, err
	}
	i := skipJSONSpace(body, 0)
	if i >= len(body) || body[i] != '{' {
		return nil, errors.New("invalid object")
	}
	i++
	found := false
	for {
		i = skipJSONSpace(body, i)
		if i >= len(body) {
			return nil, errors.New("unterminated object")
		}
		if body[i] == '}' {
			break
		}
		keyStart := i
		keyEnd, err := scanJSONString(body, keyStart)
		if err != nil {
			return nil, err
		}
		var key string
		if json.Unmarshal(body[keyStart:keyEnd], &key) != nil {
			return nil, errors.New("invalid key")
		}
		i = skipJSONSpace(body, keyEnd)
		if i >= len(body) || body[i] != ':' {
			return nil, errors.New("missing colon")
		}
		i = skipJSONSpace(body, i+1)
		valueStart := i
		valueEnd, err := scanJSONValue(body, valueStart)
		if err != nil {
			return nil, err
		}
		if key == "model" {
			if found || valueStart >= len(body) || body[valueStart] != '"' {
				return nil, errors.New("invalid model")
			}
			found = true
			result := make([]byte, 0, len(body)-valueEnd+valueStart+len(encoded))
			result = append(result, body[:valueStart]...)
			result = append(result, encoded...)
			result = append(result, body[valueEnd:]...)
			return result, nil
		}
		i = skipJSONSpace(body, valueEnd)
		if i < len(body) && body[i] == ',' {
			i++
			continue
		}
		if i < len(body) && body[i] == '}' {
			break
		}
		return nil, errors.New("invalid separator")
	}
	return nil, errors.New("model missing")
}
func skipJSONSpace(body []byte, i int) int {
	for i < len(body) && (body[i] == ' ' || body[i] == '\n' || body[i] == '\r' || body[i] == '\t') {
		i++
	}
	return i
}
func scanJSONString(body []byte, start int) (int, error) {
	if start >= len(body) || body[start] != '"' {
		return 0, errors.New("string expected")
	}
	escaped := false
	for i := start + 1; i < len(body); i++ {
		if escaped {
			escaped = false
			continue
		}
		if body[i] == '\\' {
			escaped = true
			continue
		}
		if body[i] == '"' {
			return i + 1, nil
		}
		if body[i] < 0x20 {
			return 0, errors.New("invalid string")
		}
	}
	return 0, errors.New("unterminated string")
}
func scanJSONValue(body []byte, start int) (int, error) {
	if start >= len(body) {
		return 0, errors.New("missing value")
	}
	if body[start] == '"' {
		return scanJSONString(body, start)
	}
	if body[start] == '{' || body[start] == '[' {
		stack := []byte{body[start]}
		inString, escaped := false, false
		for i := start + 1; i < len(body); i++ {
			b := body[i]
			if inString {
				if escaped {
					escaped = false
				} else if b == '\\' {
					escaped = true
				} else if b == '"' {
					inString = false
				}
				continue
			}
			if b == '"' {
				inString = true
				continue
			}
			if b == '{' || b == '[' {
				stack = append(stack, b)
				continue
			}
			if b == '}' || b == ']' {
				top := stack[len(stack)-1]
				if (top == '{' && b != '}') || (top == '[' && b != ']') {
					return 0, errors.New("mismatched value")
				}
				stack = stack[:len(stack)-1]
				if len(stack) == 0 {
					return i + 1, nil
				}
			}
		}
		return 0, errors.New("unterminated value")
	}
	i := start
	for i < len(body) && body[i] != ',' && body[i] != '}' && body[i] != ']' && body[i] != ' ' && body[i] != '\n' && body[i] != '\r' && body[i] != '\t' {
		i++
	}
	if i == start {
		return 0, errors.New("empty value")
	}
	return i, nil
}
func nativeStatus(status joboperation.Status) string {
	switch status {
	case joboperation.Queued, joboperation.Pending:
		return "PENDING"
	case joboperation.Processing, joboperation.Reconciling:
		return "RUNNING"
	case joboperation.Succeeded:
		return "SUCCEEDED"
	case joboperation.Failed:
		return "FAILED"
	case joboperation.Canceled:
		return "CANCELLED"
	}
	return "PENDING"
}
func writeError(writer http.ResponseWriter, status int, message string) {
	writeJSON(writer, status, map[string]any{"error": message})
}
func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
