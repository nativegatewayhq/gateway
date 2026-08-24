package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	conformance "github.com/nativegatewayhq/gateway/plugin-sdk/conformance/v1"
	manifest "github.com/nativegatewayhq/gateway/plugin-sdk/manifest/v1"
	registry "github.com/nativegatewayhq/gateway/plugin-sdk/registry/v1"
)

func TestStableConfigurationAndVerificationExitCodesAreSecretSafe(t *testing.T) {
	secret := "do-not-print-private-material"
	var stdout, stderr bytes.Buffer
	if exit := run([]string{"verify", "-trust-file", secret}, time.Now, &stdout, &stderr); exit != 2 || stdout.Len() != 0 || strings.Contains(stderr.String(), secret) || stderr.String() != "adapter registry configuration failed\n" {
		t.Fatalf("configuration run = %d %q %q", exit, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	exit := run([]string{"verify", "-trust-file", "/missing/trust.json", "-index-file", "/missing/index.json", "-admission-dir", "/missing/admissions", "-manifest-dir", "/missing/manifests", "-report-dir", "/missing/reports"}, func() time.Time { return time.Date(2026, 8, 24, 7, 0, 0, 0, time.UTC) }, &stdout, &stderr)
	if exit != 1 || stdout.Len() != 0 || stderr.String() != "adapter registry verification failed\n" {
		t.Fatalf("verification run = %d %q %q", exit, stdout.String(), stderr.String())
	}
}

func TestRejectsUnknownCommandAndArguments(t *testing.T) {
	for _, arguments := range [][]string{{}, {"download"}, {"verify", "unexpected"}} {
		if exit := run(arguments, time.Now, &bytes.Buffer{}, &bytes.Buffer{}); exit != 2 {
			t.Fatalf("run(%q) = %d", arguments, exit)
		}
	}
}

func TestVerifyAndMatrixCommandsUseSameOfflineSnapshot(t *testing.T) {
	arguments, now := testBundle(t)
	var stdout, stderr bytes.Buffer
	if exit := run(append([]string{"verify"}, arguments...), func() time.Time { return now }, &stdout, &stderr); exit != 0 || stderr.Len() != 0 || !strings.Contains(stdout.String(), "verified sequence 1") {
		t.Fatalf("verify = %d %q %q", exit, stdout.String(), stderr.String())
	}
	stdout.Reset()
	if exit := run(append(append([]string{"matrix"}, arguments...), "-json"), func() time.Time { return now }, &stdout, &stderr); exit != 0 || !strings.Contains(stdout.String(), `"schema_version":"nativegateway.adapter-matrix/v1"`) || strings.Contains(stdout.String(), "secret") || strings.Contains(stdout.String(), "endpoint") {
		t.Fatalf("matrix = %d %q %q", exit, stdout.String(), stderr.String())
	}
}

func testBundle(t *testing.T) ([]string, time.Time) {
	t.Helper()
	now := time.Date(2026, 8, 24, 7, 0, 0, 0, time.UTC)
	seed := bytes.Repeat([]byte{7}, ed25519.SeedSize)
	privateKey := ed25519.NewKeyFromSeed(seed)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	keyDigest := sha256.Sum256(publicKey)
	keyID := "sha256:" + hex.EncodeToString(keyDigest[:])
	trust := registry.TrustPolicy{SchemaVersion: registry.TrustSchema, Threshold: 1, MinimumSequence: 1, Keys: []registry.TrustedKey{{KeyID: keyID, Algorithm: "ed25519", PublicKey: base64.StdEncoding.EncodeToString(publicKey), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour)}}}
	manifestBody := []byte(`{"schema_version":"nativegateway.provider/v1","id":"provider.example","version":"1.0.0","gateway_compatibility":">=0.1.0 <1.0.0","transport":{"kind":"http-sidecar","endpoint_ref":"sidecar","auth_secret_ref":"token"},"models":[{"id":"example-image-v1","protocols":["openai"],"operations":["image.generate"],"capabilities":{"media_type":"application/json","output":["base64"],"maximum_images":2}}]}`)
	validated, err := manifest.Parse(manifestBody, "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	report := conformance.Report{SchemaVersion: conformance.ReportSchema, PluginID: "provider.example", PluginVersion: "1.0.0", ManifestDigest: hex.EncodeToString(validated.Digest[:]), SDKVersion: conformance.SDKVersion, Outcome: "pass"}
	for _, id := range conformance.RequiredCheckIDs() {
		report.Checks = append(report.Checks, conformance.Check{ID: id, Outcome: "pass"})
	}
	reportBody, err := conformance.CanonicalReport(report)
	if err != nil {
		t.Fatal(err)
	}
	reportDigest := registry.Digest(reportBody)
	artifactDigest := "sha256:" + strings.Repeat("1", 64)
	predicate := registry.Admission{PluginID: "provider.example", PluginVersion: "1.0.0", ManifestDigest: "sha256:" + hex.EncodeToString(validated.Digest[:]), RuntimeSchema: registry.RuntimeSchema, RuntimeSDK: registry.RuntimeSDK, GatewayCompatibility: ">=0.1.0 <1.0.0", Platform: "linux/arm64", Artifact: registry.Descriptor{MediaType: "application/vnd.oci.image.manifest.v1+json", Digest: artifactDigest, Size: 123}, Conformance: registry.ConformanceEvidence{ReportDigest: reportDigest, SchemaVersion: conformance.ReportSchema, RequiredChecksDigest: conformance.RequiredChecksDigest(), Outcome: "pass"}, Source: registry.SourceEvidence{Repository: "https://github.com/example/provider", Commit: strings.Repeat("3", 40)}, Builder: registry.BuilderEvidence{ID: "https://github.com/nativegatewayhq/registry/builders/release-v1", InvocationDigest: "sha256:" + strings.Repeat("4", 64)}, SBOM: registry.Descriptor{MediaType: "application/spdx+json", Digest: "sha256:" + strings.Repeat("5", 64), Size: 234}, Provenance: registry.Descriptor{MediaType: "application/vnd.in-toto+json", Digest: "sha256:" + strings.Repeat("6", 64), Size: 345}}
	statement := registry.Statement{Type: registry.StatementType, Subject: []registry.Subject{{Name: "ghcr.io/example/provider@" + artifactDigest, Digest: map[string]string{"sha256": strings.TrimPrefix(artifactDigest, "sha256:")}}}, PredicateType: registry.AdmissionPredicateType, Predicate: predicate}
	statementBody, _ := registry.CanonicalStatement(statement)
	admissionEnvelope := signEnvelope(t, registry.AdmissionPayloadType, statementBody, keyID, privateKey)
	admissionBody, _ := registry.CanonicalEnvelope(admissionEnvelope)
	admissionDigest := registry.Digest(admissionBody)
	index := registry.Index{SchemaVersion: registry.IndexSchema, Sequence: 1, CreatedAt: now, ExpiresAt: now.Add(12 * time.Hour), Releases: []registry.Release{{PluginID: "provider.example", PluginVersion: "1.0.0", Status: "active", Admissions: []registry.AdmissionRef{{Platform: "linux/arm64", EnvelopeDigest: admissionDigest}}}}}
	indexBody, _ := registry.CanonicalIndex(index)
	indexEnvelope := signEnvelope(t, registry.IndexPayloadType, indexBody, keyID, privateKey)
	trustBody, _ := registry.CanonicalTrustPolicy(trust)
	indexEnvelopeBody, _ := registry.CanonicalEnvelope(indexEnvelope)
	directory := t.TempDir()
	manifestDirectory := filepath.Join(directory, "manifests")
	admissionDirectory := filepath.Join(directory, "admissions")
	reportDirectory := filepath.Join(directory, "reports")
	if err = os.Mkdir(manifestDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(admissionDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(reportDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	trustPath := filepath.Join(directory, "trust.json")
	indexPath := filepath.Join(directory, "index.dsse.json")
	files := map[string][]byte{trustPath: trustBody, indexPath: indexEnvelopeBody, filepath.Join(manifestDirectory, "provider.example.json"): manifestBody, filepath.Join(admissionDirectory, strings.TrimPrefix(admissionDigest, "sha256:")+".dsse.json"): admissionBody, filepath.Join(reportDirectory, strings.TrimPrefix(reportDigest, "sha256:")+".json"): reportBody}
	for path, body := range files {
		if err = os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return []string{"-trust-file", trustPath, "-index-file", indexPath, "-admission-dir", admissionDirectory, "-manifest-dir", manifestDirectory, "-report-dir", reportDirectory, "-platform", "linux/arm64"}, now.Add(time.Hour)
}

func signEnvelope(t *testing.T, payloadType string, payload []byte, keyID string, key ed25519.PrivateKey) registry.Envelope {
	t.Helper()
	pae, err := registry.PreAuthenticationEncoding(payloadType, payload)
	if err != nil {
		t.Fatal(err)
	}
	return registry.Envelope{PayloadType: payloadType, Payload: base64.StdEncoding.EncodeToString(payload), Signatures: []registry.Signature{{KeyID: keyID, Sig: base64.StdEncoding.EncodeToString(ed25519.Sign(key, pae))}}}
}
