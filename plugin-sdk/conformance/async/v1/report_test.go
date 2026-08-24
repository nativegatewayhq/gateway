package asyncconformance

import (
	"bytes"
	"strings"
	"testing"
)

func TestCanonicalAsyncReportAndRequiredCheckDigest(t *testing.T) {
	report := Report{SchemaVersion: ReportSchema, PluginID: "provider.example", PluginVersion: "1.0.0", ManifestDigest: strings.Repeat("a", 64), SDKVersion: SDKVersion, Outcome: "pass"}
	for _, id := range RequiredCheckIDs() {
		report.Checks = append(report.Checks, Check{ID: id, Outcome: "pass"})
	}
	body, err := CanonicalReport(report)
	if err != nil || RequiredChecksDigest() != "sha256:6862be10259747feea09f20e443b6328d488532e167555dcca055141489035ab" {
		t.Fatalf("report=%s digest=%s err=%v", body, RequiredChecksDigest(), err)
	}
	decoded, err := DecodeReport(bytes.NewReader(body), MaximumReportBytes)
	if err != nil || decoded.PluginID != report.PluginID {
		t.Fatalf("DecodeReport()=%#v, %v", decoded, err)
	}
	report.Checks[0].ID = "callback.changed"
	if _, err = CanonicalReport(report); err == nil {
		t.Fatal("accepted a changed required check set")
	}
}

func TestAsyncReportRejectsUnknownAndSecretFields(t *testing.T) {
	for _, body := range []string{`{"schema_version":"nativegateway.plugin-async-conformance/v1","secret":"x"}`, `{"schema_version":"nativegateway.plugin-async-conformance/v1","schema_version":"duplicate"}`} {
		if _, err := DecodeReport(strings.NewReader(body), MaximumReportBytes); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
}
