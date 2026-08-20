package apikey

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

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
