package fixtures_test

import (
	"bytes"
	"encoding/json"
	"os"
	"sort"
	"testing"

	manifest "github.com/nativegatewayhq/gateway/plugin-sdk/manifest/v1"
	runtimev1 "github.com/nativegatewayhq/gateway/plugin-sdk/runtime/v1"
)

type corpus struct {
	SchemaVersion string `json:"schema_version"`
	Fixtures      []struct {
		Path, Kind, Outcome string
	} `json:"fixtures"`
}

func TestVersionedFixtureCorpus(t *testing.T) {
	body, err := os.ReadFile("index.json")
	if err != nil {
		t.Fatal(err)
	}
	var index corpus
	if json.Unmarshal(body, &index) != nil || index.SchemaVersion != "nativegateway.plugin-fixtures/v1" || len(index.Fixtures) < 9 {
		t.Fatalf("invalid fixture index")
	}
	paths := make([]string, 0, len(index.Fixtures))
	for _, fixture := range index.Fixtures {
		paths = append(paths, fixture.Path)
		body, err = os.ReadFile(fixture.Path)
		if err != nil {
			t.Fatal(err)
		}
		valid := false
		switch fixture.Kind {
		case "manifest":
			_, err = manifest.Parse(body, "0.1.0")
			valid = err == nil
		case "request":
			_, err = runtimev1.DecodeRequest(bytes.NewReader(body), 1<<20)
			valid = err == nil
		case "response":
			expected := runtimev1.Expectation{Identity: runtimev1.Identity{RequestID: "req_test", PluginID: "provider.example", PluginVersion: "1.0.0", ManifestDigest: "62892b5cc04328d02a3277730b1a10eae5b029c43690b0a58c4271c68af9204f"}, Protocol: "openai", Model: "example-image-v1", Output: "base64", MaximumImages: 2}
			_, err = runtimev1.DecodeResponse(bytes.NewReader(body), 1<<20, expected)
			valid = err == nil
		default:
			t.Fatalf("unknown fixture kind %q", fixture.Kind)
		}
		if valid != (fixture.Outcome == "pass") {
			t.Fatalf("fixture %s outcome %s, err=%v", fixture.Path, fixture.Outcome, err)
		}
	}
	if !sort.StringsAreSorted(paths) {
		t.Fatalf("fixture index must be stable sorted: %#v", paths)
	}
}
