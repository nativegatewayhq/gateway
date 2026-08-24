package fixtures_test

import (
	"bytes"
	"encoding/json"
	"os"
	"sort"
	"testing"

	registry "github.com/nativegatewayhq/gateway/plugin-sdk/registry/v1"
)

type corpus struct {
	SchemaVersion string                                 `json:"schema_version"`
	Fixtures      []struct{ Path, Kind, Outcome string } `json:"fixtures"`
}

func TestInvalidRegistryCorpusFailsClosed(t *testing.T) {
	body, err := os.ReadFile("index.json")
	if err != nil {
		t.Fatal(err)
	}
	var index corpus
	if json.Unmarshal(body, &index) != nil || index.SchemaVersion != "nativegateway.adapter-registry-fixtures/v1" || len(index.Fixtures) != 3 {
		t.Fatal("invalid corpus index")
	}
	paths := make([]string, 0, len(index.Fixtures))
	for _, fixture := range index.Fixtures {
		paths = append(paths, fixture.Path)
		body, err = os.ReadFile(fixture.Path)
		if err != nil {
			t.Fatal(err)
		}
		switch fixture.Kind {
		case "envelope":
			_, err = registry.DecodeEnvelope(bytes.NewReader(body), registry.MaximumEnvelopeBytes)
		case "index":
			_, err = registry.DecodeIndex(bytes.NewReader(body), registry.MaximumIndexBytes)
		case "trust":
			_, err = registry.DecodeTrustPolicy(bytes.NewReader(body), registry.MaximumTrustBytes)
		default:
			t.Fatalf("unknown fixture kind %q", fixture.Kind)
		}
		if err == nil || fixture.Outcome != "registry.invalid" {
			t.Fatalf("fixture %s did not fail closed", fixture.Path)
		}
	}
	if !sort.StringsAreSorted(paths) {
		t.Fatalf("fixture paths are not stable sorted: %#v", paths)
	}
}
