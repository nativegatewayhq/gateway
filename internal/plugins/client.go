package plugins

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	manifest "github.com/nativegatewayhq/gateway/plugin-sdk/manifest/v1"
)

const RequestSchema = "nativegateway.plugin-request/v1"
const ResponseSchema = "nativegateway.plugin-response/v1"

var (
	ErrUnavailable     = errors.New("plugin unavailable")
	ErrTimeout         = errors.New("plugin timeout")
	ErrCanceled        = errors.New("plugin request canceled")
	ErrInvalidRequest  = errors.New("invalid plugin request")
	ErrInvalidResponse = errors.New("invalid plugin response")
)

type ImageInput struct {
	Prompt  string `json:"prompt"`
	Images  int    `json:"images"`
	Size    string `json:"size,omitempty"`
	Quality string `json:"quality,omitempty"`
}
type ExecuteRequest struct {
	SchemaVersion  string     `json:"schema_version"`
	RequestID      string     `json:"request_id"`
	PluginID       string     `json:"plugin_id"`
	PluginVersion  string     `json:"plugin_version"`
	ManifestDigest string     `json:"manifest_digest"`
	Operation      string     `json:"operation"`
	Protocol       string     `json:"protocol"`
	Model          string     `json:"model"`
	Input          ImageInput `json:"input"`
}
type Image struct {
	MIMEType string `json:"mime_type"`
	Base64   string `json:"base64,omitempty"`
	URL      string `json:"url,omitempty"`
}
type Usage struct {
	Images int `json:"images"`
}
type Result struct {
	Images []Image `json:"images"`
	Usage  Usage   `json:"usage"`
}
type PluginError struct {
	Category  string `json:"category"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}
type ExecuteResponse struct {
	SchemaVersion  string       `json:"schema_version"`
	RequestID      string       `json:"request_id"`
	PluginID       string       `json:"plugin_id"`
	PluginVersion  string       `json:"plugin_version"`
	ManifestDigest string       `json:"manifest_digest"`
	Result         *Result      `json:"result,omitempty"`
	Error          *PluginError `json:"error,omitempty"`
}

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
		_, readErr := io.Copy(io.Discard, io.LimitReader(response.Body, 4097))
		closeErr := response.Body.Close()
		if readErr != nil || closeErr != nil || response.StatusCode < 200 || response.StatusCode >= 300 {
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
	body, err := json.Marshal(envelope)
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
	limited := io.LimitReader(response.Body, client.registry.config.MaximumResponseBytes+1)
	responseBody, readErr := io.ReadAll(limited)
	if readErr != nil || int64(len(responseBody)) > client.registry.config.MaximumResponseBytes {
		return ExecuteResponse{}, ErrInvalidResponse
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return ExecuteResponse{}, ErrUnavailable
	}
	if manifest.HasDuplicateKeys(responseBody) {
		return ExecuteResponse{}, ErrInvalidResponse
	}
	decoder := json.NewDecoder(bytes.NewReader(responseBody))
	decoder.DisallowUnknownFields()
	var result ExecuteResponse
	if decoder.Decode(&result) != nil || decoder.Decode(&struct{}{}) != io.EOF || result.SchemaVersion != ResponseSchema || result.RequestID != requestID || result.PluginID != binding.PluginID || result.PluginVersion != binding.Version || result.ManifestDigest != binding.DigestHex() || (result.Result == nil) == (result.Error == nil) {
		return ExecuteResponse{}, ErrInvalidResponse
	}
	if result.Error != nil {
		if !validError(*result.Error) {
			return ExecuteResponse{}, ErrInvalidResponse
		}
		return result, nil
	}
	if !validResult(*result.Result, binding) {
		return ExecuteResponse{}, ErrInvalidResponse
	}
	return result, nil
}

func validError(value PluginError) bool {
	switch value.Category {
	case "invalid_request", "authentication", "rate_limited", "unavailable", "internal":
	default:
		return false
	}
	return len(value.Message) > 0 && len(value.Message) <= 512 && strings.TrimSpace(value.Message) == value.Message
}
func validResult(value Result, binding Binding) bool {
	if len(value.Images) < 1 || len(value.Images) > binding.MaximumImages || value.Usage.Images != len(value.Images) {
		return false
	}
	for _, image := range value.Images {
		if len(image.MIMEType) < 7 || len(image.MIMEType) > 128 || !strings.HasPrefix(image.MIMEType, "image/") || (image.Base64 == "") == (image.URL == "") {
			return false
		}
		if binding.Output == "base64" {
			if image.URL != "" {
				return false
			}
			decoded, err := base64.StdEncoding.DecodeString(image.Base64)
			if err != nil || len(decoded) == 0 || int64(len(decoded)) > 64<<20 || !matchesImageType(image.MIMEType, decoded) {
				return false
			}
		} else {
			if image.Base64 != "" {
				return false
			}
			parsed, err := url.Parse(image.URL)
			if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
				return false
			}
			if _, ok := binding.ResultOrigins[parsed.Scheme+"://"+parsed.Host]; !ok {
				return false
			}
		}
	}
	return true
}

func matchesImageType(mimeType string, body []byte) bool {
	switch mimeType {
	case "image/png":
		return len(body) >= 8 && bytes.Equal(body[:8], []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
	case "image/jpeg":
		return len(body) >= 3 && body[0] == 0xff && body[1] == 0xd8 && body[2] == 0xff
	case "image/gif":
		return len(body) >= 6 && (string(body[:6]) == "GIF87a" || string(body[:6]) == "GIF89a")
	case "image/webp":
		return len(body) >= 12 && string(body[:4]) == "RIFF" && string(body[8:12]) == "WEBP"
	default:
		return false
	}
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
