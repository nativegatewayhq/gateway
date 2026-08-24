package manifest

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
)

// RenderMarkdown produces deterministic, credential-free capability reference
// documentation from already validated Provider Manifests.
func RenderMarkdown(items []Validated) ([]byte, error) {
	if len(items) < 1 || len(items) > 256 {
		return nil, ErrInvalid
	}
	ordered := append([]Validated(nil), items...)
	sort.Slice(ordered, func(left, right int) bool {
		if ordered[left].Manifest.ID == ordered[right].Manifest.ID {
			return ordered[left].Manifest.Version < ordered[right].Manifest.Version
		}
		return ordered[left].Manifest.ID < ordered[right].Manifest.ID
	})
	seen := map[string]bool{}
	var output strings.Builder
	output.WriteString("# Provider plugin capabilities\n\nGenerated from trusted Provider Manifest v1 files. Endpoint origins and credentials are intentionally excluded.\n\n")
	for _, item := range ordered {
		key := item.Manifest.ID + "@" + item.Manifest.Version
		if seen[key] || len(item.Canonical) == 0 || sha256.Sum256(item.Canonical) != item.Digest {
			return nil, ErrInvalid
		}
		seen[key] = true
		_, _ = fmt.Fprintf(&output, "## %s %s\n\n", item.Manifest.ID, item.Manifest.Version)
		_, _ = fmt.Fprintf(&output, "- Gateway compatibility: `%s`\n", item.Manifest.GatewayCompatibility)
		_, _ = fmt.Fprintf(&output, "- Manifest digest: `sha256:%x`\n", item.Digest)
		_, _ = fmt.Fprintf(&output, "- Endpoint reference: `%s`\n", item.Manifest.Transport.EndpointRef)
		_, _ = fmt.Fprintf(&output, "- Authentication secret reference: `%s`\n\n", item.Manifest.Transport.AuthSecretRef)
		output.WriteString("| Model | Protocols | Operations | Output | Maximum images |\n|---|---|---|---|---:|\n")
		models := append([]Model(nil), item.Manifest.Models...)
		sort.Slice(models, func(left, right int) bool { return models[left].ID < models[right].ID })
		for _, model := range models {
			protocols := append([]string(nil), model.Protocols...)
			operations := append([]string(nil), model.Operations...)
			outputs := append([]string(nil), model.Capabilities.Output...)
			sort.Strings(protocols)
			sort.Strings(operations)
			sort.Strings(outputs)
			_, _ = fmt.Fprintf(&output, "| `%s` | %s | %s | %s | %d |\n", model.ID, codeList(protocols), codeList(operations), codeList(outputs), model.Capabilities.MaximumImages)
		}
		videoModels := append([]VideoModel(nil), item.Manifest.VideoModels...)
		if len(videoModels) > 0 {
			output.WriteString("\n| Video model | Inputs | Maximum duration | Ratios | Audio |\n|---|---|---:|---|---|\n")
			sort.Slice(videoModels, func(left, right int) bool { return videoModels[left].ID < videoModels[right].ID })
			for _, model := range videoModels {
				inputs := []string{}
				if model.Capabilities.TextToVideo {
					inputs = append(inputs, "text_to_video")
				}
				if model.Capabilities.ImageToVideo {
					inputs = append(inputs, "image_to_video")
				}
				_, _ = fmt.Fprintf(&output, "| `%s` | %s | %d | %s | %t |\n", model.ID, codeList(inputs), model.Capabilities.MaximumDurationSeconds, codeList(model.Capabilities.Ratios), model.Capabilities.Audio)
			}
		}
		output.WriteByte('\n')
	}
	return bytes.Clone([]byte(output.String())), nil
}

func codeList(values []string) string {
	result := make([]string, len(values))
	for index, value := range values {
		result[index] = "`" + value + "`"
	}
	return strings.Join(result, ", ")
}
