package requestid

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNew(t *testing.T) {
	t.Parallel()

	first := New()
	second := New()
	if !Valid(first) || !strings.HasPrefix(first, "req_") {
		t.Fatalf("New() = %q, want valid req_ ID", first)
	}
	if first == second {
		t.Fatalf("two generated IDs matched: %q", first)
	}
}

func TestValid(t *testing.T) {
	t.Parallel()

	tests := map[string]bool{
		"req_abc-123.test:value": true,
		"":                       false,
		"contains space":         false,
		"contains/newline\n":     false,
		strings.Repeat("a", 129): false,
	}
	for value, want := range tests {
		if got := Valid(value); got != want {
			t.Errorf("Valid(%q) = %v, want %v", value, got, want)
		}
	}
}

func TestMiddleware(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name     string
		incoming string
		want     string
	}{
		{name: "accepts safe ID", incoming: "client-request-1", want: "client-request-1"},
		{name: "replaces invalid ID", incoming: "secret\nvalue"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var contextID string
			handler := Middleware(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				contextID = FromContext(request.Context())
				writer.WriteHeader(http.StatusNoContent)
			}))
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			request.Header.Set(HeaderName, tt.incoming)
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			got := response.Header().Get(HeaderName)
			if tt.want != "" && got != tt.want {
				t.Fatalf("response request ID = %q, want %q", got, tt.want)
			}
			if !Valid(got) || contextID != got {
				t.Fatalf("invalid propagation: header=%q context=%q", got, contextID)
			}
		})
	}
}
