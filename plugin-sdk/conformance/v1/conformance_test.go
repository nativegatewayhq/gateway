package conformance

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	manifest "github.com/nativegatewayhq/gateway/plugin-sdk/manifest/v1"
	runtimev1 "github.com/nativegatewayhq/gateway/plugin-sdk/runtime/v1"
)

const testSecret = "0123456789abcdef-test-secret"

var png = []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 0}

func TestRunnerPassesReferenceContract(t *testing.T) {
	runner := newRunner(t, referenceHandler(false))
	report, err := runner.Run(context.Background())
	if err != nil || report.Outcome != "pass" || len(report.Checks) != 10 {
		t.Fatalf("Run() = %#v, %v", report, err)
	}
	for index := 1; index < len(report.Checks); index++ {
		if report.Checks[index-1].ID >= report.Checks[index].ID {
			t.Fatalf("checks are not sorted: %#v", report.Checks)
		}
	}
	var encoded bytes.Buffer
	if err := EncodeReport(&encoded, report); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{testSecret, "127.0.0.1", "nativegateway conformance fixture", base64.StdEncoding.EncodeToString(png)} {
		if strings.Contains(encoded.String(), forbidden) {
			t.Fatalf("report leaked forbidden value %q", forbidden)
		}
	}
	decoded, err := DecodeReport(&encoded, 1<<20)
	if err != nil || decoded.Outcome != "pass" {
		t.Fatalf("DecodeReport() = %#v, %v", decoded, err)
	}
}

func TestRunnerReportsInvalidResponseIdentity(t *testing.T) {
	report, err := newRunner(t, referenceHandler(true)).Run(context.Background())
	if err != nil || report.Outcome != "fail" {
		t.Fatalf("Run() = %#v, %v", report, err)
	}
	found := false
	for _, check := range report.Checks {
		if check.ID == "execute.success" && check.Outcome == "fail" && check.Category == "schema" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing stable identity failure: %#v", report.Checks)
	}
}

func TestConfigurationAndReportFailClosed(t *testing.T) {
	validated := testManifest(t)
	base := Config{Manifest: validated, Endpoint: "http://127.0.0.1:8080", Secret: []byte(testSecret), Timeout: time.Second, MaximumRequestBytes: 1024, MaximumResponseBytes: 1 << 20}
	for _, endpoint := range []string{"http://example.com", "http://user@example.com", "https://example.com/path", "https://example.com?secret=x"} {
		config := base
		config.Endpoint = endpoint
		if _, err := NewWithHTTPClient(config, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, nil })}); err == nil {
			t.Fatalf("accepted endpoint %q", endpoint)
		}
	}
	badReports := []string{
		`{"schema_version":"nativegateway.plugin-conformance/v1","plugin_id":"provider.example","plugin_version":"1.0.0","manifest_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","sdk_version":"runtime/v1","outcome":"pass","checks":[],"secret":"x"}`,
		`{"schema_version":"nativegateway.plugin-conformance/v1","schema_version":"nativegateway.plugin-conformance/v1"}`,
	}
	for _, body := range badReports {
		if _, err := DecodeReport(strings.NewReader(body), 1<<20); err == nil {
			t.Fatalf("accepted invalid report %s", body)
		}
	}
}

func newRunner(t *testing.T, handler http.Handler) *Runner {
	t.Helper()
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if err := request.Context().Err(); err != nil {
			return nil, err
		}
		return recorder.Result(), nil
	})}
	runner, err := NewWithHTTPClient(Config{Manifest: testManifest(t), Endpoint: "http://127.0.0.1:8080", Secret: []byte(testSecret), Timeout: 100 * time.Millisecond, MaximumRequestBytes: 1024, MaximumResponseBytes: 1 << 20}, client)
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

func referenceHandler(corruptIdentity bool) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+testSecret {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		if request.URL.Path == "/plugin/v1/health" && request.Method == http.MethodGet {
			_ = runtimev1.EncodeHealth(writer)
			return
		}
		if request.URL.Path != "/plugin/v1/execute" || request.Method != http.MethodPost {
			writer.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		decoded, err := runtimev1.DecodeRequest(request.Body, 1024)
		if err != nil {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		if request.Header.Get(TestModeHeader) == SDKVersion && request.Header.Get(TestCaseHeader) == "cancel" {
			<-request.Context().Done()
			return
		}
		expected := runtimev1.Expectation{Identity: decoded.Identity(), Protocol: decoded.Protocol, Model: decoded.Model, Output: "base64", MaximumImages: 2}
		if request.Header.Get(TestCaseHeader) == "error" {
			_ = runtimev1.EncodeResponse(writer, runtimev1.Failure(decoded.Identity(), runtimev1.InvalidRequest("conformance invalid request")), expected)
			return
		}
		identity := decoded.Identity()
		if corruptIdentity {
			identity.RequestID += "-corrupt"
		}
		response := runtimev1.Success(identity, runtimev1.Result{Images: []runtimev1.Image{{MIMEType: "image/png", Base64: base64.StdEncoding.EncodeToString(png)}}, Usage: runtimev1.Usage{Images: 1}})
		// The corrupt response deliberately bypasses EncodeResponse expectation validation.
		if corruptIdentity {
			body, _ := runtimev1.CanonicalResponse(response, runtimev1.Expectation{Identity: identity, Protocol: decoded.Protocol, Model: decoded.Model, Output: "base64", MaximumImages: 2})
			_, _ = writer.Write(body)
			return
		}
		_ = runtimev1.EncodeResponse(writer, response, expected)
	})
}

func testManifest(t *testing.T) manifest.Validated {
	t.Helper()
	body := []byte(`{"schema_version":"nativegateway.provider/v1","id":"provider.example","version":"1.0.0","gateway_compatibility":">=0.1.0 <1.0.0","transport":{"kind":"http-sidecar","endpoint_ref":"example-sidecar","auth_secret_ref":"example-sidecar-token"},"models":[{"id":"example-image-v1","protocols":["openai","gemini"],"operations":["image.generate"],"capabilities":{"media_type":"application/json","output":["base64"],"maximum_images":2}}]}`)
	validated, err := manifest.Parse(body, "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	return validated
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := function(request)
	if response != nil && response.Body == nil {
		response.Body = io.NopCloser(bytes.NewReader(nil))
	}
	return response, err
}
