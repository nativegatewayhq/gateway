// Package clientip resolves a request source across explicitly trusted proxies.
package clientip

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
)

const (
	maxForwardedBytes = 8192
	maxForwardedHops  = 32
)

var (
	ErrUnavailable = errors.New("client IP unavailable")
	ErrAmbiguous   = errors.New("client IP ambiguous")
)

type Resolver struct{ trusted []netip.Prefix }

func New(trusted []netip.Prefix) (*Resolver, error) {
	canonical, err := CanonicalPrefixes(trusted, 128)
	if err != nil {
		return nil, err
	}
	return &Resolver{trusted: canonical}, nil
}

func (resolver *Resolver) Resolve(request *http.Request) (netip.Addr, error) {
	peer, err := parseRemoteAddr(request.RemoteAddr)
	if err != nil {
		return netip.Addr{}, ErrUnavailable
	}
	if !contains(resolver.trusted, peer) {
		return peer, nil
	}
	forwarded := strings.TrimSpace(strings.Join(request.Header.Values("Forwarded"), ","))
	xff := strings.TrimSpace(strings.Join(request.Header.Values("X-Forwarded-For"), ","))
	if forwarded != "" && xff != "" {
		return netip.Addr{}, ErrAmbiguous
	}
	if forwarded == "" && xff == "" {
		return netip.Addr{}, ErrUnavailable
	}
	if len(forwarded)+len(xff) > maxForwardedBytes {
		return netip.Addr{}, ErrUnavailable
	}
	var chain []netip.Addr
	if forwarded != "" {
		chain, err = parseForwarded(forwarded)
	} else {
		chain, err = parseXForwardedFor(xff)
	}
	if err != nil || len(chain) == 0 || len(chain) > maxForwardedHops {
		return netip.Addr{}, ErrUnavailable
	}
	chain = append(chain, peer)
	for index := len(chain) - 1; index >= 0; index-- {
		if contains(resolver.trusted, chain[index]) {
			continue
		}
		return chain[index], nil
	}
	return netip.Addr{}, ErrUnavailable
}

func parseRemoteAddr(value string) (netip.Addr, error) {
	host, port, err := net.SplitHostPort(value)
	if err != nil || port == "" || strings.Contains(host, "%") {
		return netip.Addr{}, ErrUnavailable
	}
	address, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, ErrUnavailable
	}
	return address.Unmap(), nil
}

func parseXForwardedFor(value string) ([]netip.Addr, error) {
	parts := strings.Split(value, ",")
	if len(parts) > maxForwardedHops {
		return nil, ErrUnavailable
	}
	addresses := make([]netip.Addr, 0, len(parts))
	for _, part := range parts {
		address, err := parseForwardedAddress(strings.TrimSpace(part))
		if err != nil {
			return nil, err
		}
		addresses = append(addresses, address)
	}
	return addresses, nil
}

func parseForwarded(value string) ([]netip.Addr, error) {
	elements, err := splitQuoted(value, ',')
	if err != nil || len(elements) > maxForwardedHops {
		return nil, ErrUnavailable
	}
	addresses := make([]netip.Addr, 0, len(elements))
	for _, element := range elements {
		parameters, splitErr := splitQuoted(element, ';')
		if splitErr != nil {
			return nil, ErrUnavailable
		}
		found := false
		for _, parameter := range parameters {
			name, raw, ok := strings.Cut(parameter, "=")
			if !ok || strings.TrimSpace(name) == "" {
				return nil, ErrUnavailable
			}
			if !strings.EqualFold(strings.TrimSpace(name), "for") {
				continue
			}
			if found {
				return nil, ErrAmbiguous
			}
			found = true
			address, parseErr := parseForwardedAddress(strings.TrimSpace(raw))
			if parseErr != nil {
				return nil, parseErr
			}
			addresses = append(addresses, address)
		}
		if !found {
			return nil, ErrUnavailable
		}
	}
	return addresses, nil
}

func splitQuoted(value string, delimiter byte) ([]string, error) {
	parts := []string{}
	start, quoted, escaped := 0, false, false
	for index := 0; index < len(value); index++ {
		character := value[index]
		if escaped {
			escaped = false
			continue
		}
		if quoted && character == '\\' {
			escaped = true
			continue
		}
		if character == '"' {
			quoted = !quoted
			continue
		}
		if character == delimiter && !quoted {
			part := strings.TrimSpace(value[start:index])
			if part == "" {
				return nil, ErrUnavailable
			}
			parts = append(parts, part)
			start = index + 1
		}
	}
	if quoted || escaped {
		return nil, ErrUnavailable
	}
	part := strings.TrimSpace(value[start:])
	if part == "" {
		return nil, ErrUnavailable
	}
	return append(parts, part), nil
}

func parseForwardedAddress(value string) (netip.Addr, error) {
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		value = value[1 : len(value)-1]
		if strings.Contains(value, "\\") {
			return netip.Addr{}, ErrUnavailable
		}
	}
	if value == "" || strings.EqualFold(value, "unknown") || strings.HasPrefix(value, "_") || strings.Contains(value, "%") {
		return netip.Addr{}, ErrUnavailable
	}
	if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
		value = value[1 : len(value)-1]
	}
	address, err := netip.ParseAddr(value)
	if err != nil {
		return netip.Addr{}, ErrUnavailable
	}
	return address.Unmap(), nil
}

func CanonicalPrefixes(prefixes []netip.Prefix, maximum int) ([]netip.Prefix, error) {
	if maximum < 1 || len(prefixes) > maximum {
		return nil, ErrUnavailable
	}
	canonical := make([]netip.Prefix, 0, len(prefixes))
	for _, prefix := range prefixes {
		if !prefix.IsValid() {
			return nil, ErrUnavailable
		}
		address := prefix.Addr()
		bits := prefix.Bits()
		if address.Is4In6() {
			if bits < 96 {
				return nil, ErrUnavailable
			}
			address, bits = address.Unmap(), bits-96
		} else {
			address = address.Unmap()
		}
		prefix = netip.PrefixFrom(address, bits).Masked()
		contained := false
		for _, existing := range canonical {
			if existing.Contains(prefix.Addr()) && existing.Bits() <= prefix.Bits() {
				contained = true
				break
			}
		}
		if contained {
			continue
		}
		kept := canonical[:0]
		for _, existing := range canonical {
			if prefix.Contains(existing.Addr()) && prefix.Bits() <= existing.Bits() {
				continue
			}
			kept = append(kept, existing)
		}
		canonical = append(kept, prefix)
	}
	slices.SortFunc(canonical, func(left, right netip.Prefix) int {
		if compared := left.Addr().Compare(right.Addr()); compared != 0 {
			return compared
		}
		return left.Bits() - right.Bits()
	})
	return canonical, nil
}

func contains(prefixes []netip.Prefix, address netip.Addr) bool {
	address = address.Unmap()
	for _, prefix := range prefixes {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

type resolution struct {
	address netip.Addr
	err     error
}
type contextKey struct{}

func Middleware(resolver *Resolver, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		address, err := resolver.Resolve(request)
		ctx := context.WithValue(request.Context(), contextKey{}, resolution{address: address, err: err})
		next.ServeHTTP(writer, request.WithContext(ctx))
	})
}

func (resolver *Resolver) Middleware(next http.Handler) http.Handler {
	return Middleware(resolver, next)
}

func FromContext(ctx context.Context) (netip.Addr, error) {
	value, ok := ctx.Value(contextKey{}).(resolution)
	if !ok {
		return netip.Addr{}, ErrUnavailable
	}
	return value.address, value.err
}
