package plugins

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	runtimev1 "github.com/nativegatewayhq/gateway/plugin-sdk/runtime/v1"
)

const RequestSchema = runtimev1.RequestSchema
const ResponseSchema = runtimev1.ResponseSchema

var (
	ErrUnavailable     = errors.New("plugin unavailable")
	ErrTimeout         = errors.New("plugin timeout")
	ErrCanceled        = errors.New("plugin request canceled")
	ErrInvalidRequest  = errors.New("invalid plugin request")
	ErrInvalidResponse = errors.New("invalid plugin response")
)

type ImageInput = runtimev1.ImageInput
type ExecuteRequest = runtimev1.ExecuteRequest
type Image = runtimev1.Image
type Usage = runtimev1.Usage
type Result = runtimev1.Result
type PluginError = runtimev1.PluginError
type ExecuteResponse = runtimev1.ExecuteResponse

type Client struct {
	registry  *Registry
	http      *http.Client
	semaphore chan struct{}
}

func (client *Client) Health(ctx context.Context) error {
	if client == nil || client.registry == nil {
		return ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, client.registry.config.Timeout)
	defer cancel()
	seen := map[string]bool{}
	for _, binding := range client.registry.Bindings() {
		origin := binding.Origin.String()
		if seen[origin] {
			continue
		}
		seen[origin] = true
		requestURL := *binding.Origin
		requestURL.Path = "/plugin/v1/health"
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
		if err != nil {
			return ErrUnavailable
		}
		request.Header.Set("Authorization", "Bearer "+binding.BearerToken)
		request.Header.Set("Accept", "application/json")
		response, err := client.http.Do(request)
		if err != nil {
			return ErrUnavailable
		}
		healthBody, readErr := io.ReadAll(io.LimitReader(response.Body, 4097))
		decodeErr := readErr
		if decodeErr == nil && len(healthBody) > 0 {
			_, decodeErr = runtimev1.DecodeHealth(bytes.NewReader(healthBody), 4096)
		}
		closeErr := response.Body.Close()
		if decodeErr != nil || closeErr != nil || response.StatusCode < 200 || response.StatusCode >= 300 {
			return ErrUnavailable
		}
	}
	return nil
}

func NewClient(registry *Registry) *Client {
	if registry == nil {
		return nil
	}
	config := registry.config
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: min(config.Timeout, 10*time.Second), KeepAlive: 30 * time.Second}).DialContext, ForceAttemptHTTP2: true, MaxIdleConns: config.MaximumConcurrency, MaxIdleConnsPerHost: config.MaximumConcurrency, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: min(config.Timeout, 10*time.Second), ResponseHeaderTimeout: config.Timeout, ExpectContinueTimeout: time.Second}
	return NewClientWithHTTPClient(registry, &http.Client{Transport: transport})
}

func NewClientWithHTTPClient(registry *Registry, httpClient *http.Client) *Client {
	if registry == nil || httpClient == nil {
		return nil
	}
	copyClient := *httpClient
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{registry: registry, http: &copyClient, semaphore: make(chan struct{}, registry.config.MaximumConcurrency)}
}

func (client *Client) Execute(ctx context.Context, channelID, requestID, protocol string, input ImageInput) (ExecuteResponse, error) {
	if client == nil || client.registry == nil || len(requestID) == 0 || len(requestID) > 128 || input.Images < 1 || len(input.Prompt) == 0 || len(input.Prompt) > 1<<20 {
		return ExecuteResponse{}, ErrInvalidRequest
	}
	binding, ok := client.registry.Binding(channelID)
	if !ok || binding.Protocol != protocol || input.Images > binding.MaximumImages {
		return ExecuteResponse{}, ErrInvalidRequest
	}
	ctx, cancel := context.WithTimeout(ctx, client.registry.config.Timeout)
	defer cancel()
	select {
	case client.semaphore <- struct{}{}:
		defer func() { <-client.semaphore }()
	case <-ctx.Done():
		return ExecuteResponse{}, classifyContext(ctx.Err())
	}
	envelope := ExecuteRequest{SchemaVersion: RequestSchema, RequestID: requestID, PluginID: binding.PluginID, PluginVersion: binding.Version, ManifestDigest: binding.DigestHex(), Operation: "image.generate", Protocol: protocol, Model: binding.Model, Input: input}
	body, err := runtimev1.CanonicalRequest(envelope)
	if err != nil || int64(len(body)) > client.registry.config.MaximumRequestBytes {
		return ExecuteResponse{}, ErrInvalidRequest
	}
	requestURL := *binding.Origin
	requestURL.Path = "/plugin/v1/execute"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL.String(), bytes.NewReader(body))
	if err != nil {
		return ExecuteResponse{}, ErrInvalidRequest
	}
	request.Header.Set("Authorization", "Bearer "+binding.BearerToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-Native-Gateway-Request-ID", requestID)
	response, err := client.http.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return ExecuteResponse{}, classifyContext(ctx.Err())
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return ExecuteResponse{}, ErrTimeout
		}
		return ExecuteResponse{}, ErrUnavailable
	}
	if ctx.Err() != nil {
		_ = response.Body.Close()
		return ExecuteResponse{}, classifyContext(ctx.Err())
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return ExecuteResponse{}, ErrUnavailable
	}
	expected := runtimev1.Expectation{Identity: runtimev1.Identity{RequestID: requestID, PluginID: binding.PluginID, PluginVersion: binding.Version, ManifestDigest: binding.DigestHex()}, Protocol: protocol, Model: binding.Model, Output: binding.Output, MaximumImages: binding.MaximumImages}
	result, decodeErr := runtimev1.DecodeResponse(response.Body, client.registry.config.MaximumResponseBytes, expected)
	if decodeErr != nil {
		return ExecuteResponse{}, ErrInvalidResponse
	}
	if result.Result != nil && binding.Output == "url" {
		for _, image := range result.Result.Images {
			parsed, parseErr := url.Parse(image.URL)
			if parseErr != nil {
				return ExecuteResponse{}, ErrInvalidResponse
			}
			if _, allowed := binding.ResultOrigins[parsed.Scheme+"://"+parsed.Host]; !allowed {
				return ExecuteResponse{}, ErrInvalidResponse
			}
		}
	}
	return result, nil
}
func classifyContext(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrTimeout
	}
	if errors.Is(err, context.Canceled) {
		return ErrCanceled
	}
	return fmt.Errorf("%w", ErrUnavailable)
}
