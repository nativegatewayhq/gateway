package openai

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

type responsesStreamResult struct {
	Usage          chatpricing.Usage
	TerminalDigest [32]byte
	Terminal       string
	UsageFound     bool
	FirstByte      time.Duration
	SequenceSeen   bool
	LastSequence   int64
}

func relayResponsesStream(w http.ResponseWriter, source io.Reader, maximumEventBytes int64, observe bool) (responsesStreamResult, error) {
	if _, ok := w.(http.Flusher); !ok {
		return responsesStreamResult{}, errStreamWrite
	}
	controller := http.NewResponseController(w)
	reader := bufio.NewReaderSize(source, 32*1024)
	var result responsesStreamResult
	var event bytes.Buffer
	var observed int64
	started := time.Now()
	for {
		line, readErr := reader.ReadString('\n')
		observed += int64(len(line))
		if maximumEventBytes < 1 || observed > maximumEventBytes || int64(len(line))+int64(event.Len()) > maximumEventBytes {
			return result, errStreamProtocol
		}
		if len(line) > 0 {
			if _, writeErr := io.WriteString(w, line); writeErr != nil {
				return result, errStreamWrite
			}
			if result.FirstByte == 0 {
				result.FirstByte = time.Since(started)
			}
			if flushErr := controller.Flush(); flushErr != nil {
				return result, errStreamWrite
			}
			trimmed := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
			if trimmed == "" {
				if observe && event.Len() > 0 {
					if err := observeResponsesSSEEvent(event.Bytes(), &result); err != nil {
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
				if observe && event.Len() > 0 {
					if err := observeResponsesSSEEvent(event.Bytes(), &result); err != nil {
						return result, err
					}
				}
				return result, nil
			}
			return result, readErr
		}
	}
}

func observeResponsesSSEEvent(event []byte, result *responsesStreamResult) error {
	var eventName string
	var dataLines []string
	for _, line := range strings.Split(strings.TrimSuffix(string(event), "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, ":"):
			continue
		case line == "event":
			eventName = ""
		case strings.HasPrefix(line, "event:"):
			eventName = strings.TrimPrefix(line, "event:")
			if strings.HasPrefix(eventName, " ") {
				eventName = eventName[1:]
			}
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
	envelope, err := parseResponsesStreamEnvelope(data)
	if err != nil || (eventName != "" && eventName != envelope.Type) || (result.SequenceSeen && envelope.Sequence <= result.LastSequence) {
		return errStreamProtocol
	}
	result.SequenceSeen, result.LastSequence = true, envelope.Sequence
	terminal := ""
	switch envelope.Type {
	case "response.completed":
		terminal = "complete"
	case "response.failed":
		terminal = "response_failed"
	case "response.incomplete":
		terminal = "response_incomplete"
	case "error":
		terminal = "error_event"
	default:
		if result.Terminal != "" {
			return errStreamProtocol
		}
		return nil
	}
	if result.Terminal != "" {
		return errStreamProtocol
	}
	result.Terminal = terminal
	result.TerminalDigest = sha256.Sum256(data)
	if terminal != "complete" {
		return nil
	}
	response, err := collectJSONFields(envelope.Response, "status", "usage")
	if err != nil || len(response["status"]) != 1 || len(response["usage"]) != 1 || bytes.Equal(response["usage"][0], []byte("null")) {
		return errStreamProtocol
	}
	var status string
	if json.Unmarshal(response["status"][0], &status) != nil || status != "completed" {
		return errStreamProtocol
	}
	usage, err := parseResponsesUsage(response["usage"][0])
	if err != nil {
		return errStreamProtocol
	}
	result.Usage, result.UsageFound = usage, true
	return nil
}

type responsesStreamEnvelope struct {
	Type     string
	Sequence int64
	Response json.RawMessage
}

func parseResponsesStreamEnvelope(data []byte) (responsesStreamEnvelope, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return responsesStreamEnvelope{}, errStreamProtocol
	}
	var envelope responsesStreamEnvelope
	typeCount, sequenceCount, responseCount := 0, 0, 0
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return responsesStreamEnvelope{}, errStreamProtocol
		}
		switch key.(string) {
		case "type":
			typeCount++
			if decoder.Decode(&envelope.Type) != nil {
				return responsesStreamEnvelope{}, errStreamProtocol
			}
		case "sequence_number":
			sequenceCount++
			if decoder.Decode(&envelope.Sequence) != nil || envelope.Sequence < 0 {
				return responsesStreamEnvelope{}, errStreamProtocol
			}
		case "response":
			responseCount++
			if decoder.Decode(&envelope.Response) != nil {
				return responsesStreamEnvelope{}, errStreamProtocol
			}
		default:
			var ignored json.RawMessage
			if decoder.Decode(&ignored) != nil {
				return responsesStreamEnvelope{}, errStreamProtocol
			}
		}
	}
	if _, err = decoder.Token(); err != nil || decoder.Decode(&struct{}{}) != io.EOF || typeCount != 1 || sequenceCount != 1 || responseCount > 1 || envelope.Type == "" {
		return responsesStreamEnvelope{}, errStreamProtocol
	}
	return envelope, nil
}
