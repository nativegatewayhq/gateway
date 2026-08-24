package async

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
)

var testIdentity = Identity{RequestID: "request_1", GatewayJobID: "job_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", PluginID: "provider.example", PluginVersion: "1.0.0", ManifestDigest: strings.Repeat("b", 64)}
var testExpectation = Expectation{Identity: testIdentity, Output: "url", MaximumImages: 2}

func TestCanonicalAsyncLifecycleAndStrictDecode(t *testing.T) {
	submit := SubmitRequest{SchemaVersion: SubmitRequestSchema, RequestID: testIdentity.RequestID, GatewayJobID: testIdentity.GatewayJobID, PluginID: testIdentity.PluginID, PluginVersion: testIdentity.PluginVersion, ManifestDigest: testIdentity.ManifestDigest, Protocol: "replicate", Operation: "image.generate", Model: "image-v1", Input: ImageInput{Prompt: "draw a lighthouse", Images: 2}, CallbackURL: "https://gateway.example/internal/webhooks/plugin/job/token"}
	body, err := CanonicalSubmitRequest(submit)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeSubmitRequest(bytes.NewReader(body), 1<<20)
	if err != nil || decoded.Identity() != testIdentity {
		t.Fatalf("DecodeSubmitRequest() = %#v, %v", decoded, err)
	}
	control := ControlRequest{SchemaVersion: ControlRequestSchema, RequestID: testIdentity.RequestID, GatewayJobID: testIdentity.GatewayJobID, PluginID: testIdentity.PluginID, PluginVersion: testIdentity.PluginVersion, ManifestDigest: testIdentity.ManifestDigest, Action: "poll", ProviderJobRef: "upstream:job-1"}
	body, err = CanonicalControlRequest(control)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = DecodeControlRequest(bytes.NewReader(body), 1<<20); err != nil {
		t.Fatal(err)
	}
	response := SubmitResponse{SchemaVersion: SubmitResponseSchema, RequestID: testIdentity.RequestID, GatewayJobID: testIdentity.GatewayJobID, PluginID: testIdentity.PluginID, PluginVersion: testIdentity.PluginVersion, ManifestDigest: testIdentity.ManifestDigest, ProviderJobRef: "upstream:job-1", Observation: Observation{Status: "QUEUED"}}
	body, err = CanonicalSubmitResponse(response, testExpectation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = DecodeSubmitResponse(bytes.NewReader(body), 1<<20, testExpectation); err != nil {
		t.Fatal(err)
	}
	terminal := ObservationResponse{SchemaVersion: ObservationResponseSchema, RequestID: testIdentity.RequestID, GatewayJobID: testIdentity.GatewayJobID, PluginID: testIdentity.PluginID, PluginVersion: testIdentity.PluginVersion, ManifestDigest: testIdentity.ManifestDigest, Observation: Observation{Status: "SUCCEEDED", Result: &Result{Images: []Image{{MIMEType: "image/png", URL: "https://result.example/1.png"}}, Usage: Usage{Dimension: "output", Unit: "image", Quantity: 1}}}}
	body, err = CanonicalObservationResponse(terminal, testExpectation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = DecodeObservationResponse(bytes.NewReader(body), 1<<20, testExpectation); err != nil {
		t.Fatal(err)
	}
}

func TestCallbackSignatureBindsTimestampDeliveryAndExactBody(t *testing.T) {
	secret, _ := hex.DecodeString(strings.Repeat("42", 32))
	body := []byte(`{"schema_version":"nativegateway.plugin-async-callback/v1"}`)
	signature, err := SignCallback(secret, 1787550000, "delivery_abcdefghijklmnop", body)
	if err != nil || VerifyCallbackSignature(secret, 1787550000, "delivery_abcdefghijklmnop", body, signature) != nil {
		t.Fatalf("signature=%q err=%v", signature, err)
	}
	for _, mutation := range []struct {
		timestamp int64
		delivery  string
		body      []byte
	}{
		{1787550001, "delivery_abcdefghijklmnop", body},
		{1787550000, "delivery_qrstuvwxyzABCDEF", body},
		{1787550000, "delivery_abcdefghijklmnop", append(append([]byte(nil), body...), ' ')},
	} {
		if VerifyCallbackSignature(secret, mutation.timestamp, mutation.delivery, mutation.body, signature) == nil {
			t.Fatal("accepted a mutated callback signature input")
		}
	}
	if _, err = SignCallback(secret[:16], 1787550000, "delivery_abcdefghijklmnop", body); err == nil {
		t.Fatal("accepted a non-32-byte callback key")
	}
}

func TestAsyncWireRejectsAmbiguousIdentityResultAndUnknownFields(t *testing.T) {
	invalid := []string{`{"schema_version":"nativegateway.plugin-async-control-request/v1","request_id":"r","request_id":"r2"}`, `{"schema_version":"nativegateway.plugin-async-control-request/v1","request_id":"r","unknown":true}`, `{"schema_version":"nativegateway.plugin-async-control-request/v1"} trailing`}
	for _, body := range invalid {
		if _, err := DecodeControlRequest(strings.NewReader(body), 1<<20); err == nil {
			t.Fatalf("accepted invalid body %q", body)
		}
	}
	bad := ObservationResponse{SchemaVersion: ObservationResponseSchema, RequestID: testIdentity.RequestID, GatewayJobID: testIdentity.GatewayJobID, PluginID: testIdentity.PluginID, PluginVersion: testIdentity.PluginVersion, ManifestDigest: testIdentity.ManifestDigest, Observation: Observation{Status: "SUCCEEDED", Result: &Result{Images: []Image{{MIMEType: "image/png", URL: "https://result.example/1.png"}}, Usage: Usage{Dimension: "output", Unit: "image", Quantity: 2}}}}
	if _, err := CanonicalObservationResponse(bad, testExpectation); err == nil {
		t.Fatal("accepted usage/result mismatch")
	}
	bad.Observation.Error = &PluginError{Category: "internal", Message: "failed"}
	if _, err := CanonicalObservationResponse(bad, testExpectation); err == nil {
		t.Fatal("accepted ambiguous success/error")
	}
}

func TestCallbackBindsDeliveryProviderRefAndTerminalObservation(t *testing.T) {
	callback := Callback{SchemaVersion: CallbackSchema, DeliveryID: "delivery_abcdefghijklmnop", RequestID: testIdentity.RequestID, GatewayJobID: testIdentity.GatewayJobID, PluginID: testIdentity.PluginID, PluginVersion: testIdentity.PluginVersion, ManifestDigest: testIdentity.ManifestDigest, Protocol: "replicate", Operation: "image.generate", Model: "example-image-v1", ProviderJobRef: "upstream:job-1", Observation: Observation{Status: "CANCELED"}}
	body, err := CanonicalCallback(callback, testExpectation)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeCallback(bytes.NewReader(body), 1<<20, testExpectation)
	if err != nil || decoded.DeliveryID != callback.DeliveryID {
		t.Fatalf("DecodeCallback() = %#v, %v", decoded, err)
	}
	callback.ProviderJobRef = "https://untrusted.example/control"
	if _, err = CanonicalCallback(callback, testExpectation); err == nil {
		t.Fatal("accepted a control URL as opaque Provider Job ref")
	}
}
