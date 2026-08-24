package manifest

import (
	"strings"
	"testing"
)

func TestRenderMarkdownIsDeterministicAndSecretFree(t *testing.T) {
	first, err := Parse([]byte(validManifest), "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	left, err := RenderMarkdown([]Validated{first})
	if err != nil {
		t.Fatal(err)
	}
	right, err := RenderMarkdown([]Validated{first})
	if err != nil || string(left) != string(right) {
		t.Fatalf("non-deterministic markdown: %v", err)
	}
	for _, expected := range []string{"# Provider plugin capabilities", "provider.example 1.0.0", "`example-image-v1`", "`gemini`, `openai`", "example-token"} {
		if !strings.Contains(string(left), expected) {
			t.Fatalf("missing %q in:\n%s", expected, left)
		}
	}
	for _, forbidden := range []string{"Authorization", "Bearer", "SERVICE_KEY", "http://", "https://"} {
		if strings.Contains(string(left), forbidden) {
			t.Fatalf("render included forbidden %q", forbidden)
		}
	}
}

func TestRenderMarkdownRejectsForgedValidatedValue(t *testing.T) {
	if _, err := RenderMarkdown([]Validated{{Manifest: Manifest{ID: "provider.example"}, Canonical: []byte("{}")}}); err == nil {
		t.Fatal("accepted forged validated manifest")
	}
}
