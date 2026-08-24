package plugin

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/nativegatewayhq/gateway/internal/jobs"
	"github.com/nativegatewayhq/gateway/internal/plugins"
	joboperation "github.com/nativegatewayhq/gateway/operations/job"
	videov1 "github.com/nativegatewayhq/gateway/plugin-sdk/video/v1"
	providerrunway "github.com/nativegatewayhq/gateway/providers/runway"
)

func videoInput(payload any, binding plugins.Binding) (videov1.Input, error) {
	submit, ok := payload.(providerrunway.SubmitPayload)
	if !ok || submit.Path != "/v1/text_to_video" && submit.Path != "/v1/image_to_video" {
		return videov1.Input{}, errors.New("unsupported payload")
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(submit.Body, &root) != nil {
		return videov1.Input{}, errors.New("invalid payload")
	}
	input := videov1.Input{Kind: strings.TrimPrefix(submit.Path, "/v1/")}
	_ = json.Unmarshal(root["promptText"], &input.Prompt)
	_ = json.Unmarshal(root["duration"], &input.DurationSeconds)
	_ = json.Unmarshal(root["ratio"], &input.Ratio)
	_ = json.Unmarshal(root["audio"], &input.Audio)
	_ = json.Unmarshal(root["seed"], &input.Seed)
	if input.DurationSeconds == 0 {
		input.DurationSeconds = min(binding.MaximumDurationSeconds, 5)
	}
	if input.Ratio == "" {
		ratios := make([]string, 0, len(binding.Ratios))
		for ratio := range binding.Ratios {
			ratios = append(ratios, ratio)
		}
		sort.Strings(ratios)
		if len(ratios) > 0 {
			input.Ratio = ratios[0]
		}
	}
	if input.Kind == "image_to_video" {
		var uri string
		if json.Unmarshal(root["promptImage"], &uri) != nil || !strings.HasPrefix(uri, "runway://") {
			return videov1.Input{}, errors.New("invalid source")
		}
		// Runway upload capabilities currently do not persist a MIME descriptor.
		// The native upload endpoint accepts image assets; use the conservative
		// JPEG descriptor until that store gains an additive media-type column.
		input.Source = &videov1.SourceAsset{URI: uri, ContentType: "image/jpeg"}
	}
	return input, nil
}

func videoObservation(value videov1.Observation, jobID, providerRef, source string) (joboperation.Observation, error) {
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
		if value.Usage == nil {
			value.Usage = &videov1.Usage{Dimension: "provider_credit", Unit: "microcredit", Quantity: 0}
		}
		result.Usage = videoUsage(value.Usage, source)
	case "FAILED":
		if value.Error == nil {
			return joboperation.Observation{}, errors.New("missing error")
		}
		result.Status, result.FailureCategory = joboperation.Failed, value.Error.Category
		body, _ := json.Marshal(map[string]any{"id": jobID, "status": "FAILED", "failure": "plugin video request failed"})
		result.Snapshot = snapshot(http.StatusOK, body)
		result.Usage = videoUsage(value.Usage, source)
	case "SUCCEEDED":
		if value.Result == nil || value.Usage == nil {
			return joboperation.Observation{}, errors.New("missing result evidence")
		}
		body, err := json.Marshal(map[string]any{"id": jobID, "status": "SUCCEEDED", "output": []string{value.Result.URL}, "artifacts": []map[string]any{{"url": value.Result.URL, "contentType": value.Result.ContentType}}, "duration": value.Result.DurationSeconds})
		if err != nil {
			return joboperation.Observation{}, err
		}
		result.Status, result.Snapshot, result.Usage = joboperation.Succeeded, snapshot(http.StatusOK, body), videoUsage(value.Usage, source)
	default:
		return joboperation.Observation{}, errors.New("invalid status")
	}
	return result, nil
}

func videoUsage(value *videov1.Usage, source string) *joboperation.Usage {
	if value == nil {
		return nil
	}
	return &joboperation.Usage{Dimension: "provider_credit", Unit: "microcredit", Quantity: value.Quantity, Provenance: source, ExtractorVersion: "runway-task-cost-v1"}
}

func isVideoBinding(client *plugins.Client, channelID string) bool {
	binding, ok := client.Binding(channelID)
	return ok && binding.Video
}

func videoKnownFailure(category string, status int) *jobs.ProviderError {
	body, _ := json.Marshal(map[string]string{"error": "plugin video request rejected"})
	return &jobs.ProviderError{Category: category, Known: true, Observation: joboperation.Observation{Status: joboperation.Failed, FailureCategory: category, Snapshot: snapshot(status, body), Usage: videoUsage(&videov1.Usage{Dimension: "provider_credit", Unit: "microcredit", Quantity: 0}, "submit")}}
}
