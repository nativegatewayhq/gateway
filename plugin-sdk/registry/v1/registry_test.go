package registry

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	asyncconformance "github.com/nativegatewayhq/gateway/plugin-sdk/conformance/async/v1"
	conformance "github.com/nativegatewayhq/gateway/plugin-sdk/conformance/v1"
	manifest "github.com/nativegatewayhq/gateway/plugin-sdk/manifest/v1"
)

var testNow = time.Date(2026, 8, 24, 7, 0, 0, 0, time.UTC)

func TestThresholdIndexAndAdmissionBindAllEvidence(t *testing.T) {
	trust, signers := testTrust(t)
	validated := testManifest(t)
	report := testReport(validated)
	reportBody, err := conformance.CanonicalReport(report)
	if err != nil {
		t.Fatal(err)
	}
	statement := testStatement(validated, Digest(reportBody))
	admissionEnvelope := signValue(t, AdmissionPayloadType, statement, signers)
	admissionBody, _ := CanonicalEnvelope(admissionEnvelope)
	admissionDigest := Digest(admissionBody)
	index := Index{SchemaVersion: IndexSchema, Sequence: 1, CreatedAt: testNow, ExpiresAt: testNow.Add(24 * time.Hour), Releases: []Release{{PluginID: "provider.example", PluginVersion: "1.0.0", Status: "active", Admissions: []AdmissionRef{{Platform: "linux/arm64", EnvelopeDigest: admissionDigest}}}}}
	indexEnvelope := signValue(t, IndexPayloadType, index, signers)
	verifiedIndex, err := VerifyIndex(indexEnvelope, trust, testNow.Add(time.Hour), 1)
	if err != nil || verifiedIndex.Index.Sequence != 1 || verifiedIndex.EnvelopeDigest == "" {
		t.Fatalf("VerifyIndex() = %#v, %v", verifiedIndex, err)
	}
	verifiedAdmission, err := VerifyAdmission(admissionEnvelope, trust, verifiedIndex.Index.CreatedAt, AdmissionExpectation{PluginID: "provider.example", PluginVersion: "1.0.0", Platform: "linux/arm64", EnvelopeDigest: admissionDigest, GatewayVersion: "0.1.0", Manifest: validated})
	if err != nil || verifiedAdmission.Statement.Predicate.Artifact.Digest == "" {
		t.Fatalf("VerifyAdmission() = %#v, %v", verifiedAdmission, err)
	}
	if err = VerifyConformanceReport(verifiedAdmission, report); err != nil {
		t.Fatal(err)
	}
	report.Checks = report.Checks[:len(report.Checks)-1]
	if err = VerifyConformanceReport(verifiedAdmission, report); err == nil {
		t.Fatal("accepted incomplete conformance check set")
	}
}

func TestAsyncAdmissionBindsAsyncRuntimeAndConformanceProfile(t *testing.T) {
	validated, err := manifest.Parse([]byte(`{"schema_version":"nativegateway.provider/v1","id":"provider.async-example","version":"1.0.0","gateway_compatibility":">=0.1.0 <1.0.0","transport":{"kind":"http-sidecar","endpoint_ref":"async-sidecar","auth_secret_ref":"async-token"},"models":[{"id":"async-image-v1","protocols":["replicate"],"operations":["image.generate"],"capabilities":{"media_type":"application/json","output":["url"],"maximum_images":2},"async":{"contract":"async/v1","callback":true}}]}`), "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	report := asyncconformance.Report{SchemaVersion: asyncconformance.ReportSchema, PluginID: validated.Manifest.ID, PluginVersion: validated.Manifest.Version, ManifestDigest: hex.EncodeToString(validated.Digest[:]), SDKVersion: asyncconformance.SDKVersion, Outcome: "pass"}
	for _, id := range asyncconformance.RequiredCheckIDs() {
		report.Checks = append(report.Checks, asyncconformance.Check{ID: id, Outcome: "pass"})
	}
	reportBody, err := asyncconformance.CanonicalReport(report)
	if err != nil {
		t.Fatal(err)
	}
	statement := testStatement(validated, Digest(reportBody))
	statement.Predicate.RuntimeSchema = AsyncRuntimeSchema
	statement.Predicate.RuntimeSDK = AsyncRuntimeSDK
	statement.Predicate.Conformance.SchemaVersion = asyncconformance.ReportSchema
	statement.Predicate.Conformance.RequiredChecksDigest = asyncconformance.RequiredChecksDigest()
	trust, signers := testTrust(t)
	envelope := signValue(t, AdmissionPayloadType, statement, signers)
	body, _ := CanonicalEnvelope(envelope)
	verified, err := VerifyAdmission(envelope, trust, testNow, AdmissionExpectation{PluginID: validated.Manifest.ID, PluginVersion: validated.Manifest.Version, Platform: "linux/arm64", EnvelopeDigest: Digest(body), GatewayVersion: "0.1.0", Manifest: validated})
	if err != nil || VerifyAsyncConformanceReport(verified, report) != nil {
		t.Fatalf("async admission = %#v, %v", verified, err)
	}
	statement.Predicate.RuntimeSDK = RuntimeSDK
	if _, err = CanonicalStatement(statement); err == nil {
		t.Fatal("accepted a mixed async schema and sync SDK profile")
	}
}

func TestVerificationRejectsThresholdTamperExpiryAndRollback(t *testing.T) {
	trust, signers := testTrust(t)
	index := Index{SchemaVersion: IndexSchema, Sequence: 2, CreatedAt: testNow, ExpiresAt: testNow.Add(time.Hour), PreviousIndexDigest: "sha256:" + repeat("a", 64), Releases: []Release{{PluginID: "provider.example", PluginVersion: "1.0.0", Status: "active", Admissions: []AdmissionRef{{Platform: "linux/arm64", EnvelopeDigest: "sha256:" + repeat("b", 64)}}}}}
	envelope := signValue(t, IndexPayloadType, index, signers)
	if _, err := VerifyIndex(envelope, trust, testNow.Add(2*time.Hour), 1); err == nil {
		t.Fatal("accepted expired index")
	}
	if _, err := VerifyIndex(envelope, trust, testNow.Add(30*time.Minute), 3); err == nil {
		t.Fatal("accepted rollback below minimum sequence")
	}
	oneSignature := envelope
	oneSignature.Signatures = oneSignature.Signatures[:1]
	if _, err := VerifyIndex(oneSignature, trust, testNow.Add(30*time.Minute), 1); err == nil {
		t.Fatal("accepted insufficient threshold")
	}
	expiredKey := trust
	expiredKey.Keys = append([]TrustedKey(nil), trust.Keys...)
	expiredKey.Keys[0].NotAfter = index.CreatedAt
	if _, err := VerifyIndex(envelope, expiredKey, testNow.Add(30*time.Minute), 1); err == nil {
		t.Fatal("accepted a signature at the key not-after boundary")
	}
	futureKey := trust
	futureKey.Keys = append([]TrustedKey(nil), trust.Keys...)
	futureKey.Keys[0].NotBefore = index.CreatedAt.Add(time.Second)
	if _, err := VerifyIndex(envelope, futureKey, testNow.Add(30*time.Minute), 1); err == nil {
		t.Fatal("accepted a signature before the rotated key validity window")
	}
	tampered := envelope
	payload, _ := base64.StdEncoding.DecodeString(tampered.Payload)
	payload[len(payload)-2] ^= 1
	tampered.Payload = base64.StdEncoding.EncodeToString(payload)
	if _, err := VerifyIndex(tampered, trust, testNow.Add(30*time.Minute), 1); err == nil {
		t.Fatal("accepted tampered payload")
	}
}

func TestCanonicalIndexGolden(t *testing.T) {
	index := Index{SchemaVersion: IndexSchema, Sequence: 1, CreatedAt: testNow, ExpiresAt: testNow.Add(time.Hour), Releases: []Release{{PluginID: "provider.example", PluginVersion: "1.0.0", Status: "active", Admissions: []AdmissionRef{{Platform: "linux/arm64", EnvelopeDigest: "sha256:" + repeat("a", 64)}}}}}
	body, err := CanonicalIndex(index)
	want := `{"schema_version":"nativegateway.adapter-registry/v1","sequence":1,"created_at":"2026-08-24T07:00:00Z","expires_at":"2026-08-24T08:00:00Z","releases":[{"plugin_id":"provider.example","plugin_version":"1.0.0","status":"active","admissions":[{"platform":"linux/arm64","envelope_digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}]}`
	if err != nil || string(body) != want || Digest(body) != "sha256:3cf534b1427d41d7f55d3a039b17e467efd76ca70166d7f4c36f755e42c9cfb9" {
		t.Fatalf("canonical index = %q, digest=%s, err=%v", body, Digest(body), err)
	}
}

func TestStrictOrderingUnknownAndTrustKeyIdentity(t *testing.T) {
	trust, _ := testTrust(t)
	reversed := trust
	reversed.Keys = append([]TrustedKey(nil), trust.Keys...)
	reversed.Keys[0], reversed.Keys[1] = reversed.Keys[1], reversed.Keys[0]
	if _, err := CanonicalTrustPolicy(reversed); err == nil {
		t.Fatal("accepted unordered trust keys")
	}
	badKey := trust
	badKey.Keys = append([]TrustedKey(nil), trust.Keys...)
	badKey.Keys[0].KeyID = "sha256:" + repeat("0", 64)
	if _, err := CanonicalTrustPolicy(badKey); err == nil {
		t.Fatal("accepted mismatched key ID")
	}
	unknown := `{"schema_version":"nativegateway.adapter-trust/v1","threshold":1,"minimum_sequence":1,"keys":[],"secret":"x"}`
	if _, err := DecodeTrustPolicy(bytes.NewBufferString(unknown), MaximumTrustBytes); err == nil {
		t.Fatal("accepted unknown trust field")
	}
}

func TestSafeBundleLoaderAndDeterministicMatrix(t *testing.T) {
	trust, signers := testTrust(t)
	validated := testManifest(t)
	report := testReport(validated)
	reportBody, _ := conformance.CanonicalReport(report)
	admissionEnvelope := signValue(t, AdmissionPayloadType, testStatement(validated, Digest(reportBody)), signers)
	admissionBody, _ := CanonicalEnvelope(admissionEnvelope)
	admissionDigest := Digest(admissionBody)
	index := Index{SchemaVersion: IndexSchema, Sequence: 1, CreatedAt: testNow, ExpiresAt: testNow.Add(24 * time.Hour), Releases: []Release{{PluginID: "provider.example", PluginVersion: "1.0.0", Status: "active", Admissions: []AdmissionRef{{Platform: "linux/arm64", EnvelopeDigest: admissionDigest}}}}}
	indexEnvelope := signValue(t, IndexPayloadType, index, signers)
	directory := t.TempDir()
	admissions := filepath.Join(directory, "admissions")
	if err := os.Mkdir(admissions, 0o700); err != nil {
		t.Fatal(err)
	}
	trustBody, _ := CanonicalTrustPolicy(trust)
	indexBody, _ := CanonicalEnvelope(indexEnvelope)
	trustPath := filepath.Join(directory, "trust.json")
	indexPath := filepath.Join(directory, "index.dsse.json")
	admissionPath := filepath.Join(admissions, strings.TrimPrefix(admissionDigest, "sha256:")+".dsse.json")
	for path, body := range map[string][]byte{trustPath: trustBody, indexPath: indexBody, admissionPath: admissionBody} {
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := LoadSnapshot(BundleConfig{TrustPolicyFile: trustPath, IndexEnvelopeFile: indexPath, AdmissionDirectory: admissions, GatewayVersion: "0.1.0", Platform: "linux/arm64", MinimumSequence: 1, Now: testNow.Add(time.Hour)}, []manifest.Validated{validated})
	if err != nil || len(snapshot.Admissions) != 1 {
		t.Fatalf("LoadSnapshot() = %#v, %v", snapshot, err)
	}
	reports := filepath.Join(directory, "reports")
	if err := os.Mkdir(reports, 0o700); err != nil {
		t.Fatal(err)
	}
	reportPath := filepath.Join(reports, strings.TrimPrefix(Digest(reportBody), "sha256:")+".json")
	if err := os.WriteFile(reportPath, reportBody, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyReportDirectory(snapshot, reports); err != nil {
		t.Fatalf("VerifyReportDirectory() = %v", err)
	}
	if err := os.WriteFile(reportPath, append(append([]byte(nil), reportBody...), ' '), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyReportDirectory(snapshot, reports); err == nil {
		t.Fatal("accepted a report whose filename digest did not match its bytes")
	}
	if err := os.WriteFile(reportPath, reportBody, 0o600); err != nil {
		t.Fatal(err)
	}
	matrix, err := BuildMatrix(snapshot)
	canonical, canonicalErr := CanonicalMatrix(matrix)
	markdown, markdownErr := RenderMatrixMarkdown(matrix)
	if err != nil || canonicalErr != nil || markdownErr != nil || !bytes.Contains(canonical, []byte(`"plugin_id":"provider.example"`)) || !bytes.Contains(markdown, []byte("Official Adapter compatibility matrix")) {
		t.Fatalf("matrix errors = %v, %v, %v\n%s\n%s", err, canonicalErr, markdownErr, canonical, markdown)
	}
	for _, forbidden := range []string{"secret", "endpoint", "prompt", "Authorization"} {
		if bytes.Contains(canonical, []byte(forbidden)) || bytes.Contains(markdown, []byte(forbidden)) {
			t.Fatalf("matrix leaked %q", forbidden)
		}
	}
	decodedMatrix, decodeErr := DecodeMatrix(bytes.NewReader(canonical), 1<<20)
	if decodeErr != nil || decodedMatrix.IndexDigest != matrix.IndexDigest {
		t.Fatalf("DecodeMatrix() = %#v, %v", decodedMatrix, decodeErr)
	}
	yanked := index
	yanked.Releases = append([]Release(nil), index.Releases...)
	yanked.Releases[0].Status = "yanked"
	yanked.Releases[0].YankReason = "upstream security review"
	yankedEnvelope := signValue(t, IndexPayloadType, yanked, signers)
	yankedBody, _ := CanonicalEnvelope(yankedEnvelope)
	if err := os.WriteFile(indexPath, yankedBody, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSnapshot(BundleConfig{TrustPolicyFile: trustPath, IndexEnvelopeFile: indexPath, AdmissionDirectory: admissions, GatewayVersion: "0.1.0", Platform: "linux/arm64", MinimumSequence: 1, Now: testNow.Add(time.Hour)}, []manifest.Validated{validated}); err == nil {
		t.Fatal("accepted yanked release")
	}
	if err := os.WriteFile(indexPath, indexBody, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(indexPath, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSnapshot(BundleConfig{TrustPolicyFile: trustPath, IndexEnvelopeFile: indexPath, AdmissionDirectory: admissions, GatewayVersion: "0.1.0", Platform: "linux/arm64", MinimumSequence: 1, Now: testNow.Add(time.Hour)}, []manifest.Validated{validated}); err == nil {
		t.Fatal("accepted writable index file")
	}
}

func TestBundleRejectsSameSequenceEquivocationAndBrokenChain(t *testing.T) {
	trust, signers := testTrust(t)
	validated := testManifest(t)
	reportBody, _ := conformance.CanonicalReport(testReport(validated))
	admissionEnvelope := signValue(t, AdmissionPayloadType, testStatement(validated, Digest(reportBody)), signers)
	admissionBody, _ := CanonicalEnvelope(admissionEnvelope)
	admissionDigest := Digest(admissionBody)
	index := Index{SchemaVersion: IndexSchema, Sequence: 2, CreatedAt: testNow, ExpiresAt: testNow.Add(24 * time.Hour), PreviousIndexDigest: "sha256:" + repeat("9", 64), Releases: []Release{{PluginID: "provider.example", PluginVersion: "1.0.0", Status: "active", Admissions: []AdmissionRef{{Platform: "linux/arm64", EnvelopeDigest: admissionDigest}}}}}
	indexEnvelope := signValue(t, IndexPayloadType, index, signers)
	directory := t.TempDir()
	admissions := filepath.Join(directory, "admissions")
	_ = os.Mkdir(admissions, 0o700)
	trustBody, _ := CanonicalTrustPolicy(trust)
	indexBody, _ := CanonicalEnvelope(indexEnvelope)
	trustPath := filepath.Join(directory, "trust.json")
	indexPath := filepath.Join(directory, "index.dsse.json")
	_ = os.WriteFile(trustPath, trustBody, 0o600)
	_ = os.WriteFile(indexPath, indexBody, 0o600)
	_ = os.WriteFile(filepath.Join(admissions, strings.TrimPrefix(admissionDigest, "sha256:")+".dsse.json"), admissionBody, 0o600)
	base := BundleConfig{TrustPolicyFile: trustPath, IndexEnvelopeFile: indexPath, AdmissionDirectory: admissions, GatewayVersion: "0.1.0", Platform: "linux/arm64", MinimumSequence: 1, Now: testNow.Add(time.Hour), LastSequence: 1, LastIndexDigest: "sha256:" + repeat("8", 64)}
	if _, err := LoadSnapshot(base, []manifest.Validated{validated}); err == nil {
		t.Fatal("accepted broken previous-index chain")
	}
	base.LastSequence = 2
	if _, err := LoadSnapshot(base, []manifest.Validated{validated}); err == nil {
		t.Fatal("accepted same-sequence equivocation")
	}
}

func testTrust(t *testing.T) (TrustPolicy, []ed25519.PrivateKey) {
	t.Helper()
	keys := make([]TrustedKey, 0, 2)
	signers := make(map[string]ed25519.PrivateKey)
	for range 2 {
		publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(publicKey)
		keyID := "sha256:" + hex.EncodeToString(digest[:])
		keys = append(keys, TrustedKey{KeyID: keyID, Algorithm: "ed25519", PublicKey: base64.StdEncoding.EncodeToString(publicKey), NotBefore: testNow.Add(-time.Hour), NotAfter: testNow.Add(7 * 24 * time.Hour)})
		signers[keyID] = privateKey
	}
	sort.Slice(keys, func(left, right int) bool { return keys[left].KeyID < keys[right].KeyID })
	orderedSigners := make([]ed25519.PrivateKey, len(keys))
	for index, key := range keys {
		orderedSigners[index] = signers[key.KeyID]
	}
	return TrustPolicy{SchemaVersion: TrustSchema, Threshold: 2, MinimumSequence: 1, Keys: keys}, orderedSigners
}

func signValue(t *testing.T, payloadType string, value any, signers []ed25519.PrivateKey) Envelope {
	t.Helper()
	var payload []byte
	var err error
	switch typed := value.(type) {
	case Index:
		payload, err = CanonicalIndex(typed)
	case Statement:
		payload, err = CanonicalStatement(typed)
	default:
		t.Fatal("unsupported signed value")
	}
	if err != nil {
		t.Fatal(err)
	}
	pae, err := PreAuthenticationEncoding(payloadType, payload)
	if err != nil {
		t.Fatal(err)
	}
	signatures := make([]Signature, 0, len(signers))
	for _, signer := range signers {
		publicKey := signer.Public().(ed25519.PublicKey)
		digest := sha256.Sum256(publicKey)
		signatures = append(signatures, Signature{KeyID: "sha256:" + hex.EncodeToString(digest[:]), Sig: base64.StdEncoding.EncodeToString(ed25519.Sign(signer, pae))})
	}
	sort.Slice(signatures, func(left, right int) bool { return signatures[left].KeyID < signatures[right].KeyID })
	return Envelope{PayloadType: payloadType, Payload: base64.StdEncoding.EncodeToString(payload), Signatures: signatures}
}

func testManifest(t *testing.T) manifest.Validated {
	t.Helper()
	body := []byte(`{"schema_version":"nativegateway.provider/v1","id":"provider.example","version":"1.0.0","gateway_compatibility":">=0.1.0 <1.0.0","transport":{"kind":"http-sidecar","endpoint_ref":"example-sidecar","auth_secret_ref":"example-sidecar-token"},"models":[{"id":"example-image-v1","protocols":["openai"],"operations":["image.generate"],"capabilities":{"media_type":"application/json","output":["base64"],"maximum_images":2}}]}`)
	value, err := manifest.Parse(body, "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func testReport(value manifest.Validated) conformance.Report {
	checks := make([]conformance.Check, 0, len(conformance.RequiredCheckIDs()))
	for _, id := range conformance.RequiredCheckIDs() {
		checks = append(checks, conformance.Check{ID: id, Outcome: "pass"})
	}
	return conformance.Report{SchemaVersion: conformance.ReportSchema, PluginID: value.Manifest.ID, PluginVersion: value.Manifest.Version, ManifestDigest: hex.EncodeToString(value.Digest[:]), SDKVersion: conformance.SDKVersion, Outcome: "pass", Checks: checks}
}

func testStatement(value manifest.Validated, reportDigest string) Statement {
	artifactDigest := "sha256:" + repeat("1", 64)
	artifact := Descriptor{MediaType: "application/vnd.oci.image.manifest.v1+json", Digest: artifactDigest, Size: 1234}
	predicate := Admission{PluginID: value.Manifest.ID, PluginVersion: value.Manifest.Version, ManifestDigest: "sha256:" + hex.EncodeToString(value.Digest[:]), RuntimeSchema: RuntimeSchema, RuntimeSDK: RuntimeSDK, GatewayCompatibility: value.Manifest.GatewayCompatibility, Platform: "linux/arm64", Artifact: artifact, Conformance: ConformanceEvidence{ReportDigest: reportDigest, SchemaVersion: conformance.ReportSchema, RequiredChecksDigest: conformance.RequiredChecksDigest(), Outcome: "pass"}, Source: SourceEvidence{Repository: "https://github.com/example/provider-adapter", Commit: repeat("2", 40)}, Builder: BuilderEvidence{ID: "https://github.com/nativegatewayhq/registry/builders/release-v1", InvocationDigest: "sha256:" + repeat("3", 64)}, SBOM: Descriptor{MediaType: "application/spdx+json", Digest: "sha256:" + repeat("4", 64), Size: 2345}, Provenance: Descriptor{MediaType: "application/vnd.in-toto+json", Digest: "sha256:" + repeat("5", 64), Size: 3456}}
	return Statement{Type: StatementType, Subject: []Subject{{Name: "ghcr.io/example/provider-adapter@" + artifactDigest, Digest: map[string]string{"sha256": strings.TrimPrefix(artifactDigest, "sha256:")}}}, PredicateType: AdmissionPredicateType, Predicate: predicate}
}

func repeat(value string, count int) string {
	result := ""
	for range count {
		result += value
	}
	return result
}
