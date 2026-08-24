package manifest

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validManifest = `{"schema_version":"nativegateway.provider/v1","id":"provider.example","version":"1.0.0","gateway_compatibility":">=0.1.0 <1.0.0","transport":{"kind":"http-sidecar","endpoint_ref":"example-sidecar","auth_secret_ref":"example-token"},"models":[{"id":"example-image-v1","protocols":["openai","gemini"],"operations":["image.generate"],"capabilities":{"media_type":"application/json","output":["base64"],"maximum_images":2}}]}`

const validAsyncManifest = `{"schema_version":"nativegateway.provider/v1","id":"provider.async-example","version":"1.0.0","gateway_compatibility":">=0.1.0 <1.0.0","transport":{"kind":"http-sidecar","endpoint_ref":"async-sidecar","auth_secret_ref":"async-token"},"models":[{"id":"async-image-v1","protocols":["fal","replicate"],"operations":["image.generate"],"capabilities":{"media_type":"application/json","output":["url"],"maximum_images":4},"async":{"contract":"async/v1","callback":true}}]}`

func TestParseCanonicalDigestAndValidation(t *testing.T) {
	first, err := Parse([]byte(validManifest), "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	second, err := Parse([]byte(" \n"+validManifest+"\n"), "0.9.9")
	if err != nil || first.Digest != second.Digest {
		t.Fatalf("digest stable=%v err=%v", first.Digest == second.Digest, err)
	}
	if hex.EncodeToString(first.Digest[:]) != "b2210aa93268add9e0fafd5e7735fd82b3b5db33bcd7a63e1bbeb74d7975c1a9" {
		t.Fatalf("manifest digest changed: %x", first.Digest)
	}
	for _, value := range []string{`{"schema_version":"nativegateway.provider/v1","schema_version":"nativegateway.provider/v1"}`, `{"schema_version":"nativegateway.provider/v1","secret":"private"}`, validManifest[:len(validManifest)-1] + `,"unknown":true}`, validManifest} {
		version := "1.0.0"
		if value == validManifest {
			version = "1.0.0"
		}
		if _, err = Parse([]byte(value), version); err == nil {
			t.Fatalf("invalid accepted: %.80s", value)
		}
	}
}

func TestParseAsyncCapabilityWithoutChangingSyncContract(t *testing.T) {
	validated, err := Parse([]byte(validAsyncManifest), "0.1.0")
	if err != nil || validated.Manifest.Models[0].Async == nil || !validated.Manifest.Models[0].Async.Callback {
		t.Fatalf("async manifest = %#v, %v", validated.Manifest, err)
	}
	for _, invalid := range []string{
		strings.Replace(validAsyncManifest, `"contract":"async/v1"`, `"contract":"async/v2"`, 1),
		strings.Replace(validAsyncManifest, `"fal","replicate"`, `"openai"`, 1),
		strings.Replace(validManifest, `"capabilities":`, `"async":{"contract":"async/v1","callback":false},"capabilities":`, 1),
	} {
		if _, err = Parse([]byte(invalid), "0.1.0"); err == nil {
			t.Fatalf("invalid async manifest accepted: %s", invalid)
		}
	}
}

func TestLoadDirectoryRejectsSymlinkAndWritableManifest(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "provider.json")
	if err := os.WriteFile(path, []byte(validManifest), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadDirectory(directory, "0.1.0")
	if err != nil || len(loaded) != 1 {
		t.Fatalf("loaded=%d err=%v", len(loaded), err)
	}
	if err = os.Chmod(path, 0o622); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadDirectory(directory, "0.1.0"); err == nil {
		t.Fatal("writable manifest accepted")
	}
	if err = os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(path, filepath.Join(directory, "linked.json")); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadDirectory(directory, "0.1.0"); err == nil {
		t.Fatal("symlink accepted")
	}
}
