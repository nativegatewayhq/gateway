package audioassets

import (
	"strings"
	"time"
)

type Mode string

const (
	Disabled Mode = "disabled"
	Managed  Mode = "managed"
)

type Config struct {
	Mode                                                                       Mode
	Endpoint, Region, Bucket, AccessKeyID, SecretAccessKey, TemporaryDirectory string
	ServerSideEncryption                                                       string
	MaximumBytes                                                               int64
	MaximumConcurrentUploads                                                   int
	UploadTimeout, DownloadTimeout, Retention, CleanupInterval, CleanupLease   time.Duration
	AllowedContentTypes                                                        []string
}

func DefaultConfig() Config {
	return Config{Mode: Disabled, MaximumBytes: 64 << 20, MaximumConcurrentUploads: 8, UploadTimeout: 2 * time.Minute, DownloadTimeout: 2 * time.Minute, Retention: 7 * 24 * time.Hour, CleanupInterval: time.Minute, CleanupLease: 5 * time.Minute, AllowedContentTypes: []string{"audio/mpeg", "audio/mp4", "audio/wav", "audio/x-wav", "audio/webm", "audio/ogg", "audio/flac"}}
}
func (config Config) Validate() error {
	if config.Mode != Disabled && config.Mode != Managed {
		return ErrInvalid
	}
	if config.MaximumBytes < 1 || config.MaximumBytes > 512<<20 || config.MaximumConcurrentUploads < 1 || config.MaximumConcurrentUploads > 128 || config.UploadTimeout <= 0 || config.UploadTimeout > 10*time.Minute || config.DownloadTimeout <= 0 || config.DownloadTimeout > 10*time.Minute || config.Retention < time.Hour || config.Retention > 30*24*time.Hour || config.CleanupInterval <= 0 || config.CleanupInterval > time.Hour || config.CleanupLease <= 0 || config.CleanupLease > 10*time.Minute || len(config.AllowedContentTypes) < 1 || len(config.AllowedContentTypes) > 32 {
		return ErrInvalid
	}
	seen := map[string]bool{}
	for _, value := range config.AllowedContentTypes {
		if !strings.HasPrefix(value, "audio/") || len(value) > 100 || seen[value] {
			return ErrInvalid
		}
		seen[value] = true
	}
	if config.Mode == Managed {
		if config.ServerSideEncryption != "" && config.ServerSideEncryption != "AES256" {
			return ErrInvalid
		}
		if _, err := NewS3(S3Config{Endpoint: config.Endpoint, Region: config.Region, Bucket: config.Bucket, AccessKeyID: config.AccessKeyID, SecretAccessKey: config.SecretAccessKey, ServerSideEncryption: config.ServerSideEncryption, UploadTimeout: config.UploadTimeout, DownloadTimeout: config.DownloadTimeout}); err != nil {
			return err
		}
	}
	return nil
}
