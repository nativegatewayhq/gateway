package apikey

import (
	"bytes"
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestGenerateCanonicalNetworkAllowlist(t *testing.T) {
	record, _, err := GenerateForProjectWithAccess(bytes.NewReader(make([]byte, 64)), "networked", "project_test", nil, RateLimitPolicy{}, nil, []netip.Prefix{netip.MustParsePrefix("192.0.2.8/24"), netip.MustParsePrefix("192.0.2.0/25")})
	principal := Principal{NetworkAccessMode: record.NetworkAccessMode, NetworkPrefixes: record.NetworkPrefixes}
	if err != nil || record.NetworkAccessMode != NetworkAccessAllowlist || len(record.NetworkPrefixes) != 1 || record.NetworkPrefixes[0].String() != "192.0.2.0/24" || !principal.AuthorizeNetwork(netip.MustParseAddr("192.0.2.9")) {
		t.Fatalf("got %#v, %v", record, err)
	}
}

type memoryStore struct {
	record Record
	err    error
}

func (store *memoryStore) Create(_ context.Context, record Record) error {
	store.record = record
	return store.err
}
func (store *memoryStore) FindActiveByDigest(_ context.Context, digest [32]byte, _ time.Time) (Principal, error) {
	if store.err != nil {
		return Principal{}, store.err
	}
	if digest != store.record.Digest {
		return Principal{}, ErrUnauthorized
	}
	return Principal{APIKeyID: store.record.ID}, nil
}

func TestGenerateAndAuthenticate(t *testing.T) {
	entropy := bytes.Repeat([]byte{7}, randomKeyBytes+16)
	record, raw, err := Generate(bytes.NewReader(entropy), "test key", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(raw, keyPrefix) || strings.Contains(string(record.Digest[:]), raw) {
		t.Fatalf("unsafe generated result")
	}
	store := &memoryStore{record: record}
	principal, err := NewService(store).Authenticate(context.Background(), raw)
	if err != nil || principal.APIKeyID != record.ID {
		t.Fatalf("Authenticate() = %+v, %v", principal, err)
	}
	if _, err := NewService(store).Authenticate(context.Background(), raw+"wrong"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("error = %v", err)
	}
}

func TestGenerateForProjectWithRateLimitPolicy(t *testing.T) {
	record, _, err := GenerateForProjectWithPolicy(bytes.NewReader(make([]byte, 64)), "limited", "project_test", nil, RateLimitPolicy{RequestsPerMinute: 60, Burst: 10})
	if err != nil || record.RateLimit.RequestsPerMinute != 60 || record.RateLimit.Burst != 10 {
		t.Fatalf("record=%+v error=%v", record, err)
	}
	for _, policy := range []RateLimitPolicy{{RequestsPerMinute: 60}, {Burst: 1}, {RequestsPerMinute: 10, Burst: 11}, {RequestsPerMinute: 1_000_001, Burst: 1}} {
		if _, _, err := GenerateForProjectWithPolicy(bytes.NewReader(make([]byte, 64)), "invalid", "project_test", nil, policy); err == nil {
			t.Fatalf("accepted policy=%+v", policy)
		}
	}
}

func TestModelPermissionsCanonicalizationAndAuthorization(t *testing.T) {
	permissions, err := CanonicalModelPermissions([]ModelPermission{
		{Protocol: "openai", Operation: "image.edit", Model: "gpt-image-1"},
		{Protocol: " gemini ", Operation: " image.generate ", Model: " gemini-image "},
		{Protocol: "openai", Operation: "image.edit", Model: "gpt-image-1"},
	})
	if err != nil || len(permissions) != 2 || permissions[0].Protocol != "gemini" || permissions[1].Operation != "image.edit" {
		t.Fatalf("permissions=%+v error=%v", permissions, err)
	}
	principal := Principal{ModelAccessMode: ModelAccessAllowlist, ModelPermissions: permissions}
	if !principal.AuthorizeModel("gemini", "image.generate", "gemini-image") || !principal.AuthorizeModel("openai", "image.edit", "gpt-image-1") {
		t.Fatal("expected exact permissions denied")
	}
	for _, request := range []ModelPermission{{"openai", "image.generate", "gpt-image-1"}, {"openai", "image.edit", "gpt-image-*"}, {"gemini", "image.generate", "Gemini-image"}} {
		if principal.AuthorizeModel(request.Protocol, request.Operation, request.Model) {
			t.Fatalf("unexpected permission=%+v", request)
		}
	}
	if !(Principal{}).AuthorizeModel("openai", "image.generate", "any") || !(Principal{ModelAccessMode: ModelAccessAll}).AuthorizeModel("gemini", "image.generate", "any") {
		t.Fatal("default all denied")
	}
	if (Principal{ModelAccessMode: ModelAccessAllowlist}).AuthorizeModel("openai", "image.generate", "any") {
		t.Fatal("empty allowlist allowed")
	}
}

func TestModelPermissionsRejectInvalidAndExcessiveValues(t *testing.T) {
	for _, permission := range []ModelPermission{{"anthropic", "image.generate", "model"}, {"openai", "chat", "model"}, {"gemini", "image.edit", "model"}, {"openai", "image.generate", ""}, {"openai", "image.generate", strings.Repeat("m", 201)}} {
		if _, err := CanonicalModelPermissions([]ModelPermission{permission}); !errors.Is(err, ErrPolicyInvalid) {
			t.Fatalf("permission=%+v error=%v", permission, err)
		}
	}
	excessive := make([]ModelPermission, maxModelPermissions+1)
	if _, err := CanonicalModelPermissions(excessive); !errors.Is(err, ErrPolicyInvalid) {
		t.Fatalf("error=%v", err)
	}
}

func TestGenerateRejectsInvalidNameAndEntropyFailure(t *testing.T) {
	if _, _, err := Generate(bytes.NewReader(nil), "", nil); err == nil {
		t.Fatal("empty name accepted")
	}
	if _, _, err := Generate(bytes.NewReader(nil), "valid", nil); err == nil {
		t.Fatal("entropy failure accepted")
	}
}

func TestAuthenticateHidesStoreFailure(t *testing.T) {
	record, raw, err := Generate(bytes.NewReader(bytes.Repeat([]byte{1}, randomKeyBytes+16)), "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewService(&memoryStore{record: record, err: errors.New("database secret")}).Authenticate(context.Background(), raw)
	if !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "database secret") {
		t.Fatalf("error = %v", err)
	}
}
