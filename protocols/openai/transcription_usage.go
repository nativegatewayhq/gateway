package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"

	"github.com/nativegatewayhq/gateway/internal/audiopricing"
)

var errTranscriptionUsage = errors.New("invalid transcription usage")

func extractTranscriptionUsage(body []byte) (audiopricing.TranscriptionUsage, string, error) {
	fields, err := collectJSONFields(body, "usage")
	if err != nil || len(fields["usage"]) != 1 || bytes.Equal(fields["usage"][0], []byte("null")) {
		return audiopricing.TranscriptionUsage{}, "", errTranscriptionUsage
	}
	usage, err := parseTranscriptionUsage(fields["usage"][0])
	if err != nil {
		return audiopricing.TranscriptionUsage{}, "", err
	}
	schema := "openai-transcription-token-json-v1"
	if usage.Type == audiopricing.TranscriptionDuration {
		schema = "openai-transcription-duration-json-v1"
	}
	return usage, schema, nil
}

func parseTranscriptionUsage(raw json.RawMessage) (audiopricing.TranscriptionUsage, error) {
	fields, err := collectJSONFields(raw, "type", "input_tokens", "input_token_details", "output_tokens", "total_tokens", "seconds")
	if err != nil || len(fields["type"]) != 1 {
		return audiopricing.TranscriptionUsage{}, errTranscriptionUsage
	}
	var usageType string
	if json.Unmarshal(fields["type"][0], &usageType) != nil {
		return audiopricing.TranscriptionUsage{}, errTranscriptionUsage
	}
	switch usageType {
	case string(audiopricing.TranscriptionTokens):
		if len(fields["input_tokens"]) != 1 || len(fields["input_token_details"]) != 1 || len(fields["output_tokens"]) != 1 || len(fields["total_tokens"]) != 1 || len(fields["seconds"]) != 0 {
			return audiopricing.TranscriptionUsage{}, errTranscriptionUsage
		}
		input, ok := exactNonnegativeInteger(fields["input_tokens"][0])
		if !ok {
			return audiopricing.TranscriptionUsage{}, errTranscriptionUsage
		}
		output, ok := exactNonnegativeInteger(fields["output_tokens"][0])
		if !ok {
			return audiopricing.TranscriptionUsage{}, errTranscriptionUsage
		}
		total, ok := exactNonnegativeInteger(fields["total_tokens"][0])
		if !ok || input > math.MaxInt64-output || total != input+output {
			return audiopricing.TranscriptionUsage{}, errTranscriptionUsage
		}
		details, detailsErr := collectJSONFields(fields["input_token_details"][0], "audio_tokens", "text_tokens")
		if detailsErr != nil || len(details["audio_tokens"]) != 1 || len(details["text_tokens"]) != 1 {
			return audiopricing.TranscriptionUsage{}, errTranscriptionUsage
		}
		audio, ok := exactNonnegativeInteger(details["audio_tokens"][0])
		if !ok {
			return audiopricing.TranscriptionUsage{}, errTranscriptionUsage
		}
		text, ok := exactNonnegativeInteger(details["text_tokens"][0])
		if !ok || audio > math.MaxInt64-text || audio+text != input {
			return audiopricing.TranscriptionUsage{}, errTranscriptionUsage
		}
		return audiopricing.TranscriptionUsage{Type: audiopricing.TranscriptionTokens, InputTokens: input, AudioInputTokens: audio, TextInputTokens: text, OutputTokens: output, TotalTokens: total}, nil
	case string(audiopricing.TranscriptionDuration):
		if len(fields["seconds"]) != 1 || len(fields["input_tokens"]) != 0 || len(fields["input_token_details"]) != 0 || len(fields["output_tokens"]) != 0 || len(fields["total_tokens"]) != 0 {
			return audiopricing.TranscriptionUsage{}, errTranscriptionUsage
		}
		milliseconds, ok := decimalSecondsToMilliseconds(fields["seconds"][0])
		if !ok || milliseconds < 1 {
			return audiopricing.TranscriptionUsage{}, errTranscriptionUsage
		}
		return audiopricing.TranscriptionUsage{Type: audiopricing.TranscriptionDuration, DurationMilliseconds: milliseconds}, nil
	default:
		return audiopricing.TranscriptionUsage{}, errTranscriptionUsage
	}
}

func exactNonnegativeInteger(raw []byte) (int64, bool) {
	value := string(raw)
	if value == "" || value[0] == '-' || (len(value) > 1 && value[0] == '0') {
		return 0, false
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	return parsed, err == nil && parsed >= 0
}

func decimalSecondsToMilliseconds(raw []byte) (int64, bool) {
	value := string(raw)
	if value == "" || strings.HasPrefix(value, "-") || strings.ContainsAny(value, "eE+") {
		return 0, false
	}
	parts := strings.Split(value, ".")
	if len(parts) > 2 || parts[0] == "" || (len(parts[0]) > 1 && parts[0][0] == '0') {
		return 0, false
	}
	seconds, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || seconds > math.MaxInt64/1000 {
		return 0, false
	}
	milliseconds := seconds * 1000
	if len(parts) == 1 {
		return milliseconds, true
	}
	fraction := parts[1]
	if fraction == "" || len(fraction) > 9 {
		return 0, false
	}
	for _, digit := range fraction {
		if digit < '0' || digit > '9' {
			return 0, false
		}
	}
	firstThree := fraction
	if len(firstThree) > 3 {
		firstThree = firstThree[:3]
	}
	for len(firstThree) < 3 {
		firstThree += "0"
	}
	fractionMilliseconds, _ := strconv.ParseInt(firstThree, 10, 64)
	roundUp := false
	if len(fraction) > 3 {
		for _, digit := range fraction[3:] {
			if digit != '0' {
				roundUp = true
			}
		}
	}
	if milliseconds > math.MaxInt64-fractionMilliseconds || (roundUp && milliseconds+fractionMilliseconds == math.MaxInt64) {
		return 0, false
	}
	milliseconds += fractionMilliseconds
	if roundUp {
		milliseconds++
	}
	return milliseconds, true
}
