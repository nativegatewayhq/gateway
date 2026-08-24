// Package conformance runs the public black-box HTTP sidecar contract v1.
package conformance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/nativegatewayhq/gateway/plugin-sdk/jsonstrict"
	manifest "github.com/nativegatewayhq/gateway/plugin-sdk/manifest/v1"
	runtimev1 "github.com/nativegatewayhq/gateway/plugin-sdk/runtime/v1"
)

const ReportSchema = "nativegateway.plugin-conformance/v1"
const SDKVersion = "runtime/v1"
const TestModeHeader = "X-Native-Gateway-Conformance"
const TestCaseHeader = "X-Native-Gateway-Conformance-Case"

var requiredCheckIDs = []string{"execute.cancellation", "execute.error", "execute.success", "execute.unauthenticated", "health.authenticated", "health.unauthenticated", "wire.malformed_body", "wire.oversized_body", "wire.wrong_method", "wire.wrong_path"}

var ErrInvalidConfig = errors.New("invalid conformance configuration")

var reportIDPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)
var reportVersionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
var reportDigestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var checkIDPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._][a-z0-9]+)*$`)
var categoryPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:_[a-z0-9]+)*$`)

type Config struct {
	Manifest                                  manifest.Validated
	Endpoint                                  string
	Secret                                    []byte
	Timeout                                   time.Duration
	MaximumRequestBytes, MaximumResponseBytes int64
}
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
type Runner struct {
	config Config
	origin *url.URL
	client *http.Client
}

func New(config Config) (*Runner, error) {
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: min(config.Timeout, 5*time.Second), KeepAlive: 30 * time.Second}).DialContext, ForceAttemptHTTP2: true, MaxIdleConns: 4, MaxIdleConnsPerHost: 4, IdleConnTimeout: 30 * time.Second, TLSHandshakeTimeout: min(config.Timeout, 5*time.Second), ResponseHeaderTimeout: config.Timeout}
	return NewWithHTTPClient(config, &http.Client{Transport: transport})
}
func NewWithHTTPClient(config Config, httpClient *http.Client) (*Runner, error) {
	origin, err := parseOrigin(config.Endpoint)
	if err != nil || httpClient == nil || len(config.Secret) < 16 || len(config.Secret) > 4096 || config.Timeout <= 0 || config.Timeout > time.Minute || config.MaximumRequestBytes < 1 || config.MaximumRequestBytes > 64<<20 || config.MaximumResponseBytes < 1 || config.MaximumResponseBytes > 128<<20 || config.Manifest.Manifest.SchemaVersion != manifest.SchemaVersion {
		return nil, ErrInvalidConfig
	}
	copyClient := *httpClient
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	config.Secret = bytes.Clone(config.Secret)
	return &Runner{config: config, origin: origin, client: &copyClient}, nil
}

func (runner *Runner) Run(ctx context.Context) (Report, error) {
	if runner == nil {
		return Report{}, ErrInvalidConfig
	}
	report := Report{SchemaVersion: ReportSchema, PluginID: runner.config.Manifest.Manifest.ID, PluginVersion: runner.config.Manifest.Manifest.Version, ManifestDigest: hex.EncodeToString(runner.config.Manifest.Digest[:]), SDKVersion: SDKVersion, Outcome: "pass"}
	checks := []func(context.Context) Check{runner.healthAuthenticated, runner.healthUnauthenticated, runner.executeUnauthenticated, runner.wrongMethod, runner.wrongPath, runner.malformedBody, runner.oversizedBody, runner.executeSuccess, runner.executeError, runner.executeCancellation}
	for _, run := range checks {
		check := run(ctx)
		report.Checks = append(report.Checks, check)
		if check.Outcome != "pass" {
			report.Outcome = "fail"
		}
	}
	sort.Slice(report.Checks, func(i, j int) bool { return report.Checks[i].ID < report.Checks[j].ID })
	return report, nil
}

// RequiredCheckIDs returns the stable runtime/v1 official admission check set.
func RequiredCheckIDs() []string { return append([]string(nil), requiredCheckIDs...) }

// RequiredChecksDigest binds an admission to the exact sorted runtime/v1 check set.
func RequiredChecksDigest() string {
	digest := sha256.Sum256([]byte(strings.Join(requiredCheckIDs, "\n") + "\n"))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func (runner *Runner) healthAuthenticated(ctx context.Context) Check {
	return runner.timed("health.authenticated", func() string {
		response, err := runner.do(ctx, http.MethodGet, "/plugin/v1/health", nil, true, "health")
		if err != nil {
			return "transport"
		}
		defer response.Body.Close()
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return "status"
		}
		body, err := readBounded(response.Body, 4096)
		if err != nil {
			return "response"
		}
		if len(body) > 0 {
			if _, err = runtimev1.DecodeHealth(bytes.NewReader(body), 4096); err != nil {
				return "schema"
			}
		}
		return ""
	})
}
func (runner *Runner) healthUnauthenticated(ctx context.Context) Check {
	return runner.expectUnauthorized(ctx, "health.unauthenticated", http.MethodGet, "/plugin/v1/health", nil)
}
func (runner *Runner) executeUnauthenticated(ctx context.Context) Check {
	request, _, ok := runner.testRequest("unauthenticated")
	if !ok {
		return failed("execute.unauthenticated", "manifest")
	}
	body, _ := runtimev1.CanonicalRequest(request)
	return runner.expectUnauthorized(ctx, "execute.unauthenticated", http.MethodPost, "/plugin/v1/execute", body)
}
func (runner *Runner) wrongMethod(ctx context.Context) Check {
	return runner.timed("wire.wrong_method", func() string {
		response, err := runner.do(ctx, http.MethodGet, "/plugin/v1/execute", nil, true, "wrong_method")
		if err != nil {
			return "transport"
		}
		defer response.Body.Close()
		_, _ = readBounded(response.Body, 4096)
		if response.StatusCode != http.StatusMethodNotAllowed && response.StatusCode != http.StatusNotFound {
			return "status"
		}
		return ""
	})
}
func (runner *Runner) wrongPath(ctx context.Context) Check {
	return runner.timed("wire.wrong_path", func() string {
		response, err := runner.do(ctx, http.MethodPost, "/plugin/v1/unknown", []byte(`{}`), true, "wrong_path")
		if err != nil {
			return "transport"
		}
		defer response.Body.Close()
		_, _ = readBounded(response.Body, 4096)
		if response.StatusCode != http.StatusNotFound && response.StatusCode != http.StatusMethodNotAllowed {
			return "status"
		}
		return ""
	})
}
func (runner *Runner) malformedBody(ctx context.Context) Check {
	return runner.expectRejected(ctx, "wire.malformed_body", []byte(`{"request_id":"duplicate","request_id":"identity"}`), "malformed")
}
func (runner *Runner) oversizedBody(ctx context.Context) Check {
	body := bytes.Repeat([]byte{'x'}, int(runner.config.MaximumRequestBytes)+1)
	return runner.expectRejected(ctx, "wire.oversized_body", body, "oversized")
}
func (runner *Runner) executeSuccess(ctx context.Context) Check {
	return runner.executeCase(ctx, "execute.success", "success", false)
}
func (runner *Runner) executeError(ctx context.Context) Check {
	return runner.executeCase(ctx, "execute.error", "error", true)
}
func (runner *Runner) executeCancellation(ctx context.Context) Check {
	return runner.timed("execute.cancellation", func() string {
		request, _, ok := runner.testRequest("cancel")
		if !ok {
			return "manifest"
		}
		body, _ := runtimev1.CanonicalRequest(request)
		cancelTimeout := min(runner.config.Timeout/4, 100*time.Millisecond)
		if cancelTimeout < 10*time.Millisecond {
			cancelTimeout = 10 * time.Millisecond
		}
		cancelCtx, cancel := context.WithTimeout(ctx, cancelTimeout)
		defer cancel()
		response, err := runner.do(cancelCtx, http.MethodPost, "/plugin/v1/execute", body, true, "cancel")
		if response != nil {
			_ = response.Body.Close()
		}
		if err == nil {
			return "not_canceled"
		}
		if !errors.Is(cancelCtx.Err(), context.DeadlineExceeded) {
			return "transport"
		}
		return ""
	})
}
func (runner *Runner) expectRejected(ctx context.Context, id string, body []byte, testCase string) Check {
	return runner.timed(id, func() string {
		response, err := runner.do(ctx, http.MethodPost, "/plugin/v1/execute", body, true, testCase)
		if err != nil {
			return "transport"
		}
		defer response.Body.Close()
		_, _ = readBounded(response.Body, 4096)
		if response.StatusCode < 400 || response.StatusCode >= 500 {
			return "status"
		}
		return ""
	})
}

func (runner *Runner) executeCase(ctx context.Context, id, testCase string, wantError bool) Check {
	return runner.timed(id, func() string {
		request, model, ok := runner.testRequest(testCase)
		if !ok {
			return "manifest"
		}
		body, err := runtimev1.CanonicalRequest(request)
		if err != nil {
			return "request"
		}
		response, err := runner.do(ctx, http.MethodPost, "/plugin/v1/execute", body, true, testCase)
		if err != nil {
			return "transport"
		}
		defer response.Body.Close()
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return "status"
		}
		expected := runtimev1.Expectation{Identity: request.Identity(), Protocol: request.Protocol, Model: request.Model, Output: model.Capabilities.Output[0], MaximumImages: model.Capabilities.MaximumImages}
		decoded, err := runtimev1.DecodeResponse(response.Body, runner.config.MaximumResponseBytes, expected)
		if err != nil {
			return "schema"
		}
		if (decoded.Error != nil) != wantError {
			return "outcome"
		}
		if wantError && decoded.Error.Category != "invalid_request" {
			return "category"
		}
		return ""
	})
}
func (runner *Runner) expectUnauthorized(ctx context.Context, id, method, path string, body []byte) Check {
	return runner.timed(id, func() string {
		response, err := runner.do(ctx, method, path, body, false, "unauthenticated")
		if err != nil {
			return "transport"
		}
		defer response.Body.Close()
		_, _ = readBounded(response.Body, 4096)
		if response.StatusCode != http.StatusUnauthorized && response.StatusCode != http.StatusForbidden {
			return "status"
		}
		return ""
	})
}

func (runner *Runner) testRequest(testCase string) (runtimev1.ExecuteRequest, manifest.Model, bool) {
	for _, model := range runner.config.Manifest.Manifest.Models {
		if len(model.Protocols) == 0 {
			continue
		}
		requestID := "conformance_" + testCase + "_" + hex.EncodeToString(runner.config.Manifest.Digest[:4])
		return runtimev1.ExecuteRequest{SchemaVersion: runtimev1.RequestSchema, RequestID: requestID, PluginID: runner.config.Manifest.Manifest.ID, PluginVersion: runner.config.Manifest.Manifest.Version, ManifestDigest: hex.EncodeToString(runner.config.Manifest.Digest[:]), Operation: "image.generate", Protocol: model.Protocols[0], Model: model.ID, Input: runtimev1.ImageInput{Prompt: "nativegateway conformance fixture", Images: 1}}, model, true
	}
	return runtimev1.ExecuteRequest{}, manifest.Model{}, false
}
func (runner *Runner) do(ctx context.Context, method, path string, body []byte, authenticated bool, testCase string) (*http.Response, error) {
	requestURL := *runner.origin
	requestURL.Path = path
	request, err := http.NewRequestWithContext(ctx, method, requestURL.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if authenticated {
		request.Header.Set("Authorization", "Bearer "+string(runner.config.Secret))
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set(TestModeHeader, SDKVersion)
	request.Header.Set(TestCaseHeader, testCase)
	return runner.client.Do(request)
}
func (runner *Runner) timed(id string, run func() string) Check {
	started := time.Now()
	category := run()
	duration := time.Since(started).Milliseconds()
	if duration < 0 {
		duration = 0
	}
	if category != "" {
		return Check{ID: id, Outcome: "fail", Category: category, DurationMS: duration}
	}
	return Check{ID: id, Outcome: "pass", DurationMS: duration}
}
func failed(id, category string) Check { return Check{ID: id, Outcome: "fail", Category: category} }

func EncodeReport(writer io.Writer, report Report) error {
	if ValidateReport(report) != nil {
		return ErrInvalidConfig
	}
	return json.NewEncoder(writer).Encode(report)
}
func CanonicalReport(report Report) ([]byte, error) {
	if ValidateReport(report) != nil {
		return nil, ErrInvalidConfig
	}
	return json.Marshal(report)
}
func DecodeReport(reader io.Reader, maximum int64) (Report, error) {
	body, err := readBounded(reader, maximum)
	if err != nil || jsonstrict.Validate(body) != nil {
		return Report{}, ErrInvalidConfig
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var report Report
	if decoder.Decode(&report) != nil || decoder.Decode(&struct{}{}) != io.EOF || ValidateReport(report) != nil {
		return Report{}, ErrInvalidConfig
	}
	return report, nil
}
func ValidateReport(report Report) error {
	if report.SchemaVersion != ReportSchema || report.SDKVersion != SDKVersion || report.Outcome != "pass" && report.Outcome != "fail" || len(report.PluginID) > 128 || !reportIDPattern.MatchString(report.PluginID) || !reportVersionPattern.MatchString(report.PluginVersion) || !reportDigestPattern.MatchString(report.ManifestDigest) || len(report.Checks) < 1 || len(report.Checks) > 64 {
		return ErrInvalidConfig
	}
	previous := ""
	failedFound := false
	for _, check := range report.Checks {
		if len(check.ID) > 80 || !checkIDPattern.MatchString(check.ID) || check.ID <= previous || (check.Outcome != "pass" && check.Outcome != "fail") || len(check.Category) > 80 || (check.Category != "" && !categoryPattern.MatchString(check.Category)) || (check.Outcome == "pass") != (check.Category == "") || check.DurationMS < 0 || check.DurationMS > 60000 {
			return ErrInvalidConfig
		}
		previous = check.ID
		failedFound = failedFound || check.Outcome == "fail"
	}
	if (report.Outcome == "fail") != failedFound {
		return ErrInvalidConfig
	}
	return nil
}
func parseOrigin(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return nil, ErrInvalidConfig
	}
	host := strings.ToLower(parsed.Hostname())
	loopback := host == "localhost" || host == "127.0.0.1" || host == "::1"
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && loopback) {
		return nil, ErrInvalidConfig
	}
	parsed.Path = ""
	return parsed, nil
}
func readBounded(reader io.Reader, maximum int64) ([]byte, error) {
	if reader == nil || maximum < 1 || maximum > 128<<20 {
		return nil, ErrInvalidConfig
	}
	body, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil || int64(len(body)) > maximum {
		return nil, ErrInvalidConfig
	}
	return body, nil
}
