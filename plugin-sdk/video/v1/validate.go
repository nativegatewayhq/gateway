package video

import (
	"net/url"
	"regexp"
	"strings"
)

var idPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)
var versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var jobPattern = regexp.MustCompile(`^job_[a-f0-9]{32}$`)
var deliveryPattern = regexp.MustCompile(`^delivery_[A-Za-z0-9_-]{16,128}$`)
var refPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,499}$`)

func ValidateSubmitRequest(value SubmitRequest, expected Expectation) error {
	if value.SchemaVersion != SubmitRequestSchema || !validIdentity(value.Identity()) || value.Protocol != "runway" || value.Operation != "video.generate" || !validID(value.Model, 200) || !validExpectation(expected) || value.Identity() != expected.Identity || !validInput(value.Input, expected) || value.CallbackURL != "" && !validCallbackURL(value.CallbackURL) {
		return ErrInvalid
	}
	return nil
}
func ValidateControlRequest(value ControlRequest) error {
	if value.SchemaVersion != ControlRequestSchema || !validIdentity(value.Identity()) || (value.Action != "poll" && value.Action != "cancel") || !refPattern.MatchString(value.ProviderJobRef) {
		return ErrInvalid
	}
	return nil
}
func ValidateSubmitResponse(value SubmitResponse, expected Expectation) error {
	if value.SchemaVersion != SubmitResponseSchema || value.Identity() != expected.Identity || !refPattern.MatchString(value.ProviderJobRef) || validateObservation(value.Observation, expected) != nil {
		return ErrInvalid
	}
	return nil
}
func ValidateObservationResponse(value ObservationResponse, expected Expectation) error {
	if value.SchemaVersion != ObservationResponseSchema || value.Identity() != expected.Identity || validateObservation(value.Observation, expected) != nil {
		return ErrInvalid
	}
	return nil
}
func ValidateCallback(value Callback, expected Expectation) error {
	if value.SchemaVersion != CallbackSchema || !deliveryPattern.MatchString(value.DeliveryID) || value.Identity() != expected.Identity || value.Protocol != "runway" || value.Operation != "video.generate" || !validID(value.Model, 200) || !refPattern.MatchString(value.ProviderJobRef) || validateObservation(value.Observation, expected) != nil {
		return ErrInvalid
	}
	return nil
}
func validInput(value Input, expected Expectation) bool {
	if (value.Kind != "text_to_video" && value.Kind != "image_to_video") || value.Kind == "text_to_video" && !expected.TextToVideo || value.Kind == "image_to_video" && !expected.ImageToVideo || len(value.Prompt) > 1<<20 || value.Kind == "text_to_video" && strings.TrimSpace(value.Prompt) == "" || value.Prompt != "" && strings.TrimSpace(value.Prompt) == "" || value.DurationSeconds < 1 || value.DurationSeconds > expected.MaximumDurationSeconds || !expected.Ratios[value.Ratio] || value.Audio && !expected.Audio {
		return false
	}
	if value.Seed != nil && (*value.Seed < 0 || *value.Seed > 1<<53-1) {
		return false
	}
	if value.Kind == "text_to_video" {
		return value.Source == nil
	}
	return value.Source != nil && validSource(*value.Source)
}
func validSource(value SourceAsset) bool {
	return len(value.URI) >= 13 && len(value.URI) <= 5000 && strings.HasPrefix(value.URI, "runway://") && strings.TrimSpace(value.URI) == value.URI && (value.ContentType == "image/png" || value.ContentType == "image/jpeg" || value.ContentType == "image/webp")
}
func validateObservation(value Observation, expected Expectation) error {
	if !validExpectation(expected) {
		return ErrInvalid
	}
	if value.Progress != nil && (*value.Progress < 0 || *value.Progress > 100) {
		return ErrInvalid
	}
	switch value.Status {
	case "QUEUED", "PROCESSING", "RECONCILING":
		if value.Result != nil || value.Usage != nil || value.Error != nil {
			return ErrInvalid
		}
	case "SUCCEEDED":
		if value.Result == nil || value.Usage == nil || value.Error != nil || !validResult(*value.Result, expected) || !validUsage(*value.Usage, true) {
			return ErrInvalid
		}
	case "FAILED":
		if value.Result != nil || value.Error == nil || !validError(*value.Error) || (value.Usage != nil && !validUsage(*value.Usage, false)) {
			return ErrInvalid
		}
	case "CANCELED":
		if value.Result != nil || value.Error != nil || (value.Usage != nil && !validUsage(*value.Usage, false)) {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}
func validResult(value Result, expected Expectation) bool {
	parsed, err := url.Parse(value.URL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || (value.ContentType != "video/mp4" && value.ContentType != "video/webm") || value.DurationSeconds < 1 || value.DurationSeconds > expected.MaximumDurationSeconds {
		return false
	}
	return expected.ResultOrigins[parsed.Scheme+"://"+parsed.Host]
}
func validUsage(value Usage, positive bool) bool {
	return value.Dimension == "provider_credit" && value.Unit == "microcredit" && value.Quantity >= 0 && value.Quantity <= 1<<53-1 && (!positive || value.Quantity > 0)
}
func validError(value PluginError) bool {
	switch value.Category {
	case "invalid_request", "authentication", "rate_limited", "unavailable", "timeout", "internal":
	default:
		return false
	}
	return len(value.Message) > 0 && len(value.Message) <= 512 && strings.TrimSpace(value.Message) == value.Message && !strings.ContainsAny(value.Message, "\r\n")
}
func validExpectation(value Expectation) bool {
	return validIdentity(value.Identity) && value.MaximumDurationSeconds >= 1 && value.MaximumDurationSeconds <= 600 && len(value.Ratios) >= 1 && len(value.Ratios) <= 16 && len(value.ResultOrigins) >= 1 && (value.TextToVideo || value.ImageToVideo)
}
func validIdentity(value Identity) bool {
	return validRequestID(value.RequestID) && jobPattern.MatchString(value.GatewayJobID) && validID(value.PluginID, 128) && versionPattern.MatchString(value.PluginVersion) && digestPattern.MatchString(value.ManifestDigest)
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
func validCallbackURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || !strings.HasPrefix(parsed.Path, "/internal/webhooks/plugin-video/") {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return parsed.Scheme == "https" || parsed.Scheme == "http" && (host == "localhost" || host == "127.0.0.1" || host == "::1")
}
