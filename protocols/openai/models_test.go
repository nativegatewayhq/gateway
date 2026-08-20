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
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
)

type availability []providercredentials.ProviderID

func (value availability) ConfiguredProviders() []providercredentials.ProviderID {
	return append([]providercredentials.ProviderID(nil), value...)
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

func TestModelsHandlerReturnsEmptyListWithoutProviderCredentials(t *testing.T) {
	t.Parallel()
	handler := NewModelsHandler(slog.Default(), acceptingAuth(t), testRegistry(t), availability{})
	response := modelsRequest(handler, http.MethodGet, true)
	if response.Code != 200 || response.Body.String() != "{\"object\":\"list\",\"data\":[]}\n" {
		t.Fatalf("response = %d %q", response.Code, response.Body.String())
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

func modelsRequest(handler http.Handler, method string, authenticate bool) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "/v1/models", nil)
	if authenticate {
		request.Header.Set("Authorization", "Bearer service-secret")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
