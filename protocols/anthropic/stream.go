package anthropic

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

var errStreamProtocol = errors.New("invalid Anthropic SSE stream")
var errStreamWrite = errors.New("Anthropic SSE client write failed")

type streamResult struct {
	Usage                         chatpricing.Usage
	TerminalDigest                [32]byte
	Started, UsageFound, Terminal bool
	TerminalCategory              string
	FirstByte                     time.Duration
}

func relayStream(writer http.ResponseWriter, source io.Reader, maximum int64, observe bool) (streamResult, error) {
	if _, ok := writer.(http.Flusher); !ok {
		return streamResult{}, errStreamWrite
	}
	controller := http.NewResponseController(writer)
	reader := bufio.NewReaderSize(source, 32*1024)
	var result streamResult
	var event bytes.Buffer
	var total int64
	started := time.Now()
	for {
		line, readErr := reader.ReadString('\n')
		total += int64(len(line))
		if maximum < 1 || total > maximum || int64(event.Len()+len(line)) > maximum {
			return result, errStreamProtocol
		}
		if len(line) > 0 {
			if _, err := io.WriteString(writer, line); err != nil {
				return result, errStreamWrite
			}
			if result.FirstByte == 0 {
				result.FirstByte = time.Since(started)
			}
			if err := controller.Flush(); err != nil {
				return result, errStreamWrite
			}
			trimmed := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
			if trimmed == "" {
				if observe && event.Len() > 0 {
					if err := observeEvent(event.Bytes(), &result); err != nil {
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
					return result, errStreamProtocol
				}
				return result, nil
			}
			return result, readErr
		}
	}
}

func observeEvent(event []byte, result *streamResult) error {
	var name string
	var dataLines []string
	for _, line := range strings.Split(strings.TrimSuffix(string(event), "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, ":"):
			continue
		case strings.HasPrefix(line, "event:"):
			name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
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
	fields, err := collectFields(data)
	if err != nil || len(fields["type"]) != 1 {
		return errStreamProtocol
	}
	var kind string
	if json.Unmarshal(fields["type"][0], &kind) != nil || (name != "" && name != kind) {
		return errStreamProtocol
	}
	if result.Terminal {
		return errStreamProtocol
	}
	switch kind {
	case "ping":
		return nil
	case "error":
		result.Terminal = true
		result.TerminalCategory = "error_event"
		result.TerminalDigest = sha256.Sum256(data)
		return nil
	case "message_start":
		if result.Started || len(fields["message"]) != 1 {
			return errStreamProtocol
		}
		message, err := collectFields(fields["message"][0])
		if err != nil || len(message["usage"]) != 1 {
			return errStreamProtocol
		}
		usage, err := parseStartUsage(message["usage"][0])
		if err != nil {
			return err
		}
		result.Usage = usage
		result.Started = true
		return nil
	case "message_delta":
		if !result.Started || len(fields["usage"]) != 1 {
			return errStreamProtocol
		}
		usageFields, err := collectFields(fields["usage"][0])
		if err != nil || len(usageFields["output_tokens"]) != 1 {
			return errStreamProtocol
		}
		output, err := nonnegativeInteger(usageFields["output_tokens"][0])
		if err != nil || (result.UsageFound && output < result.Usage.CompletionTokens) {
			return errStreamProtocol
		}
		result.Usage.CompletionTokens = output
		result.UsageFound = true
		return nil
	case "message_stop":
		if !result.Started || !result.UsageFound {
			return errStreamProtocol
		}
		result.Terminal = true
		result.TerminalCategory = "complete"
		result.TerminalDigest = sha256.Sum256(data)
		return nil
	case "content_block_start", "content_block_delta", "content_block_stop":
		if !result.Started {
			return errStreamProtocol
		}
		return nil
	default:
		return nil
	}
}

func parseStartUsage(raw []byte) (chatpricing.Usage, error) {
	fields, err := collectFields(raw)
	if err != nil || len(fields["input_tokens"]) != 1 {
		return chatpricing.Usage{}, errStreamProtocol
	}
	input, err := nonnegativeInteger(fields["input_tokens"][0])
	if err != nil {
		return chatpricing.Usage{}, errStreamProtocol
	}
	read, write := int64(0), int64(0)
	if values := fields["cache_read_input_tokens"]; len(values) > 1 {
		return chatpricing.Usage{}, errStreamProtocol
	} else if len(values) == 1 {
		read, err = nonnegativeInteger(values[0])
		if err != nil {
			return chatpricing.Usage{}, errStreamProtocol
		}
	}
	if values := fields["cache_creation_input_tokens"]; len(values) > 1 {
		return chatpricing.Usage{}, errStreamProtocol
	} else if len(values) == 1 {
		write, err = nonnegativeInteger(values[0])
		if err != nil {
			return chatpricing.Usage{}, errStreamProtocol
		}
	}
	if input > int64(^uint64(0)>>1)-read || input+read > int64(^uint64(0)>>1)-write {
		return chatpricing.Usage{}, errStreamProtocol
	}
	return chatpricing.Usage{PromptTokens: input + read + write, CachedInputTokens: read, CacheWriteTokens: write}, nil
}
func nonnegativeInteger(raw []byte) (int64, error) {
	var number json.Number
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&number) != nil {
		return 0, errStreamProtocol
	}
	value, err := number.Int64()
	if err != nil || value < 0 {
		return 0, errStreamProtocol
	}
	return value, nil
}
func collectFields(raw []byte) (map[string][]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errStreamProtocol
	}
	result := map[string][]json.RawMessage{}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, errStreamProtocol
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return nil, errStreamProtocol
		}
		key := keyToken.(string)
		result[key] = append(result[key], value)
	}
	if _, err = decoder.Token(); err != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, errStreamProtocol
	}
	return result, nil
}
