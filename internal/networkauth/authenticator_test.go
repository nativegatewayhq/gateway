package networkauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/nativegatewayhq/gateway/internal/apikey"
	"github.com/nativegatewayhq/gateway/internal/clientip"
)

type stubAuthenticator struct{ principal apikey.Principal }

func (stub stubAuthenticator) Authenticate(context.Context, string) (apikey.Principal, error) {
	return stub.principal, nil
}

func resolvedContext(t *testing.T, address string) context.Context {
	t.Helper()
	resolver, _ := clientip.New(nil)
	request := httptest.NewRequest("GET", "/", nil)
	request.RemoteAddr = netip.MustParseAddr(address).String() + ":443"
	var captured context.Context
	resolver.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) { captured = request.Context() })).ServeHTTP(httptest.NewRecorder(), request)
	return captured
}

func TestGuardAllowsMatchingNetworkAndDeniesOtherNetwork(t *testing.T) {
	principal := apikey.Principal{APIKeyID: "key_test", ProjectID: "project_test", NetworkAccessMode: apikey.NetworkAccessAllowlist, NetworkPrefixes: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}}
	guard, _ := NewGuardedAuthenticator(stubAuthenticator{principal})
	if _, err := guard.Authenticate(resolvedContext(t, "192.0.2.4"), "key"); err != nil {
		t.Fatal(err)
	}
	_, err := guard.Authenticate(resolvedContext(t, "198.51.100.4"), "key")
	var denied *DeniedError
	if !errors.As(err, &denied) || denied.APIKeyID != "key_test" || denied.ClientIP.String() != "198.51.100.4" {
		t.Fatalf("got %#v", err)
	}
}

func TestGuardFailsClosedOnlyForRestrictedKeys(t *testing.T) {
	restricted, _ := NewGuardedAuthenticator(stubAuthenticator{apikey.Principal{NetworkAccessMode: apikey.NetworkAccessAllowlist, NetworkPrefixes: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}}})
	if _, err := restricted.Authenticate(context.Background(), "key"); !errors.Is(err, ErrDenied) {
		t.Fatalf("got %v", err)
	}
	unrestricted, _ := NewGuardedAuthenticator(stubAuthenticator{apikey.Principal{NetworkAccessMode: apikey.NetworkAccessAll}})
	if _, err := unrestricted.Authenticate(context.Background(), "key"); err != nil {
		t.Fatal(err)
	}
}
