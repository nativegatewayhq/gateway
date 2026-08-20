package gemini

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/nativegatewayhq/gateway/internal/chatpricing"
)

var (
	errGeminiStreamProtocol = errors.New("invalid Gemini SSE stream")
	errGeminiStreamWrite    = errors.New("Gemini SSE client write failed")
)

type geminiStreamResult struct {
	Usage          chatpricing.Usage
	TerminalDigest [32]byte
	UsageFound     bool
	Terminal       bool
	FinishSeen     bool
	FirstByte      time.Duration
}

func relayGeminiStream(writer http.ResponseWriter, source io.Reader, maximumBytes int64, observe bool) (geminiStreamResult, error) {
	if _, ok := writer.(http.Flusher); !ok {
		return geminiStreamResult{}, errGeminiStreamWrite
	}
	controller := http.NewResponseController(writer)
	reader := bufio.NewReaderSize(source, 32*1024)
	var result geminiStreamResult
	var event bytes.Buffer
	var observed int64
	started := time.Now()
	for {
		line, readErr := reader.ReadString('\n')
		observed += int64(len(line))
		if maximumBytes < 1 || observed > maximumBytes || int64(event.Len())+int64(len(line)) > maximumBytes {
			return result, errGeminiStreamProtocol
		}
		if len(line) > 0 {
			if _, err := io.WriteString(writer, line); err != nil {
				return result, errGeminiStreamWrite
			}
			if result.FirstByte == 0 {
				result.FirstByte = time.Since(started)
			}
			if err := controller.Flush(); err != nil {
				return result, errGeminiStreamWrite
			}
			trimmed := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
			if trimmed == "" {
				if observe && event.Len() > 0 {
					if err := observeGeminiSSEEvent(event.Bytes(), &result); err != nil {
						return result, err
					}
				}
				event.Reset()
			} else {
				event.WriteString(trimmed)
				event.WriteByte('\n')
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				if event.Len() != 0 {
					return result, errGeminiStreamProtocol
				}
				return result, nil
			}
			return result, readErr
		}
	}
}

func observeGeminiSSEEvent(event []byte, result *geminiStreamResult) error {
	var dataLines []string
	for _, line := range strings.Split(strings.TrimSuffix(string(event), "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, ":"):
			continue
		case line == "data":
			dataLines = append(dataLines, "")
		case strings.HasPrefix(line, "data:"):
			value := strings.TrimPrefix(line, "data:")
			if strings.HasPrefix(value, " ") {
				value = value[1:]
			}
			dataLines = append(dataLines, value)
		}
	}
	if len(dataLines) == 0 {
		return nil
	}
	data := []byte(strings.Join(dataLines, "\n"))
	root, err := strictJSONObject(data)
	if err != nil {
		return errGeminiStreamProtocol
	}
	if result.Terminal {
		return errGeminiStreamProtocol
	}
	finishSeen, candidatesPresent, err := geminiFinishState(root["candidates"])
	if err != nil {
		return errGeminiStreamProtocol
	}
	usageRaw, usagePresent := root["usageMetadata"]
	if usagePresent {
		usage, err := extractGeminiUsage(data)
		if err != nil || (result.UsageFound && !geminiUsageMonotonic(result.Usage, usage)) {
			return errGeminiStreamProtocol
		}
		result.Usage = usage
		result.UsageFound = true
		result.TerminalDigest = sha256.Sum256(data)
		_ = usageRaw
	}
	if finishSeen {
		result.FinishSeen = true
	}
	if usagePresent && (result.FinishSeen || !candidatesPresent) {
		result.Terminal = true
	}
	return nil
}

func geminiFinishState(raw json.RawMessage) (finishSeen, candidatesPresent bool, err error) {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return false, false, nil
	}
	var candidates []json.RawMessage
	if json.Unmarshal(raw, &candidates) != nil {
		return false, false, errGeminiStreamProtocol
	}
	for _, candidate := range candidates {
		fields, parseErr := strictJSONObject(candidate)
		if parseErr != nil {
			return false, false, parseErr
		}
		if finish, ok := fields["finishReason"]; ok {
			var value string
			if json.Unmarshal(finish, &value) != nil || value == "" {
				return false, false, errGeminiStreamProtocol
			}
			finishSeen = true
		}
	}
	return finishSeen, len(candidates) > 0, nil
}

func geminiUsageMonotonic(previous, current chatpricing.Usage) bool {
	return current.PromptTokens >= previous.PromptTokens &&
		current.CachedInputTokens >= previous.CachedInputTokens &&
		current.CompletionTokens >= previous.CompletionTokens &&
		current.ToolUsePromptTokens >= previous.ToolUsePromptTokens &&
		current.ThoughtsTokens >= previous.ThoughtsTokens
}
