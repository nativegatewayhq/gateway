// Package fal implements the fixed-origin fal Queue adapter.
package fal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/nativegatewayhq/gateway/internal/jobs"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	joboperation "github.com/nativegatewayhq/gateway/operations/job"
)

var ErrInvalidConfig = errors.New("invalid fal provider configuration")

type Config struct {
	Endpoint, PublicBaseURL string
	Timeout                 time.Duration
	MaximumBodyBytes        int64
}

type SubmitPayload struct {
	Body       []byte
	WebhookURL string
}

func (payload SubmitPayload) WithWebhook(callback string) (any, error) {
	parsed, err := url.Parse(callback)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, ErrInvalidConfig
	}
	payload.WebhookURL = callback
	return payload, nil
}

type Client struct {
	endpoint, publicBase *url.URL
	credentials          *providercredentials.Registry
	http                 *http.Client
	maximumBodyBytes     int64
}

func New(config Config, credentials *providercredentials.Registry) (*Client, error) {
	endpoint, err := validatedBase(config.Endpoint, true)
	if err != nil {
		return nil, ErrInvalidConfig
	}
	publicBase, err := validatedBase(config.PublicBaseURL, true)
	if err != nil || credentials == nil || config.Timeout <= 0 || config.Timeout > 10*time.Minute || config.MaximumBodyBytes < 1 || config.MaximumBodyBytes > 256*1024*1024 {
		return nil, ErrInvalidConfig
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	return &Client{endpoint: endpoint, publicBase: publicBase, credentials: credentials, http: &http.Client{Timeout: config.Timeout, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, maximumBodyBytes: config.MaximumBodyBytes}, nil
}

func validatedBase(value string, allowLoopback bool) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return nil, ErrInvalidConfig
	}
	loopback := parsed.Hostname() == "127.0.0.1" || parsed.Hostname() == "::1" || parsed.Hostname() == "localhost"
	if parsed.Scheme != "https" && !(allowLoopback && parsed.Scheme == "http" && loopback) {
		return nil, ErrInvalidConfig
	}
	parsed.Path = ""
	return parsed, nil
}

func (client *Client) Submit(ctx context.Context, value joboperation.Job, payload any) (jobs.SubmitResult, error) {
	requestPayload, ok := payload.(SubmitPayload)
	if !ok || len(requestPayload.Body) == 0 || int64(len(requestPayload.Body)) > client.maximumBodyBytes || !validModel(value.Model) {
		return jobs.SubmitResult{}, &jobs.ProviderError{Category: "invalid_request", Known: true, Observation: failure(http.StatusBadRequest, []byte(`{"detail":"invalid queue request"}`))}
	}
	query := ""
	if requestPayload.WebhookURL != "" {
		query = url.Values{"fal_webhook": {requestPayload.WebhookURL}}.Encode()
	}
	response, body, err := client.call(ctx, http.MethodPost, modelPath(value.Model), value.ChannelID, bytes.NewReader(requestPayload.Body), query)
	if err != nil {
		return jobs.SubmitResult{}, err
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return jobs.SubmitResult{}, providerFailure(response.StatusCode, body)
	}
	providerID, snapshot, err := client.submitSnapshot(value.ID, value.Model, response.StatusCode, response.Header, body)
	if err != nil {
		return jobs.SubmitResult{}, &jobs.ProviderError{Category: "invalid_response"}
	}
	return jobs.SubmitResult{ProviderJobID: providerID, Observation: joboperation.Observation{Status: joboperation.Queued, Snapshot: snapshot}, PollAfter: time.Second}, nil
}

func (client *Client) Poll(ctx context.Context, attempt jobs.ProviderAttempt) (joboperation.Observation, error) {
	if attempt.ProviderJobID == "" || !validModel(attempt.Model) {
		return joboperation.Observation{Status: joboperation.Reconciling, FailureCategory: "provider_error"}, nil
	}
	base := modelPath(attempt.Model) + "/requests/" + url.PathEscape(attempt.ProviderJobID)
	response, body, err := client.call(ctx, http.MethodGet, base+"/status", attempt.ChannelID, nil, "")
	if err != nil {
		return joboperation.Observation{}, err
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return joboperation.Observation{}, providerFailure(response.StatusCode, body)
	}
	status, snapshot, err := client.statusSnapshot(attempt.JobID, attempt.Model, response.StatusCode, response.Header, body)
	if err != nil {
		return joboperation.Observation{}, &jobs.ProviderError{Category: "invalid_response"}
	}
	if status != joboperation.Succeeded {
		return joboperation.Observation{Status: status, Snapshot: snapshot}, nil
	}
	resultResponse, resultBody, err := client.call(ctx, http.MethodGet, base, attempt.ChannelID, nil, "")
	if err != nil {
		return joboperation.Observation{}, err
	}
	if resultResponse.StatusCode < 200 || resultResponse.StatusCode > 299 {
		return joboperation.Observation{}, providerFailure(resultResponse.StatusCode, resultBody)
	}
	resultSnapshot, err := client.resultSnapshot(attempt.JobID, resultResponse.StatusCode, resultResponse.Header, resultBody)
	if err != nil {
		return joboperation.Observation{}, &jobs.ProviderError{Category: "invalid_response"}
	}
	return joboperation.Observation{Status: joboperation.Succeeded, Snapshot: resultSnapshot, Usage: falOutputUsage(resultBody, "poll")}, nil
}

func (client *Client) resultSnapshot(jobID string, status int, headers http.Header, body []byte) (joboperation.Snapshot, error) {
	var value map[string]any
	if json.Unmarshal(body, &value) != nil || value == nil {
		return joboperation.Snapshot{}, errors.New("invalid result")
	}
	if _, exists := value["request_id"]; exists {
		value["request_id"] = jobID
	}
	delete(value, "status_url")
	delete(value, "response_url")
	delete(value, "cancel_url")
	rewritten, err := json.Marshal(value)
	if err != nil {
		return joboperation.Snapshot{}, err
	}
	return sanitizedSnapshot(jobID, status, headers, rewritten, client.maximumBodyBytes)
}

func (client *Client) WebhookObservation(jobID string, body []byte) (string, joboperation.Observation, error) {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(body, &envelope) != nil || envelope == nil {
		return "", joboperation.Observation{}, errors.New("invalid fal webhook")
	}
	var providerID, status string
	if json.Unmarshal(envelope["request_id"], &providerID) != nil || providerID == "" || len(providerID) > 500 || json.Unmarshal(envelope["status"], &status) != nil {
		return "", joboperation.Observation{}, errors.New("invalid fal webhook")
	}
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case "OK", "COMPLETED", "SUCCEEDED":
		var payload map[string]any
		if json.Unmarshal(envelope["payload"], &payload) != nil || payload == nil {
			return "", joboperation.Observation{}, errors.New("invalid fal webhook payload")
		}
		resultBody, err := json.Marshal(payload)
		if err != nil {
			return "", joboperation.Observation{}, err
		}
		snapshot, err := client.resultSnapshot(jobID, http.StatusOK, http.Header{"Content-Type": {"application/json"}}, resultBody)
		if err != nil {
			return "", joboperation.Observation{}, err
		}
		return providerID, joboperation.Observation{Status: joboperation.Succeeded, ProviderJobID: providerID, Snapshot: snapshot, Usage: falOutputUsage(resultBody, "webhook")}, nil
	case "ERROR", "FAILED":
		failureBody := []byte(`{"detail":"Queue request failed"}`)
		usage := &joboperation.Usage{Dimension: "output", Unit: "image", Quantity: 0, Provenance: "webhook", ExtractorVersion: "fal-output-v1"}
		if payload := envelope["payload"]; len(payload) > 0 && string(payload) != "null" {
			usage = falOutputUsage(payload, "webhook")
		}
		return providerID, joboperation.Observation{Status: joboperation.Failed, ProviderJobID: providerID, FailureCategory: "provider_error", Snapshot: joboperation.Snapshot{Status: http.StatusInternalServerError, Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: failureBody, SHA256: sha256.Sum256(failureBody)}, Usage: usage}, nil
	case "CANCELED", "CANCELLED":
		usage := &joboperation.Usage{Dimension: "output", Unit: "image", Quantity: 0, Provenance: "webhook", ExtractorVersion: "fal-output-v1"}
		if payload := envelope["payload"]; len(payload) > 0 && string(payload) != "null" {
			usage = falOutputUsage(payload, "webhook")
		}
		return providerID, joboperation.Observation{Status: joboperation.Canceled, ProviderJobID: providerID, FailureCategory: "canceled", Usage: usage}, nil
	default:
		return "", joboperation.Observation{}, errors.New("invalid fal webhook status")
	}
}

func falOutputUsage(body []byte, source string) *joboperation.Usage {
	quantity := int64(0)
	var value map[string]json.RawMessage
	if json.Unmarshal(body, &value) == nil {
		if raw, exists := value["images"]; exists {
			var images []json.RawMessage
			if json.Unmarshal(raw, &images) == nil {
				for _, image := range images {
					if usableImage(image) && quantity < joboperation.MaximumObservedUsage {
						quantity++
					}
				}
			}
		} else if raw, exists := value["image"]; exists && usableImage(raw) {
			quantity = 1
		}
	}
	return &joboperation.Usage{Dimension: "output", Unit: "image", Quantity: quantity, Provenance: source, ExtractorVersion: "fal-output-v1"}
}

func usableImage(raw json.RawMessage) bool {
	var object struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(raw, &object) != nil || object.URL == "" || len(object.URL) > 8192 || strings.ContainsAny(object.URL, "\r\n") {
		return false
	}
	parsed, err := url.Parse(object.URL)
	return err == nil && ((parsed.Scheme == "https" && parsed.Host != "") || (parsed.Scheme == "data" && strings.HasPrefix(object.URL, "data:image/")))
}

func (client *Client) Cancel(ctx context.Context, attempt jobs.ProviderAttempt) (joboperation.Observation, error) {
	if attempt.ProviderJobID == "" || !validModel(attempt.Model) {
		return joboperation.Observation{}, &jobs.ProviderError{Category: "invalid_request"}
	}
	path := modelPath(attempt.Model) + "/requests/" + url.PathEscape(attempt.ProviderJobID) + "/cancel"
	response, body, err := client.call(ctx, http.MethodPut, path, attempt.ChannelID, nil, "")
	if err != nil {
		return joboperation.Observation{}, err
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return joboperation.Observation{}, providerFailure(response.StatusCode, body)
	}
	return joboperation.Observation{Status: joboperation.Canceled}, nil
}

func (client *Client) call(ctx context.Context, method, path, channel string, body io.Reader, rawQuery string) (*http.Response, []byte, error) {
	target := *client.endpoint
	target.Path = path
	target.RawQuery = rawQuery
	request, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return nil, nil, err
	}
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	outbound, err := providercredentials.PrepareOutboundChannel(request, channel, providercredentials.Fal, client.credentials)
	if err != nil {
		return nil, nil, err
	}
	defer providercredentials.ClearApplied(outbound)
	response, err := client.http.Do(outbound)
	if err != nil {
		return nil, nil, err
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return response, nil, &jobs.ProviderError{Category: "invalid_response"}
	}
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, client.maximumBodyBytes+1))
	if err != nil {
		return response, nil, err
	}
	if int64(len(responseBody)) > client.maximumBodyBytes {
		return response, nil, &jobs.ProviderError{Category: "invalid_response"}
	}
	return response, responseBody, nil
}

func (client *Client) submitSnapshot(jobID, model string, status int, headers http.Header, body []byte) (string, joboperation.Snapshot, error) {
	var value map[string]any
	if json.Unmarshal(body, &value) != nil {
		return "", joboperation.Snapshot{}, errors.New("invalid response")
	}
	providerID, _ := value["request_id"].(string)
	if providerID == "" || len(providerID) > 500 {
		return "", joboperation.Snapshot{}, errors.New("missing request ID")
	}
	applyPublicIdentity(value, jobID, model, client.publicBase.String())
	value["status"] = "IN_QUEUE"
	if _, exists := value["queue_position"]; !exists {
		value["queue_position"] = 0
	}
	rewritten, err := json.Marshal(value)
	if err != nil {
		return "", joboperation.Snapshot{}, err
	}
	snapshot, err := sanitizedSnapshot(jobID, status, headers, rewritten, client.maximumBodyBytes)
	return providerID, snapshot, err
}

func (client *Client) statusSnapshot(jobID, model string, status int, headers http.Header, body []byte) (joboperation.Status, joboperation.Snapshot, error) {
	var value map[string]any
	if json.Unmarshal(body, &value) != nil {
		return "", joboperation.Snapshot{}, errors.New("invalid response")
	}
	native, _ := value["status"].(string)
	mapped := mapStatus(native)
	if mapped == "" {
		return "", joboperation.Snapshot{}, errors.New("invalid status")
	}
	applyPublicIdentity(value, jobID, model, client.publicBase.String())
	switch strings.ToUpper(strings.TrimSpace(native)) {
	case "IN_QUEUE", "QUEUED":
		if _, exists := value["queue_position"]; !exists {
			value["queue_position"] = 0
		}
	case "IN_PROGRESS", "PROCESSING", "COMPLETED", "SUCCEEDED":
		if _, exists := value["logs"]; !exists {
			value["logs"] = nil
		}
		if mapped == joboperation.Succeeded {
			if _, exists := value["metrics"]; !exists {
				value["metrics"] = map[string]any{}
			}
		}
	}
	rewritten, err := json.Marshal(value)
	if err != nil {
		return "", joboperation.Snapshot{}, err
	}
	snapshot, err := sanitizedSnapshot(jobID, status, headers, rewritten, client.maximumBodyBytes)
	return mapped, snapshot, err
}

func sanitizedSnapshot(jobID string, status int, headers http.Header, body []byte, maximum int64) (joboperation.Snapshot, error) {
	if status < 100 || status > 599 || len(body) == 0 || int64(len(body)) > maximum || !json.Valid(body) || strings.Contains(string(body), "queue.fal.run") {
		return joboperation.Snapshot{}, errors.New("unsafe snapshot")
	}
	copyHeaders := map[string][]string{"Content-Type": {"application/json"}}
	return joboperation.Snapshot{Status: status, Headers: copyHeaders, Body: append([]byte(nil), body...)}, nil
}

func applyPublicIdentity(value map[string]any, jobID, model, base string) {
	value["request_id"] = jobID
	prefix := strings.TrimSuffix(base, "/") + "/" + model + "/requests/" + jobID
	value["status_url"] = prefix + "/status"
	value["response_url"] = prefix
	value["cancel_url"] = prefix + "/cancel"
}

func mapStatus(value string) joboperation.Status {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "IN_QUEUE", "QUEUED":
		return joboperation.Queued
	case "IN_PROGRESS", "PROCESSING":
		return joboperation.Processing
	case "COMPLETED", "SUCCEEDED":
		return joboperation.Succeeded
	case "FAILED", "ERROR":
		return joboperation.Failed
	case "CANCELED", "CANCELLED":
		return joboperation.Canceled
	default:
		return ""
	}
}

func failure(status int, body []byte) joboperation.Observation {
	if !json.Valid(body) {
		body = []byte(`{"detail":"queue request failed"}`)
	}
	return joboperation.Observation{Status: joboperation.Failed, Snapshot: joboperation.Snapshot{Status: status, Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: body}, FailureCategory: statusCategory(status)}
}

func providerFailure(status int, body []byte) error {
	known := status >= 400 && status < 500 && status != 408 && status != 409 && status != 429
	return &jobs.ProviderError{Category: statusCategory(status), Known: known, Observation: failure(status, body)}
}

func statusCategory(status int) string {
	switch {
	case status == 429:
		return "rate_limited"
	case status >= 500:
		return "unavailable"
	case status >= 400:
		return "rejected"
	default:
		return "invalid_response"
	}
}

func validModel(model string) bool {
	if model == "" || len(model) > 200 {
		return false
	}
	parts := strings.Split(model, "/")
	if len(parts) < 2 {
		return false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return false
		}
		for _, character := range part {
			if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || strings.ContainsRune("._-", character) {
				continue
			}
			return false
		}
	}
	return true
}

func modelPath(model string) string { return "/" + model }
