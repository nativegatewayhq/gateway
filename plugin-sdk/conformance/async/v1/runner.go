package asyncconformance

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"time"

	asyncv1 "github.com/nativegatewayhq/gateway/plugin-sdk/async/v1"
	manifest "github.com/nativegatewayhq/gateway/plugin-sdk/manifest/v1"
	runtimev1 "github.com/nativegatewayhq/gateway/plugin-sdk/runtime/v1"
)

const TestModeHeader = "X-Native-Gateway-Conformance"
const TestCaseHeader = "X-Native-Gateway-Conformance-Case"

type Config struct {
	Manifest                                  manifest.Validated
	Endpoint                                  string
	Secret, CallbackSecret                    []byte
	Timeout                                   time.Duration
	MaximumRequestBytes, MaximumResponseBytes int64
}
type Runner struct {
	config Config
	origin *url.URL
	client *http.Client
}

func New(config Config) (*Runner, error) {
	return NewWithHTTPClient(config, &http.Client{Timeout: config.Timeout})
}
func NewWithHTTPClient(config Config, client *http.Client) (*Runner, error) {
	origin, err := url.Parse(config.Endpoint)
	loopback := origin != nil && (origin.Hostname() == "localhost" || origin.Hostname() == "127.0.0.1" || origin.Hostname() == "::1")
	if err != nil || origin.Host == "" || origin.User != nil || origin.RawQuery != "" || origin.Fragment != "" || origin.Path != "" && origin.Path != "/" || origin.Scheme != "https" && !(origin.Scheme == "http" && loopback) || client == nil || len(config.Secret) < 16 || len(config.Secret) > 4096 || len(config.CallbackSecret) != 32 || config.Timeout <= 0 || config.Timeout > time.Minute || config.MaximumRequestBytes < 1 || config.MaximumRequestBytes > 64<<20 || config.MaximumResponseBytes < 1 || config.MaximumResponseBytes > 128<<20 {
		return nil, ErrInvalid
	}
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Runner{config: config, origin: origin, client: &copyClient}, nil
}

func (runner *Runner) Run(ctx context.Context) (Report, error) {
	model, ok := runner.asyncModel()
	if !ok {
		return Report{}, ErrInvalid
	}
	checks := []func(context.Context, manifest.Model) Check{runner.callbackSignature, runner.callbackTamper, runner.controlAuthentication, runner.controlCancel, runner.healthAuthenticated, runner.healthUnauthenticated, runner.pollProcessing, runner.pollSuccess, runner.submitAuthentication, runner.submitCancellation, runner.submitQueued, runner.malformed, runner.oversized, runner.wrongPath}
	report := Report{SchemaVersion: ReportSchema, PluginID: runner.config.Manifest.Manifest.ID, PluginVersion: runner.config.Manifest.Manifest.Version, ManifestDigest: hex.EncodeToString(runner.config.Manifest.Digest[:]), SDKVersion: SDKVersion, Outcome: "pass"}
	for _, execute := range checks {
		check := execute(ctx, model)
		report.Checks = append(report.Checks, check)
		if check.Outcome == "fail" {
			report.Outcome = "fail"
		}
	}
	sort.Slice(report.Checks, func(i, j int) bool { return report.Checks[i].ID < report.Checks[j].ID })
	return report, nil
}
func (runner *Runner) asyncModel() (manifest.Model, bool) {
	for _, model := range runner.config.Manifest.Manifest.Models {
		if model.Async != nil {
			return model, true
		}
	}
	return manifest.Model{}, false
}
func (runner *Runner) identity(test string) asyncv1.Identity {
	return asyncv1.Identity{RequestID: "conformance_" + test, GatewayJobID: "job_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", PluginID: runner.config.Manifest.Manifest.ID, PluginVersion: runner.config.Manifest.Manifest.Version, ManifestDigest: hex.EncodeToString(runner.config.Manifest.Digest[:])}
}
func (runner *Runner) submit(model manifest.Model, test string) asyncv1.SubmitRequest {
	i := runner.identity(test)
	return asyncv1.SubmitRequest{SchemaVersion: asyncv1.SubmitRequestSchema, RequestID: i.RequestID, GatewayJobID: i.GatewayJobID, PluginID: i.PluginID, PluginVersion: i.PluginVersion, ManifestDigest: i.ManifestDigest, Protocol: model.Protocols[0], Operation: "image.generate", Model: model.ID, Input: asyncv1.ImageInput{Prompt: "nativegateway conformance fixture", Images: 1}}
}
func (runner *Runner) expectation(model manifest.Model, identity asyncv1.Identity) asyncv1.Expectation {
	return asyncv1.Expectation{Identity: identity, Output: model.Capabilities.Output[0], MaximumImages: model.Capabilities.MaximumImages}
}
func (runner *Runner) submitQueued(ctx context.Context, model manifest.Model) Check {
	return runner.submitCase(ctx, model, "submit.queued", "queued")
}
func (runner *Runner) submitCase(ctx context.Context, model manifest.Model, id, test string) Check {
	return runner.timed(id, func() string {
		request := runner.submit(model, test)
		body, _ := asyncv1.CanonicalSubmitRequest(request)
		response, err := runner.do(ctx, "/plugin/async/v1/submit", body, true, test)
		if err != nil {
			return "transport"
		}
		defer response.Body.Close()
		if response.StatusCode/100 != 2 {
			return "status"
		}
		decoded, err := asyncv1.DecodeSubmitResponse(response.Body, runner.config.MaximumResponseBytes, runner.expectation(model, request.Identity()))
		if err != nil {
			return "schema"
		}
		if decoded.Observation.Status != "QUEUED" {
			return "outcome"
		}
		return ""
	})
}
func (runner *Runner) submitAuthentication(ctx context.Context, model manifest.Model) Check {
	request := runner.submit(model, "unauthenticated")
	body, _ := asyncv1.CanonicalSubmitRequest(request)
	return runner.reject(ctx, "submit.authentication", "/plugin/async/v1/submit", body, false, "unauthenticated")
}
func (runner *Runner) submitCancellation(ctx context.Context, model manifest.Model) Check {
	return runner.timed("submit.cancellation", func() string {
		request := runner.submit(model, "cancel")
		body, _ := asyncv1.CanonicalSubmitRequest(request)
		cancelCtx, cancel := context.WithTimeout(ctx, min(runner.config.Timeout/4, 100*time.Millisecond))
		defer cancel()
		response, err := runner.do(cancelCtx, "/plugin/async/v1/submit", body, true, "cancel")
		if response != nil {
			response.Body.Close()
		}
		if err == nil || !errors.Is(cancelCtx.Err(), context.DeadlineExceeded) {
			return "not_canceled"
		}
		return ""
	})
}
func (runner *Runner) controlAuthentication(ctx context.Context, model manifest.Model) Check {
	request := runner.control("poll", "auth")
	body, _ := asyncv1.CanonicalControlRequest(request)
	return runner.reject(ctx, "control.authentication", "/plugin/async/v1/poll", body, false, "unauthenticated")
}
func (runner *Runner) control(action, test string) asyncv1.ControlRequest {
	i := runner.identity(test)
	return asyncv1.ControlRequest{SchemaVersion: asyncv1.ControlRequestSchema, RequestID: i.RequestID, GatewayJobID: i.GatewayJobID, PluginID: i.PluginID, PluginVersion: i.PluginVersion, ManifestDigest: i.ManifestDigest, Action: action, ProviderJobRef: "conformance:job-1"}
}
func (runner *Runner) controlCancel(ctx context.Context, model manifest.Model) Check {
	return runner.controlCase(ctx, model, "control.cancel", "cancel", "cancel", "CANCELED")
}
func (runner *Runner) pollProcessing(ctx context.Context, model manifest.Model) Check {
	return runner.controlCase(ctx, model, "poll.processing", "poll", "processing", "PROCESSING")
}
func (runner *Runner) pollSuccess(ctx context.Context, model manifest.Model) Check {
	return runner.controlCase(ctx, model, "poll.success", "poll", "success", "SUCCEEDED")
}
func (runner *Runner) controlCase(ctx context.Context, model manifest.Model, id, action, test, status string) Check {
	return runner.timed(id, func() string {
		request := runner.control(action, test)
		body, _ := asyncv1.CanonicalControlRequest(request)
		response, err := runner.do(ctx, "/plugin/async/v1/"+action, body, true, test)
		if err != nil {
			return "transport"
		}
		defer response.Body.Close()
		if response.StatusCode/100 != 2 {
			return "status"
		}
		decoded, err := asyncv1.DecodeObservationResponse(response.Body, runner.config.MaximumResponseBytes, runner.expectation(model, request.Identity()))
		if err != nil {
			return "schema"
		}
		if decoded.Observation.Status != status {
			return "outcome"
		}
		return ""
	})
}
func (runner *Runner) malformed(ctx context.Context, model manifest.Model) Check {
	return runner.reject(ctx, "wire.malformed_body", "/plugin/async/v1/submit", []byte(`{"request_id":"one","request_id":"two"}`), true, "malformed")
}
func (runner *Runner) oversized(ctx context.Context, model manifest.Model) Check {
	return runner.reject(ctx, "wire.oversized_body", "/plugin/async/v1/submit", bytes.Repeat([]byte{'x'}, int(runner.config.MaximumRequestBytes)+1), true, "oversized")
}
func (runner *Runner) wrongPath(ctx context.Context, model manifest.Model) Check {
	return runner.reject(ctx, "wire.wrong_path", "/plugin/async/v1/unknown", []byte(`{}`), true, "wrong_path")
}
func (runner *Runner) callbackSignature(context.Context, manifest.Model) Check {
	return runner.timed("callback.signature", func() string {
		body := []byte(`{"ok":true}`)
		signature, err := asyncv1.SignCallback(runner.config.CallbackSecret, 1700000000, "delivery_abcdefghijklmnop", body)
		if err != nil || asyncv1.VerifyCallbackSignature(runner.config.CallbackSecret, 1700000000, "delivery_abcdefghijklmnop", body, signature) != nil {
			return "signature"
		}
		return ""
	})
}
func (runner *Runner) callbackTamper(context.Context, manifest.Model) Check {
	return runner.timed("callback.tamper", func() string {
		body := []byte(`{"ok":true}`)
		signature, _ := asyncv1.SignCallback(runner.config.CallbackSecret, 1700000000, "delivery_abcdefghijklmnop", body)
		if asyncv1.VerifyCallbackSignature(runner.config.CallbackSecret, 1700000000, "delivery_abcdefghijklmnop", append(body, 'x'), signature) == nil {
			return "accepted"
		}
		return ""
	})
}
func (runner *Runner) healthAuthenticated(ctx context.Context, _ manifest.Model) Check {
	return runner.timed("health.authenticated", func() string {
		response, err := runner.doMethod(ctx, http.MethodGet, "/plugin/v1/health", nil, true, "health")
		if err != nil {
			return "transport"
		}
		defer response.Body.Close()
		if response.StatusCode/100 != 2 {
			return "status"
		}
		if _, err = runtimev1.DecodeHealth(response.Body, 4096); err != nil {
			return "schema"
		}
		return ""
	})
}
func (runner *Runner) healthUnauthenticated(ctx context.Context, _ manifest.Model) Check {
	return runner.timed("health.unauthenticated", func() string {
		response, err := runner.doMethod(ctx, http.MethodGet, "/plugin/v1/health", nil, false, "unauthenticated")
		if err != nil {
			return "transport"
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized && response.StatusCode != http.StatusForbidden {
			return "status"
		}
		return ""
	})
}
func (runner *Runner) reject(ctx context.Context, id, path string, body []byte, auth bool, test string) Check {
	return runner.timed(id, func() string {
		response, err := runner.do(ctx, path, body, auth, test)
		if err != nil {
			return "transport"
		}
		defer response.Body.Close()
		io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		if response.StatusCode < 400 || response.StatusCode >= 500 {
			return "status"
		}
		return ""
	})
}
func (runner *Runner) do(ctx context.Context, path string, body []byte, auth bool, test string) (*http.Response, error) {
	return runner.doMethod(ctx, http.MethodPost, path, body, auth, test)
}
func (runner *Runner) doMethod(ctx context.Context, method, path string, body []byte, auth bool, test string) (*http.Response, error) {
	target := *runner.origin
	target.Path = path
	request, err := http.NewRequestWithContext(ctx, method, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if auth {
		request.Header.Set("Authorization", "Bearer "+string(runner.config.Secret))
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(TestModeHeader, SDKVersion)
	request.Header.Set(TestCaseHeader, test)
	return runner.client.Do(request)
}
func (runner *Runner) timed(id string, execute func() string) Check {
	started := time.Now()
	category := execute()
	duration := time.Since(started).Milliseconds()
	if category != "" {
		return Check{ID: id, Outcome: "fail", Category: category, DurationMS: duration}
	}
	return Check{ID: id, Outcome: "pass", DurationMS: duration}
}
