package httpserver

import (
	"context"
	"errors"
	"net/http"

	"github.com/nativegatewayhq/gateway/internal/apikey"
)

type Authenticator interface {
	Authenticate(context.Context, string) (apikey.Principal, error)
}

type principalContextKey struct{}

func PrincipalFromContext(ctx context.Context) (apikey.Principal, bool) {
	principal, ok := ctx.Value(principalContextKey{}).(apikey.Principal)
	return principal, ok
}

// Authenticate protects a protocol route. Health routes deliberately do not use it.
func Authenticate(authenticator Authenticator, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		raw, err := apikey.Extract(request)
		if err != nil {
			if errors.Is(err, apikey.ErrAmbiguous) {
				writeError(writer, request, http.StatusBadRequest, "ambiguous_credentials", "multiple credential locations are not allowed")
				return
			}
			writeError(writer, request, http.StatusUnauthorized, "unauthorized", "authentication required")
			return
		}
		principal, err := authenticator.Authenticate(request.Context(), raw)
		if err != nil {
			if errors.Is(err, apikey.ErrUnavailable) {
				writeError(writer, request, http.StatusServiceUnavailable, "authentication_unavailable", "authentication service unavailable")
				return
			}
			writeError(writer, request, http.StatusUnauthorized, "unauthorized", "authentication required")
			return
		}
		ctx := context.WithValue(request.Context(), principalContextKey{}, principal)
		next.ServeHTTP(writer, request.WithContext(ctx))
	})
}
