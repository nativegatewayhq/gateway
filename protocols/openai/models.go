package openai

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	"github.com/nativegatewayhq/gateway/internal/requestid"
)

type ProviderAvailability interface {
	ConfiguredProviders() []providercredentials.ProviderID
}

type ModelsHandler struct {
	common       *Handler
	availability ProviderAvailability
}

func NewModelsHandler(logger *slog.Logger, authenticator Authenticator, models ModelRegistry, availability ProviderAvailability) *ModelsHandler {
	return &ModelsHandler{common: NewImagesHandler(logger, authenticator, models, nil, 1), availability: availability}
}

type modelObject struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}
type modelList struct {
	Object string        `json:"object"`
	Data   []modelObject `json:"data"`
}

func (handler *ModelsHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	tracked := &statusWriter{ResponseWriter: writer}
	started := time.Now()
	count := 0
	defer func() {
		handler.common.logger.Info("openai models request completed", "request_id", requestid.FromContext(request.Context()), "protocol", "openai", "operation", "models.list", "status", tracked.statusCode(), "count", count, "duration", time.Since(started))
	}()
	if request.Method != http.MethodGet {
		tracked.Header().Set("Allow", http.MethodGet)
		writeError(tracked, 405, "invalid_request_error", "method_not_allowed", "method not allowed")
		return
	}
	if _, authenticated := handler.common.authenticate(tracked, request); !authenticated {
		return
	}
	configured := map[providercredentials.ProviderID]bool{}
	if handler.availability != nil {
		for _, provider := range handler.availability.ConfiguredProviders() {
			configured[provider] = true
		}
	}
	data := []modelObject{}
	if handler.common.models != nil {
		for _, model := range handler.common.models.List() {
			if configured[model.Provider] {
				data = append(data, modelObject{model.Model, "model", model.Created, model.Owner})
			}
		}
	}
	sort.Slice(data, func(i, j int) bool { return data[i].ID < data[j].ID })
	count = len(data)
	tracked.Header().Set("Content-Type", "application/json")
	tracked.WriteHeader(200)
	_ = json.NewEncoder(tracked).Encode(modelList{Object: "list", Data: data})
}
