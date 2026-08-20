// Package networkauth enforces API key source-network restrictions.
package networkauth

import (
	"context"
	"errors"
	"net/netip"

	"github.com/nativegatewayhq/gateway/internal/apikey"
	"github.com/nativegatewayhq/gateway/internal/clientip"
)

var ErrDenied = errors.New("client network not allowed")

type DeniedError struct {
	APIKeyID  string
	ProjectID string
	ClientIP  netip.Addr
}

func (err *DeniedError) Error() string { return ErrDenied.Error() }
func (err *DeniedError) Unwrap() error { return ErrDenied }

type Authenticator interface {
	Authenticate(context.Context, string) (apikey.Principal, error)
}

type GuardedAuthenticator struct{ authenticator Authenticator }

func NewGuardedAuthenticator(authenticator Authenticator) (*GuardedAuthenticator, error) {
	if authenticator == nil {
		return nil, errors.New("network authenticator dependency required")
	}
	return &GuardedAuthenticator{authenticator: authenticator}, nil
}

func (guard *GuardedAuthenticator) Authenticate(ctx context.Context, raw string) (apikey.Principal, error) {
	principal, err := guard.authenticator.Authenticate(ctx, raw)
	if err != nil || principal.NetworkAccessMode == "" || principal.NetworkAccessMode == apikey.NetworkAccessAll {
		return principal, err
	}
	address, resolutionErr := clientip.FromContext(ctx)
	if resolutionErr != nil || !principal.AuthorizeNetwork(address) {
		return apikey.Principal{}, &DeniedError{APIKeyID: principal.APIKeyID, ProjectID: principal.ProjectID, ClientIP: address}
	}
	return principal, nil
}
