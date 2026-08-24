package async

import (
	"encoding/base64"
	"net/url"
	"regexp"
	"strings"

	runtimev1 "github.com/nativegatewayhq/gateway/plugin-sdk/runtime/v1"
)

var idPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)
var versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var jobPattern = regexp.MustCompile(`^job_[a-f0-9]{32}$`)
var deliveryPattern = regexp.MustCompile(`^delivery_[A-Za-z0-9_-]{16,128}$`)
var providerRefPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,499}$`)

func ValidateSubmitRequest(value SubmitRequest) error {
	if value.SchemaVersion != SubmitRequestSchema || !validIdentity(value.Identity()) || (value.Protocol != "replicate" && value.Protocol != "fal") || value.Operation != "image.generate" || !validID(value.Model, 200) || len(value.Input.Prompt) < 1 || len(value.Input.Prompt) > 1<<20 || value.Input.Images < 1 || value.Input.Images > 10 || !validOption(value.Input.Size, 80) || !validOption(value.Input.Quality, 80) || value.CallbackURL != "" && !validCallbackURL(value.CallbackURL) {
		return ErrInvalid
	}
	return nil
}
func ValidateControlRequest(value ControlRequest) error {
	if value.SchemaVersion != ControlRequestSchema || !validIdentity(value.Identity()) || (value.Action != "poll" && value.Action != "cancel") || !providerRefPattern.MatchString(value.ProviderJobRef) {
		return ErrInvalid
	}
	return nil
}
func ValidateSubmitResponse(value SubmitResponse, expected Expectation) error {
	if value.SchemaVersion != SubmitResponseSchema || value.Identity() != expected.Identity || !validExpectation(expected) || !providerRefPattern.MatchString(value.ProviderJobRef) || validateObservation(value.Observation, expected) != nil {
		return ErrInvalid
	}
	return nil
}
func ValidateObservationResponse(value ObservationResponse, expected Expectation) error {
	if value.SchemaVersion != ObservationResponseSchema || value.Identity() != expected.Identity || !validExpectation(expected) || validateObservation(value.Observation, expected) != nil {
		return ErrInvalid
	}
	return nil
}
func ValidateCallback(value Callback, expected Expectation) error {
	if value.SchemaVersion != CallbackSchema || !deliveryPattern.MatchString(value.DeliveryID) || value.Identity() != expected.Identity || !validExpectation(expected) || (value.Protocol != "replicate" && value.Protocol != "fal") || value.Operation != "image.generate" || !validID(value.Model, 200) || !providerRefPattern.MatchString(value.ProviderJobRef) || validateObservation(value.Observation, expected) != nil {
		return ErrInvalid
	}
	return nil
}

func validateObservation(value Observation, expected Expectation) error {
	switch value.Status {
	case "QUEUED", "PROCESSING", "RECONCILING":
		if value.Result != nil || value.Error != nil {
			return ErrInvalid
		}
	case "SUCCEEDED":
		if value.Result == nil || value.Error != nil || !validResult(*value.Result, expected) {
			return ErrInvalid
		}
	case "FAILED":
		if value.Result != nil || value.Error == nil || !validError(*value.Error) {
			return ErrInvalid
		}
	case "CANCELED":
		if value.Result != nil || value.Error != nil {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}
func validResult(value Result, expected Expectation) bool {
	if len(value.Images) < 1 || len(value.Images) > expected.MaximumImages || value.Usage.Dimension != "output" || value.Usage.Unit != "image" || value.Usage.Quantity != int64(len(value.Images)) {
		return false
	}
	for _, image := range value.Images {
		if (image.Base64 == "") == (image.URL == "") {
			return false
		}
		if expected.Output == "base64" {
			body, err := base64.StdEncoding.DecodeString(image.Base64)
			if err != nil || image.URL != "" || len(body) < 1 || len(body) > 64<<20 || !runtimev1.MatchesImageType(image.MIMEType, body) {
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
	case "invalid_request", "authentication", "rate_limited", "unavailable", "timeout", "internal":
	default:
		return false
	}
	return len(value.Message) > 0 && len(value.Message) <= 512 && strings.TrimSpace(value.Message) == value.Message && !strings.ContainsAny(value.Message, "\r\n")
}
func validIdentity(value Identity) bool {
	return validRequestID(value.RequestID) && jobPattern.MatchString(value.GatewayJobID) && validID(value.PluginID, 128) && versionPattern.MatchString(value.PluginVersion) && digestPattern.MatchString(value.ManifestDigest)
}
func validExpectation(value Expectation) bool {
	return validIdentity(value.Identity) && (value.Output == "base64" || value.Output == "url") && value.MaximumImages >= 1 && value.MaximumImages <= 10
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
	return len(value) <= maximum && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\r\n")
}
func validCallbackURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || !strings.HasPrefix(parsed.Path, "/internal/webhooks/plugin/") {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return parsed.Scheme == "https" || parsed.Scheme == "http" && (host == "localhost" || host == "127.0.0.1" || host == "::1")
}
func supportedMIME(value string) bool {
	return value == "image/png" || value == "image/jpeg" || value == "image/gif" || value == "image/webp"
}
