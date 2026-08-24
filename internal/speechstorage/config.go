// Package speechstorage manages private, tenant-scoped generated speech outputs.
package speechstorage

import (
	"errors"
	"os"
	"strings"
	"time"

	"github.com/nativegatewayhq/gateway/internal/audioassets"
)

var ErrInvalid = errors.New("invalid speech output storage configuration")

type Mode string

const (
	Disabled Mode = "disabled"
	Managed  Mode = "managed"
)

type Config struct {
	Mode                                                                         Mode
	Endpoint, Region, Bucket, AccessKeyID, SecretAccessKey, ServerSideEncryption string
	TemporaryDirectory                                                           string
	MaximumBytes                                                                 int64
	MaximumConcurrentCaptures                                                    int
	UploadTimeout, DownloadTimeout, Retention, CleanupInterval, CleanupLease     time.Duration
}

func DefaultConfig() Config {
	return Config{Mode: Disabled, TemporaryDirectory: os.TempDir(), MaximumBytes: 256 << 20, MaximumConcurrentCaptures: 4, UploadTimeout: 2 * time.Minute, DownloadTimeout: 2 * time.Minute, Retention: 7 * 24 * time.Hour, CleanupInterval: time.Minute, CleanupLease: 2 * time.Minute}
}
func (config Config) Validate() error {
	if config.Mode != Disabled && config.Mode != Managed || config.MaximumBytes < 1 || config.MaximumBytes > 512<<20 || config.MaximumConcurrentCaptures < 1 || config.MaximumConcurrentCaptures > 64 || config.UploadTimeout <= 0 || config.UploadTimeout > 10*time.Minute || config.DownloadTimeout <= 0 || config.DownloadTimeout > 10*time.Minute || config.Retention < time.Hour || config.Retention > 30*24*time.Hour || config.CleanupInterval < time.Second || config.CleanupLease < time.Second || config.CleanupLease > 10*time.Minute || strings.TrimSpace(config.TemporaryDirectory) == "" {
		return ErrInvalid
	}
	if config.Mode == Managed {
		if _, err := audioassets.NewS3(audioassets.S3Config{Endpoint: config.Endpoint, Region: config.Region, Bucket: config.Bucket, AccessKeyID: config.AccessKeyID, SecretAccessKey: config.SecretAccessKey, ServerSideEncryption: config.ServerSideEncryption, UploadTimeout: config.UploadTimeout, DownloadTimeout: config.DownloadTimeout}); err != nil {
			return ErrInvalid
		}
	}
	return nil
}
