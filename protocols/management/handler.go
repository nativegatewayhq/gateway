// Package management exposes payload-free, tenant-scoped asynchronous Job reads.
package management

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/nativegatewayhq/gateway/internal/apikey"
	"github.com/nativegatewayhq/gateway/internal/jobs"
	joboperation "github.com/nativegatewayhq/gateway/operations/job"
)

type Authenticator interface {
	Authenticate(context.Context, string) (apikey.Principal, error)
}
type Repository interface {
	ListManagement(context.Context, jobs.ManagementListRequest) ([]jobs.ManagementJob, bool, error)
	GetManagement(context.Context, joboperation.Owner, string, func(string, string, string) bool) (jobs.ManagementDetail, error)
}
type Handler struct {
	auth       Authenticator
	repository Repository
	cursor     *CursorCodec
	now        func() time.Time
}

func NewHandler(auth Authenticator, repository Repository, secrets [][]byte) (*Handler, error) {
	codec, err := NewCursorCodec(secrets)
	if err != nil || auth == nil || repository == nil {
		return nil, errors.New("invalid management handler configuration")
	}
	return &Handler{auth: auth, repository: repository, cursor: codec, now: time.Now}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if r.ContentLength > 0 || r.Header.Get("Content-Type") != "" {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	principal, err := h.auth.Authenticate(r.Context(), bearer(r.Header.Get("Authorization")))
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	owner := joboperation.Owner{OrganizationID: principal.OrganizationID, ProjectID: principal.ProjectID, APIKeyID: principal.APIKeyID}
	path := strings.TrimPrefix(r.URL.Path, "/gateway/v1/jobs")
	if path == "" || path == "/" {
		h.list(w, r, principal, owner)
		return
	}
	id := strings.TrimPrefix(path, "/")
	if strings.Contains(id, "/") || len(r.URL.Query()) != 0 {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	detail, err := h.repository.GetManagement(r.Context(), owner, id, principal.AuthorizeModel)
	if errors.Is(err, joboperation.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	if errors.Is(err, joboperation.ErrInvalid) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": detail.Job, "events": detail.Events, "events_truncated": detail.Truncated})
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request, principal apikey.Principal, owner joboperation.Owner) {
	filter, limit, token, err := parseQuery(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	req := jobs.ManagementListRequest{Owner: owner, Filter: filter, Limit: limit, AllowAllModels: principal.ModelAccessMode == "" || principal.ModelAccessMode == apikey.ModelAccessAll}
	for _, p := range principal.ModelPermissions {
		if (p.Protocol == "replicate" || p.Protocol == "fal") && p.Operation == "image.generate" {
			req.AllowedModels = append(req.AllowedModels, jobs.ModelAccess{Protocol: p.Protocol, Operation: p.Operation, Model: p.Model})
		}
	}
	binding := cursorBinding(owner, filter, limit)
	if token != "" {
		payload, decodeErr := h.cursor.Decode(token, binding, h.now())
		if decodeErr != nil {
			writeError(w, http.StatusBadRequest, "invalid_cursor")
			return
		}
		req.BeforeCreatedAt, req.BeforeID = payload.CreatedAt, payload.ID
	}
	items, more, err := h.repository.ListManagement(r.Context(), req)
	if errors.Is(err, joboperation.ErrInvalid) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable")
		return
	}
	next := ""
	if more && len(items) > 0 {
		last := items[len(items)-1]
		next, _ = h.cursor.Encode(CursorPayload{CreatedAt: last.CreatedAt, ID: last.ID}, binding)
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": items, "next_cursor": next})
}

func parseQuery(values url.Values) (jobs.ManagementFilter, int, string, error) {
	allowed := map[string]bool{"protocol": true, "status": true, "settlement_state": true, "model": true, "limit": true, "cursor": true}
	for key, vals := range values {
		if !allowed[key] || len(vals) != 1 {
			return jobs.ManagementFilter{}, 0, "", errors.New("invalid query")
		}
	}
	limit := 25
	if raw := values.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 100 {
			return jobs.ManagementFilter{}, 0, "", errors.New("invalid limit")
		}
		limit = parsed
	}
	return jobs.ManagementFilter{Protocol: values.Get("protocol"), Status: values.Get("status"), SettlementState: values.Get("settlement_state"), Model: values.Get("model")}, limit, values.Get("cursor"), nil
}

type CursorPayload struct {
	Version   int       `json:"v"`
	CreatedAt time.Time `json:"created_at"`
	ID        string    `json:"id"`
	Binding   string    `json:"binding"`
}
type CursorCodec struct{ secrets [][]byte }

func NewCursorCodec(secrets [][]byte) (*CursorCodec, error) {
	if len(secrets) < 1 || len(secrets) > 2 {
		return nil, errors.New("one or two secrets required")
	}
	for _, s := range secrets {
		if len(s) != 32 {
			return nil, errors.New("secret must be 32 bytes")
		}
	}
	return &CursorCodec{secrets: secrets}, nil
}
func (c *CursorCodec) Encode(p CursorPayload, binding string) (string, error) {
	p.Version = 1
	p.Binding = binding
	body, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, c.secrets[0])
	mac.Write(body)
	return base64.RawURLEncoding.EncodeToString(body) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}
func (c *CursorCodec) Decode(token, binding string, now time.Time) (CursorPayload, error) {
	var p CursorPayload
	if len(token) > 2048 {
		return p, errors.New("cursor too long")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return p, errors.New("invalid cursor")
	}
	body, e1 := base64.RawURLEncoding.DecodeString(parts[0])
	sig, e2 := base64.RawURLEncoding.DecodeString(parts[1])
	if e1 != nil || e2 != nil {
		return p, errors.New("invalid cursor")
	}
	valid := false
	for _, s := range c.secrets {
		m := hmac.New(sha256.New, s)
		m.Write(body)
		valid = valid || hmac.Equal(sig, m.Sum(nil))
	}
	if !valid || json.Unmarshal(body, &p) != nil || p.Version != 1 || p.Binding != binding || !joboperation.ValidID(p.ID) || p.CreatedAt.IsZero() || p.CreatedAt.After(now.Add(time.Minute)) {
		return CursorPayload{}, errors.New("invalid cursor")
	}
	return p, nil
}
func cursorBinding(owner joboperation.Owner, filter jobs.ManagementFilter, limit int) string {
	sum := sha256.Sum256([]byte(owner.OrganizationID + "\x00" + owner.ProjectID + "\x00" + owner.APIKeyID + "\x00" + filter.Protocol + "\x00" + filter.Status + "\x00" + filter.SettlementState + "\x00" + filter.Model + "\x00" + strconv.Itoa(limit)))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
func bearer(value string) string {
	parts := strings.Fields(value)
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		return parts[1]
	}
	return ""
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": code}})
}
