package openai

import "testing"

func TestExtractTranslationDurationIsExactAndDuplicateSafe(t *testing.T) {
	for input, expected := range map[string]int64{`{"duration":1}`: 1000, `{"duration":1.0001,"text":"private"}`: 1001, `{"duration":0.001}`: 1, `{"duration":12.345000000}`: 12345} {
		actual, err := extractTranslationDuration([]byte(input))
		if err != nil || actual != expected {
			t.Fatalf("input=%s actual=%d err=%v", input, actual, err)
		}
	}
	for _, input := range []string{`{}`, `{"duration":null}`, `{"duration":"1"}`, `{"duration":true}`, `{"duration":0}`, `{"duration":-1}`, `{"duration":1e1}`, `{"duration":1,"duration":2}`, `{"duration":1.1234567890}`, `{"duration":9223372036854776}`} {
		if _, err := extractTranslationDuration([]byte(input)); err == nil {
			t.Fatalf("invalid duration accepted: %s", input)
		}
	}
}
