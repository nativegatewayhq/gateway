package replicate

import (
	"encoding/json"
	"testing"
)

func TestReplicateOutputUsageCountsOnlyUsableImages(t *testing.T) {
	raw := json.RawMessage(`["https://delivery.example/a.png",{"url":"https://delivery.example/b.png"},"http://unsafe.example/c.png",{"url":"javascript:alert(1)"},null]`)
	usage := replicateOutputUsage(raw, "poll")
	if usage.Quantity != 2 || usage.Provenance != "poll" || usage.ExtractorVersion != "replicate-output-v1" {
		t.Fatalf("usage=%+v", usage)
	}
	if usage := replicateOutputUsage(json.RawMessage(`"data:image/png;base64,AA=="`), "webhook"); usage.Quantity != 1 {
		t.Fatalf("data usage=%+v", usage)
	}
}
