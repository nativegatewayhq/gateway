package openai

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"strings"

	"github.com/nativegatewayhq/gateway/internal/apikey"
	"github.com/nativegatewayhq/gateway/internal/audioassets"
	"github.com/nativegatewayhq/gateway/internal/idempotency"
)

type AudioAssetService interface {
	Create(context.Context, apikey.Principal, string, audioassets.Upload) (audioassets.Asset, error)
	Get(context.Context, apikey.Principal, string) (audioassets.Asset, error)
	Delete(context.Context, apikey.Principal, string) (audioassets.Asset, error)
}
type AudioAssetHandler struct {
	common       *Handler
	service      AudioAssetService
	maximumBytes int64
	slots        chan struct{}
	allowed      map[string]bool
	tempDir      string
}
type audioAssetResponse struct {
	ID          string `json:"id"`
	Object      string `json:"object"`
	Bytes       int64  `json:"bytes"`
	ContentType string `json:"content_type"`
	Status      string `json:"status"`
	CreatedAt   int64  `json:"created_at"`
	ExpiresAt   int64  `json:"expires_at"`
}

func NewAudioAssetHandler(logger *slog.Logger, authenticator Authenticator, service AudioAssetService, maximumBytes int64, maximumConcurrent int, allowed []string, tempDir string) *AudioAssetHandler {
	set := map[string]bool{}
	for _, value := range allowed {
		set[value] = true
	}
	return &AudioAssetHandler{common: NewImagesHandler(logger, authenticator, nil, nil, 1), service: service, maximumBytes: maximumBytes, slots: make(chan struct{}, maximumConcurrent), allowed: set, tempDir: tempDir}
}
func (handler *AudioAssetHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.common.authenticate(writer, request)
	if !ok {
		return
	}
	if handler.service == nil || handler.maximumBytes < 1 || cap(handler.slots) < 1 {
		writeError(writer, 503, "server_error", "audio_asset_unavailable", "audio asset unavailable")
		return
	}
	if request.URL.Path == "/v1/audio/assets" {
		if request.Method != http.MethodPost {
			writer.Header().Set("Allow", http.MethodPost)
			writeError(writer, 405, "invalid_request_error", "method_not_allowed", "method not allowed")
			return
		}
		handler.create(writer, request, principal)
		return
	}
	id, ok := audioAssetPathID(request.URL.Path)
	if !ok {
		writeError(writer, 404, "invalid_request_error", "asset_not_found", "audio asset not found")
		return
	}
	switch request.Method {
	case http.MethodGet:
		asset, err := handler.service.Get(request.Context(), principal, id)
		handler.respond(writer, asset, err)
	case http.MethodDelete:
		asset, err := handler.service.Delete(request.Context(), principal, id)
		handler.respond(writer, asset, err)
	default:
		writer.Header().Set("Allow", "GET, DELETE")
		writeError(writer, 405, "invalid_request_error", "method_not_allowed", "method not allowed")
	}
}
func (handler *AudioAssetHandler) create(writer http.ResponseWriter, request *http.Request, principal apikey.Principal) {
	key := request.Header.Get("Idempotency-Key")
	if !idempotency.Valid(key) {
		writeError(writer, 400, "invalid_request_error", "invalid_idempotency_key", "valid Idempotency-Key is required")
		return
	}
	media, params, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || media != "multipart/form-data" || params["boundary"] == "" {
		writeError(writer, 400, "invalid_request_error", "invalid_multipart", "invalid audio asset upload")
		return
	}
	select {
	case handler.slots <- struct{}{}:
		defer func() { <-handler.slots }()
	default:
		writeError(writer, 503, "server_error", "upload_capacity_exhausted", "audio asset capacity unavailable")
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, handler.maximumBytes+64*1024)
	reader := multipart.NewReader(request.Body, params["boundary"])
	var file *os.File
	var contentType string
	var size int64
	var digest [32]byte
	defer func() {
		if file != nil {
			name := file.Name()
			_ = file.Close()
			_ = os.Remove(name)
		}
	}()
	parts := 0
	for {
		part, partErr := reader.NextPart()
		if errors.Is(partErr, io.EOF) {
			break
		}
		if partErr != nil {
			writeError(writer, 400, "invalid_request_error", "invalid_multipart", "invalid audio asset upload")
			return
		}
		parts++
		if parts != 1 || part.FormName() != "file" || part.FileName() == "" {
			writeError(writer, 400, "invalid_request_error", "invalid_multipart", "exactly one audio file is required")
			return
		}
		contentType = strings.ToLower(strings.TrimSpace(part.Header.Get("Content-Type")))
		if !handler.allowed[contentType] {
			writeError(writer, 400, "invalid_request_error", "unsupported_audio_type", "audio content type is not supported")
			return
		}
		file, err = os.CreateTemp(handler.tempDir, "gateway-audio-asset-*")
		if err != nil {
			writeError(writer, 503, "server_error", "spool_unavailable", "audio asset unavailable")
			return
		}
		_ = file.Chmod(0600)
		hash := sha256.New()
		size, err = io.Copy(io.MultiWriter(file, hash), io.LimitReader(part, handler.maximumBytes+1))
		if err != nil || size < 1 || size > handler.maximumBytes {
			writeError(writer, 413, "invalid_request_error", "audio_asset_too_large", "audio asset is too large")
			return
		}
		copy(digest[:], hash.Sum(nil))
		if _, err = file.Seek(0, io.SeekStart); err != nil {
			writeError(writer, 503, "server_error", "spool_unavailable", "audio asset unavailable")
			return
		}
		header := make([]byte, 512)
		n, _ := io.ReadFull(file, header)
		if !validAudioMagic(contentType, header[:n]) {
			writeError(writer, 400, "invalid_request_error", "audio_content_mismatch", "audio content does not match content type")
			return
		}
		if _, err = file.Seek(0, io.SeekStart); err != nil {
			writeError(writer, 503, "server_error", "spool_unavailable", "audio asset unavailable")
			return
		}
	}
	if parts != 1 || file == nil {
		writeError(writer, 400, "invalid_request_error", "invalid_multipart", "exactly one audio file is required")
		return
	}
	asset, err := handler.service.Create(request.Context(), principal, key, audioassets.Upload{ContentType: contentType, Size: size, SHA256: digest, Body: file})
	if err != nil {
		handler.respond(writer, audioassets.Asset{}, err)
		return
	}
	writeAudioAsset(writer, http.StatusCreated, asset)
}
func (handler *AudioAssetHandler) respond(writer http.ResponseWriter, asset audioassets.Asset, err error) {
	if err == nil {
		writeAudioAsset(writer, http.StatusOK, asset)
		return
	}
	switch {
	case errors.Is(err, audioassets.ErrDenied):
		writeError(writer, 404, "invalid_request_error", "asset_not_found", "audio asset not found")
	case errors.Is(err, audioassets.ErrConflict):
		writeError(writer, 409, "invalid_request_error", "idempotency_conflict", "audio asset request conflicts")
	case errors.Is(err, audioassets.ErrPending):
		writeError(writer, 409, "invalid_request_error", "asset_pending", "audio asset is pending")
	case errors.Is(err, audioassets.ErrInvalid):
		writeError(writer, 400, "invalid_request_error", "invalid_audio_asset", "invalid audio asset")
	default:
		writeError(writer, 503, "server_error", "audio_asset_unavailable", "audio asset unavailable")
	}
}
func writeAudioAsset(writer http.ResponseWriter, status int, asset audioassets.Asset) {
	value := audioAssetResponse{ID: asset.ID, Object: "audio.asset", Bytes: asset.ByteLength, ContentType: asset.ContentType, Status: strings.ToLower(string(asset.State)), CreatedAt: asset.CreatedAt.Unix(), ExpiresAt: asset.ExpiresAt.Unix()}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
func audioAssetPathID(value string) (string, bool) {
	const prefix = "/v1/audio/assets/"
	if !strings.HasPrefix(value, prefix) {
		return "", false
	}
	id := strings.TrimPrefix(value, prefix)
	return id, len(id) == len("audasset_")+32 && !strings.Contains(id, "/")
}
func validAudioMagic(contentType string, body []byte) bool {
	switch contentType {
	case "audio/wav", "audio/x-wav":
		return len(body) >= 12 && string(body[:4]) == "RIFF" && string(body[8:12]) == "WAVE"
	case "audio/mpeg":
		return len(body) >= 3 && (string(body[:3]) == "ID3" || (body[0] == 0xff && body[1]&0xe0 == 0xe0))
	case "audio/ogg":
		return len(body) >= 4 && string(body[:4]) == "OggS"
	case "audio/flac":
		return len(body) >= 4 && string(body[:4]) == "fLaC"
	case "audio/webm":
		return len(body) >= 4 && body[0] == 0x1a && body[1] == 0x45 && body[2] == 0xdf && body[3] == 0xa3
	case "audio/mp4":
		return len(body) >= 12 && string(body[4:8]) == "ftyp"
	}
	return false
}

func requestedAudioAsset(header http.Header) (string, bool) {
	values, exists := header[http.CanonicalHeaderKey("X-Native-Gateway-Audio-Asset")]
	if !exists {
		return "", false
	}
	if len(values) != 1 {
		return "", true
	}
	value := values[0]
	if strings.TrimSpace(value) != value || len(value) != len("audasset_")+32 || !strings.HasPrefix(value, "audasset_") {
		return "", true
	}
	for _, character := range strings.TrimPrefix(value, "audasset_") {
		if !(character >= '0' && character <= '9' || character >= 'a' && character <= 'f') {
			return "", true
		}
	}
	return value, true
}

func applyMaterializedAudio(form *transcriptionForm, materialized audioassets.Materialized) error {
	if form == nil || form.file == nil || materialized.Body == nil || materialized.Asset.ByteLength < 1 || materialized.Asset.SHA256 == ([32]byte{}) {
		return audioassets.ErrInvalid
	}
	hash := sha256.New()
	read, err := form.file.ReadFrom(io.TeeReader(materialized.Body, hash))
	if err != nil || read != materialized.Asset.ByteLength {
		return audioassets.ErrStorage
	}
	var digest [32]byte
	copy(digest[:], hash.Sum(nil))
	if digest != materialized.Asset.SHA256 {
		return audioassets.ErrStorage
	}
	form.assetID = materialized.Asset.ID
	form.fileType = materialized.Asset.ContentType
	form.filename = "managed-audio" + audioAssetExtension(materialized.Asset.ContentType)
	return nil
}
func audioAssetExtension(contentType string) string {
	switch contentType {
	case "audio/wav", "audio/x-wav":
		return ".wav"
	case "audio/mpeg":
		return ".mp3"
	case "audio/ogg":
		return ".ogg"
	case "audio/flac":
		return ".flac"
	case "audio/webm":
		return ".webm"
	case "audio/mp4":
		return ".m4a"
	}
	return ".bin"
}
