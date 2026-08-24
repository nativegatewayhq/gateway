package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const manifestBody = `{"schema_version":"nativegateway.provider/v1","id":"provider.example","version":"1.0.0","gateway_compatibility":">=0.1.0 <1.0.0","transport":{"kind":"http-sidecar","endpoint_ref":"example-sidecar","auth_secret_ref":"example-sidecar-token"},"models":[{"id":"example-image-v1","protocols":["openai"],"operations":["image.generate"],"capabilities":{"media_type":"application/json","output":["base64"],"maximum_images":2}}]}`

func TestResolveEnvironmentAndFileReferences(t *testing.T) {
	directory := manifestDirectory(t)
	validated, endpoint, secret, err := resolve(directory, "provider.example", "0.1.0", `{"example-sidecar":"http://127.0.0.1:8081"}`, `{"example-sidecar-token":"PLUGIN_TOKEN"}`, `{}`, func(name string) string {
		if name == "PLUGIN_TOKEN" {
			return "0123456789abcdef"
		}
		return ""
	})
	if err != nil || validated.Manifest.ID != "provider.example" || endpoint != "http://127.0.0.1:8081" || string(secret) != "0123456789abcdef" {
		t.Fatalf("resolve(env) = %q, %q, %q, %v", validated.Manifest.ID, endpoint, secret, err)
	}
	secretPath := filepath.Join(t.TempDir(), "plugin-secret")
	if err := os.WriteFile(secretPath, []byte("fedcba9876543210"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, secret, err = resolve(directory, "provider.example", "0.1.0", `{"example-sidecar":"https://sidecar.example"}`, `{}`, `{"example-sidecar-token":"`+secretPath+`"}`, func(string) string { return "" })
	if err != nil || string(secret) != "fedcba9876543210" {
		t.Fatalf("resolve(file) = %q, %v", secret, err)
	}
}

func TestResolveRejectsAmbiguousUnsafeAndDuplicateMappings(t *testing.T) {
	directory := manifestDirectory(t)
	tests := []struct{ endpoints, environment, files string }{
		{`{"example-sidecar":"http://127.0.0.1:1","example-sidecar":"http://127.0.0.1:2"}`, `{"example-sidecar-token":"PLUGIN_TOKEN"}`, `{}`},
		{`{"example-sidecar":"http://127.0.0.1:1"}`, `{"example-sidecar-token":"plugin-token"}`, `{}`},
		{`{"example-sidecar":"http://127.0.0.1:1"}`, `{"example-sidecar-token":"PLUGIN_TOKEN"}`, `{"example-sidecar-token":"/tmp/secret"}`},
	}
	for _, test := range tests {
		if _, _, _, err := resolve(directory, "provider.example", "0.1.0", test.endpoints, test.environment, test.files, func(string) string { return "0123456789abcdef" }); err == nil {
			t.Fatalf("accepted invalid mappings: %#v", test)
		}
	}
}

func TestConfigurationExitDoesNotLeakArguments(t *testing.T) {
	secret := "0123456789abcdef-sensitive"
	var stdout, stderr bytes.Buffer
	exit := run([]string{"-manifest-dir", "/missing", "-plugin-id", secret}, func(string) string { return secret }, &stdout, &stderr)
	if exit != 2 || stdout.Len() != 0 || strings.Contains(stderr.String(), secret) || stderr.String() != "plugin conformance configuration failed\n" {
		t.Fatalf("run() = %d, %q, %q", exit, stdout.String(), stderr.String())
	}
}

func manifestDirectory(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "provider.example.json"), []byte(manifestBody), 0o600); err != nil {
		t.Fatal(err)
	}
	return directory
}
