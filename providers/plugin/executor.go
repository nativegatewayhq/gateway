// Package plugin adapts native image requests to the bounded sidecar contract.
package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nativegatewayhq/gateway/internal/plugins"
	"github.com/nativegatewayhq/gateway/internal/requestid"
	"github.com/nativegatewayhq/gateway/providers/google"
	"github.com/nativegatewayhq/gateway/providers/openaiimages"
)

type Executor struct{ client *plugins.Client }

func New(client *plugins.Client) *Executor { return &Executor{client: client} }

func (executor *Executor) Generate(ctx context.Context, request openaiimages.Request) (*http.Response, error) {
	if request.Operation != openaiimages.Generate || request.Body == nil {
		return nil, openaiimages.ErrInvalidRequest
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, openaiimages.ErrInvalidRequest
	}
	input, err := openAIInput(body)
	if err != nil {
		return nil, openaiimages.ErrInvalidRequest
	}
	result, err := executor.client.Execute(ctx, request.ChannelID, effectiveRequestID(ctx), "openai", input)
	if err != nil {
		return nil, openAIError(err)
	}
	status := http.StatusOK
	var native any
	if result.Error != nil {
		status = statusFor(result.Error.Category)
		native = map[string]any{"error": map[string]any{"message": result.Error.Message, "type": "server_error", "code": "plugin_" + result.Error.Category}}
	} else {
		images := make([]map[string]string, 0, len(result.Result.Images))
		for _, image := range result.Result.Images {
			value := map[string]string{}
			if image.Base64 != "" {
				value["b64_json"] = image.Base64
			} else {
				value["url"] = image.URL
			}
			images = append(images, value)
		}
		native = map[string]any{"created": time.Now().Unix(), "data": images}
	}
	return response(status, native), nil
}

func (executor *Executor) GenerateContent(ctx context.Context, request google.GenerateContentRequest) (*http.Response, error) {
	if request.Streaming || request.Action == "streamGenerateContent" || request.Body == nil {
		return nil, google.ErrInvalidRequest
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, google.ErrInvalidRequest
	}
	input, err := geminiInput(body)
	if err != nil {
		return nil, google.ErrInvalidRequest
	}
	result, err := executor.client.Execute(ctx, request.ChannelID, effectiveRequestID(ctx), "gemini", input)
	if err != nil {
		return nil, googleError(err)
	}
	status := http.StatusOK
	var native any
	if result.Error != nil {
		status = statusFor(result.Error.Category)
		native = map[string]any{"error": map[string]any{"code": status, "message": result.Error.Message, "status": strings.ToUpper(result.Error.Category)}}
	} else {
		parts := make([]map[string]any, 0, len(result.Result.Images))
		for _, image := range result.Result.Images {
			if image.Base64 != "" {
				parts = append(parts, map[string]any{"inlineData": map[string]string{"mimeType": image.MIMEType, "data": image.Base64}})
			} else {
				parts = append(parts, map[string]any{"fileData": map[string]string{"mimeType": image.MIMEType, "fileUri": image.URL}})
			}
		}
		native = map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"role": "model", "parts": parts}, "finishReason": "STOP"}}}
	}
	return response(status, native), nil
}

func openAIInput(body []byte) (plugins.ImageInput, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	var value struct {
		Model             string `json:"model"`
		Prompt            string `json:"prompt"`
		N                 int    `json:"n,omitempty"`
		Size              string `json:"size,omitempty"`
		Quality           string `json:"quality,omitempty"`
		ResponseFormat    string `json:"response_format,omitempty"`
		Background        string `json:"background,omitempty"`
		Moderation        string `json:"moderation,omitempty"`
		OutputCompression int    `json:"output_compression,omitempty"`
		OutputFormat      string `json:"output_format,omitempty"`
		User              string `json:"user,omitempty"`
		Style             string `json:"style,omitempty"`
	}
	if decoder.Decode(&value) != nil || decoder.Decode(&struct{}{}) != io.EOF || value.Model == "" || value.Prompt == "" {
		return plugins.ImageInput{}, plugins.ErrInvalidRequest
	}
	if value.N == 0 {
		value.N = 1
	}
	return plugins.ImageInput{Prompt: value.Prompt, Images: value.N, Size: value.Size, Quality: value.Quality}, nil
}

func geminiInput(body []byte) (plugins.ImageInput, error) {
	var value struct {
		Contents []struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"contents"`
		GenerationConfig struct {
			CandidateCount int `json:"candidateCount"`
			ImageConfig    struct {
				AspectRatio string `json:"aspectRatio"`
				ImageSize   string `json:"imageSize"`
			} `json:"imageConfig"`
		} `json:"generationConfig"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if decoder.Decode(&value) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return plugins.ImageInput{}, plugins.ErrInvalidRequest
	}
	var prompts []string
	for _, content := range value.Contents {
		for _, part := range content.Parts {
			if strings.TrimSpace(part.Text) != "" {
				prompts = append(prompts, part.Text)
			}
		}
	}
	if len(prompts) == 0 {
		return plugins.ImageInput{}, plugins.ErrInvalidRequest
	}
	count := value.GenerationConfig.CandidateCount
	if count == 0 {
		count = 1
	}
	return plugins.ImageInput{Prompt: strings.Join(prompts, "\n"), Images: count, Size: value.GenerationConfig.ImageConfig.ImageSize, Quality: value.GenerationConfig.ImageConfig.AspectRatio}, nil
}

func statusFor(category string) int {
	switch category {
	case "invalid_request":
		return 400
	case "authentication":
		return 502
	case "rate_limited":
		return 429
	default:
		return 502
	}
}
func response(status int, value any) *http.Response {
	body, _ := json.Marshal(value)
	return &http.Response{StatusCode: status, Status: strconv.Itoa(status) + " " + http.StatusText(status), Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body))}
}
func openAIError(err error) error {
	switch {
	case errors.Is(err, plugins.ErrTimeout):
		return openaiimages.ErrTimeout
	case errors.Is(err, plugins.ErrCanceled):
		return openaiimages.ErrCanceled
	case errors.Is(err, plugins.ErrInvalidRequest):
		return openaiimages.ErrInvalidRequest
	default:
		return openaiimages.ErrUpstream
	}
}
func googleError(err error) error {
	switch {
	case errors.Is(err, plugins.ErrTimeout):
		return google.ErrTimeout
	case errors.Is(err, plugins.ErrCanceled):
		return google.ErrCanceled
	case errors.Is(err, plugins.ErrInvalidRequest):
		return google.ErrInvalidRequest
	default:
		return google.ErrUpstream
	}
}

func effectiveRequestID(ctx context.Context) string {
	if value := requestid.FromContext(ctx); value != "" {
		return value
	}
	return requestid.New()
}
