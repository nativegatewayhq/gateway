package config

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestJobManagementConfiguration(t *testing.T) {
	base := map[string]string{"GATEWAY_DATABASE_URL": "postgres://test", "GATEWAY_REPLICATE_API_TOKEN": "token", "GATEWAY_REPLICATE_MODELS": "owner/model:version", "GATEWAY_PUBLIC_BASE_URL": "https://gateway.example", "GATEWAY_JOB_MANAGEMENT_MODE": "required", "GATEWAY_JOB_MANAGEMENT_CURSOR_SECRETS": base64.StdEncoding.EncodeToString([]byte(strings.Repeat("s", 32)))}
	lookup := func(key string) (string, bool) { v, ok := base[key]; return v, ok }
	cfg, err := Load(lookup)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.JobManagementMode != JobManagementRequired || len(cfg.JobManagementCursorSecrets) != 1 {
		t.Fatalf("unexpected config: %#v", cfg)
	}
	base["GATEWAY_JOB_MANAGEMENT_CURSOR_SECRETS"] = "short"
	if _, err := Load(lookup); err == nil {
		t.Fatal("invalid secret accepted")
	}
	delete(base, "GATEWAY_JOB_MANAGEMENT_CURSOR_SECRETS")
	if _, err := Load(lookup); err == nil {
		t.Fatal("missing secret accepted")
	}
}
