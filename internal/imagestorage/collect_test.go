package imagestorage

import (
	"context"
	"encoding/base64"
	"errors"
	"net/netip"
	"os"
	"testing"
)

type resolverFake struct {
	addresses []netip.Addr
	err       error
}

func (fake resolverFake) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return fake.addresses, fake.err
}

func collectorTestConfig(t *testing.T) Config {
	t.Helper()
	config := managedTestConfig("http://127.0.0.1:9000")
	config.TemporaryDirectory = t.TempDir()
	config.FetchOrigins = map[string][]string{"openai": {"https://images.example.com"}}
	return config
}

func TestCollectorDecodesBoundedBase64ToTemporaryFile(t *testing.T) {
	collector, err := NewCollector(collectorTestConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 520)...)
	collected, err := collector.DecodeBase64(base64.StdEncoding.EncodeToString(png), "image/png")
	if err != nil || collected.Size != int64(len(png)) || collected.ContentType != "image/png" || collected.Extension != "png" {
		t.Fatalf("collected=%+v err=%v", collected, err)
	}
	name := collected.File.Name()
	if err := collected.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(name); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary file remains: %v", err)
	}
}

func TestCollectorRejectsTypeMismatchAndOversizeBeforeDecode(t *testing.T) {
	config := collectorTestConfig(t)
	config.MaximumImageBytes = 16
	config.MaximumTotalBytes = 16
	collector, err := NewCollector(config)
	if err != nil {
		t.Fatal(err)
	}
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 520)...)
	if _, err := collector.DecodeBase64(base64.StdEncoding.EncodeToString(png), "image/jpeg"); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err=%v", err)
	}
	config.MaximumImageBytes = 1024
	config.MaximumTotalBytes = 1024
	collector, _ = NewCollector(config)
	if _, err := collector.DecodeBase64(base64.StdEncoding.EncodeToString(png), "image/jpeg"); !errors.Is(err, ErrInvalidContent) {
		t.Fatalf("err=%v", err)
	}
}

func TestCollectorAuthorizationRejectsPrivateDNSAndUnknownOrigin(t *testing.T) {
	collector, err := NewCollector(collectorTestConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	collector.resolver = resolverFake{addresses: []netip.Addr{netip.MustParseAddr("10.0.0.1")}}
	if _, _, err := collector.authorize(context.Background(), "openai", "https://images.example.com/result.png"); !errors.Is(err, ErrFetchRejected) {
		t.Fatalf("private err=%v", err)
	}
	collector.resolver = resolverFake{addresses: []netip.Addr{netip.MustParseAddr("8.8.8.8")}}
	for _, raw := range []string{"https://unknown.example.com/result.png", "https://user:secret@images.example.com/result.png", "http://images.example.com/result.png", "https://127.0.0.1/result.png"} {
		if _, _, err := collector.authorize(context.Background(), "openai", raw); !errors.Is(err, ErrFetchRejected) {
			t.Fatalf("raw=%q err=%v", raw, err)
		}
	}
	for _, address := range []string{"100.64.0.1", "192.0.2.1", "198.51.100.1", "203.0.113.1", "2001:db8::1"} {
		collector.resolver = resolverFake{addresses: []netip.Addr{netip.MustParseAddr(address)}}
		if _, _, err := collector.authorize(context.Background(), "openai", "https://images.example.com/result.png"); !errors.Is(err, ErrFetchRejected) {
			t.Fatalf("reserved=%s err=%v", address, err)
		}
	}
}

func TestCollectorCanonicalizesTrailingSlashOrigin(t *testing.T) {
	config := collectorTestConfig(t)
	config.FetchOrigins["openai"] = []string{"https://images.example.com/"}
	collector, err := NewCollector(config)
	if err != nil {
		t.Fatal(err)
	}
	collector.resolver = resolverFake{addresses: []netip.Addr{netip.MustParseAddr("8.8.8.8")}}
	if _, _, err := collector.authorize(context.Background(), "openai", "https://images.example.com/result.png"); err != nil {
		t.Fatalf("err=%v", err)
	}
}
