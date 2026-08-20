// Package imagestorage safely persists generated image results.
package imagestorage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
)

type Mode string

const (
	Provider Mode = "provider"
	Managed  Mode = "managed"
)

var (
	ErrInvalidConfig = errors.New("invalid image storage configuration")
	ErrUnavailable   = errors.New("image storage unavailable")
	ErrInvalidObject = errors.New("invalid image storage object")
	keyPartPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
)

type Config struct {
	Mode               Mode
	Endpoint           string
	Region             string
	Bucket             string
	AccessKeyID        string
	SecretAccessKey    string
	CDNBaseURL         string
	MaximumImages      int
	MaximumImageBytes  int64
	MaximumTotalBytes  int64
	FetchTimeout       time.Duration
	UploadTimeout      time.Duration
	TemporaryDirectory string
	FetchOrigins       map[string][]string
}

func DefaultConfig() Config {
	return Config{Mode: Provider, MaximumImages: 10, MaximumImageBytes: 32 << 20, MaximumTotalBytes: 64 << 20, FetchTimeout: 30 * time.Second, UploadTimeout: time.Minute}
}

func (config Config) Validate() error {
	if config.Mode != Provider && config.Mode != Managed {
		return ErrInvalidConfig
	}
	if config.Mode == Provider {
		return nil
	}
	if strings.TrimSpace(config.Region) == "" || !keyPartPattern.MatchString(config.Bucket) || strings.TrimSpace(config.AccessKeyID) == "" || strings.TrimSpace(config.SecretAccessKey) == "" {
		return ErrInvalidConfig
	}
	endpoint, err := parseServiceURL(config.Endpoint, true)
	if err != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.User != nil {
		return ErrInvalidConfig
	}
	cdn, err := parseServiceURL(config.CDNBaseURL, false)
	if err != nil || cdn.RawQuery != "" || cdn.Fragment != "" || cdn.User != nil {
		return ErrInvalidConfig
	}
	if config.MaximumImages < 1 || config.MaximumImages > 100 || config.MaximumImageBytes < 1 || config.MaximumImageBytes > 256<<20 || config.MaximumTotalBytes < config.MaximumImageBytes || config.MaximumTotalBytes > 512<<20 {
		return ErrInvalidConfig
	}
	if config.FetchTimeout <= 0 || config.FetchTimeout > 5*time.Minute || config.UploadTimeout <= 0 || config.UploadTimeout > 10*time.Minute {
		return ErrInvalidConfig
	}
	for provider, origins := range config.FetchOrigins {
		if !keyPartPattern.MatchString(provider) || len(origins) > 32 {
			return ErrInvalidConfig
		}
		for _, origin := range origins {
			parsed, err := url.Parse(origin)
			if err != nil || parsed.Scheme != "https" || parsed.Host == "" || (parsed.Port() != "" && parsed.Port() != "443") || parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
				return ErrInvalidConfig
			}
		}
	}
	return nil
}

func parseServiceURL(raw string, allowLoopbackHTTP bool) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" {
		return nil, ErrInvalidConfig
	}
	if parsed.Scheme == "https" {
		return parsed, nil
	}
	if allowLoopbackHTTP && parsed.Scheme == "http" && (parsed.Hostname() == "127.0.0.1" || parsed.Hostname() == "::1" || parsed.Hostname() == "localhost") {
		return parsed, nil
	}
	return nil, ErrInvalidConfig
}

type Object struct {
	Key         string
	ContentType string
	Size        int64
	SHA256      [sha256.Size]byte
}

type StoredObject struct {
	Key         string
	URL         string
	ContentType string
	Size        int64
	SHA256      [sha256.Size]byte
}

type ObjectStore interface {
	Put(context.Context, Object, io.Reader) (StoredObject, error)
	Ready(context.Context) error
}

func ObjectKey(protocol, chargeOrRequestID string, resultIndex int, digest [sha256.Size]byte, extension string) (string, error) {
	protocol = strings.ToLower(strings.TrimSpace(protocol))
	chargeOrRequestID = strings.ToLower(strings.TrimSpace(chargeOrRequestID))
	extension = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(extension), "."))
	if !keyPartPattern.MatchString(protocol) || !keyPartPattern.MatchString(chargeOrRequestID) || resultIndex < 0 || resultIndex > 999 || !keyPartPattern.MatchString(extension) {
		return "", ErrInvalidObject
	}
	return fmt.Sprintf("images/%s/%s/%03d-%s.%s", protocol, chargeOrRequestID, resultIndex, hex.EncodeToString(digest[:]), extension), nil
}

func publicURL(base, key string) (string, error) {
	parsed, err := parseServiceURL(base, false)
	if err != nil || strings.HasPrefix(key, "/") || strings.Contains(key, "..") {
		return "", ErrInvalidObject
	}
	parsed.Path = path.Join(parsed.Path, key)
	parsed.RawPath = ""
	return parsed.String(), nil
}
