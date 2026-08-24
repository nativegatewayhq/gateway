package registry

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"strconv"
	"time"

	asyncconformance "github.com/nativegatewayhq/gateway/plugin-sdk/conformance/async/v1"
	conformance "github.com/nativegatewayhq/gateway/plugin-sdk/conformance/v1"
	videoconformance "github.com/nativegatewayhq/gateway/plugin-sdk/conformance/video/v1"
	manifest "github.com/nativegatewayhq/gateway/plugin-sdk/manifest/v1"
)

type AdmissionExpectation struct {
	PluginID, PluginVersion, Platform, EnvelopeDigest, GatewayVersion string
	Manifest                                                          manifest.Validated
}

// PreAuthenticationEncoding implements DSSE v1 PAE for external signers.
func PreAuthenticationEncoding(payloadType string, payload []byte) ([]byte, error) {
	if (payloadType != IndexPayloadType && payloadType != AdmissionPayloadType) || len(payload) < 2 || len(payload) > MaximumIndexBytes {
		return nil, ErrInvalid
	}
	prefix := "DSSEv1 " + strconv.Itoa(len(payloadType)) + " " + payloadType + " " + strconv.Itoa(len(payload)) + " "
	return append([]byte(prefix), payload...), nil
}

func VerifyIndex(envelope Envelope, trust TrustPolicy, now time.Time, minimumSequence uint64) (VerifiedIndex, error) {
	if validateEnvelope(envelope) != nil || validateTrustPolicy(trust) != nil || envelope.PayloadType != IndexPayloadType || !validTime(now.UTC().Truncate(time.Second)) {
		return VerifiedIndex{}, ErrInvalid
	}
	payload, err := base64.StdEncoding.DecodeString(envelope.Payload)
	if err != nil || len(payload) > MaximumIndexBytes {
		return VerifiedIndex{}, ErrInvalid
	}
	index, err := DecodeIndex(bytes.NewReader(payload), MaximumIndexBytes)
	if err != nil {
		return VerifiedIndex{}, ErrInvalid
	}
	canonical, err := CanonicalIndex(index)
	if err != nil || !bytes.Equal(canonical, payload) || index.CreatedAt.After(now.Add(5*time.Minute)) || !now.Before(index.ExpiresAt) || index.Sequence < trust.MinimumSequence || index.Sequence < minimumSequence || verifySignatures(envelope, trust, index.CreatedAt) != nil {
		return VerifiedIndex{}, ErrInvalid
	}
	canonicalEnvelope, err := CanonicalEnvelope(envelope)
	if err != nil {
		return VerifiedIndex{}, ErrInvalid
	}
	return VerifiedIndex{Index: index, EnvelopeDigest: Digest(canonicalEnvelope), PayloadDigest: Digest(payload)}, nil
}

func VerifyAdmission(envelope Envelope, trust TrustPolicy, signedAt time.Time, expected AdmissionExpectation) (VerifiedAdmission, error) {
	if validateEnvelope(envelope) != nil || validateTrustPolicy(trust) != nil || envelope.PayloadType != AdmissionPayloadType || !validTime(signedAt) || !validID(expected.PluginID, 128) || !versionPattern.MatchString(expected.PluginVersion) || !validPlatform(expected.Platform) || !sha256Pattern.MatchString(expected.EnvelopeDigest) || expected.Manifest.Manifest.ID != expected.PluginID || expected.Manifest.Manifest.Version != expected.PluginVersion {
		return VerifiedAdmission{}, ErrInvalid
	}
	canonicalEnvelope, err := CanonicalEnvelope(envelope)
	if err != nil || Digest(canonicalEnvelope) != expected.EnvelopeDigest || verifySignatures(envelope, trust, signedAt) != nil {
		return VerifiedAdmission{}, ErrInvalid
	}
	payload, err := base64.StdEncoding.DecodeString(envelope.Payload)
	if err != nil || len(payload) > MaximumAdmissionBytes {
		return VerifiedAdmission{}, ErrInvalid
	}
	statement, err := DecodeStatement(bytes.NewReader(payload), MaximumAdmissionBytes)
	if err != nil {
		return VerifiedAdmission{}, ErrInvalid
	}
	canonical, err := CanonicalStatement(statement)
	predicate := statement.Predicate
	expectedRuntime := manifest.ExecutionContract(expected.Manifest)
	expectedSchema := RuntimeSchema
	if expectedRuntime == AsyncRuntimeSDK {
		expectedSchema = AsyncRuntimeSchema
	} else if expectedRuntime == VideoRuntimeSDK {
		expectedSchema = VideoRuntimeSchema
	}
	if err != nil || !bytes.Equal(canonical, payload) || predicate.PluginID != expected.PluginID || predicate.PluginVersion != expected.PluginVersion || predicate.Platform != expected.Platform || predicate.ManifestDigest != "sha256:"+expected.ManifestDigestHex() || predicate.GatewayCompatibility != expected.Manifest.Manifest.GatewayCompatibility || predicate.RuntimeSDK != expectedRuntime || predicate.RuntimeSchema != expectedSchema || !manifest.IsCompatible(predicate.GatewayCompatibility, expected.GatewayVersion) {
		return VerifiedAdmission{}, ErrInvalid
	}
	return VerifiedAdmission{Statement: statement, EnvelopeDigest: expected.EnvelopeDigest, PayloadDigest: Digest(payload)}, nil
}

func VerifyVideoConformanceReport(admission VerifiedAdmission, report videoconformance.Report) error {
	if admission.Statement.Predicate.Conformance.SchemaVersion != videoconformance.ReportSchema || admission.Statement.Predicate.RuntimeSDK != VideoRuntimeSDK || report.PluginID != admission.Statement.Predicate.PluginID || report.PluginVersion != admission.Statement.Predicate.PluginVersion || "sha256:"+report.ManifestDigest != admission.Statement.Predicate.ManifestDigest || report.Outcome != "pass" {
		return ErrInvalid
	}
	body, err := videoconformance.CanonicalReport(report)
	if err != nil || Digest(body) != admission.Statement.Predicate.Conformance.ReportDigest || videoconformance.RequiredChecksDigest() != admission.Statement.Predicate.Conformance.RequiredChecksDigest {
		return ErrInvalid
	}
	return nil
}

func VerifyAsyncConformanceReport(admission VerifiedAdmission, report asyncconformance.Report) error {
	if admission.Statement.Predicate.Conformance.SchemaVersion != asyncconformance.ReportSchema || admission.Statement.Predicate.RuntimeSDK != AsyncRuntimeSDK || report.PluginID != admission.Statement.Predicate.PluginID || report.PluginVersion != admission.Statement.Predicate.PluginVersion || "sha256:"+report.ManifestDigest != admission.Statement.Predicate.ManifestDigest || report.Outcome != "pass" {
		return ErrInvalid
	}
	body, err := asyncconformance.CanonicalReport(report)
	if err != nil || Digest(body) != admission.Statement.Predicate.Conformance.ReportDigest || asyncconformance.RequiredChecksDigest() != admission.Statement.Predicate.Conformance.RequiredChecksDigest {
		return ErrInvalid
	}
	return nil
}

func VerifyConformanceReport(admission VerifiedAdmission, report conformance.Report) error {
	if admission.Statement.Predicate.Conformance.SchemaVersion != conformance.ReportSchema || report.PluginID != admission.Statement.Predicate.PluginID || report.PluginVersion != admission.Statement.Predicate.PluginVersion || "sha256:"+report.ManifestDigest != admission.Statement.Predicate.ManifestDigest || report.Outcome != "pass" {
		return ErrInvalid
	}
	body, err := conformance.CanonicalReport(report)
	if err != nil || Digest(body) != admission.Statement.Predicate.Conformance.ReportDigest || conformance.RequiredChecksDigest() != admission.Statement.Predicate.Conformance.RequiredChecksDigest {
		return ErrInvalid
	}
	required := conformance.RequiredCheckIDs()
	if len(report.Checks) != len(required) {
		return ErrInvalid
	}
	for index, check := range report.Checks {
		if check.ID != required[index] || check.Outcome != "pass" || check.Category != "" {
			return ErrInvalid
		}
	}
	return nil
}

func (expectation AdmissionExpectation) ManifestDigestHex() string {
	return fmtHex(expectation.Manifest.Digest[:])
}

func verifySignatures(envelope Envelope, trust TrustPolicy, at time.Time) error {
	pae, err := PreAuthenticationEncoding(envelope.PayloadType, mustDecodeBase64(envelope.Payload))
	if err != nil {
		return ErrInvalid
	}
	keys := make(map[string]TrustedKey, len(trust.Keys))
	for _, key := range trust.Keys {
		keys[key.KeyID] = key
	}
	valid := 0
	for _, signature := range envelope.Signatures {
		key, ok := keys[signature.KeyID]
		if !ok || at.Before(key.NotBefore) || !at.Before(key.NotAfter) {
			continue
		}
		publicKey, publicErr := base64.StdEncoding.DecodeString(key.PublicKey)
		signatureBytes, signatureErr := base64.StdEncoding.DecodeString(signature.Sig)
		if publicErr == nil && signatureErr == nil && ed25519.Verify(ed25519.PublicKey(publicKey), pae, signatureBytes) {
			valid++
		}
	}
	if valid < trust.Threshold {
		return ErrInvalid
	}
	return nil
}

func mustDecodeBase64(value string) []byte {
	decoded, _ := base64.StdEncoding.DecodeString(value)
	return decoded
}

func fmtHex(value []byte) string {
	const alphabet = "0123456789abcdef"
	result := make([]byte, len(value)*2)
	for index, item := range value {
		result[index*2] = alphabet[item>>4]
		result[index*2+1] = alphabet[item&15]
	}
	return string(result)
}
