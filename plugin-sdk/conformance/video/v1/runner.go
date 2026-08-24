package videoconformance

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	manifest "github.com/nativegatewayhq/gateway/plugin-sdk/manifest/v1"
	runtimev1 "github.com/nativegatewayhq/gateway/plugin-sdk/runtime/v1"
	videov1 "github.com/nativegatewayhq/gateway/plugin-sdk/video/v1"
)

const TestModeHeader = "X-Native-Gateway-Conformance"
const TestCaseHeader = "X-Native-Gateway-Conformance-Case"

type Config struct {
	Manifest                                  manifest.Validated
	Endpoint                                  string
	ResultOrigins                             []string
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
	if err != nil || origin.Host == "" || origin.User != nil || origin.RawQuery != "" || origin.Fragment != "" || origin.Path != "" && origin.Path != "/" || origin.Scheme != "https" && !(origin.Scheme == "http" && loopback) || client == nil || len(config.Secret) < 16 || len(config.Secret) > 4096 || len(config.CallbackSecret) != 32 || len(config.ResultOrigins) < 1 || len(config.ResultOrigins) > 32 || config.Timeout <= 0 || config.Timeout > time.Minute || config.MaximumRequestBytes < 1 || config.MaximumRequestBytes > 64<<20 || config.MaximumResponseBytes < 1 || config.MaximumResponseBytes > 128<<20 {
		return nil, ErrInvalid
	}
	for _, raw := range config.ResultOrigins {
		parsed, parseErr := url.Parse(raw)
		if parseErr != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.Path != "" && parsed.Path != "/" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return nil, ErrInvalid
		}
	}
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Runner{config: config, origin: origin, client: &copyClient}, nil
}

func (runner *Runner) Run(ctx context.Context) (Report, error) {
	model, ok := runner.videoModel()
	if !ok {
		return Report{}, ErrInvalid
	}
	checks := []func(context.Context, manifest.VideoModel) Check{runner.callbackSignature, runner.callbackTamper, runner.controlAuthentication, runner.controlCancel, runner.healthAuthenticated, runner.healthUnauthenticated, runner.pollProcessing, runner.pollSuccess, runner.submitAuthentication, runner.submitCancellation, runner.submitImage, runner.submitText, runner.malformed, runner.oversized, runner.wrongPath}
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
func (runner *Runner) videoModel() (manifest.VideoModel, bool) {
	for _, model := range runner.config.Manifest.Manifest.VideoModels {
		return model, true
	}
	return manifest.VideoModel{}, false
}
func (runner *Runner) identity(test string) videov1.Identity {
	return videov1.Identity{RequestID: "conformance_" + test, GatewayJobID: "job_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", PluginID: runner.config.Manifest.Manifest.ID, PluginVersion: runner.config.Manifest.Manifest.Version, ManifestDigest: hex.EncodeToString(runner.config.Manifest.Digest[:])}
}
func (runner *Runner) submit(model manifest.VideoModel, test, kind string) videov1.SubmitRequest {
	i := runner.identity(test)
	input := videov1.Input{Kind: kind, Prompt: "nativegateway conformance fixture", DurationSeconds: min(model.Capabilities.MaximumDurationSeconds, 5), Ratio: model.Capabilities.Ratios[0]}
	if kind == "image_to_video" {
		input.Source = &videov1.SourceAsset{URI: "runway://conformance_asset", ContentType: "image/png"}
	}
	return videov1.SubmitRequest{SchemaVersion: videov1.SubmitRequestSchema, RequestID: i.RequestID, GatewayJobID: i.GatewayJobID, PluginID: i.PluginID, PluginVersion: i.PluginVersion, ManifestDigest: i.ManifestDigest, Protocol: "runway", Operation: "video.generate", Model: model.ID, Input: input}
}
func (runner *Runner) expectation(model manifest.VideoModel, identity videov1.Identity) videov1.Expectation {
	ratios := map[string]bool{}
	for _, ratio := range model.Capabilities.Ratios {
		ratios[ratio] = true
	}
	origins := map[string]bool{}
	for _, raw := range runner.config.ResultOrigins {
		origins[strings.TrimSuffix(raw, "/")] = true
	}
	return videov1.Expectation{Identity: identity, MaximumDurationSeconds: model.Capabilities.MaximumDurationSeconds, Ratios: ratios, Audio: model.Capabilities.Audio, TextToVideo: model.Capabilities.TextToVideo, ImageToVideo: model.Capabilities.ImageToVideo, ResultOrigins: origins}
}
func (runner *Runner) submitText(ctx context.Context, model manifest.VideoModel) Check {
	if !model.Capabilities.TextToVideo {
		return Check{ID: "submit.text", Outcome: "pass"}
	}
	return runner.submitCase(ctx, model, "submit.text", "text", "text_to_video")
}
func (runner *Runner) submitImage(ctx context.Context, model manifest.VideoModel) Check {
	if !model.Capabilities.ImageToVideo {
		return Check{ID: "submit.image", Outcome: "pass"}
	}
	return runner.submitCase(ctx, model, "submit.image", "image", "image_to_video")
}
func (runner *Runner) submitCase(ctx context.Context, model manifest.VideoModel, id, test, kind string) Check {
	return runner.timed(id, func() string {
		request := runner.submit(model, test, kind)
		body, _ := videov1.CanonicalSubmitRequest(request, runner.expectation(model, request.Identity()))
		response, err := runner.do(ctx, "/plugin/video/v1/submit", body, true, test)
		if err != nil {
			return "transport"
		}
		defer response.Body.Close()
		if response.StatusCode/100 != 2 {
			return "status"
		}
		decoded, err := videov1.DecodeSubmitResponse(response.Body, runner.config.MaximumResponseBytes, runner.expectation(model, request.Identity()))
		if err != nil {
			return "schema"
		}
		if decoded.Observation.Status != "QUEUED" {
			return "outcome"
		}
		return ""
	})
}
func (runner *Runner) submitAuthentication(ctx context.Context, model manifest.VideoModel) Check {
	kind := "text_to_video"
	if !model.Capabilities.TextToVideo {
		kind = "image_to_video"
	}
	request := runner.submit(model, "unauthenticated", kind)
	body, _ := videov1.CanonicalSubmitRequest(request, runner.expectation(model, request.Identity()))
	return runner.reject(ctx, "submit.authentication", "/plugin/video/v1/submit", body, false, "unauthenticated")
}
func (runner *Runner) submitCancellation(ctx context.Context, model manifest.VideoModel) Check {
	return runner.timed("submit.cancellation", func() string {
		kind := "text_to_video"
		if !model.Capabilities.TextToVideo {
			kind = "image_to_video"
		}
		request := runner.submit(model, "cancel", kind)
		body, _ := videov1.CanonicalSubmitRequest(request, runner.expectation(model, request.Identity()))
		cancelCtx, cancel := context.WithTimeout(ctx, min(runner.config.Timeout/4, 100*time.Millisecond))
		defer cancel()
		response, err := runner.do(cancelCtx, "/plugin/video/v1/submit", body, true, "cancel")
		if response != nil {
			response.Body.Close()
		}
		if err == nil || !errors.Is(cancelCtx.Err(), context.DeadlineExceeded) {
			return "not_canceled"
		}
		return ""
	})
}
func (runner *Runner) controlAuthentication(ctx context.Context, model manifest.VideoModel) Check {
	request := runner.control("poll", "auth")
	body, _ := videov1.CanonicalControlRequest(request)
	return runner.reject(ctx, "control.authentication", "/plugin/video/v1/poll", body, false, "unauthenticated")
}
func (runner *Runner) control(action, test string) videov1.ControlRequest {
	i := runner.identity(test)
	return videov1.ControlRequest{SchemaVersion: videov1.ControlRequestSchema, RequestID: i.RequestID, GatewayJobID: i.GatewayJobID, PluginID: i.PluginID, PluginVersion: i.PluginVersion, ManifestDigest: i.ManifestDigest, Action: action, ProviderJobRef: "conformance:job-1"}
}
func (runner *Runner) controlCancel(ctx context.Context, model manifest.VideoModel) Check {
	return runner.controlCase(ctx, model, "control.cancel", "cancel", "cancel", "CANCELED")
}
func (runner *Runner) pollProcessing(ctx context.Context, model manifest.VideoModel) Check {
	return runner.controlCase(ctx, model, "poll.processing", "poll", "processing", "PROCESSING")
}
func (runner *Runner) pollSuccess(ctx context.Context, model manifest.VideoModel) Check {
	return runner.controlCase(ctx, model, "poll.success", "poll", "success", "SUCCEEDED")
}
func (runner *Runner) controlCase(ctx context.Context, model manifest.VideoModel, id, action, test, status string) Check {
	return runner.timed(id, func() string {
		request := runner.control(action, test)
		body, _ := videov1.CanonicalControlRequest(request)
		response, err := runner.do(ctx, "/plugin/video/v1/"+action, body, true, test)
		if err != nil {
			return "transport"
		}
		defer response.Body.Close()
		if response.StatusCode/100 != 2 {
			return "status"
		}
		decoded, err := videov1.DecodeObservationResponse(response.Body, runner.config.MaximumResponseBytes, runner.expectation(model, request.Identity()))
		if err != nil {
			return "schema"
		}
		if decoded.Observation.Status != status {
			return "outcome"
		}
		return ""
	})
}
func (runner *Runner) malformed(ctx context.Context, model manifest.VideoModel) Check {
	return runner.reject(ctx, "wire.malformed_body", "/plugin/video/v1/submit", []byte(`{"request_id":"one","request_id":"two"}`), true, "malformed")
}
func (runner *Runner) oversized(ctx context.Context, model manifest.VideoModel) Check {
	return runner.reject(ctx, "wire.oversized_body", "/plugin/video/v1/submit", bytes.Repeat([]byte{'x'}, int(runner.config.MaximumRequestBytes)+1), true, "oversized")
}
func (runner *Runner) wrongPath(ctx context.Context, model manifest.VideoModel) Check {
	return runner.reject(ctx, "wire.wrong_path", "/plugin/video/v1/unknown", []byte(`{}`), true, "wrong_path")
}
func (runner *Runner) callbackSignature(context.Context, manifest.VideoModel) Check {
	return runner.timed("callback.signature", func() string {
		body := []byte(`{"ok":true}`)
		signature, err := videov1.SignCallback(runner.config.CallbackSecret, 1700000000, "delivery_abcdefghijklmnop", body)
		if err != nil || videov1.VerifyCallbackSignature(runner.config.CallbackSecret, 1700000000, "delivery_abcdefghijklmnop", body, signature) != nil {
			return "signature"
		}
		return ""
	})
}
func (runner *Runner) callbackTamper(context.Context, manifest.VideoModel) Check {
	return runner.timed("callback.tamper", func() string {
		body := []byte(`{"ok":true}`)
		signature, _ := videov1.SignCallback(runner.config.CallbackSecret, 1700000000, "delivery_abcdefghijklmnop", body)
		if videov1.VerifyCallbackSignature(runner.config.CallbackSecret, 1700000000, "delivery_abcdefghijklmnop", append(body, 'x'), signature) == nil {
			return "accepted"
		}
		return ""
	})
}
func (runner *Runner) healthAuthenticated(ctx context.Context, _ manifest.VideoModel) Check {
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
func (runner *Runner) healthUnauthenticated(ctx context.Context, _ manifest.VideoModel) Check {
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
