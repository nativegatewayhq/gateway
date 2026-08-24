package plugins

import (
	"bytes"
	"context"

	videov1 "github.com/nativegatewayhq/gateway/plugin-sdk/video/v1"
)

func (client *Client) SubmitVideo(ctx context.Context, channelID, requestID, gatewayJobID string, input videov1.Input, callbackURL string) (videov1.SubmitResponse, error) {
	binding, ok := client.videoBinding(channelID)
	if !ok || callbackURL != "" && !binding.Callback {
		return videov1.SubmitResponse{}, ErrInvalidRequest
	}
	identity := videoIdentity(binding, requestID, gatewayJobID)
	expected := videoExpectation(binding, identity)
	request := videov1.SubmitRequest{SchemaVersion: videov1.SubmitRequestSchema, RequestID: identity.RequestID, GatewayJobID: identity.GatewayJobID, PluginID: identity.PluginID, PluginVersion: identity.PluginVersion, ManifestDigest: identity.ManifestDigest, Protocol: "runway", Operation: "video.generate", Model: binding.Model, Input: input, CallbackURL: callbackURL}
	body, err := videov1.CanonicalSubmitRequest(request, expected)
	if err != nil || int64(len(body)) > client.registry.config.MaximumRequestBytes {
		return videov1.SubmitResponse{}, ErrInvalidRequest
	}
	responseBody, err := client.postAsync(ctx, binding, "/plugin/video/v1/submit", requestID, body)
	if err != nil {
		return videov1.SubmitResponse{}, err
	}
	response, err := videov1.DecodeSubmitResponse(bytes.NewReader(responseBody), client.registry.config.MaximumResponseBytes, expected)
	if err != nil {
		return videov1.SubmitResponse{}, ErrInvalidResponse
	}
	return response, nil
}

func (client *Client) ControlVideo(ctx context.Context, channelID, requestID, gatewayJobID, action, providerJobRef string) (videov1.ObservationResponse, error) {
	binding, ok := client.videoBinding(channelID)
	if !ok {
		return videov1.ObservationResponse{}, ErrInvalidRequest
	}
	identity := videoIdentity(binding, requestID, gatewayJobID)
	request := videov1.ControlRequest{SchemaVersion: videov1.ControlRequestSchema, RequestID: identity.RequestID, GatewayJobID: identity.GatewayJobID, PluginID: identity.PluginID, PluginVersion: identity.PluginVersion, ManifestDigest: identity.ManifestDigest, Action: action, ProviderJobRef: providerJobRef}
	body, err := videov1.CanonicalControlRequest(request)
	if err != nil || int64(len(body)) > client.registry.config.MaximumRequestBytes {
		return videov1.ObservationResponse{}, ErrInvalidRequest
	}
	responseBody, err := client.postAsync(ctx, binding, "/plugin/video/v1/"+action, requestID, body)
	if err != nil {
		return videov1.ObservationResponse{}, err
	}
	response, err := videov1.DecodeObservationResponse(bytes.NewReader(responseBody), client.registry.config.MaximumResponseBytes, videoExpectation(binding, identity))
	if err != nil {
		return videov1.ObservationResponse{}, ErrInvalidResponse
	}
	return response, nil
}

func (client *Client) videoBinding(channelID string) (Binding, bool) {
	if client == nil || client.registry == nil {
		return Binding{}, false
	}
	binding, ok := client.registry.Binding(channelID)
	return binding, ok && binding.Video && binding.Protocol == "runway"
}

func videoIdentity(binding Binding, requestID, gatewayJobID string) videov1.Identity {
	return videov1.Identity{RequestID: requestID, GatewayJobID: gatewayJobID, PluginID: binding.PluginID, PluginVersion: binding.Version, ManifestDigest: binding.DigestHex()}
}

func videoExpectation(binding Binding, identity videov1.Identity) videov1.Expectation {
	ratios := make(map[string]bool, len(binding.Ratios))
	for ratio := range binding.Ratios {
		ratios[ratio] = true
	}
	origins := make(map[string]bool, len(binding.ResultOrigins))
	for origin := range binding.ResultOrigins {
		origins[origin] = true
	}
	return videov1.Expectation{Identity: identity, MaximumDurationSeconds: binding.MaximumDurationSeconds, Ratios: ratios, Audio: binding.Audio, TextToVideo: binding.TextToVideo, ImageToVideo: binding.ImageToVideo, ResultOrigins: origins}
}
