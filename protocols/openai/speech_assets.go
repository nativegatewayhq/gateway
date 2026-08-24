package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/nativegatewayhq/gateway/internal/apikey"
	"github.com/nativegatewayhq/gateway/internal/speechstorage"
)

type SpeechAssetService interface {
	Get(context.Context, apikey.Principal, string) (speechstorage.Asset, error)
	Open(context.Context, apikey.Principal, string) (speechstorage.Asset, io.ReadCloser, error)
	Delete(context.Context, apikey.Principal, string) (speechstorage.Asset, error)
}
type SpeechAssetHandler struct {
	common  *Handler
	service SpeechAssetService
}
type speechAssetResponse struct {
	ID          string `json:"id"`
	Object      string `json:"object"`
	Bytes       int64  `json:"bytes"`
	ContentType string `json:"content_type"`
	Status      string `json:"status"`
	CreatedAt   int64  `json:"created_at"`
	ExpiresAt   int64  `json:"expires_at"`
}

func NewSpeechAssetHandler(logger *slog.Logger, auth Authenticator, service SpeechAssetService) *SpeechAssetHandler {
	return &SpeechAssetHandler{common: NewImagesHandler(logger, auth, nil, nil, 1), service: service}
}
func (h *SpeechAssetHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	owner, ok := h.common.authenticate(w, r)
	if !ok {
		return
	}
	id, content, ok := speechAssetPath(r.URL.Path)
	if !ok {
		speechAssetError(w, http.StatusNotFound)
		return
	}
	if content {
		h.content(w, r, owner, id)
		return
	}
	switch r.Method {
	case http.MethodGet:
		a, err := h.service.Get(r.Context(), owner, id)
		h.respond(w, a, err)
	case http.MethodDelete:
		a, err := h.service.Delete(r.Context(), owner, id)
		h.respond(w, a, err)
	default:
		w.Header().Set("Allow", "GET, DELETE")
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method_not_allowed", "method not allowed")
	}
}
func (h *SpeechAssetHandler) respond(w http.ResponseWriter, a speechstorage.Asset, err error) {
	if err != nil {
		if errors.Is(err, speechstorage.ErrDenied) {
			speechAssetError(w, http.StatusNotFound)
		} else {
			writeError(w, http.StatusServiceUnavailable, "server_error", "speech_asset_unavailable", "speech asset unavailable")
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(speechAssetResponse{ID: a.ID, Object: "audio.speech.asset", Bytes: a.ByteLength, ContentType: a.ContentType, Status: strings.ToLower(string(a.State)), CreatedAt: a.CreatedAt.Unix(), ExpiresAt: a.ExpiresAt.Unix()})
}
func (h *SpeechAssetHandler) content(w http.ResponseWriter, r *http.Request, owner apikey.Principal, id string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method_not_allowed", "method not allowed")
		return
	}
	asset, body, err := h.service.Open(r.Context(), owner, id)
	if err != nil {
		speechAssetError(w, http.StatusNotFound)
		return
	}
	defer body.Close()
	start, end, partial, err := parseSingleRange(r.Header.Get("Range"), asset.ByteLength)
	if err != nil {
		w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(asset.ByteLength, 10))
		writeError(w, http.StatusRequestedRangeNotSatisfiable, "invalid_request_error", "invalid_range", "invalid range")
		return
	}
	if start > 0 {
		if _, err = io.CopyN(io.Discard, body, start); err != nil {
			writeError(w, http.StatusBadGateway, "server_error", "speech_asset_unavailable", "speech asset unavailable")
			return
		}
	}
	length := end - start + 1
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Type", asset.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	w.Header().Set("Cache-Control", "private, max-age=300")
	w.Header().Set("ETag", `"`+asset.ID+`"`)
	if partial {
		w.Header().Set("Content-Range", "bytes "+strconv.FormatInt(start, 10)+"-"+strconv.FormatInt(end, 10)+"/"+strconv.FormatInt(asset.ByteLength, 10))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.CopyN(w, body, length)
}
func speechAssetPath(path string) (string, bool, bool) {
	const prefix = "/v1/audio/speech/assets/"
	if !strings.HasPrefix(path, prefix) {
		return "", false, false
	}
	rest := strings.TrimPrefix(path, prefix)
	content := strings.HasSuffix(rest, "/content")
	if content {
		rest = strings.TrimSuffix(rest, "/content")
	}
	if len(rest) != len("speechasset_")+32 || strings.Contains(rest, "/") {
		return "", false, false
	}
	return rest, content, true
}
func parseSingleRange(value string, size int64) (int64, int64, bool, error) {
	if value == "" {
		return 0, size - 1, false, nil
	}
	if !strings.HasPrefix(value, "bytes=") || strings.Contains(value, ",") {
		return 0, 0, false, errors.New("invalid range")
	}
	parts := strings.Split(strings.TrimPrefix(value, "bytes="), "-")
	if len(parts) != 2 {
		return 0, 0, false, errors.New("invalid range")
	}
	if parts[0] == "" {
		suffix, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || suffix < 1 {
			return 0, 0, false, errors.New("invalid range")
		}
		if suffix > size {
			suffix = size
		}
		return size - suffix, size - 1, true, nil
	}
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false, errors.New("invalid range")
	}
	end := size - 1
	if parts[1] != "" {
		end, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil || end < start {
			return 0, 0, false, errors.New("invalid range")
		}
		if end >= size {
			end = size - 1
		}
	}
	return start, end, true, nil
}
func speechAssetError(w http.ResponseWriter, status int) {
	writeError(w, status, "invalid_request_error", "speech_asset_not_found", "speech asset not found")
}
