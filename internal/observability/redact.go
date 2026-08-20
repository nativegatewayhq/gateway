package observability

import (
	"net/url"
	"strings"
)

const redactedValue = "[REDACTED]"

var sensitiveQueryKeys = map[string]struct{}{
	"access_token": {},
	"api_key":      {},
	"key":          {},
	"token":        {},
	"x-api-key":    {},
}

// RedactURL returns a copy safe for diagnostic use. Request logging should
// still prefer route patterns and avoid URLs entirely.
func RedactURL(input *url.URL) *url.URL {
	if input == nil {
		return nil
	}

	redacted := *input
	query := redacted.Query()
	for key := range query {
		if _, sensitive := sensitiveQueryKeys[strings.ToLower(key)]; sensitive {
			query.Set(key, redactedValue)
		}
	}
	redacted.RawQuery = query.Encode()
	redacted.User = nil

	return &redacted
}
