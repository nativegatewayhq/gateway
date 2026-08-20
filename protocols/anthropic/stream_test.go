package anthropic

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nativegatewayhq/gateway/internal/chatpricing"
)

func TestRelayAnthropicStreamPreservesBytesAndExtractsUsage(t *testing.T) {
	input := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":5,\"cache_creation_input_tokens\":2,\"cache_read_input_tokens\":3,\"output_tokens\":0}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":4}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	w := httptest.NewRecorder()
	result, err := relayStream(w, strings.NewReader(input), 8192, true)
	if err != nil || w.Body.String() != input || !result.Terminal || result.TerminalCategory != "complete" || result.TerminalDigest == ([32]byte{}) || result.Usage != (chatpricing.Usage{PromptTokens: 10, CachedInputTokens: 3, CacheWriteTokens: 2, CompletionTokens: 4}) {
		t.Fatalf("result=%+v err=%v body=%q", result, err, w.Body.String())
	}
}
func TestRelayAnthropicStreamRejectsInvalidLifecycle(t *testing.T) {
	tests := []string{
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":2}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":1}}\n\n",
		"event: ping\ndata: {\"type\":\"message_stop\"}\n\n",
	}
	for _, input := range tests {
		if _, err := relayStream(httptest.NewRecorder(), strings.NewReader(input), 4096, true); !errors.Is(err, errStreamProtocol) {
			t.Fatalf("accepted %q err=%v", input, err)
		}
	}
}
func TestRelayAnthropicErrorEventIsTerminalEvidence(t *testing.T) {
	input := "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\"}}\n\n"
	result, err := relayStream(httptest.NewRecorder(), strings.NewReader(input), 4096, true)
	if err != nil || !result.Terminal || result.TerminalCategory != "error_event" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
