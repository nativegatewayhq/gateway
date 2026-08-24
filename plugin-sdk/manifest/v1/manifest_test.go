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

const validVideoManifest = `{"schema_version":"nativegateway.provider/v1","id":"provider.video-example","version":"1.0.0","gateway_compatibility":">=0.1.0 <1.0.0","transport":{"kind":"http-sidecar","endpoint_ref":"video-sidecar","auth_secret_ref":"video-token"},"models":[],"video_models":[{"id":"example-video-v1","protocols":["runway"],"operations":["video.generate"],"capabilities":{"media_type":"application/json","output":["url"],"text_to_video":true,"image_to_video":true,"audio":false,"maximum_duration_seconds":60,"ratios":["16:9","9:16"]},"async":{"contract":"video/v1","callback":true}}]}`

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

func TestParseVideoCapabilityPreservesImageDigests(t *testing.T) {
	video, err := Parse([]byte(validVideoManifest), "0.1.0")
	if err != nil || ExecutionContract(video) != "video/v1" || len(video.Manifest.VideoModels) != 1 {
		t.Fatalf("video=%#v err=%v", video.Manifest, err)
	}
	syncValue, _ := Parse([]byte(validManifest), "0.1.0")
	if hex.EncodeToString(syncValue.Digest[:]) != "b2210aa93268add9e0fafd5e7735fd82b3b5db33bcd7a63e1bbeb74d7975c1a9" {
		t.Fatal("sync digest changed")
	}
	for _, invalid := range []string{strings.Replace(validVideoManifest, `"contract":"video/v1"`, `"contract":"async/v1"`, 1), strings.Replace(validVideoManifest, `"protocols":["runway"]`, `"protocols":["replicate"]`, 1), strings.Replace(validVideoManifest, `"text_to_video":true,"image_to_video":true`, `"text_to_video":false,"image_to_video":false`, 1), strings.Replace(validVideoManifest, `"ratios":["16:9","9:16"]`, `"ratios":["16:9","16:9"]`, 1), strings.Replace(validVideoManifest, `"models":[]`, `"models":[`+validManifest[strings.Index(validManifest, `{"id":"example-image-v1"`):len(validManifest)-2]+`]`, 1)} {
		if _, err = Parse([]byte(invalid), "0.1.0"); err == nil {
			t.Fatalf("invalid video accepted: %.120s", invalid)
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
