// Package manifest defines the canonical Native Gateway Provider Manifest v1.
package manifest

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

const SchemaVersion = "nativegateway.provider/v1"
const MaximumBytes = 1 << 20

var (
	ErrInvalid     = errors.New("invalid provider manifest")
	idPattern      = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)
	versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
)

type Manifest struct {
	SchemaVersion        string    `json:"schema_version"`
	ID                   string    `json:"id"`
	Version              string    `json:"version"`
	GatewayCompatibility string    `json:"gateway_compatibility"`
	Transport            Transport `json:"transport"`
	Models               []Model   `json:"models"`
}
type Transport struct {
	Kind          string `json:"kind"`
	EndpointRef   string `json:"endpoint_ref"`
	AuthSecretRef string `json:"auth_secret_ref"`
}
type Model struct {
	ID           string       `json:"id"`
	Protocols    []string     `json:"protocols"`
	Operations   []string     `json:"operations"`
	Capabilities Capabilities `json:"capabilities"`
}
type Capabilities struct {
	MediaType     string   `json:"media_type"`
	Output        []string `json:"output"`
	MaximumImages int      `json:"maximum_images"`
}
type Validated struct {
	Manifest  Manifest
	Canonical []byte
	Digest    [32]byte
}

func Parse(body []byte, gatewayVersion string) (Validated, error) {
	if len(body) == 0 || len(body) > MaximumBytes || HasDuplicateKeys(body) {
		return Validated{}, ErrInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var value Manifest
	if err := decoder.Decode(&value); err != nil {
		return Validated{}, ErrInvalid
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return Validated{}, ErrInvalid
	}
	if err := validate(value, gatewayVersion); err != nil {
		return Validated{}, err
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return Validated{}, ErrInvalid
	}
	return Validated{Manifest: value, Canonical: canonical, Digest: sha256.Sum256(canonical)}, nil
}

func validate(value Manifest, gatewayVersion string) error {
	if value.SchemaVersion != SchemaVersion || !validID(value.ID, 128) || !versionPattern.MatchString(value.Version) || !compatible(value.GatewayCompatibility, gatewayVersion) || value.Transport.Kind != "http-sidecar" || !validID(value.Transport.EndpointRef, 128) || !validID(value.Transport.AuthSecretRef, 128) || len(value.Models) < 1 || len(value.Models) > 128 {
		return ErrInvalid
	}
	models := map[string]bool{}
	for _, model := range value.Models {
		if !validID(model.ID, 200) || models[model.ID] || len(model.Protocols) < 1 || len(model.Protocols) > 2 || len(model.Operations) != 1 || model.Operations[0] != "image.generate" || model.Capabilities.MediaType != "application/json" || model.Capabilities.MaximumImages < 1 || model.Capabilities.MaximumImages > 10 || len(model.Capabilities.Output) != 1 || (model.Capabilities.Output[0] != "base64" && model.Capabilities.Output[0] != "url") {
			return ErrInvalid
		}
		models[model.ID] = true
		protocols := map[string]bool{}
		for _, protocol := range model.Protocols {
			if protocol != "openai" && protocol != "gemini" || protocols[protocol] {
				return ErrInvalid
			}
			protocols[protocol] = true
		}
	}
	return nil
}
func validID(value string, maximum int) bool {
	return len(value) > 0 && len(value) <= maximum && idPattern.MatchString(value) && strings.TrimSpace(value) == value
}

func compatible(expression, current string) bool {
	currentVersion, ok := parseVersion(current)
	if !ok {
		return false
	}
	parts := strings.Fields(expression)
	if len(parts) != 2 {
		return false
	}
	lower, ok1 := parseVersion(strings.TrimPrefix(parts[0], ">="))
	upper, ok2 := parseVersion(strings.TrimPrefix(parts[1], "<"))
	return ok1 && ok2 && strings.HasPrefix(parts[0], ">=") && strings.HasPrefix(parts[1], "<") && compare(currentVersion, lower) >= 0 && compare(currentVersion, upper) < 0
}
func parseVersion(value string) ([3]int, bool) {
	match := versionPattern.FindStringSubmatch(value)
	if match == nil {
		return [3]int{}, false
	}
	var result [3]int
	for index := range 3 {
		parsed, err := strconv.Atoi(match[index+1])
		if err != nil {
			return [3]int{}, false
		}
		result[index] = parsed
	}
	return result, true
}
func compare(left, right [3]int) int {
	for index := range 3 {
		if left[index] < right[index] {
			return -1
		}
		if left[index] > right[index] {
			return 1
		}
	}
	return 0
}

// HasDuplicateKeys reports malformed JSON and duplicate object member names at
// any depth. It is exported so sidecar contract decoders can share the same
// ambiguity-free JSON boundary as manifests.
func HasDuplicateKeys(body []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(body))
	return scanValue(decoder) != nil || decoder.Decode(&struct{}{}) != io.EOF
}
func scanValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]bool{}
		for decoder.More() {
			token, err = decoder.Token()
			if err != nil {
				return err
			}
			key, ok := token.(string)
			if !ok || seen[key] {
				return fmt.Errorf("duplicate key")
			}
			seen[key] = true
			if err = scanValue(decoder); err != nil {
				return err
			}
		}
		token, err = decoder.Token()
		if err != nil || token != json.Delim('}') {
			return ErrInvalid
		}
	case '[':
		for decoder.More() {
			if err = scanValue(decoder); err != nil {
				return err
			}
		}
		token, err = decoder.Token()
		if err != nil || token != json.Delim(']') {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}
