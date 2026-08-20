// Package runway implements the fixed-origin Runway task adapter.
package runway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/nativegatewayhq/gateway/internal/jobs"
	"github.com/nativegatewayhq/gateway/internal/pricing"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	joboperation "github.com/nativegatewayhq/gateway/operations/job"
)

const APIVersion = "2024-11-06"

var ErrInvalidConfig = errors.New("invalid Runway provider configuration")

type Config struct {
	Endpoint         string
	Timeout          time.Duration
	MaximumBodyBytes int64
}

type SubmitPayload struct {
	Path string
	Body []byte
}

type UploadResponse struct {
	Status int
	Body   []byte
	URI    string
}

type Client struct {
	endpoint         *url.URL
	credentials      *providercredentials.Registry
	http             *http.Client
	maximumBodyBytes int64
}

func New(config Config, credentials *providercredentials.Registry) (*Client, error) {
	endpoint, err := url.Parse(strings.TrimSpace(config.Endpoint))
	if err != nil || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || (endpoint.Path != "" && endpoint.Path != "/") {
		return nil, ErrInvalidConfig
	}
	loopback := endpoint.Hostname() == "127.0.0.1" || endpoint.Hostname() == "::1" || endpoint.Hostname() == "localhost"
	if endpoint.Scheme != "https" && !(endpoint.Scheme == "http" && loopback) {
		return nil, ErrInvalidConfig
	}
	if credentials == nil || config.Timeout <= 0 || config.Timeout > 10*time.Minute || config.MaximumBodyBytes < 1 || config.MaximumBodyBytes > 256*1024*1024 {
		return nil, ErrInvalidConfig
	}
	endpoint.Path = ""
	transport := http.DefaultTransport.(*http.Transport).Clone()
	return &Client{endpoint: endpoint, credentials: credentials, maximumBodyBytes: config.MaximumBodyBytes, http: &http.Client{Timeout: config.Timeout, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (client *Client) Submit(ctx context.Context, value joboperation.Job, payload any) (jobs.SubmitResult, error) {
	submit, ok := payload.(SubmitPayload)
	if !ok || (submit.Path != "/v1/text_to_video" && submit.Path != "/v1/image_to_video") || len(submit.Body) == 0 || int64(len(submit.Body)) > client.maximumBodyBytes {
		return jobs.SubmitResult{}, knownFailure(http.StatusBadRequest, []byte(`{"error":"invalid request"}`))
	}
	request, err := client.request(ctx, http.MethodPost, submit.Path, value.ChannelID, bytes.NewReader(submit.Body))
	if err != nil {
		return jobs.SubmitResult{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, body, err := client.do(request)
	if err != nil {
		return jobs.SubmitResult{}, err
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		known := response.StatusCode >= 400 && response.StatusCode < 500 && response.StatusCode != 408 && response.StatusCode != 409 && response.StatusCode != 429
		providerErr := providerFailure(response.StatusCode, body)
		providerErr.Known = known
		return jobs.SubmitResult{}, providerErr
	}
	var envelope struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(body, &envelope) != nil || envelope.ID == "" || len(envelope.ID) > 500 {
		return jobs.SubmitResult{}, &jobs.ProviderError{Category: "invalid_response"}
	}
	return jobs.SubmitResult{ProviderJobID: envelope.ID, Observation: joboperation.Observation{Status: joboperation.Queued}, PollAfter: 5 * time.Second}, nil
}

// CreateEphemeralUpload obtains a Provider-signed direct-upload form. Media
// bytes are uploaded by the SDK directly to the returned storage URL.
func (client *Client) CreateEphemeralUpload(ctx context.Context, channelID string, body []byte) (UploadResponse, error) {
	if len(body) == 0 || len(body) > 4096 {
		return UploadResponse{}, &jobs.ProviderError{Category: "invalid_request"}
	}
	request, err := client.request(ctx, http.MethodPost, "/v1/uploads", channelID, bytes.NewReader(body))
	if err != nil {
		return UploadResponse{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, responseBody, err := client.do(request)
	if err != nil {
		return UploadResponse{}, err
	}
	result := UploadResponse{Status: response.StatusCode, Body: responseBody}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return result, nil
	}
	var envelope struct {
		UploadURL string            `json:"uploadUrl"`
		Fields    map[string]string `json:"fields"`
		RunwayURI string            `json:"runwayUri"`
	}
	if json.Unmarshal(responseBody, &envelope) != nil || !validUploadURL(envelope.UploadURL) || len(envelope.Fields) == 0 || len(envelope.Fields) > 32 || !validUploadURI(envelope.RunwayURI) {
		return UploadResponse{}, &jobs.ProviderError{Category: "invalid_response"}
	}
	for key, value := range envelope.Fields {
		if key == "" || len(key) > 128 || len(value) > 8192 || strings.TrimSpace(key) != key || strings.IndexFunc(key+value, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
			return UploadResponse{}, &jobs.ProviderError{Category: "invalid_response"}
		}
	}
	result.URI = envelope.RunwayURI
	return result, nil
}

func validUploadURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil && parsed.Fragment == "" && len(value) <= 8192
}

func validUploadURI(value string) bool {
	return strings.HasPrefix(value, "runway://") && len(value) >= 13 && len(value) <= 5000 && strings.TrimSpace(value) == value && strings.IndexFunc(value, func(r rune) bool { return r < 0x21 || r == 0x7f }) == -1
}

func (client *Client) Poll(ctx context.Context, attempt jobs.ProviderAttempt) (joboperation.Observation, error) {
	if attempt.ProviderJobID == "" {
		return joboperation.Observation{}, &jobs.ProviderError{Category: "invalid_request"}
	}
	request, err := client.request(ctx, http.MethodGet, "/v1/tasks/"+url.PathEscape(attempt.ProviderJobID), attempt.ChannelID, nil)
	if err != nil {
		return joboperation.Observation{}, err
	}
	response, body, err := client.do(request)
	if err != nil {
		return joboperation.Observation{}, err
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return joboperation.Observation{}, providerFailure(response.StatusCode, body)
	}
	return observation(attempt.JobID, response.StatusCode, body, "poll")
}

func (client *Client) Cancel(ctx context.Context, attempt jobs.ProviderAttempt) (joboperation.Observation, error) {
	if attempt.ProviderJobID == "" {
		return joboperation.Observation{}, &jobs.ProviderError{Category: "invalid_request"}
	}
	request, err := client.request(ctx, http.MethodDelete, "/v1/tasks/"+url.PathEscape(attempt.ProviderJobID), attempt.ChannelID, nil)
	if err != nil {
		return joboperation.Observation{}, err
	}
	response, body, err := client.do(request)
	if err != nil {
		return joboperation.Observation{}, err
	}
	if response.StatusCode == http.StatusNoContent || response.StatusCode == http.StatusNotFound {
		return joboperation.Observation{Status: joboperation.Canceled, FailureCategory: "canceled"}, nil
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return joboperation.Observation{}, providerFailure(response.StatusCode, body)
	}
	return joboperation.Observation{Status: joboperation.Canceled, FailureCategory: "canceled"}, nil
}

func (client *Client) request(ctx context.Context, method, path, channel string, body io.Reader) (*http.Request, error) {
	target := *client.endpoint
	target.Path = path
	request, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return nil, err
	}
	outbound, err := providercredentials.PrepareOutboundChannel(request, channel, providercredentials.Runway, client.credentials)
	if err != nil {
		return nil, err
	}
	outbound.Header.Set("X-Runway-Version", APIVersion)
	outbound.Header.Set("Accept", "application/json")
	return outbound, nil
}

func (client *Client) do(request *http.Request) (*http.Response, []byte, error) {
	defer providercredentials.ClearApplied(request)
	response, err := client.http.Do(request)
	if err != nil {
		return nil, nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, client.maximumBodyBytes+1))
	if err != nil {
		return response, nil, err
	}
	if int64(len(body)) > client.maximumBodyBytes || (response.StatusCode >= 300 && response.StatusCode < 400) {
		return response, nil, &jobs.ProviderError{Category: "invalid_response"}
	}
	return response, body, nil
}

func observation(jobID string, statusCode int, body []byte, source string) (joboperation.Observation, error) {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(body, &envelope) != nil {
		return joboperation.Observation{}, &jobs.ProviderError{Category: "invalid_response"}
	}
	var status string
	if json.Unmarshal(envelope["status"], &status) != nil {
		return joboperation.Observation{}, &jobs.ProviderError{Category: "invalid_response"}
	}
	mapped, category, ok := mapStatus(status)
	if !ok {
		return joboperation.Observation{}, &jobs.ProviderError{Category: "invalid_response"}
	}
	envelope["id"], _ = json.Marshal(jobID)
	sanitized, err := json.Marshal(envelope)
	if err != nil {
		return joboperation.Observation{}, err
	}
	result := joboperation.Observation{Status: mapped, FailureCategory: category}
	if mapped.Terminal() {
		if rawCost, ok := envelope["cost"]; ok {
			if quantity, ok := parseCostMicros(rawCost); ok {
				result.Usage = &joboperation.Usage{Dimension: "provider_credit", Unit: "microcredit", Quantity: quantity, Provenance: source, ExtractorVersion: "runway-task-cost-v1"}
			}
		}
	}
	if mapped == joboperation.Succeeded || mapped == joboperation.Failed {
		result.Snapshot = joboperation.Snapshot{Status: statusCode, Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: sanitized, SHA256: sha256.Sum256(sanitized)}
	}
	return result, nil
}

func parseCostMicros(raw json.RawMessage) (int64, bool) {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return 0, false
	}
	number := strings.TrimSpace(string(object["credits"]))
	if number == "" || strings.HasPrefix(number, "-") || strings.ContainsAny(number, "eE+") {
		return 0, false
	}
	whole, fraction, has := strings.Cut(number, ".")
	if whole == "" {
		return 0, false
	}
	if !has {
		fraction = ""
	}
	if len(fraction) > 6 {
		return 0, false
	}
	for len(fraction) < 6 {
		fraction += "0"
	}
	wholeValue, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || wholeValue > pricing.MaxProviderCreditMicros/pricing.ProviderCreditScale {
		return 0, false
	}
	fractionValue := int64(0)
	if fraction != "" {
		fractionValue, err = strconv.ParseInt(fraction, 10, 64)
		if err != nil {
			return 0, false
		}
	}
	if wholeValue > (pricing.MaxProviderCreditMicros-fractionValue)/pricing.ProviderCreditScale {
		return 0, false
	}
	value := wholeValue*pricing.ProviderCreditScale + fractionValue
	return value, value <= pricing.MaxProviderCreditMicros
}

func mapStatus(status string) (joboperation.Status, string, bool) {
	switch status {
	case "PENDING", "THROTTLED":
		return joboperation.Queued, "", true
	case "RUNNING":
		return joboperation.Processing, "", true
	case "SUCCEEDED":
		return joboperation.Succeeded, "", true
	case "FAILED":
		return joboperation.Failed, "provider_error", true
	case "CANCELED", "CANCELLED":
		return joboperation.Canceled, "canceled", true
	default:
		return "", "", false
	}
}

func knownFailure(status int, body []byte) *jobs.ProviderError {
	err := providerFailure(status, body)
	err.Known = true
	return err
}
func providerFailure(status int, body []byte) *jobs.ProviderError {
	category := "provider_error"
	switch status {
	case 400, 401, 403, 404, 422:
		category = "invalid_request"
	case 429:
		category = "rate_limited"
	case 408, 504:
		category = "timeout"
	case 500, 502, 503:
		category = "unavailable"
	}
	snapshot := joboperation.Snapshot{Status: status, Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: body, SHA256: sha256.Sum256(body)}
	return &jobs.ProviderError{Category: category, Observation: joboperation.Observation{Status: joboperation.Failed, FailureCategory: category, Snapshot: snapshot}}
}
