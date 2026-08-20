package openai

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nativegatewayhq/gateway/internal/chatpricing"
)

func completedResponsesStream() string {
	return "event: response.created\r\ndata: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"status\":\"in_progress\"}}\r\n\r\n" +
		"event: response.output_text.delta\r\ndata: {\"type\":\"response.output_text.delta\",\"sequence_number\":1,\"delta\":\"secret output\"}\r\n\r\n" +
		"event: response.completed\r\ndata: {\"type\":\"response.completed\",\"sequence_number\":2,\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":10,\"input_tokens_details\":{\"cached_tokens\":3},\"output_tokens\":8,\"output_tokens_details\":{\"reasoning_tokens\":5}}}}\r\n\r\n"
}

func TestResponsesStreamPreservesWireAndObservesCompletedUsage(t *testing.T) {
	input := completedResponsesStream()
	w := httptest.NewRecorder()
	result, err := relayResponsesStream(w, strings.NewReader(input), 8192, true)
	if err != nil || w.Body.String() != input || result.Terminal != "complete" || !result.UsageFound || result.Usage != (chatpricing.Usage{PromptTokens: 10, CachedInputTokens: 3, CompletionTokens: 8}) || result.TerminalDigest == ([32]byte{}) {
		t.Fatalf("result=%+v err=%v body=%q", result, err, w.Body.String())
	}
}

func TestResponsesStreamClassifiesFailureWithoutUsage(t *testing.T) {
	w := httptest.NewRecorder()
	result, err := relayResponsesStream(w, strings.NewReader("event: response.failed\ndata: {\"type\":\"response.failed\",\"sequence_number\":0,\"response\":{\"status\":\"failed\"}}\n\n"), 4096, true)
	if err != nil || result.Terminal != "response_failed" || result.UsageFound {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestResponsesStreamRejectsDuplicateTerminalAndEventMismatch(t *testing.T) {
	duplicate := completedResponsesStream() + "event: response.failed\ndata: {\"type\":\"response.failed\",\"sequence_number\":3}\n\n"
	if _, err := relayResponsesStream(httptest.NewRecorder(), strings.NewReader(duplicate), 8192, true); !errors.Is(err, errStreamProtocol) {
		t.Fatalf("duplicate err=%v", err)
	}
	mismatch := "event: response.completed\ndata: {\"type\":\"response.failed\",\"sequence_number\":0}\n\n"
	if _, err := relayResponsesStream(httptest.NewRecorder(), strings.NewReader(mismatch), 4096, true); !errors.Is(err, errStreamProtocol) {
		t.Fatalf("mismatch err=%v", err)
	}
}

func TestResponsesStreamRejectsSequenceRegression(t *testing.T) {
	stream := "event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":2}\n\nevent: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"sequence_number\":1}\n\n"
	if _, err := relayResponsesStream(httptest.NewRecorder(), strings.NewReader(stream), 4096, true); !errors.Is(err, errStreamProtocol) {
		t.Fatalf("err=%v", err)
	}
}

func TestResponsesStreamRejectsDuplicateUsageFields(t *testing.T) {
	stream := "event: response.completed\ndata: {\"type\":\"response.completed\",\"sequence_number\":0,\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"input_tokens\":2,\"output_tokens\":1}}}\n\n"
	if _, err := relayResponsesStream(httptest.NewRecorder(), strings.NewReader(stream), 4096, true); !errors.Is(err, errStreamProtocol) {
		t.Fatalf("err=%v", err)
	}
}

func TestResponsesStreamClassifiesAllNonSuccessTerminals(t *testing.T) {
	for event, category := range map[string]string{"response.failed": "response_failed", "response.incomplete": "response_incomplete", "error": "error_event"} {
		t.Run(event, func(t *testing.T) {
			stream := "event: " + event + "\ndata: {\"type\":\"" + event + "\",\"sequence_number\":0}\n\n"
			result, err := relayResponsesStream(httptest.NewRecorder(), strings.NewReader(stream), 4096, true)
			if err != nil || result.Terminal != category || result.UsageFound {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
}
