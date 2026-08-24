package openai

import "errors"

var errTranslationDuration = errors.New("invalid translation duration")

func extractTranslationDuration(body []byte) (int64, error) {
	fields, err := collectJSONFields(body, "duration")
	if err != nil || len(fields["duration"]) != 1 {
		return 0, errTranslationDuration
	}
	duration, ok := decimalSecondsToMilliseconds(fields["duration"][0])
	if !ok || duration < 1 {
		return 0, errTranslationDuration
	}
	return duration, nil
}
