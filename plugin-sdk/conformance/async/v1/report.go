// Package asyncconformance defines the public async/v1 conformance report contract.
package asyncconformance

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"

	asyncv1 "github.com/nativegatewayhq/gateway/plugin-sdk/async/v1"
	"github.com/nativegatewayhq/gateway/plugin-sdk/jsonstrict"
)

const ReportSchema = "nativegateway.plugin-async-conformance/v1"
const SDKVersion = asyncv1.ContractVersion
const MaximumReportBytes = 1 << 20

var ErrInvalid = errors.New("invalid plugin async conformance report")
var idPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)
var versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var checkPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._][a-z0-9]+)*$`)
var categoryPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:_[a-z0-9]+)*$`)
var requiredCheckIDs = []string{"callback.signature", "callback.tamper", "control.authentication", "control.cancel", "health.authenticated", "health.unauthenticated", "poll.processing", "poll.success", "submit.authentication", "submit.cancellation", "submit.queued", "wire.malformed_body", "wire.oversized_body", "wire.wrong_path"}

type Check struct {
	ID         string `json:"id"`
	Outcome    string `json:"outcome"`
	Category   string `json:"category,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}

type Report struct {
	SchemaVersion  string  `json:"schema_version"`
	PluginID       string  `json:"plugin_id"`
	PluginVersion  string  `json:"plugin_version"`
	ManifestDigest string  `json:"manifest_digest"`
	SDKVersion     string  `json:"sdk_version"`
	Outcome        string  `json:"outcome"`
	Checks         []Check `json:"checks"`
}

func RequiredCheckIDs() []string { return append([]string(nil), requiredCheckIDs...) }
func RequiredChecksDigest() string {
	digest := sha256.Sum256([]byte(strings.Join(requiredCheckIDs, "\n") + "\n"))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func CanonicalReport(value Report) ([]byte, error) {
	if ValidateReport(value) != nil {
		return nil, ErrInvalid
	}
	return json.Marshal(value)
}

func DecodeReport(reader io.Reader, maximum int64) (Report, error) {
	if reader == nil || maximum < 1 || maximum > MaximumReportBytes {
		return Report{}, ErrInvalid
	}
	body, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil || len(body) == 0 || int64(len(body)) > maximum || jsonstrict.Validate(body) != nil {
		return Report{}, ErrInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var value Report
	if decoder.Decode(&value) != nil || decoder.Decode(&struct{}{}) != io.EOF || ValidateReport(value) != nil {
		return Report{}, ErrInvalid
	}
	return value, nil
}

func EncodeReport(writer io.Writer, value Report) error {
	if writer == nil || ValidateReport(value) != nil {
		return ErrInvalid
	}
	return json.NewEncoder(writer).Encode(value)
}

func ValidateReport(value Report) error {
	if value.SchemaVersion != ReportSchema || value.SDKVersion != SDKVersion || (value.Outcome != "pass" && value.Outcome != "fail") || !validID(value.PluginID, 128) || !versionPattern.MatchString(value.PluginVersion) || !digestPattern.MatchString(value.ManifestDigest) || len(value.Checks) != len(requiredCheckIDs) {
		return ErrInvalid
	}
	failed := false
	for index, check := range value.Checks {
		if check.ID != requiredCheckIDs[index] || !checkPattern.MatchString(check.ID) || (check.Outcome != "pass" && check.Outcome != "fail") || len(check.Category) > 80 || (check.Category != "" && !categoryPattern.MatchString(check.Category)) || (check.Outcome == "pass") != (check.Category == "") || check.DurationMS < 0 || check.DurationMS > 60000 {
			return ErrInvalid
		}
		failed = failed || check.Outcome == "fail"
	}
	if (value.Outcome == "fail") != failed {
		return ErrInvalid
	}
	return nil
}

func validID(value string, maximum int) bool {
	return len(value) > 0 && len(value) <= maximum && idPattern.MatchString(value) && strings.TrimSpace(value) == value
}
