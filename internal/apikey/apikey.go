// Package apikey implements service API key generation and authentication.
package apikey

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	keyPrefix      = "ngw_sk_"
	randomKeyBytes = 32
	maxKeyLength   = 512
)

var (
	ErrUnauthorized       = errors.New("unauthorized")
	ErrUnavailable        = errors.New("authentication unavailable")
	ErrProjectUnavailable = errors.New("project unavailable")
)

type Principal struct {
	APIKeyID       string
	ProjectID      string
	OrganizationID string
	RateLimit      RateLimitPolicy
	RateLimitState *RateLimitState
}

type RateLimitState struct {
	Limit     int64
	Remaining int64
	ResetAt   time.Time
}

type RateLimitPolicy struct {
	RequestsPerMinute int64
	Burst             int64
}

func (policy RateLimitPolicy) Enabled() bool { return policy.RequestsPerMinute > 0 }

func (policy RateLimitPolicy) Valid() bool {
	return (policy.RequestsPerMinute == 0 && policy.Burst == 0) ||
		(policy.RequestsPerMinute >= 1 && policy.RequestsPerMinute <= 1_000_000 && policy.Burst >= 1 && policy.Burst <= policy.RequestsPerMinute)
}

type Record struct {
	ID        string
	Name      string
	Digest    [32]byte
	Prefix    string
	ExpiresAt *time.Time
	ProjectID string
	RateLimit RateLimitPolicy
}

type Store interface {
	Create(context.Context, Record) error
	FindActiveByDigest(context.Context, [32]byte, time.Time) (Principal, error)
}

type Service struct {
	store Store
	now   func() time.Time
}

func NewService(store Store) *Service { return &Service{store: store, now: time.Now} }

func (service *Service) Authenticate(ctx context.Context, raw string) (Principal, error) {
	if len(raw) < len(keyPrefix)+1 || len(raw) > maxKeyLength || !strings.HasPrefix(raw, keyPrefix) {
		return Principal{}, ErrUnauthorized
	}
	digest := sha256.Sum256([]byte(raw))
	principal, err := service.store.FindActiveByDigest(ctx, digest, service.now())
	if err != nil {
		if errors.Is(err, ErrUnauthorized) {
			return Principal{}, ErrUnauthorized
		}
		return Principal{}, fmt.Errorf("%w: key lookup failed", ErrUnavailable)
	}
	return principal, nil
}

func Generate(reader io.Reader, name string, expiresAt *time.Time) (Record, string, error) {
	return GenerateForProject(reader, name, "project_legacy", expiresAt)
}

func GenerateForProject(reader io.Reader, name, projectID string, expiresAt *time.Time) (Record, string, error) {
	return GenerateForProjectWithPolicy(reader, name, projectID, expiresAt, RateLimitPolicy{})
}

func GenerateForProjectWithPolicy(reader io.Reader, name, projectID string, expiresAt *time.Time, policy RateLimitPolicy) (Record, string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 200 {
		return Record{}, "", fmt.Errorf("name must contain 1 to 200 characters")
	}
	if !strings.HasPrefix(projectID, "project_") || len(projectID) > 128 || strings.TrimSpace(projectID) != projectID {
		return Record{}, "", ErrProjectUnavailable
	}
	if !policy.Valid() {
		return Record{}, "", fmt.Errorf("rate limit policy is invalid")
	}
	secret := make([]byte, randomKeyBytes)
	if _, err := io.ReadFull(reader, secret); err != nil {
		return Record{}, "", fmt.Errorf("generate key: %w", err)
	}
	raw := keyPrefix + base64.RawURLEncoding.EncodeToString(secret)
	digest := sha256.Sum256([]byte(raw))
	idBytes := make([]byte, 16)
	if _, err := io.ReadFull(reader, idBytes); err != nil {
		return Record{}, "", fmt.Errorf("generate key id: %w", err)
	}
	record := Record{ID: "key_" + hex.EncodeToString(idBytes), Name: name, Digest: digest, Prefix: raw[:min(len(raw), 14)], ExpiresAt: expiresAt, ProjectID: projectID, RateLimit: policy}
	return record, raw, nil
}
