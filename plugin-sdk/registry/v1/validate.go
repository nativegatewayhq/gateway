package registry

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/url"
	"regexp"
	"strings"
	"time"

	conformance "github.com/nativegatewayhq/gateway/plugin-sdk/conformance/v1"
)

var idPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)
var versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
var sha256Pattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
var hexDigestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var commitPattern = regexp.MustCompile(`^(?:[a-f0-9]{40}|[a-f0-9]{64})$`)
var ociSubjectPattern = regexp.MustCompile(`^[a-z0-9.-]+(?::[0-9]+)?(?:/[a-z0-9._-]+)+@sha256:[a-f0-9]{64}$`)

func validateEnvelope(value Envelope) error {
	if (value.PayloadType != IndexPayloadType && value.PayloadType != AdmissionPayloadType) || len(value.Payload) < 4 || len(value.Payload) > 6<<20 || len(value.Signatures) < 1 || len(value.Signatures) > 16 {
		return ErrInvalid
	}
	payload, err := base64.StdEncoding.DecodeString(value.Payload)
	if err != nil || len(payload) < 2 || len(payload) > MaximumIndexBytes {
		return ErrInvalid
	}
	previous := ""
	for _, signature := range value.Signatures {
		decoded, decodeErr := base64.StdEncoding.DecodeString(signature.Sig)
		if !sha256Pattern.MatchString(signature.KeyID) || signature.KeyID <= previous || decodeErr != nil || len(decoded) != ed25519.SignatureSize {
			return ErrInvalid
		}
		previous = signature.KeyID
	}
	return nil
}

func validateTrustPolicy(value TrustPolicy) error {
	if value.SchemaVersion != TrustSchema || value.Threshold < 1 || value.Threshold > 16 || value.MinimumSequence < 1 || value.MinimumSequence > 1<<63-1 || len(value.Keys) < value.Threshold || len(value.Keys) > 32 {
		return ErrInvalid
	}
	previous := ""
	for _, key := range value.Keys {
		publicKey, err := base64.StdEncoding.DecodeString(key.PublicKey)
		digest := sha256.Sum256(publicKey)
		expectedID := "sha256:" + hex.EncodeToString(digest[:])
		if err != nil || key.Algorithm != "ed25519" || len(publicKey) != ed25519.PublicKeySize || key.KeyID != expectedID || key.KeyID <= previous || !validTime(key.NotBefore) || !validTime(key.NotAfter) || !key.NotBefore.Before(key.NotAfter) || key.NotAfter.Sub(key.NotBefore) > 5*366*24*time.Hour {
			return ErrInvalid
		}
		previous = key.KeyID
	}
	return nil
}

func validateIndex(value Index) error {
	if value.SchemaVersion != IndexSchema || value.Sequence < 1 || value.Sequence > 1<<63-1 || !validTime(value.CreatedAt) || !validTime(value.ExpiresAt) || !value.CreatedAt.Before(value.ExpiresAt) || value.ExpiresAt.Sub(value.CreatedAt) > 90*24*time.Hour || len(value.Releases) < 1 || len(value.Releases) > 4096 {
		return ErrInvalid
	}
	if value.Sequence == 1 && value.PreviousIndexDigest != "" || value.Sequence > 1 && !sha256Pattern.MatchString(value.PreviousIndexDigest) {
		return ErrInvalid
	}
	previous := ""
	for _, release := range value.Releases {
		key := release.PluginID + "\x00" + release.PluginVersion
		if !validID(release.PluginID, 128) || !versionPattern.MatchString(release.PluginVersion) || key <= previous || (release.Status != "active" && release.Status != "yanked") || !validOptionalText(release.YankReason, 256) || (release.Status == "yanked") != (release.YankReason != "") || len(release.Admissions) < 1 || len(release.Admissions) > 8 {
			return ErrInvalid
		}
		previous = key
		previousPlatform := ""
		for _, admission := range release.Admissions {
			if !validPlatform(admission.Platform) || admission.Platform <= previousPlatform || !sha256Pattern.MatchString(admission.EnvelopeDigest) {
				return ErrInvalid
			}
			previousPlatform = admission.Platform
		}
	}
	return nil
}

func validateStatement(value Statement) error {
	if value.Type != StatementType || value.PredicateType != AdmissionPredicateType || len(value.Subject) != 1 || validateAdmission(value.Predicate) != nil {
		return ErrInvalid
	}
	subject := value.Subject[0]
	if !ociSubjectPattern.MatchString(subject.Name) || len(subject.Digest) != 1 || !hexDigestPattern.MatchString(subject.Digest["sha256"]) || subject.Digest["sha256"] != strings.TrimPrefix(value.Predicate.Artifact.Digest, "sha256:") || !strings.HasSuffix(subject.Name, "@"+value.Predicate.Artifact.Digest) {
		return ErrInvalid
	}
	return nil
}

func validateAdmission(value Admission) error {
	if !validID(value.PluginID, 128) || !versionPattern.MatchString(value.PluginVersion) || !sha256Pattern.MatchString(value.ManifestDigest) || value.RuntimeSchema != RuntimeSchema || value.RuntimeSDK != RuntimeSDK || !validCompatibility(value.GatewayCompatibility) || !validPlatform(value.Platform) || validateDescriptor(value.Artifact, "artifact") != nil || validateDescriptor(value.SBOM, "sbom") != nil || validateDescriptor(value.Provenance, "provenance") != nil {
		return ErrInvalid
	}
	if value.Conformance.SchemaVersion != conformance.ReportSchema || value.Conformance.Outcome != "pass" || !sha256Pattern.MatchString(value.Conformance.ReportDigest) || value.Conformance.RequiredChecksDigest != conformance.RequiredChecksDigest() {
		return ErrInvalid
	}
	if !validHTTPSResource(value.Source.Repository, 512) || !commitPattern.MatchString(value.Source.Commit) || !validHTTPSResource(value.Builder.ID, 512) || !sha256Pattern.MatchString(value.Builder.InvocationDigest) {
		return ErrInvalid
	}
	return nil
}

func validateDescriptor(value Descriptor, kind string) error {
	if !sha256Pattern.MatchString(value.Digest) || value.Size < 1 || value.Size > 1<<40 {
		return ErrInvalid
	}
	switch kind {
	case "artifact":
		if value.MediaType != "application/vnd.oci.image.manifest.v1+json" {
			return ErrInvalid
		}
	case "sbom":
		if value.MediaType != "application/spdx+json" && value.MediaType != "application/vnd.cyclonedx+json" {
			return ErrInvalid
		}
	case "provenance":
		if value.MediaType != "application/vnd.in-toto+json" {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

func validTime(value time.Time) bool {
	return !value.IsZero() && value.Nanosecond() == 0 && value.Location() == time.UTC && value.Year() >= 2020 && value.Year() <= 2200
}

func validID(value string, maximum int) bool {
	return len(value) > 0 && len(value) <= maximum && idPattern.MatchString(value) && strings.TrimSpace(value) == value
}

func validPlatform(value string) bool { return value == "linux/amd64" || value == "linux/arm64" }

func validHTTPSResource(raw string, maximum int) bool {
	if len(raw) < 9 || len(raw) > maximum || !validOptionalText(raw, maximum) {
		return false
	}
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == "" && parsed.Path != ""
}

func validOptionalText(value string, maximum int) bool {
	if len(value) > maximum || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func validCompatibility(value string) bool {
	parts := strings.Fields(value)
	return len(parts) == 2 && strings.HasPrefix(parts[0], ">=") && strings.HasPrefix(parts[1], "<") && versionPattern.MatchString(strings.TrimPrefix(parts[0], ">=")) && versionPattern.MatchString(strings.TrimPrefix(parts[1], "<"))
}
