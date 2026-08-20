// Package telemetry owns the Gateway OpenTelemetry SDK and bounded instruments.
package telemetry

import (
	"errors"
	"net/url"
	"strings"
	"time"
)

type Mode string

const (
	Disabled Mode = "disabled"
	Optional Mode = "optional"
	Required Mode = "required"
)

var ErrInvalidConfig = errors.New("invalid telemetry configuration")

type Config struct {
	Mode            Mode
	Endpoint        string
	Authorization   string
	ServiceName     string
	ServiceVersion  string
	Environment     string
	SampleRatio     float64
	ExportInterval  time.Duration
	ExportTimeout   time.Duration
	ShutdownTimeout time.Duration
}

func DefaultConfig() Config {
	return Config{Mode: Disabled, ServiceName: "native-ai-gateway", ServiceVersion: "development", Environment: "development", SampleRatio: 0.1, ExportInterval: 30 * time.Second, ExportTimeout: 10 * time.Second, ShutdownTimeout: 5 * time.Second}
}

func (config Config) Validate() error {
	if config.Mode != Disabled && config.Mode != Optional && config.Mode != Required {
		return ErrInvalidConfig
	}
	if config.Mode == Disabled {
		return nil
	}
	endpoint, err := url.Parse(strings.TrimSpace(config.Endpoint))
	if err != nil || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return ErrInvalidConfig
	}
	if endpoint.Scheme != "https" && !(endpoint.Scheme == "http" && (endpoint.Hostname() == "127.0.0.1" || endpoint.Hostname() == "::1" || endpoint.Hostname() == "localhost")) {
		return ErrInvalidConfig
	}
	if !boundedName(config.ServiceName, 100) || !boundedName(config.ServiceVersion, 100) || !boundedName(config.Environment, 100) || config.SampleRatio < 0 || config.SampleRatio > 1 {
		return ErrInvalidConfig
	}
	if len(config.Authorization) > 4096 || strings.ContainsAny(config.Authorization, "\r\n") {
		return ErrInvalidConfig
	}
	if config.ExportInterval < time.Second || config.ExportInterval > 10*time.Minute || config.ExportTimeout <= 0 || config.ExportTimeout > time.Minute || config.ShutdownTimeout <= 0 || config.ShutdownTimeout > time.Minute {
		return ErrInvalidConfig
	}
	return nil
}

func boundedName(value string, maximum int) bool {
	value = strings.TrimSpace(value)
	return value != "" && len(value) <= maximum && !strings.ContainsAny(value, "\r\n")
}
