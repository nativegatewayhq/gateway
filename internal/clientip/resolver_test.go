package clientip

import (
	"errors"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestUntrustedPeerIgnoresForwardingHeaders(t *testing.T) {
	resolver, _ := New([]netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")})
	request := httptest.NewRequest("GET", "/", nil)
	request.RemoteAddr = "198.51.100.9:443"
	request.Header.Set("X-Forwarded-For", "192.0.2.5")
	address, err := resolver.Resolve(request)
	if err != nil || address.String() != "198.51.100.9" {
		t.Fatalf("got %v, %v", address, err)
	}
}

func TestTrustedProxyStripsTrustedHopsRightToLeft(t *testing.T) {
	resolver, _ := New([]netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")})
	request := httptest.NewRequest("GET", "/", nil)
	request.RemoteAddr = "10.0.0.3:443"
	request.Header.Set("Forwarded", `for=192.0.2.7, for="10.0.0.2"`)
	address, err := resolver.Resolve(request)
	if err != nil || address.String() != "192.0.2.7" {
		t.Fatalf("got %v, %v", address, err)
	}
}

func TestTrustedProxyRejectsAmbiguousOrMissingChain(t *testing.T) {
	resolver, _ := New([]netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")})
	request := httptest.NewRequest("GET", "/", nil)
	request.RemoteAddr = "10.0.0.3:443"
	request.Header.Set("Forwarded", "for=192.0.2.7")
	request.Header.Set("X-Forwarded-For", "192.0.2.7")
	if _, err := resolver.Resolve(request); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("got %v", err)
	}
	request.Header.Del("Forwarded")
	request.Header.Del("X-Forwarded-For")
	if _, err := resolver.Resolve(request); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v", err)
	}
}

func TestCanonicalPrefixesNormalizesAndRemovesContainedPrefixes(t *testing.T) {
	prefixes, err := CanonicalPrefixes([]netip.Prefix{
		netip.MustParsePrefix("192.0.2.99/24"),
		netip.MustParsePrefix("192.0.2.128/25"),
		netip.MustParsePrefix("::ffff:198.51.100.0/120"),
	}, 8)
	if err != nil || len(prefixes) != 2 || prefixes[0].String() != "192.0.2.0/24" || prefixes[1].String() != "198.51.100.0/24" {
		t.Fatalf("got %v, %v", prefixes, err)
	}
}
