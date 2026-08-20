package fal

import "testing"

func TestFalOutputUsageCountsOnlyUsableImages(t *testing.T) {
	usage := falOutputUsage([]byte(`{"images":[{"url":"https://delivery.example/a.png"},{"url":"data:image/png;base64,AA=="},{"url":"http://unsafe.example/b.png"},{"content_type":"image/png"}]}`), "webhook")
	if usage.Quantity != 2 || usage.Provenance != "webhook" || usage.ExtractorVersion != "fal-output-v1" {
		t.Fatalf("usage=%+v", usage)
	}
	usage = falOutputUsage([]byte(`{"image":{"url":"https://delivery.example/one.png"}}`), "poll")
	if usage.Quantity != 1 {
		t.Fatalf("single usage=%+v", usage)
	}
}
