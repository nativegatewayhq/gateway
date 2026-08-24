package registry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

const MatrixSchema = "nativegateway.adapter-matrix/v1"

type Matrix struct {
	SchemaVersion string      `json:"schema_version"`
	IndexSequence uint64      `json:"index_sequence"`
	IndexDigest   string      `json:"index_digest"`
	Entries       []MatrixRow `json:"entries"`
}

type MatrixRow struct {
	PluginID             string `json:"plugin_id"`
	PluginVersion        string `json:"plugin_version"`
	Platform             string `json:"platform"`
	Status               string `json:"status"`
	GatewayCompatibility string `json:"gateway_compatibility"`
	RuntimeSDK           string `json:"runtime_sdk"`
	ManifestDigest       string `json:"manifest_digest"`
	ArtifactDigest       string `json:"artifact_digest"`
}

func BuildMatrix(snapshot Snapshot) (Matrix, error) {
	if snapshot.Index.Index.Sequence < 1 || !sha256Pattern.MatchString(snapshot.Index.EnvelopeDigest) || len(snapshot.Admissions) < 1 {
		return Matrix{}, ErrInvalid
	}
	result := Matrix{SchemaVersion: MatrixSchema, IndexSequence: snapshot.Index.Index.Sequence, IndexDigest: snapshot.Index.EnvelopeDigest}
	for key, admission := range snapshot.Admissions {
		predicate := admission.Statement.Predicate
		if key != predicate.PluginID+"@"+predicate.PluginVersion {
			return Matrix{}, ErrInvalid
		}
		status := ""
		for _, release := range snapshot.Index.Index.Releases {
			if release.PluginID == predicate.PluginID && release.PluginVersion == predicate.PluginVersion {
				status = release.Status
			}
		}
		if status == "" {
			return Matrix{}, ErrInvalid
		}
		result.Entries = append(result.Entries, MatrixRow{PluginID: predicate.PluginID, PluginVersion: predicate.PluginVersion, Platform: predicate.Platform, Status: status, GatewayCompatibility: predicate.GatewayCompatibility, RuntimeSDK: predicate.RuntimeSDK, ManifestDigest: predicate.ManifestDigest, ArtifactDigest: predicate.Artifact.Digest})
	}
	sort.Slice(result.Entries, func(left, right int) bool {
		leftKey := result.Entries[left].PluginID + "\x00" + result.Entries[left].PluginVersion + "\x00" + result.Entries[left].Platform
		rightKey := result.Entries[right].PluginID + "\x00" + result.Entries[right].PluginVersion + "\x00" + result.Entries[right].Platform
		return leftKey < rightKey
	})
	return result, nil
}

func CanonicalMatrix(value Matrix) ([]byte, error) {
	if validateMatrix(value) != nil {
		return nil, ErrInvalid
	}
	return json.Marshal(value)
}

func DecodeMatrix(reader io.Reader, maximum int64) (Matrix, error) {
	var value Matrix
	if decode(reader, maximum, &value) != nil || validateMatrix(value) != nil {
		return Matrix{}, ErrInvalid
	}
	return value, nil
}

func RenderMatrixMarkdown(value Matrix) ([]byte, error) {
	if validateMatrix(value) != nil {
		return nil, ErrInvalid
	}
	var output strings.Builder
	output.WriteString("# Official Adapter compatibility matrix\n\n")
	_, _ = fmt.Fprintf(&output, "Registry sequence: `%d`  \nRegistry index: `%s`\n\n", value.IndexSequence, value.IndexDigest)
	output.WriteString("| Plugin | Version | Platform | Status | Gateway | Runtime | Manifest | Artifact |\n|---|---|---|---|---|---|---|---|\n")
	for _, row := range value.Entries {
		_, _ = fmt.Fprintf(&output, "| `%s` | `%s` | `%s` | `%s` | `%s` | `%s` | `%s` | `%s` |\n", row.PluginID, row.PluginVersion, row.Platform, row.Status, row.GatewayCompatibility, row.RuntimeSDK, row.ManifestDigest, row.ArtifactDigest)
	}
	return bytes.Clone([]byte(output.String())), nil
}

func validateMatrix(value Matrix) error {
	if value.SchemaVersion != MatrixSchema || value.IndexSequence < 1 || !sha256Pattern.MatchString(value.IndexDigest) || len(value.Entries) < 1 || len(value.Entries) > 4096 {
		return ErrInvalid
	}
	previous := ""
	for _, row := range value.Entries {
		key := row.PluginID + "\x00" + row.PluginVersion + "\x00" + row.Platform
		if !validID(row.PluginID, 128) || !versionPattern.MatchString(row.PluginVersion) || !validPlatform(row.Platform) || row.Status != "active" && row.Status != "yanked" || !validCompatibility(row.GatewayCompatibility) || row.RuntimeSDK != RuntimeSDK || !sha256Pattern.MatchString(row.ManifestDigest) || !sha256Pattern.MatchString(row.ArtifactDigest) || key <= previous {
			return ErrInvalid
		}
		previous = key
	}
	return nil
}
