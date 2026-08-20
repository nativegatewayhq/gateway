package apikey

import (
	"errors"
	"net/http"
	"strings"
)

var (
	ErrMissing   = errors.New("credential missing")
	ErrMalformed = errors.New("credential malformed")
	ErrAmbiguous = errors.New("multiple credential locations")
)

type candidate struct {
	present bool
	value   string
}

func Extract(request *http.Request) (string, error) {
	candidates := []candidate{
		headerCandidate(request.Header.Values("Authorization"), true),
		headerCandidate(request.Header.Values("x-api-key"), false),
		headerCandidate(request.Header.Values("x-goog-api-key"), false),
		queryCandidate(request.URL.Query()["key"]),
	}
	present := 0
	value := ""
	for _, item := range candidates {
		if !item.present {
			continue
		}
		present++
		if item.value == "" {
			return "", ErrMalformed
		}
		value = item.value
	}
	if present == 0 {
		return "", ErrMissing
	}
	if present != 1 {
		return "", ErrAmbiguous
	}
	if len(value) > maxKeyLength || hasControl(value) {
		return "", ErrMalformed
	}
	return value, nil
}

func headerCandidate(values []string, bearer bool) candidate {
	if len(values) == 0 {
		return candidate{}
	}
	if len(values) != 1 {
		return candidate{present: true}
	}
	value := values[0]
	if bearer {
		parts := strings.Split(value, " ")
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
			return candidate{present: true}
		}
		value = parts[1]
	}
	return candidate{present: true, value: value}
}

func queryCandidate(values []string) candidate {
	if len(values) == 0 {
		return candidate{}
	}
	if len(values) != 1 {
		return candidate{present: true}
	}
	return candidate{present: true, value: values[0]}
}

func hasControl(value string) bool {
	for _, char := range value {
		if char < 0x20 || char == 0x7f {
			return true
		}
	}
	return false
}
