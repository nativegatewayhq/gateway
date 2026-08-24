package plugins

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"

	asyncv1 "github.com/nativegatewayhq/gateway/plugin-sdk/async/v1"
)

func (client *Client) SubmitAsync(ctx context.Context, channelID, requestID, gatewayJobID, protocol string, input asyncv1.ImageInput, callbackURL string) (asyncv1.SubmitResponse, error) {
	binding, ok := client.asyncBinding(channelID, protocol)
	if !ok || callbackURL != "" && !binding.Callback {
		return asyncv1.SubmitResponse{}, ErrInvalidRequest
	}
	identity := asyncIdentity(binding, requestID, gatewayJobID)
	request := asyncv1.SubmitRequest{SchemaVersion: asyncv1.SubmitRequestSchema, RequestID: identity.RequestID, GatewayJobID: identity.GatewayJobID, PluginID: identity.PluginID, PluginVersion: identity.PluginVersion, ManifestDigest: identity.ManifestDigest, Protocol: protocol, Operation: "image.generate", Model: binding.Model, Input: input, CallbackURL: callbackURL}
	body, err := asyncv1.CanonicalSubmitRequest(request)
	if err != nil || int64(len(body)) > client.registry.config.MaximumRequestBytes {
		return asyncv1.SubmitResponse{}, ErrInvalidRequest
	}
	responseBody, err := client.postAsync(ctx, binding, "/plugin/async/v1/submit", requestID, body)
	if err != nil {
		return asyncv1.SubmitResponse{}, err
	}
	expected := asyncv1.Expectation{Identity: identity, Output: binding.Output, MaximumImages: binding.MaximumImages}
	response, err := asyncv1.DecodeSubmitResponse(bytes.NewReader(responseBody), client.registry.config.MaximumResponseBytes, expected)
	if err != nil || !bindingAllowsAsyncResult(binding, response.Observation.Result) {
		return asyncv1.SubmitResponse{}, ErrInvalidResponse
	}
	return response, nil
}

func (client *Client) ControlAsync(ctx context.Context, channelID, requestID, gatewayJobID, action, providerJobRef string) (asyncv1.ObservationResponse, error) {
	binding, ok := client.asyncBinding(channelID, "")
	if !ok {
		return asyncv1.ObservationResponse{}, ErrInvalidRequest
	}
	identity := asyncIdentity(binding, requestID, gatewayJobID)
	request := asyncv1.ControlRequest{SchemaVersion: asyncv1.ControlRequestSchema, RequestID: identity.RequestID, GatewayJobID: identity.GatewayJobID, PluginID: identity.PluginID, PluginVersion: identity.PluginVersion, ManifestDigest: identity.ManifestDigest, Action: action, ProviderJobRef: providerJobRef}
	body, err := asyncv1.CanonicalControlRequest(request)
	if err != nil || int64(len(body)) > client.registry.config.MaximumRequestBytes {
		return asyncv1.ObservationResponse{}, ErrInvalidRequest
	}
	responseBody, err := client.postAsync(ctx, binding, "/plugin/async/v1/"+action, requestID, body)
	if err != nil {
		return asyncv1.ObservationResponse{}, err
	}
	expected := asyncv1.Expectation{Identity: identity, Output: binding.Output, MaximumImages: binding.MaximumImages}
	response, err := asyncv1.DecodeObservationResponse(bytes.NewReader(responseBody), client.registry.config.MaximumResponseBytes, expected)
	if err != nil || !bindingAllowsAsyncResult(binding, response.Observation.Result) {
		return asyncv1.ObservationResponse{}, ErrInvalidResponse
	}
	return response, nil
}

func (client *Client) asyncBinding(channelID, protocol string) (Binding, bool) {
	if client == nil || client.registry == nil {
		return Binding{}, false
	}
	binding, ok := client.registry.Binding(channelID)
	if !ok || !binding.Async || protocol != "" && binding.Protocol != protocol {
		return Binding{}, false
	}
	return binding, true
}

func (client *Client) postAsync(ctx context.Context, binding Binding, path, requestID string, body []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, client.registry.config.Timeout)
	defer cancel()
	select {
	case client.semaphore <- struct{}{}:
		defer func() { <-client.semaphore }()
	case <-ctx.Done():
		return nil, classifyContext(ctx.Err())
	}
	requestURL := *binding.Origin
	requestURL.Path = path
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL.String(), bytes.NewReader(body))
	if err != nil {
		return nil, ErrInvalidRequest
	}
	request.Header.Set("Authorization", "Bearer "+binding.BearerToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-Native-Gateway-Request-ID", requestID)
	response, err := client.http.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, classifyContext(ctx.Err())
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return nil, ErrTimeout
		}
		return nil, ErrUnavailable
	}
	defer response.Body.Close()
	if ctx.Err() != nil {
		return nil, classifyContext(ctx.Err())
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, ErrUnavailable
	}
	responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, client.registry.config.MaximumResponseBytes+1))
	if readErr != nil {
		return nil, ErrUnavailable
	}
	if int64(len(responseBody)) > client.registry.config.MaximumResponseBytes {
		return nil, ErrInvalidResponse
	}
	return responseBody, nil
}

func asyncIdentity(binding Binding, requestID, gatewayJobID string) asyncv1.Identity {
	return asyncv1.Identity{RequestID: requestID, GatewayJobID: gatewayJobID, PluginID: binding.PluginID, PluginVersion: binding.Version, ManifestDigest: binding.DigestHex()}
}

func bindingAllowsAsyncResult(binding Binding, result *asyncv1.Result) bool {
	if result == nil || binding.Output != "url" {
		return true
	}
	for _, image := range result.Images {
		parsed, err := url.Parse(image.URL)
		if err != nil {
			return false
		}
		if _, allowed := binding.ResultOrigins[parsed.Scheme+"://"+parsed.Host]; !allowed {
			return false
		}
	}
	return true
}
