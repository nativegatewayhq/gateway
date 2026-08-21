// Package videostorage persists asynchronous video results without buffering
// complete media files in memory.
package videostorage

import (
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/nativegatewayhq/gateway/internal/imagestorage"
)

type Mode string

const (
	Provider Mode = "provider"
	Managed  Mode = "managed"
)

var (
	ErrInvalidConfig = errors.New("invalid video storage configuration")
	ErrInvalid       = errors.New("invalid video result")
	ErrUnavailable   = errors.New("video storage unavailable")
	ErrTooLarge      = errors.New("video result too large")
	ErrFetchRejected = errors.New("video result fetch rejected")
)

type Config struct {
	Mode                                      Mode
	Endpoint, Region, Bucket                  string
	AccessKeyID, SecretAccessKey, CDNBaseURL  string
	TemporaryDirectory                        string
	MaximumVideos, MaximumConcurrentDownloads int
	MaximumVideoBytes, MaximumTotalBytes      int64
	FetchTimeout, UploadTimeout               time.Duration
	FetchOrigins                              map[string][]string
}

func DefaultConfig() Config {
	return Config{Mode: Provider, MaximumVideos: 4, MaximumConcurrentDownloads: 2, MaximumVideoBytes: 512 << 20, MaximumTotalBytes: 1 << 30, FetchTimeout: 2 * time.Minute, UploadTimeout: 10 * time.Minute}
}

func (config Config) Validate() error {
	if config.Mode != Provider && config.Mode != Managed {
		return ErrInvalidConfig
	}
	if config.Mode == Provider {
		return nil
	}
	if strings.TrimSpace(config.Endpoint) == "" || strings.TrimSpace(config.Region) == "" || strings.TrimSpace(config.Bucket) == "" || strings.TrimSpace(config.AccessKeyID) == "" || strings.TrimSpace(config.SecretAccessKey) == "" || strings.TrimSpace(config.CDNBaseURL) == "" {
		return ErrInvalidConfig
	}
	for _, raw := range []string{config.Endpoint, config.CDNBaseURL} {
		parsed, err := url.Parse(raw)
		loopback := err == nil && (parsed.Hostname() == "localhost" || parsed.Hostname() == "127.0.0.1" || parsed.Hostname() == "::1")
		if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Scheme != "https" && !(raw == config.Endpoint && parsed.Scheme == "http" && loopback)) {
			return ErrInvalidConfig
		}
	}
	if config.MaximumVideos < 1 || config.MaximumVideos > 10 || config.MaximumConcurrentDownloads < 1 || config.MaximumConcurrentDownloads > 32 || config.MaximumVideoBytes < 1 || config.MaximumVideoBytes > 2<<30 || config.MaximumTotalBytes < config.MaximumVideoBytes || config.MaximumTotalBytes > 8<<30 || config.FetchTimeout <= 0 || config.FetchTimeout > 10*time.Minute || config.UploadTimeout <= 0 || config.UploadTimeout > 30*time.Minute {
		return ErrInvalidConfig
	}
	if len(config.FetchOrigins["runway"]) == 0 || len(config.FetchOrigins["runway"]) > 32 {
		return ErrInvalidConfig
	}
	for _, raw := range config.FetchOrigins["runway"] {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Path != "" && parsed.Path != "/" || parsed.RawQuery != "" || parsed.Fragment != "" {
			return ErrInvalidConfig
		}
	}
	return nil
}

func (config Config) ImageObjectConfig() imagestorage.Config {
	return imagestorage.Config{Mode: imagestorage.Managed, Endpoint: config.Endpoint, Region: config.Region, Bucket: config.Bucket, AccessKeyID: config.AccessKeyID, SecretAccessKey: config.SecretAccessKey, CDNBaseURL: config.CDNBaseURL, MaximumImages: config.MaximumVideos, MaximumImageBytes: min(config.MaximumVideoBytes, 256<<20), MaximumTotalBytes: min(config.MaximumTotalBytes, 512<<20), FetchTimeout: min(config.FetchTimeout, 5*time.Minute), UploadTimeout: min(config.UploadTimeout, 10*time.Minute), TemporaryDirectory: config.TemporaryDirectory, FetchOrigins: config.FetchOrigins}
}
