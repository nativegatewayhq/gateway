package videoconformance

import (
	"bytes"
	"testing"
)

func passingReport() Report {
	checks := make([]Check, len(requiredCheckIDs))
	for index, id := range requiredCheckIDs {
		checks[index] = Check{ID: id, Outcome: "pass"}
	}
	return Report{SchemaVersion: ReportSchema, PluginID: "provider.video-example", PluginVersion: "1.0.0", ManifestDigest: string(bytes.Repeat([]byte{'a'}, 64)), SDKVersion: SDKVersion, Outcome: "pass", Checks: checks}
}
func TestVideoReportHasStableOrderedCheckDigest(t *testing.T) {
	value := passingReport()
	body, err := CanonicalReport(value)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeReport(bytes.NewReader(body), MaximumReportBytes)
	if err != nil || len(decoded.Checks) != 15 {
		t.Fatalf("decoded=%#v err=%v", decoded, err)
	}
	if RequiredChecksDigest() != "sha256:4ec21959a000b79f01828e7885b2367592200aea0ab74ee193107facef772f96" {
		t.Fatalf("digest=%s", RequiredChecksDigest())
	}
	value.Checks[0], value.Checks[1] = value.Checks[1], value.Checks[0]
	if ValidateReport(value) == nil {
		t.Fatal("unordered checks accepted")
	}
}
