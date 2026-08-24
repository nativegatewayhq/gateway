package openai

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nativegatewayhq/gateway/internal/apikey"
	"github.com/nativegatewayhq/gateway/internal/audiopricing"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	audiooperation "github.com/nativegatewayhq/gateway/operations/audio"
	chatoperation "github.com/nativegatewayhq/gateway/operations/chat"
	imageoperation "github.com/nativegatewayhq/gateway/operations/image"
	videooperation "github.com/nativegatewayhq/gateway/operations/video"
)

type availability []providercredentials.ProviderID

func (value availability) ConfiguredProviders() []providercredentials.ProviderID {
	return append([]providercredentials.ProviderID(nil), value...)
}

type channelAvailability map[string]bool

type transcriptionPricingFunc func(context.Context, audiopricing.TranscriptionPriceRequest) (audiopricing.TranscriptionEstimate, error)

func (function transcriptionPricingFunc) EstimateTranscription(ctx context.Context, request audiopricing.TranscriptionPriceRequest) (audiopricing.TranscriptionEstimate, error) {
	return function(ctx, request)
}

func (channelAvailability) ConfiguredProviders() []providercredentials.ProviderID { return nil }
func (value channelAvailability) ConfiguredChannel(_ context.Context, channelID string, _ providercredentials.ProviderID) bool {
	return value[channelID]
}

func TestModelsHandlerFiltersConfiguredProvidersAndUsesStableSchema(t *testing.T) {
	t.Parallel()
	handler := NewModelsHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), acceptingAuth(t), testRegistry(t), availability{providercredentials.XAI, providercredentials.OpenAI})
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer service-secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 || response.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("response = %d %v", response.Code, response.Header())
	}
	var list modelList
	if err := json.Unmarshal(response.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if list.Object != "list" || len(list.Data) != 2 || list.Data[0].ID != "gpt-image-1" || list.Data[1].ID != "grok-imagine-image-quality" {
		t.Fatalf("list = %+v", list)
	}
	for _, model := range list.Data {
		if model.Object != "model" || model.OwnedBy == "" {
			t.Fatalf("model = %+v", model)
		}
	}
}

func TestModelsHandlerUsesChannelCredentialAvailability(t *testing.T) {
	handler := NewModelsHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), acceptingAuth(t), testRegistry(t), channelAvailability{"channel_00000000000000000000000000000002": true})
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer service-secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var list modelList
	if err := json.Unmarshal(response.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if response.Code != 200 || len(list.Data) != 1 || list.Data[0].ID != "grok-imagine-image-quality" {
		t.Fatalf("response=%d list=%+v", response.Code, list)
	}
}

func TestModelsHandlerReturnsEmptyListWithoutProviderCredentials(t *testing.T) {
	t.Parallel()
	handler := NewModelsHandler(slog.Default(), acceptingAuth(t), testRegistry(t), availability{})
	response := modelsRequest(handler, http.MethodGet, true)
	if response.Code != 200 || response.Body.String() != "{\"object\":\"list\",\"data\":[]}\n" {
		t.Fatalf("response = %d %q", response.Code, response.Body.String())
	}
}

func TestModelsHandlerListsLogicalModelOnceForMultipleCandidates(t *testing.T) {
	registry, err := imageoperation.NewRegistry(imageoperation.ModelRoute{Protocol: "openai", Model: "logical-image", Owner: "gateway", Capabilities: []imageoperation.Capability{{Operation: imageoperation.Generate, MediaType: imageoperation.JSON}}, Policy: imageoperation.Priority, Candidates: []imageoperation.ChannelCandidate{
		{ID: "candidate_openai", Provider: providercredentials.OpenAI, ProviderModel: "openai-model", ChannelID: "channel_00000000000000000000000000000001", Enabled: true, Priority: 20},
		{ID: "candidate_xai", Provider: providercredentials.XAI, ProviderModel: "xai-model", ChannelID: "channel_00000000000000000000000000000002", Enabled: true, Priority: 10},
	}})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewModelsHandler(slog.Default(), acceptingAuth(t), registry, availability{providercredentials.OpenAI, providercredentials.XAI})
	response := modelsRequest(handler, http.MethodGet, true)
	var list modelList
	if err := json.Unmarshal(response.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if response.Code != 200 || len(list.Data) != 1 || list.Data[0].ID != "logical-image" {
		t.Fatalf("response=%d list=%+v", response.Code, list)
	}
}

func TestModelsHandlerIntersectsDispatchAvailabilityWithKeyPermissions(t *testing.T) {
	principal := apikey.Principal{ModelAccessMode: apikey.ModelAccessAllowlist, ModelPermissions: []apikey.ModelPermission{{Protocol: "openai", Operation: "image.edit", Model: "grok-imagine-image-quality"}}}
	handler := NewModelsHandler(slog.Default(), authFunc(func(context.Context, string) (apikey.Principal, error) { return principal, nil }), testRegistry(t), availability{providercredentials.OpenAI, providercredentials.XAI})
	response := modelsRequest(handler, http.MethodGet, true)
	var list modelList
	if err := json.Unmarshal(response.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if response.Code != 200 || len(list.Data) != 1 || list.Data[0].ID != "grok-imagine-image-quality" {
		t.Fatalf("response=%d list=%+v", response.Code, list)
	}
}

func TestModelsHandlerRequiresAuthenticationAndGET(t *testing.T) {
	t.Parallel()
	authCalls := 0
	handler := NewModelsHandler(slog.Default(), authFunc(func(context.Context, string) (apikey.Principal, error) { authCalls++; return apikey.Principal{}, nil }), testRegistry(t), availability{})
	if response := modelsRequest(handler, http.MethodGet, false); response.Code != 401 || authCalls != 0 {
		t.Fatalf("missing auth = %d/%d", response.Code, authCalls)
	}
	if response := modelsRequest(handler, http.MethodPost, true); response.Code != 405 || response.Header().Get("Allow") != "GET" || authCalls != 0 {
		t.Fatalf("method = %d/%s/%d", response.Code, response.Header().Get("Allow"), authCalls)
	}
}

func TestModelsHandlerMergesAuthorizedChatModels(t *testing.T) {
	chat, err := chatoperation.NewRegistry([]string{"gpt-4.1", "gpt-image-1"})
	if err != nil {
		t.Fatal(err)
	}
	principal := apikey.Principal{ModelAccessMode: apikey.ModelAccessAllowlist, ModelPermissions: []apikey.ModelPermission{{Protocol: "openai", Operation: "chat.completions", Model: "gpt-4.1"}, {Protocol: "openai", Operation: "image.generate", Model: "gpt-image-1"}}}
	handler := NewModelsHandlerWithChat(slog.Default(), authFunc(func(context.Context, string) (apikey.Principal, error) { return principal, nil }), testRegistry(t), chat, availability{providercredentials.OpenAI})
	response := modelsRequest(handler, http.MethodGet, true)
	var list modelList
	if err := json.Unmarshal(response.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if response.Code != 200 || len(list.Data) != 2 || list.Data[0].ID != "gpt-4.1" || list.Data[1].ID != "gpt-image-1" {
		t.Fatalf("list=%+v", list)
	}
}

func TestModelsHandlerListsLogicalChatWhenAnyCandidateIsConfigured(t *testing.T) {
	chat, err := chatoperation.NewRouteRegistry([]chatoperation.Route{{Model: "logical-chat", Owner: "gateway", Policy: chatoperation.Priority, Candidates: []chatoperation.Candidate{{ID: "candidate_openai", Provider: providercredentials.OpenAI, ProviderModel: "gpt", ChannelID: "channel_00000000000000000000000000000001", Enabled: true}, {ID: "candidate_xai", Provider: providercredentials.XAI, ProviderModel: "grok", ChannelID: "channel_00000000000000000000000000000002", Enabled: true}}}})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewModelsHandlerWithChat(slog.Default(), acceptingAuth(t), nil, chat, channelAvailability{"channel_00000000000000000000000000000002": true})
	response := modelsRequest(handler, http.MethodGet, true)
	var list modelList
	if err := json.Unmarshal(response.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if response.Code != 200 || len(list.Data) != 1 || list.Data[0].ID != "logical-chat" {
		t.Fatalf("list=%+v", list)
	}
}

func TestModelsHandlerIncludesAuthorizedConfiguredRunwayVideo(t *testing.T) {
	video, _ := videooperation.NewRegistry([]string{"logical-video"})
	principal := apikey.Principal{ModelAccessMode: apikey.ModelAccessAllowlist, ModelPermissions: []apikey.ModelPermission{{Protocol: "runway", Operation: "video.generate", Model: "logical-video"}}}
	handler := NewModelsHandlerWithAll(slog.Default(), authFunc(func(context.Context, string) (apikey.Principal, error) { return principal, nil }), nil, nil, nil, video, channelAvailability{"channel_00000000000000000000000000000007": true})
	response := modelsRequest(handler, http.MethodGet, true)
	var list modelList
	if err := json.Unmarshal(response.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || len(list.Data) != 1 || list.Data[0].ID != "logical-video" || list.Data[0].OwnedBy != "runway" {
		t.Fatalf("list=%+v", list)
	}
}

func TestModelsHandlerIncludesAuthorizedConfiguredSpeech(t *testing.T) {
	audio, _ := audiooperation.NewRegistry([]string{"tts-1"})
	principal := apikey.Principal{ModelAccessMode: apikey.ModelAccessAllowlist, ModelPermissions: []apikey.ModelPermission{{Protocol: "openai", Operation: audiooperation.Speech, Model: "tts-1"}}}
	handler := NewModelsHandlerWithAllAndAudio(slog.Default(), authFunc(func(context.Context, string) (apikey.Principal, error) { return principal, nil }), nil, nil, nil, nil, audio, channelAvailability{"channel_00000000000000000000000000000001": true})
	response := modelsRequest(handler, http.MethodGet, true)
	var list modelList
	if err := json.Unmarshal(response.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || len(list.Data) != 1 || list.Data[0].ID != "tts-1" || list.Data[0].OwnedBy != "openai" {
		t.Fatalf("list=%+v", list)
	}
}

func TestModelsHandlerIncludesAuthorizedConfiguredTranscription(t *testing.T) {
	transcriptions, _ := audiooperation.NewTranscriptionRegistry([]string{"gpt-4o-transcribe"}, nil)
	principal := apikey.Principal{ModelAccessMode: apikey.ModelAccessAllowlist, ModelPermissions: []apikey.ModelPermission{{Protocol: "openai", Operation: audiooperation.Transcription, Model: "gpt-4o-transcribe"}}}
	handler := NewModelsHandlerWithAllAudioOperations(slog.Default(), authFunc(func(context.Context, string) (apikey.Principal, error) { return principal, nil }), nil, nil, nil, nil, nil, transcriptions, channelAvailability{"channel_00000000000000000000000000000001": true})
	response := modelsRequest(handler, http.MethodGet, true)
	var list modelList
	if err := json.Unmarshal(response.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if response.Code != 200 || len(list.Data) != 1 || list.Data[0].ID != "gpt-4o-transcribe" {
		t.Fatalf("list=%+v", list)
	}
}

func TestModelsHandlerHidesManagedTranscriptionWithoutActivePrice(t *testing.T) {
	transcriptions, _ := audiooperation.NewTranscriptionRegistry([]string{"gpt-4o-transcribe"}, nil)
	principal := apikey.Principal{ModelAccessMode: apikey.ModelAccessAllowlist, ModelPermissions: []apikey.ModelPermission{{Protocol: "openai", Operation: audiooperation.Transcription, Model: "gpt-4o-transcribe"}}}
	handler := NewModelsHandlerWithAllAudioOperations(slog.Default(), authFunc(func(context.Context, string) (apikey.Principal, error) { return principal, nil }), nil, nil, nil, nil, nil, transcriptions, channelAvailability{"channel_00000000000000000000000000000001": true})
	handler.SetTranscriptionPricing(transcriptionPricingFunc(func(context.Context, audiopricing.TranscriptionPriceRequest) (audiopricing.TranscriptionEstimate, error) {
		return audiopricing.TranscriptionEstimate{}, audiopricing.ErrUnavailable
	}))
	response := modelsRequest(handler, http.MethodGet, true)
	var list modelList
	if err := json.Unmarshal(response.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if response.Code != 200 || len(list.Data) != 0 {
		t.Fatalf("list=%+v", list)
	}
}

func modelsRequest(handler http.Handler, method string, authenticate bool) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "/v1/models", nil)
	if authenticate {
		request.Header.Set("Authorization", "Bearer service-secret")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
