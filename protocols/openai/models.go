package openai

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	"github.com/nativegatewayhq/gateway/internal/requestid"
	audiooperation "github.com/nativegatewayhq/gateway/operations/audio"
	chatoperation "github.com/nativegatewayhq/gateway/operations/chat"
	responsesoperation "github.com/nativegatewayhq/gateway/operations/responses"
	videooperation "github.com/nativegatewayhq/gateway/operations/video"
)

type ProviderAvailability interface {
	ConfiguredProviders() []providercredentials.ProviderID
}

type ChannelProviderAvailability interface {
	ConfiguredChannel(context.Context, string, providercredentials.ProviderID) bool
}

type ModelsHandler struct {
	common       *Handler
	availability ProviderAvailability
	chat         interface{ List() []chatoperation.Model }
	responses    interface {
		List() []responsesoperation.Model
	}
	video          interface{ List() []videooperation.Route }
	audio          interface{ List() []audiooperation.Model }
	transcriptions interface {
		List() []audiooperation.TranscriptionModel
	}
}

func NewModelsHandlerWithAllAudioOperations(logger *slog.Logger, authenticator Authenticator, models ModelRegistry, chat interface{ List() []chatoperation.Model }, responses interface {
	List() []responsesoperation.Model
}, video interface{ List() []videooperation.Route }, audio interface{ List() []audiooperation.Model }, transcriptions interface {
	List() []audiooperation.TranscriptionModel
}, availability ProviderAvailability) *ModelsHandler {
	return &ModelsHandler{common: NewImagesHandler(logger, authenticator, models, nil, 1), chat: chat, responses: responses, video: video, audio: audio, transcriptions: transcriptions, availability: availability}
}

func NewModelsHandlerWithAllAndAudio(logger *slog.Logger, authenticator Authenticator, models ModelRegistry, chat interface{ List() []chatoperation.Model }, responses interface {
	List() []responsesoperation.Model
}, video interface{ List() []videooperation.Route }, audio interface{ List() []audiooperation.Model }, availability ProviderAvailability) *ModelsHandler {
	return &ModelsHandler{common: NewImagesHandler(logger, authenticator, models, nil, 1), chat: chat, responses: responses, video: video, audio: audio, availability: availability}
}

func NewModelsHandlerWithAll(logger *slog.Logger, authenticator Authenticator, models ModelRegistry, chat interface{ List() []chatoperation.Model }, responses interface {
	List() []responsesoperation.Model
}, video interface{ List() []videooperation.Route }, availability ProviderAvailability) *ModelsHandler {
	return &ModelsHandler{common: NewImagesHandler(logger, authenticator, models, nil, 1), chat: chat, responses: responses, video: video, availability: availability}
}

func NewModelsHandlerWithChatAndResponses(logger *slog.Logger, authenticator Authenticator, models ModelRegistry, chat interface{ List() []chatoperation.Model }, responses interface {
	List() []responsesoperation.Model
}, availability ProviderAvailability) *ModelsHandler {
	return &ModelsHandler{common: NewImagesHandler(logger, authenticator, models, nil, 1), chat: chat, responses: responses, availability: availability}
}

func NewModelsHandlerWithChat(logger *slog.Logger, authenticator Authenticator, models ModelRegistry, chat interface{ List() []chatoperation.Model }, availability ProviderAvailability) *ModelsHandler {
	return &ModelsHandler{common: NewImagesHandler(logger, authenticator, models, nil, 1), chat: chat, availability: availability}
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
	principal, authenticated := handler.common.authenticate(tracked, request)
	if !authenticated {
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
			available := false
			for _, capability := range model.Capabilities {
				if !principal.AuthorizeModel("openai", string(capability.Operation), model.Model) {
					continue
				}
				candidates, err := handler.common.models.Candidates("openai", model.Model, capability.Operation, capability.MediaType)
				if err != nil {
					continue
				}
				for _, candidate := range candidates {
					channelConfigured := false
					if channelAvailability, ok := handler.availability.(ChannelProviderAvailability); ok {
						channelConfigured = channelAvailability.ConfiguredChannel(request.Context(), candidate.ChannelID, candidate.Provider)
					} else {
						channelConfigured = configured[candidate.Provider]
					}
					if channelConfigured {
						available = true
						break
					}
				}
				if available {
					break
				}
			}
			if available {
				data = append(data, modelObject{model.Model, "model", model.Created, model.Owner})
			}
		}
	}
	seen := make(map[string]bool, len(data))
	for _, item := range data {
		seen[item.ID] = true
	}
	if handler.chat != nil {
		for _, model := range handler.chat.List() {
			if seen[model.ID] || !principal.AuthorizeModel("openai", chatoperation.Completions, model.ID) {
				continue
			}
			available := false
			candidates := []chatoperation.Model{model}
			if routed, ok := handler.chat.(interface {
				Candidates(string, chatoperation.Requirements) ([]chatoperation.Model, error)
			}); ok {
				if routedCandidates, err := routed.Candidates(model.ID, chatoperation.Requirements{}); err == nil {
					candidates = routedCandidates
				}
			}
			for _, candidate := range candidates {
				if channelAvailability, ok := handler.availability.(ChannelProviderAvailability); ok {
					available = channelAvailability.ConfiguredChannel(request.Context(), candidate.ChannelID, candidate.Provider)
				} else {
					available = configured[candidate.Provider]
				}
				if available {
					break
				}
			}
			if available {
				data = append(data, modelObject{model.ID, "model", model.Created, model.Owner})
				seen[model.ID] = true
			}
		}
	}
	if handler.responses != nil {
		for _, model := range handler.responses.List() {
			if seen[model.ID] || !principal.AuthorizeModel("openai", responsesoperation.Create, model.ID) {
				continue
			}
			available := false
			candidates := []responsesoperation.Model{model}
			if routed, ok := handler.responses.(interface {
				Candidates(string, responsesoperation.Requirements) ([]responsesoperation.Model, error)
			}); ok {
				if routedCandidates, err := routed.Candidates(model.ID, responsesoperation.Requirements{}); err == nil {
					candidates = routedCandidates
				}
			}
			for _, candidate := range candidates {
				if channelAvailability, ok := handler.availability.(ChannelProviderAvailability); ok {
					available = channelAvailability.ConfiguredChannel(request.Context(), candidate.ChannelID, candidate.Provider)
				} else {
					available = configured[candidate.Provider]
				}
				if available {
					break
				}
			}
			if available {
				data = append(data, modelObject{model.ID, "model", model.Created, model.Owner})
				seen[model.ID] = true
			}
		}
	}
	if handler.video != nil {
		for _, model := range handler.video.List() {
			if seen[model.Model] || !principal.AuthorizeModel("runway", string(videooperation.Generate), model.Model) {
				continue
			}
			available := configured[model.Provider]
			if channelAvailability, ok := handler.availability.(ChannelProviderAvailability); ok {
				available = channelAvailability.ConfiguredChannel(request.Context(), model.ChannelID, model.Provider)
			}
			if available {
				data = append(data, modelObject{model.Model, "model", 0, "runway"})
				seen[model.Model] = true
			}
		}
	}
	if handler.audio != nil {
		for _, model := range handler.audio.List() {
			if seen[model.ID] || !principal.AuthorizeModel("openai", audiooperation.Speech, model.ID) {
				continue
			}
			available := configured[model.Provider]
			if channelAvailability, ok := handler.availability.(ChannelProviderAvailability); ok {
				available = channelAvailability.ConfiguredChannel(request.Context(), model.ChannelID, model.Provider)
			}
			if available {
				data = append(data, modelObject{model.ID, "model", model.Created, model.Owner})
				seen[model.ID] = true
			}
		}
	}
	if handler.transcriptions != nil {
		for _, model := range handler.transcriptions.List() {
			if seen[model.ID] || !principal.AuthorizeModel("openai", audiooperation.Transcription, model.ID) {
				continue
			}
			available := configured[model.Provider]
			if channelAvailability, ok := handler.availability.(ChannelProviderAvailability); ok {
				available = channelAvailability.ConfiguredChannel(request.Context(), model.ChannelID, model.Provider)
			}
			if available {
				data = append(data, modelObject{model.ID, "model", model.Created, model.Owner})
				seen[model.ID] = true
			}
		}
	}
	sort.Slice(data, func(i, j int) bool { return data[i].ID < data[j].ID })
	count = len(data)
	tracked.Header().Set("Content-Type", "application/json")
	tracked.WriteHeader(200)
	_ = json.NewEncoder(tracked).Encode(modelList{Object: "list", Data: data})
}
