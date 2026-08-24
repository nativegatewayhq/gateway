package manifest

import (
	"os"
	"path/filepath"
	"testing"
)

const validManifest = `{"schema_version":"nativegateway.provider/v1","id":"provider.example","version":"1.0.0","gateway_compatibility":">=0.1.0 <1.0.0","transport":{"kind":"http-sidecar","endpoint_ref":"example-sidecar","auth_secret_ref":"example-token"},"models":[{"id":"example-image-v1","protocols":["openai","gemini"],"operations":["image.generate"],"capabilities":{"media_type":"application/json","output":["base64"],"maximum_images":2}}]}`

func TestParseCanonicalDigestAndValidation(t *testing.T) {
	first, err := Parse([]byte(validManifest), "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	second, err := Parse([]byte(" \n"+validManifest+"\n"), "0.9.9")
	if err != nil || first.Digest != second.Digest {
		t.Fatalf("digest stable=%v err=%v", first.Digest == second.Digest, err)
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
