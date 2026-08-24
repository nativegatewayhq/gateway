// Package runtime defines the public Native Gateway HTTP sidecar wire v1.
package runtime

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"regexp"
	"strings"

	"github.com/nativegatewayhq/gateway/plugin-sdk/jsonstrict"
)

const RequestSchema = "nativegateway.plugin-request/v1"
const ResponseSchema = "nativegateway.plugin-response/v1"
const HealthSchema = "nativegateway.plugin-health/v1"

var ErrInvalid = errors.New("invalid plugin runtime envelope")
var idPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)
var versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

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
type HealthResponse struct {
	SchemaVersion string `json:"schema_version,omitempty"`
	Status        string `json:"status"`
}
type Identity struct{ RequestID, PluginID, PluginVersion, ManifestDigest string }
type Expectation struct {
	Identity                Identity
	Protocol, Model, Output string
	MaximumImages           int
}

func (request ExecuteRequest) Identity() Identity {
	return Identity{request.RequestID, request.PluginID, request.PluginVersion, request.ManifestDigest}
}
func (response ExecuteResponse) Identity() Identity {
	return Identity{response.RequestID, response.PluginID, response.PluginVersion, response.ManifestDigest}
}

func DecodeRequest(reader io.Reader, maximumBytes int64) (ExecuteRequest, error) {
	var value ExecuteRequest
	if err := decode(reader, maximumBytes, &value); err != nil || ValidateRequest(value) != nil {
		return ExecuteRequest{}, ErrInvalid
	}
	return value, nil
}
func DecodeResponse(reader io.Reader, maximumBytes int64, expected Expectation) (ExecuteResponse, error) {
	var value ExecuteResponse
	if err := decode(reader, maximumBytes, &value); err != nil || ValidateResponse(value, expected) != nil {
		return ExecuteResponse{}, ErrInvalid
	}
	return value, nil
}
func DecodeHealth(reader io.Reader, maximumBytes int64) (HealthResponse, error) {
	var value HealthResponse
	if err := decode(reader, maximumBytes, &value); err != nil || (value.SchemaVersion != "" && value.SchemaVersion != HealthSchema) || value.Status != "ok" {
		return HealthResponse{}, ErrInvalid
	}
	return value, nil
}

func EncodeRequest(writer io.Writer, value ExecuteRequest) error {
	if ValidateRequest(value) != nil {
		return ErrInvalid
	}
	return json.NewEncoder(writer).Encode(value)
}
func EncodeResponse(writer io.Writer, value ExecuteResponse, expected Expectation) error {
	if ValidateResponse(value, expected) != nil {
		return ErrInvalid
	}
	return json.NewEncoder(writer).Encode(value)
}
func EncodeHealth(writer io.Writer) error {
	return json.NewEncoder(writer).Encode(HealthResponse{SchemaVersion: HealthSchema, Status: "ok"})
}
func CanonicalRequest(value ExecuteRequest) ([]byte, error) {
	if ValidateRequest(value) != nil {
		return nil, ErrInvalid
	}
	return json.Marshal(value)
}
func CanonicalResponse(value ExecuteResponse, expected Expectation) ([]byte, error) {
	if ValidateResponse(value, expected) != nil {
		return nil, ErrInvalid
	}
	return json.Marshal(value)
}

func ValidateRequest(value ExecuteRequest) error {
	if value.SchemaVersion != RequestSchema || !validRequestID(value.RequestID) || !validID(value.PluginID, 128) || !versionPattern.MatchString(value.PluginVersion) || !digestPattern.MatchString(value.ManifestDigest) || value.Operation != "image.generate" || (value.Protocol != "openai" && value.Protocol != "gemini") || !validID(value.Model, 200) || len(value.Input.Prompt) < 1 || len(value.Input.Prompt) > 1<<20 || value.Input.Images < 1 || value.Input.Images > 10 || !validOption(value.Input.Size, 80) || !validOption(value.Input.Quality, 80) {
		return ErrInvalid
	}
	return nil
}
func ValidateResponse(value ExecuteResponse, expected Expectation) error {
	if value.SchemaVersion != ResponseSchema || value.Identity() != expected.Identity || !validIdentity(value.Identity()) || (value.Result == nil) == (value.Error == nil) {
		return ErrInvalid
	}
	if value.Error != nil {
		if !validError(*value.Error) {
			return ErrInvalid
		}
		return nil
	}
	if expected.MaximumImages < 1 || expected.MaximumImages > 10 || !validID(expected.Model, 200) || (expected.Protocol != "openai" && expected.Protocol != "gemini") || (expected.Output != "base64" && expected.Output != "url") || !validResult(*value.Result, expected) {
		return ErrInvalid
	}
	return nil
}
func Success(identity Identity, result Result) ExecuteResponse {
	return ExecuteResponse{SchemaVersion: ResponseSchema, RequestID: identity.RequestID, PluginID: identity.PluginID, PluginVersion: identity.PluginVersion, ManifestDigest: identity.ManifestDigest, Result: &result}
}
func Failure(identity Identity, value PluginError) ExecuteResponse {
	return ExecuteResponse{SchemaVersion: ResponseSchema, RequestID: identity.RequestID, PluginID: identity.PluginID, PluginVersion: identity.PluginVersion, ManifestDigest: identity.ManifestDigest, Error: &value}
}
func InvalidRequest(message string) PluginError {
	return PluginError{Category: "invalid_request", Message: message}
}

func decode(reader io.Reader, maximum int64, target any) error {
	if reader == nil || maximum < 1 || maximum > 128<<20 {
		return ErrInvalid
	}
	body, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil || len(body) == 0 || int64(len(body)) > maximum || jsonstrict.Validate(body) != nil {
		return ErrInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return ErrInvalid
	}
	return nil
}
func validResult(value Result, expected Expectation) bool {
	if len(value.Images) < 1 || len(value.Images) > expected.MaximumImages || value.Usage.Images != len(value.Images) {
		return false
	}
	for _, image := range value.Images {
		if (image.Base64 == "") == (image.URL == "") {
			return false
		}
		if expected.Output == "base64" {
			decoded, err := base64.StdEncoding.DecodeString(image.Base64)
			if err != nil || image.URL != "" || len(decoded) == 0 || len(decoded) > 64<<20 || !MatchesImageType(image.MIMEType, decoded) {
				return false
			}
		} else {
			parsed, err := url.Parse(image.URL)
			if err != nil || image.Base64 != "" || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || !supportedMIME(image.MIMEType) {
				return false
			}
		}
	}
	return true
}
func validError(value PluginError) bool {
	switch value.Category {
	case "invalid_request", "authentication", "rate_limited", "unavailable", "internal":
	default:
		return false
	}
	return len(value.Message) > 0 && len(value.Message) <= 512 && strings.TrimSpace(value.Message) == value.Message
}
func validID(value string, maximum int) bool {
	return len(value) > 0 && len(value) <= maximum && idPattern.MatchString(value) && strings.TrimSpace(value) == value
}
func validRequestID(value string) bool {
	if len(value) < 1 || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || strings.ContainsRune("-_.:", character) {
			continue
		}
		return false
	}
	return true
}
func validOption(value string, maximum int) bool {
	return len(value) <= maximum && strings.TrimSpace(value) == value
}
func validIdentity(value Identity) bool {
	return validRequestID(value.RequestID) && validID(value.PluginID, 128) && versionPattern.MatchString(value.PluginVersion) && digestPattern.MatchString(value.ManifestDigest)
}
func supportedMIME(value string) bool {
	return value == "image/png" || value == "image/jpeg" || value == "image/gif" || value == "image/webp"
}
func MatchesImageType(mimeType string, body []byte) bool {
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
