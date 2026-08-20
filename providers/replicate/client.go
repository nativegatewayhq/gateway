// Package replicate implements the fixed-origin Replicate Prediction adapter.
package replicate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/nativegatewayhq/gateway/internal/jobs"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	joboperation "github.com/nativegatewayhq/gateway/operations/job"
)

var ErrInvalidConfig = errors.New("invalid Replicate provider configuration")

type Config struct {
	Endpoint, PublicBaseURL string
	Timeout                 time.Duration
	MaximumBodyBytes        int64
}
type SubmitPayload struct {
	Body                []byte
	Prefer, CancelAfter string
}

func (payload SubmitPayload) WithWebhook(callback string) (any, error) {
	parsed, err := url.Parse(callback)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, ErrInvalidConfig
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(payload.Body, &envelope) != nil {
		return nil, errors.New("invalid Replicate submit payload")
	}
	if _, exists := envelope["webhook"]; exists {
		return nil, errors.New("client webhook is not allowed")
	}
	if _, exists := envelope["webhook_events_filter"]; exists {
		return nil, errors.New("client webhook filter is not allowed")
	}
	envelope["webhook"], _ = json.Marshal(callback)
	envelope["webhook_events_filter"], _ = json.Marshal([]string{"completed"})
	body, err := json.Marshal(envelope)
	if err != nil {
		return nil, err
	}
	payload.Body = body
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
	publicBase, err := validatedBase(config.PublicBaseURL, false)
	if err != nil {
		return nil, ErrInvalidConfig
	}
	if credentials == nil || config.Timeout <= 0 || config.Timeout > 10*time.Minute || config.MaximumBodyBytes < 1 || config.MaximumBodyBytes > 256*1024*1024 {
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
	if !ok || len(requestPayload.Body) == 0 || int64(len(requestPayload.Body)) > client.maximumBodyBytes {
		return jobs.SubmitResult{}, &jobs.ProviderError{Category: "invalid_request", Known: true, Observation: failureSnapshot(http.StatusBadRequest, []byte(`{"detail":"invalid prediction request"}`))}
	}
	request, err := client.request(ctx, http.MethodPost, "/v1/predictions", value.ChannelID, bytes.NewReader(requestPayload.Body))
	if err != nil {
		return jobs.SubmitResult{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	if requestPayload.Prefer != "" {
		request.Header.Set("Prefer", requestPayload.Prefer)
	}
	if requestPayload.CancelAfter != "" {
		request.Header.Set("Cancel-After", requestPayload.CancelAfter)
	}
	response, body, err := client.do(request)
	if err != nil {
		return jobs.SubmitResult{}, err
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		known := response.StatusCode >= 400 && response.StatusCode < 500 && response.StatusCode != 408 && response.StatusCode != 409 && response.StatusCode != 429
		return jobs.SubmitResult{}, &jobs.ProviderError{Category: statusCategory(response.StatusCode), Known: known, Observation: failureSnapshot(response.StatusCode, body)}
	}
	providerID, observation, sanitized, err := client.observation(value.ID, response.StatusCode, response.Header, body, "submit")
	if err != nil {
		return jobs.SubmitResult{}, &jobs.ProviderError{Category: "invalid_response"}
	}
	if observation.Status != joboperation.Canceled {
		observation.Snapshot = sanitized
	}
	return jobs.SubmitResult{ProviderJobID: providerID, Observation: observation, PollAfter: time.Second}, nil
}

func (client *Client) Poll(ctx context.Context, attempt jobs.ProviderAttempt) (joboperation.Observation, error) {
	if attempt.ProviderJobID == "" {
		return joboperation.Observation{Status: joboperation.Reconciling, FailureCategory: "provider_error"}, nil
	}
	request, err := client.request(ctx, http.MethodGet, "/v1/predictions/"+url.PathEscape(attempt.ProviderJobID), attempt.ChannelID, nil)
	if err != nil {
		return joboperation.Observation{}, err
	}
	response, body, err := client.do(request)
	if err != nil {
		return joboperation.Observation{}, err
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return joboperation.Observation{}, &jobs.ProviderError{Category: statusCategory(response.StatusCode)}
	}
	_, observation, snapshot, err := client.observation(attempt.JobID, response.StatusCode, response.Header, body, "poll")
	if err != nil {
		return joboperation.Observation{}, &jobs.ProviderError{Category: "invalid_response"}
	}
	if observation.Status != joboperation.Canceled {
		observation.Snapshot = snapshot
	}
	return observation, nil
}

func (client *Client) Cancel(ctx context.Context, attempt jobs.ProviderAttempt) (joboperation.Observation, error) {
	if attempt.ProviderJobID == "" {
		return joboperation.Observation{}, &jobs.ProviderError{Category: "invalid_request"}
	}
	request, err := client.request(ctx, http.MethodPost, "/v1/predictions/"+url.PathEscape(attempt.ProviderJobID)+"/cancel", attempt.ChannelID, nil)
	if err != nil {
		return joboperation.Observation{}, err
	}
	response, body, err := client.do(request)
	if err != nil {
		return joboperation.Observation{}, err
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return joboperation.Observation{}, &jobs.ProviderError{Category: statusCategory(response.StatusCode)}
	}
	_, observation, snapshot, err := client.observation(attempt.JobID, response.StatusCode, response.Header, body, "cancel")
	if err != nil {
		return joboperation.Observation{}, &jobs.ProviderError{Category: "invalid_response"}
	}
	if observation.Status != joboperation.Canceled {
		observation.Snapshot = snapshot
	}
	return observation, nil
}

func (client *Client) request(ctx context.Context, method, path, channel string, body io.Reader) (*http.Request, error) {
	target := *client.endpoint
	target.Path = path
	request, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return nil, err
	}
	outbound, err := providercredentials.PrepareOutboundChannel(request, channel, providercredentials.Replicate, client.credentials)
	if err != nil {
		return nil, err
	}
	return outbound, nil
}
func (client *Client) do(request *http.Request) (*http.Response, []byte, error) {
	defer providercredentials.ClearApplied(request)
	response, err := client.http.Do(request)
	if err != nil {
		return nil, nil, err
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return response, nil, &jobs.ProviderError{Category: "invalid_response"}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, client.maximumBodyBytes+1))
	if err != nil {
		return response, nil, err
	}
	if int64(len(body)) > client.maximumBodyBytes {
		return response, nil, &jobs.ProviderError{Category: "invalid_response"}
	}
	return response, body, nil
}

func (client *Client) observation(jobID string, statusCode int, headers http.Header, body []byte, source string) (string, joboperation.Observation, joboperation.Snapshot, error) {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(body, &envelope) != nil {
		return "", joboperation.Observation{}, joboperation.Snapshot{}, errors.New("invalid Replicate response")
	}
	var providerID, status string
	if json.Unmarshal(envelope["id"], &providerID) != nil || providerID == "" || len(providerID) > 500 || json.Unmarshal(envelope["status"], &status) != nil {
		return "", joboperation.Observation{}, joboperation.Snapshot{}, errors.New("invalid Replicate response")
	}
	mapped, category, ok := mapStatus(status)
	if !ok {
		return "", joboperation.Observation{}, joboperation.Snapshot{}, errors.New("invalid Replicate status")
	}
	envelope["id"], _ = json.Marshal(jobID)
	base := strings.TrimSuffix(client.publicBase.String(), "/")
	envelope["urls"], _ = json.Marshal(map[string]string{"get": base + "/v1/predictions/" + jobID, "cancel": base + "/v1/predictions/" + jobID + "/cancel"})
	sanitized, err := json.Marshal(envelope)
	if err != nil {
		return "", joboperation.Observation{}, joboperation.Snapshot{}, err
	}
	snapshot := joboperation.Snapshot{Status: statusCode, Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: sanitized, SHA256: sha256.Sum256(sanitized)}
	observation := joboperation.Observation{Status: mapped, FailureCategory: category}
	if mapped.Terminal() {
		observation.Usage = replicateOutputUsage(envelope["output"], source)
	}
	return providerID, observation, snapshot, nil
}

// WebhookObservation validates and sanitizes a native Replicate Prediction
// delivered by webhook. Only terminal completed-event payloads are accepted.
func (client *Client) WebhookObservation(jobID string, body []byte) (string, joboperation.Observation, error) {
	providerID, observation, snapshot, err := client.observation(jobID, http.StatusOK, http.Header{"Content-Type": {"application/json"}}, body, "webhook")
	if err != nil || !observation.Status.Terminal() {
		return "", joboperation.Observation{}, errors.New("invalid Replicate webhook observation")
	}
	if observation.Status != joboperation.Canceled {
		observation.Snapshot = snapshot
	}
	observation.ProviderJobID = providerID
	return providerID, observation, nil
}

func replicateOutputUsage(raw json.RawMessage, source string) *joboperation.Usage {
	quantity := int64(0)
	if len(raw) > 0 && string(raw) != "null" {
		var values []json.RawMessage
		if json.Unmarshal(raw, &values) == nil {
			for _, value := range values {
				if usableOutput(value) && quantity < joboperation.MaximumObservedUsage {
					quantity++
				}
			}
		} else if usableOutput(raw) {
			quantity = 1
		}
	}
	return &joboperation.Usage{Dimension: "output", Unit: "image", Quantity: quantity, Provenance: source, ExtractorVersion: "replicate-output-v1"}
}

func usableOutput(raw json.RawMessage) bool {
	var location string
	if json.Unmarshal(raw, &location) == nil {
		return validOutputLocation(location)
	}
	var object struct {
		URL string `json:"url"`
	}
	return json.Unmarshal(raw, &object) == nil && validOutputLocation(object.URL)
}

func validOutputLocation(value string) bool {
	if value == "" || len(value) > 8192 || strings.ContainsAny(value, "\r\n") {
		return false
	}
	parsed, err := url.Parse(value)
	return err == nil && ((parsed.Scheme == "https" && parsed.Host != "") || (parsed.Scheme == "data" && strings.HasPrefix(value, "data:image/")))
}
func mapStatus(value string) (joboperation.Status, string, bool) {
	switch value {
	case "starting":
		return joboperation.Queued, "", true
	case "processing":
		return joboperation.Processing, "", true
	case "succeeded":
		return joboperation.Succeeded, "", true
	case "failed":
		return joboperation.Failed, "provider_error", true
	case "canceled":
		return joboperation.Canceled, "canceled", true
	default:
		return "", "", false
	}
}
func statusCategory(status int) string {
	switch {
	case status == http.StatusTooManyRequests:
		return "rate_limited"
	case status == http.StatusRequestTimeout:
		return "timeout"
	case status >= 500:
		return "unavailable"
	case status >= 400:
		return "rejected"
	default:
		return "invalid_response"
	}
}
func failureSnapshot(status int, body []byte) joboperation.Observation {
	if !json.Valid(body) {
		body = []byte(`{"detail":"prediction request failed"}`)
	}
	snapshot := joboperation.Snapshot{Status: status, Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: append([]byte(nil), body...), SHA256: sha256.Sum256(body)}
	return joboperation.Observation{Status: joboperation.Failed, FailureCategory: "rejected", Snapshot: snapshot}
}
func (client *Client) String() string {
	return fmt.Sprintf("ReplicateClient(%s)", client.endpoint.Hostname())
}
