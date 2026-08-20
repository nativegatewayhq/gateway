package telemetry

import (
	"testing"
	"time"
)

func TestConfigDefaultsAndManagedValidation(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatal(err)
	}
	config := DefaultConfig()
	config.Mode, config.Endpoint = Required, "https://otel.example.com/collector"
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	config.Endpoint = "http://otel.example.com"
	if config.Validate() == nil {
		t.Fatal("accepted non-loopback plaintext endpoint")
	}
}

func TestConfigRejectsUnboundedOrSecretBearingValues(t *testing.T) {
	for _, mutate := range []func(*Config){
		func(config *Config) { config.Mode = "invalid" },
		func(config *Config) { config.SampleRatio = 1.1 },
		func(config *Config) { config.Authorization = "Bearer secret\r\nInjected: true" },
		func(config *Config) { config.ExportInterval = time.Millisecond },
		func(config *Config) { config.Endpoint = "https://user:secret@otel.example.com" },
		func(config *Config) { config.Endpoint = "https://otel.example.com?token=secret" },
	} {
		config := DefaultConfig()
		config.Mode, config.Endpoint = Required, "https://otel.example.com"
		mutate(&config)
		if config.Validate() == nil {
			t.Fatalf("accepted config=%+v", config)
		}
	}
}

func TestSignalURLAppendsSignalWithoutQuery(t *testing.T) {
	if got := signalURL("https://otel.example.com/collector/", "traces"); got != "https://otel.example.com/collector/v1/traces" {
		t.Fatalf("url=%s", got)
	}
}
