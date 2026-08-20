package httpserver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nativegatewayhq/gateway/internal/apikey"
	"github.com/nativegatewayhq/gateway/internal/requestid"
)

type stubAuthenticator struct {
	principal apikey.Principal
	err       error
}

func (stub stubAuthenticator) Authenticate(_ context.Context, _ string) (apikey.Principal, error) {
	return stub.principal, stub.err
}

func TestAuthenticateAddsPrincipal(t *testing.T) {
	var got apikey.Principal
	next := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		got, _ = PrincipalFromContext(request.Context())
		writer.WriteHeader(http.StatusNoContent)
	})
	handler := requestid.Middleware(Authenticate(stubAuthenticator{principal: apikey.Principal{APIKeyID: "key_1"}}, next))
	request := httptest.NewRequest("GET", "/protected", nil)
	request.Header.Set("x-api-key", "ngw_sk_secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || got.APIKeyID != "key_1" {
		t.Fatalf("status=%d principal=%+v", response.Code, got)
	}
}

func TestAuthenticateSafeErrors(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(*http.Request)
		auth   stubAuthenticator
		status int
		code   string
	}{
		{"missing", func(*http.Request) {}, stubAuthenticator{}, http.StatusUnauthorized, "unauthorized"},
		{"ambiguous", func(r *http.Request) {
			r.Header.Set("x-api-key", "ngw_sk_secret")
			r.Header.Set("x-goog-api-key", "ngw_sk_secret")
		}, stubAuthenticator{}, http.StatusBadRequest, "ambiguous_credentials"},
		{"unknown", func(r *http.Request) { r.Header.Set("x-api-key", "ngw_sk_secret") }, stubAuthenticator{err: apikey.ErrUnauthorized}, http.StatusUnauthorized, "unauthorized"},
		{"store unavailable", func(r *http.Request) { r.Header.Set("x-api-key", "ngw_sk_secret") }, stubAuthenticator{err: errors.New("wrapped: authentication unavailable")}, http.StatusUnauthorized, "unauthorized"},
		{"typed unavailable", func(r *http.Request) { r.Header.Set("x-api-key", "ngw_sk_secret") }, stubAuthenticator{err: apikey.ErrUnavailable}, http.StatusServiceUnavailable, "authentication_unavailable"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := requestid.Middleware(Authenticate(test.auth, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("next called") })))
			request := httptest.NewRequest("GET", "/protected?safe=true", nil)
			test.setup(request)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status || !strings.Contains(response.Body.String(), test.code) {
				t.Fatalf("response=%d %s", response.Code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), "ngw_sk_secret") {
				t.Fatal("response leaked key")
			}
		})
	}
}
