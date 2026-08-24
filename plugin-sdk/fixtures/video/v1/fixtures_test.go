package fixtures

import (
	"os"
	"testing"

	videov1 "github.com/nativegatewayhq/gateway/plugin-sdk/video/v1"
)

var identity = videov1.Identity{RequestID: "request_1", GatewayJobID: "job_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", PluginID: "provider.video-example", PluginVersion: "1.0.0", ManifestDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
var expected = videov1.Expectation{Identity: identity, MaximumDurationSeconds: 60, Ratios: map[string]bool{"16:9": true}, TextToVideo: true, ImageToVideo: true, ResultOrigins: map[string]bool{"https://assets.example.com": true}}

func TestVideoFixtureCorpus(t *testing.T) {
	file, err := os.Open("valid-submit.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = videov1.DecodeSubmitRequest(file, 1<<20, expected); err != nil {
		t.Fatal(err)
	}
	file.Close()
	file, err = os.Open("valid-poll.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = videov1.DecodeObservationResponse(file, 1<<20, expected); err != nil {
		t.Fatal(err)
	}
	file.Close()
	file, err = os.Open("invalid-duplicate.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = videov1.DecodeControlRequest(file, 1<<20); err == nil {
		t.Fatal("duplicate key accepted")
	}
	file.Close()
}
