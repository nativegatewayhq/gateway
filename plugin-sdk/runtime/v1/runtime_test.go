package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

var testIdentity = Identity{RequestID: "req_test", PluginID: "provider.example", PluginVersion: "1.0.0", ManifestDigest: "b2210aa93268add9e0fafd5e7735fd82b3b5db33bcd7a63e1bbeb74d7975c1a9"}
var testExpectation = Expectation{Identity: testIdentity, Protocol: "openai", Model: "example-image-v1", Output: "base64", MaximumImages: 2}

func TestCanonicalRequestWireGolden(t *testing.T) {
	value := ExecuteRequest{SchemaVersion: RequestSchema, RequestID: testIdentity.RequestID, PluginID: testIdentity.PluginID, PluginVersion: testIdentity.PluginVersion, ManifestDigest: testIdentity.ManifestDigest, Operation: "image.generate", Protocol: "openai", Model: "example-image-v1", Input: ImageInput{Prompt: "draw", Images: 1}}
	body, err := CanonicalRequest(value)
	if err != nil {
		t.Fatal(err)
	}
	expected := `{"schema_version":"nativegateway.plugin-request/v1","request_id":"req_test","plugin_id":"provider.example","plugin_version":"1.0.0","manifest_digest":"b2210aa93268add9e0fafd5e7735fd82b3b5db33bcd7a63e1bbeb74d7975c1a9","operation":"image.generate","protocol":"openai","model":"example-image-v1","input":{"prompt":"draw","images":1}}`
	if string(body) != expected {
		t.Fatalf("wire changed:\n%s", body)
	}
	decoded, err := DecodeRequest(bytes.NewReader(body), int64(len(body)))
	if err != nil || decoded != value {
		t.Fatalf("decoded=%+v err=%v", decoded, err)
	}
}

func TestStrictDecodeRejectsAmbiguousAndOversizedJSON(t *testing.T) {
	valid := `{"schema_version":"nativegateway.plugin-request/v1","request_id":"req_test","plugin_id":"provider.example","plugin_version":"1.0.0","manifest_digest":"b2210aa93268add9e0fafd5e7735fd82b3b5db33bcd7a63e1bbeb74d7975c1a9","operation":"image.generate","protocol":"openai","model":"example-image-v1","input":{"prompt":"draw","images":1}}`
	cases := []string{strings.Replace(valid, `"request_id":"req_test"`, `"request_id":"req_test","request_id":"other"`, 1), strings.TrimSuffix(valid, "}") + `,"unknown":true}`, valid + ` {}`, valid}
	for index, body := range cases {
		maximum := int64(len(body))
		if index == 3 {
			maximum--
		}
		if _, err := DecodeRequest(strings.NewReader(body), maximum); !errors.Is(err, ErrInvalid) {
			t.Fatalf("case %d accepted: %v", index, err)
		}
	}
}

func TestResponseValidationCoversIdentityUsageMIMEAndExclusivity(t *testing.T) {
	valid := Success(testIdentity, Result{Images: []Image{{MIMEType: "image/png", Base64: "iVBORw0KGgo="}}, Usage: Usage{Images: 1}})
	body, err := CanonicalResponse(valid, testExpectation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = DecodeResponse(bytes.NewReader(body), int64(len(body)), testExpectation); err != nil {
		t.Fatal(err)
	}
	for index := range 4 {
		var value ExecuteResponse
		encoded, _ := json.Marshal(valid)
		_ = json.Unmarshal(encoded, &value)
		switch index {
		case 0:
			value.RequestID = "wrong"
		case 1:
			value.Result.Usage.Images = 2
		case 2:
			value.Result.Images[0].MIMEType = "image/jpeg"
		case 3:
			failure := PluginError{Category: "internal", Message: "failed"}
			value.Error = &failure
		}
		encoded, _ = json.Marshal(value)
		if _, err = DecodeResponse(bytes.NewReader(encoded), int64(len(encoded)), testExpectation); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid %d accepted", index)
		}
	}
}

func TestErrorAndLegacyHealthContracts(t *testing.T) {
	response := Failure(testIdentity, PluginError{Category: "rate_limited", Message: "try later", Retryable: true})
	if _, err := CanonicalResponse(response, testExpectation); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"status":"ok"}`, `{"schema_version":"nativegateway.plugin-health/v1","status":"ok"}`} {
		if _, err := DecodeHealth(strings.NewReader(body), int64(len(body))); err != nil {
			t.Fatal(err)
		}
	}
}
