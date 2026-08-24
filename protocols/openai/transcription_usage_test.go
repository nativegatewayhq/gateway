package openai

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nativegatewayhq/gateway/internal/audiopricing"
)

func TestExtractTranscriptionTokenAndDurationUsage(t *testing.T) {
	tokenBody := []byte(`{"text":"secret transcript","usage":{"type":"tokens","input_tokens":14,"input_token_details":{"text_tokens":10,"audio_tokens":4},"output_tokens":31,"total_tokens":45}}`)
	usage, schema, err := extractTranscriptionUsage(tokenBody)
	if err != nil || schema != "openai-transcription-token-json-v1" || usage != (audiopricing.TranscriptionUsage{Type: audiopricing.TranscriptionTokens, InputTokens: 14, AudioInputTokens: 4, TextInputTokens: 10, OutputTokens: 31, TotalTokens: 45}) {
		t.Fatalf("usage=%+v schema=%s err=%v", usage, schema, err)
	}
	durationBody := []byte(`{"text":"secret transcript","usage":{"type":"duration","seconds":42.7001}}`)
	usage, schema, err = extractTranscriptionUsage(durationBody)
	if err != nil || schema != "openai-transcription-duration-json-v1" || usage.DurationMilliseconds != 42701 {
		t.Fatalf("usage=%+v schema=%s err=%v", usage, schema, err)
	}
}

func TestBillableTranscriptionSSERequiresOneMatchingTerminalUsage(t *testing.T) {
	done := `{"type":"transcript.text.done","usage":{"type":"tokens","input_tokens":1,"input_token_details":{"audio_tokens":1,"text_tokens":0},"output_tokens":1,"total_tokens":2}}`
	valid := "event: transcript.text.done\ndata: " + done + "\n\ndata: [DONE]\n\n"
	result, err := relayBillableTranscription(httptest.NewRecorder(), strings.NewReader(valid), 4096)
	if err != nil || !result.UsageFound || result.Usage.TotalTokens != 2 || !result.DoneMarker {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	for _, stream := range []string{
		"data: [DONE]\n\n",
		"event: wrong.type\ndata: " + done + "\n\n",
		"data: " + done + "\n\ndata: " + done + "\n\n",
		"data: {\"type\":\"transcript.text.delta\",\"delta\":\"private\"}\n\n",
	} {
		if _, err = relayBillableTranscription(httptest.NewRecorder(), strings.NewReader(stream), 4096); err == nil {
			t.Fatalf("invalid stream accepted: %q", stream)
		}
	}
}

func TestTranscriptionUsageFailsClosed(t *testing.T) {
	for _, body := range []string{
		`{"text":"missing"}`,
		`{"usage":{"type":"tokens","input_tokens":1,"input_tokens":1,"input_token_details":{"audio_tokens":1,"text_tokens":0},"output_tokens":1,"total_tokens":2}}`,
		`{"usage":{"type":"tokens","input_tokens":1.0,"input_token_details":{"audio_tokens":1,"text_tokens":0},"output_tokens":1,"total_tokens":2}}`,
		`{"usage":{"type":"tokens","input_tokens":1,"input_token_details":{"audio_tokens":0,"text_tokens":0},"output_tokens":1,"total_tokens":2}}`,
		`{"usage":{"type":"duration","seconds":1e3}}`,
		`{"usage":{"type":"duration","seconds":-1}}`,
	} {
		if _, _, err := extractTranscriptionUsage([]byte(body)); err == nil {
			t.Fatalf("invalid usage accepted: %s", body)
		}
	}
}
