package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/nativegatewayhq/gateway/internal/jobs"
	"github.com/nativegatewayhq/gateway/internal/plugins"
	joboperation "github.com/nativegatewayhq/gateway/operations/job"
	asyncv1 "github.com/nativegatewayhq/gateway/plugin-sdk/async/v1"
	providerfal "github.com/nativegatewayhq/gateway/providers/fal"
	providerreplicate "github.com/nativegatewayhq/gateway/providers/replicate"
)

// AsyncProvider adapts the public async sidecar contract to the durable Job
// provider interface. Selection remains bound to the immutable channel stored
// on the Job/attempt rather than to mutable model configuration.
type AsyncProvider struct {
	client    *plugins.Client
	pollAfter time.Duration
}

func NewAsync(client *plugins.Client, pollAfter time.Duration) (*AsyncProvider, error) {
	if client == nil || pollAfter <= 0 || pollAfter > 5*time.Minute {
		return nil, errors.New("invalid async plugin provider configuration")
	}
	return &AsyncProvider{client: client, pollAfter: pollAfter}, nil
}

func (provider *AsyncProvider) Submit(ctx context.Context, job joboperation.Job, payload any) (jobs.SubmitResult, error) {
	callback := ""
	if value, ok := payload.(pluginWebhookPayload); ok {
		callback = value.callback
		payload = value.payload
	}
	if isVideoBinding(provider.client, job.ChannelID) {
		binding, _ := provider.client.Binding(job.ChannelID)
		input, err := videoInput(payload, binding)
		if err != nil {
			return jobs.SubmitResult{}, videoKnownFailure("invalid_request", http.StatusBadRequest)
		}
		response, err := provider.client.SubmitVideo(ctx, job.ChannelID, job.RequestID, job.ID, input, callback)
		if err != nil {
			return jobs.SubmitResult{}, pluginProviderError(err)
		}
		observation, err := videoObservation(response.Observation, job.ID, response.ProviderJobRef, "submit")
		if err != nil {
			return jobs.SubmitResult{}, &jobs.ProviderError{Category: "invalid_response"}
		}
		return jobs.SubmitResult{ProviderJobID: response.ProviderJobRef, Observation: observation, PollAfter: provider.pollAfter}, nil
	}
	input, err := asyncImageInput(job.Protocol, payload)
	if err != nil {
		return jobs.SubmitResult{}, knownPluginFailure("invalid_request", http.StatusBadRequest)
	}
	response, err := provider.client.SubmitAsync(ctx, job.ChannelID, job.RequestID, job.ID, job.Protocol, input, callback)
	if err != nil {
		return jobs.SubmitResult{}, pluginProviderError(err)
	}
	observation, err := nativeObservation(job.Protocol, job.ID, response.ProviderJobRef, response.Observation, "submit")
	if err != nil {
		return jobs.SubmitResult{}, &jobs.ProviderError{Category: "invalid_response"}
	}
	return jobs.SubmitResult{ProviderJobID: response.ProviderJobRef, Observation: observation, PollAfter: provider.pollAfter}, nil
}

func (provider *AsyncProvider) Poll(ctx context.Context, attempt jobs.ProviderAttempt) (joboperation.Observation, error) {
	if isVideoBinding(provider.client, attempt.ChannelID) {
		response, err := provider.client.ControlVideo(ctx, attempt.ChannelID, attempt.JobID+":poll", attempt.JobID, "poll", attempt.ProviderJobID)
		if err != nil {
			return joboperation.Observation{}, pluginProviderError(err)
		}
		return videoObservation(response.Observation, attempt.JobID, attempt.ProviderJobID, "poll")
	}
	response, err := provider.client.ControlAsync(ctx, attempt.ChannelID, attempt.JobID+":poll", attempt.JobID, "poll", attempt.ProviderJobID)
	if err != nil {
		return joboperation.Observation{}, pluginProviderError(err)
	}
	return nativeObservationForAttempt(provider.client, attempt, response.Observation, "poll")
}

func (provider *AsyncProvider) Cancel(ctx context.Context, attempt jobs.ProviderAttempt) (joboperation.Observation, error) {
	if isVideoBinding(provider.client, attempt.ChannelID) {
		response, err := provider.client.ControlVideo(ctx, attempt.ChannelID, attempt.JobID+":cancel", attempt.JobID, "cancel", attempt.ProviderJobID)
		if err != nil {
			return joboperation.Observation{}, pluginProviderError(err)
		}
		return videoObservation(response.Observation, attempt.JobID, attempt.ProviderJobID, "cancel")
	}
	response, err := provider.client.ControlAsync(ctx, attempt.ChannelID, attempt.JobID+":cancel", attempt.JobID, "cancel", attempt.ProviderJobID)
	if err != nil {
		return joboperation.Observation{}, pluginProviderError(err)
	}
	return nativeObservationForAttempt(provider.client, attempt, response.Observation, "cancel")
}

// pluginWebhookPayload keeps callback injection private to the Gateway. The
// public native payload is never given the capability URL directly.
type pluginWebhookPayload struct {
	payload  any
	callback string
}

func (payload pluginWebhookPayload) WithWebhook(callback string) (any, error) {
	return pluginWebhookPayload{payload: payload.payload, callback: callback}, nil
}

// WrapAsyncPayload makes a native payload callback-capable without modifying
// the Provider-specific request body.
func WrapAsyncPayload(payload any) jobs.WebhookPayload { return pluginWebhookPayload{payload: payload} }

func asyncImageInput(protocol string, payload any) (asyncv1.ImageInput, error) {
	var body []byte
	switch value := payload.(type) {
	case providerreplicate.SubmitPayload:
		body = value.Body
	case providerfal.SubmitPayload:
		body = value.Body
	default:
		return asyncv1.ImageInput{}, errors.New("unsupported payload")
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil || root == nil {
		return asyncv1.ImageInput{}, errors.New("invalid payload")
	}
	if protocol == "replicate" {
		if raw, ok := root["input"]; ok {
			if json.Unmarshal(raw, &root) != nil {
				return asyncv1.ImageInput{}, errors.New("invalid input")
			}
		}
	}
	var prompt, size, quality string
	var images int
	_ = json.Unmarshal(root["prompt"], &prompt)
	_ = json.Unmarshal(root["size"], &size)
	_ = json.Unmarshal(root["image_size"], &size)
	_ = json.Unmarshal(root["quality"], &quality)
	_ = json.Unmarshal(root["num_outputs"], &images)
	_ = json.Unmarshal(root["num_images"], &images)
	if strings.TrimSpace(prompt) == "" {
		return asyncv1.ImageInput{}, errors.New("missing prompt")
	}
	if images == 0 {
		images = 1
	}
	return asyncv1.ImageInput{Prompt: prompt, Images: images, Size: size, Quality: quality}, nil
}

func nativeObservationForAttempt(client *plugins.Client, attempt jobs.ProviderAttempt, value asyncv1.Observation, source string) (joboperation.Observation, error) {
	binding, ok := client.Binding(attempt.ChannelID)
	if !ok {
		return joboperation.Observation{}, &jobs.ProviderError{Category: "invalid_request"}
	}
	return nativeObservation(binding.Protocol, attempt.JobID, attempt.ProviderJobID, value, source)
}

// NativeObservation is shared by polling and the signed callback ingress.
func NativeObservation(protocol, jobID, providerRef string, value asyncv1.Observation, source string) (joboperation.Observation, error) {
	return nativeObservation(protocol, jobID, providerRef, value, source)
}

func nativeObservation(protocol, jobID, providerRef string, value asyncv1.Observation, source string) (joboperation.Observation, error) {
	result := joboperation.Observation{ProviderJobID: providerRef}
	switch value.Status {
	case "QUEUED":
		result.Status = joboperation.Queued
	case "PROCESSING":
		result.Status = joboperation.Processing
	case "RECONCILING":
		result.Status = joboperation.Reconciling
	case "CANCELED":
		result.Status, result.FailureCategory = joboperation.Canceled, "canceled"
		result.Usage = pluginUsage(0, source)
	case "FAILED":
		if value.Error == nil {
			return joboperation.Observation{}, errors.New("missing error")
		}
		result.Status, result.FailureCategory = joboperation.Failed, value.Error.Category
		body, _ := json.Marshal(map[string]any{"detail": value.Error.Message})
		result.Snapshot = snapshot(http.StatusBadGateway, body)
		result.Usage = pluginUsage(0, source)
	case "SUCCEEDED":
		if value.Result == nil {
			return joboperation.Observation{}, errors.New("missing result")
		}
		body, err := nativeResultBody(protocol, jobID, value.Result.Images)
		if err != nil {
			return joboperation.Observation{}, err
		}
		result.Status = joboperation.Succeeded
		result.Snapshot = snapshot(http.StatusOK, body)
		result.Usage = pluginUsage(value.Result.Usage.Quantity, source)
	default:
		return joboperation.Observation{}, errors.New("invalid status")
	}
	return result, nil
}

func nativeResultBody(protocol, jobID string, images []asyncv1.Image) ([]byte, error) {
	values := make([]any, 0, len(images))
	for _, image := range images {
		if image.URL != "" {
			if protocol == "fal" {
				values = append(values, map[string]any{"url": image.URL, "content_type": image.MIMEType})
			} else {
				values = append(values, image.URL)
			}
		} else {
			values = append(values, map[string]any{"mime_type": image.MIMEType, "base64": image.Base64})
		}
	}
	if protocol == "replicate" {
		return json.Marshal(map[string]any{"id": jobID, "status": "succeeded", "output": values})
	}
	if protocol == "fal" {
		return json.Marshal(map[string]any{"request_id": jobID, "images": values})
	}
	return nil, errors.New("unsupported protocol")
}

func pluginUsage(quantity int64, source string) *joboperation.Usage {
	return &joboperation.Usage{Dimension: "output", Unit: "image", Quantity: quantity, Provenance: source, ExtractorVersion: "plugin-image-output-v1", ResultExtractorVersion: "plugin-image-output-v1"}
}

func snapshot(status int, body []byte) joboperation.Snapshot {
	return joboperation.Snapshot{Status: status, Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: body, SHA256: sha256.Sum256(body)}
}

func knownPluginFailure(category string, status int) *jobs.ProviderError {
	body, _ := json.Marshal(map[string]string{"detail": "plugin request rejected"})
	return &jobs.ProviderError{Category: category, Known: true, Observation: joboperation.Observation{Status: joboperation.Failed, FailureCategory: category, Snapshot: snapshot(status, body), Usage: pluginUsage(0, "submit")}}
}

func pluginProviderError(err error) error {
	switch {
	case errors.Is(err, plugins.ErrInvalidRequest):
		return knownPluginFailure("invalid_request", http.StatusBadRequest)
	case errors.Is(err, plugins.ErrTimeout):
		return &jobs.ProviderError{Category: "timeout"}
	case errors.Is(err, plugins.ErrCanceled):
		return &jobs.ProviderError{Category: "canceled"}
	case errors.Is(err, plugins.ErrInvalidResponse):
		return &jobs.ProviderError{Category: "invalid_response"}
	default:
		return &jobs.ProviderError{Category: "unavailable"}
	}
}
