// Package requestid validates, generates, and propagates request identifiers.
package requestid

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

const (
	HeaderName = "X-Request-Id"
	maxLength  = 128
)

type contextKey struct{}

// New generates a request ID using 128 bits of cryptographic randomness.
func New() string {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		panic("secure request ID generation failed")
	}
	return "req_" + hex.EncodeToString(random[:])
}

// Valid reports whether an untrusted inbound request ID is safe to propagate.
func Valid(value string) bool {
	if value == "" || len(value) > maxLength {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '-' || character == '_' || character == '.' || character == ':' {
			continue
		}
		return false
	}
	return true
}

// FromContext returns the request ID assigned by Middleware.
func FromContext(ctx context.Context) string {
	value, _ := ctx.Value(contextKey{}).(string)
	return value
}

// Middleware accepts safe caller IDs and replaces invalid values.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestID := request.Header.Get(HeaderName)
		if !Valid(requestID) {
			requestID = New()
		}

		writer.Header().Set(HeaderName, requestID)
		ctx := context.WithValue(request.Context(), contextKey{}, requestID)
		next.ServeHTTP(writer, request.WithContext(ctx))
	})
}
