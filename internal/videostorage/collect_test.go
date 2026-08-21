package videostorage

import (
	"context"
	"errors"
	"net/netip"
	"testing"
)

type resolverFake struct {
	addresses []netip.Addr
	err       error
}

func (fake resolverFake) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return fake.addresses, fake.err
}

func TestCollectorAuthorizesExactPublicOriginAndPinsAddresses(t *testing.T) {
	collector := &Collector{config: testConfig(), resolver: resolverFake{addresses: []netip.Addr{netip.MustParseAddr("8.8.8.8")}}}
	target, addresses, err := collector.authorize(context.Background(), "https://runway.example/output/task.mp4?token=secret")
	if err != nil || target.Host != "runway.example" || len(addresses) != 1 || addresses[0].String() != "8.8.8.8" {
		t.Fatalf("target=%v addresses=%v err=%v", target, addresses, err)
	}
	for _, raw := range []string{"http://runway.example/output.mp4", "https://evil.example/output.mp4", "https://user@runway.example/output.mp4", "https://127.0.0.1/output.mp4"} {
		if _, _, err = collector.authorize(context.Background(), raw); !errors.Is(err, ErrFetchRejected) {
			t.Fatalf("raw=%s err=%v", raw, err)
		}
	}
	collector.resolver = resolverFake{addresses: []netip.Addr{netip.MustParseAddr("10.0.0.1")}}
	if _, _, err = collector.authorize(context.Background(), "https://runway.example/output.mp4"); !errors.Is(err, ErrFetchRejected) {
		t.Fatalf("private resolution err=%v", err)
	}
}

func TestVideoSignaturesMustMatchDeclaredType(t *testing.T) {
	mp4 := []byte{0, 0, 0, 20, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}
	webm := []byte{0x1a, 0x45, 0xdf, 0xa3}
	if !matchesVideoSignature("video/mp4", mp4) || !matchesVideoSignature("video/quicktime", mp4) || !matchesVideoSignature("video/webm", webm) {
		t.Fatal("valid signature rejected")
	}
	if matchesVideoSignature("video/mp4", webm) || matchesVideoSignature("video/webm", mp4) || matchesVideoSignature("application/octet-stream", mp4) {
		t.Fatal("mismatched signature accepted")
	}
}
