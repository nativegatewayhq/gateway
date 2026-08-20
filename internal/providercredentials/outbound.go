package providercredentials

import (
	"fmt"
	"net/http"
	"strings"
)

var sensitiveHeaders = map[string]struct{}{
	"authorization":       {},
	"proxy-authorization": {},
	"x-api-key":           {},
	"x-goog-api-key":      {},
	"cookie":              {},
}

var sensitiveQueryKeys = map[string]struct{}{
	"key":          {},
	"api_key":      {},
	"access_token": {},
	"token":        {},
}

// PrepareOutbound clones request, removes all inbound credential locations,
// and applies exactly one provider-scoped upstream credential.
func PrepareOutbound(request *http.Request, provider ProviderID, registry *Registry) (*http.Request, error) {
	if request == nil || request.URL == nil {
		return nil, fmt.Errorf("prepare outbound request: nil request")
	}
	if registry == nil {
		return nil, ErrCredentialUnavailable
	}
	credential, err := registry.Credential(provider)
	if err != nil {
		return nil, err
	}
	outbound := request.Clone(request.Context())
	urlCopy := *request.URL
	outbound.URL = &urlCopy
	outbound.Header = request.Header.Clone()
	if outbound.Header == nil {
		outbound.Header = make(http.Header)
	}
	outbound.URL.User = nil

	for header := range outbound.Header {
		if _, sensitive := sensitiveHeaders[strings.ToLower(header)]; sensitive {
			delete(outbound.Header, header)
		}
	}
	query := outbound.URL.Query()
	for key := range query {
		if _, sensitive := sensitiveQueryKeys[strings.ToLower(key)]; sensitive {
			query.Del(key)
		}
	}
	outbound.URL.RawQuery = query.Encode()
	if err := credential.Apply(outbound, provider); err != nil {
		return nil, err
	}
	return outbound, nil
}

// PrepareOutboundChannel applies the credential scoped to the routing
// decision's immutable Provider channel.
func PrepareOutboundChannel(request *http.Request, channelID string, provider ProviderID, registry *Registry) (*http.Request, error) {
	if request == nil || request.URL == nil {
		return nil, fmt.Errorf("prepare outbound request: nil request")
	}
	if registry == nil {
		return nil, ErrCredentialUnavailable
	}
	credential, err := registry.Resolve(request.Context(), channelID, provider)
	if err != nil {
		return nil, fmt.Errorf("%w: channel credential", ErrCredentialUnavailable)
	}
	defer credential.Destroy()
	outbound := request.Clone(request.Context())
	urlCopy := *request.URL
	outbound.URL = &urlCopy
	outbound.Header = request.Header.Clone()
	if outbound.Header == nil {
		outbound.Header = make(http.Header)
	}
	outbound.URL.User = nil
	for header := range outbound.Header {
		if _, sensitive := sensitiveHeaders[strings.ToLower(header)]; sensitive {
			delete(outbound.Header, header)
		}
	}
	query := outbound.URL.Query()
	for key := range query {
		if _, sensitive := sensitiveQueryKeys[strings.ToLower(key)]; sensitive {
			query.Del(key)
		}
	}
	outbound.URL.RawQuery = query.Encode()
	if err := credential.ApplyChannel(outbound, channelID, provider); err != nil {
		return nil, err
	}
	return outbound, nil
}

// ClearApplied removes the applied upstream credential as soon as the
// transport has consumed the request headers.
func ClearApplied(request *http.Request) {
	if request == nil {
		return
	}
	request.Header.Del("Authorization")
	request.Header.Del("x-goog-api-key")
	request.Header.Del("x-api-key")
}
