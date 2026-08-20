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

var (
	errStreamProtocol = errors.New("invalid SSE stream")
	errStreamWrite    = errors.New("SSE client write failed")
)

type streamResult struct {
	Usage            chatpricing.Usage
	TerminalDigest   [32]byte
	Done, UsageFound bool
	FirstByte        time.Duration
}

func relayNativeStream(w http.ResponseWriter, source io.Reader, maximumEventBytes int64, observeUsage bool) (streamResult, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return streamResult{}, errStreamWrite
	}
	reader := bufio.NewReaderSize(source, 32*1024)
	var result streamResult
	var event bytes.Buffer
	var observed int64
	started := time.Now()
	for {
		line, err := reader.ReadString('\n')
		observed += int64(len(line))
		if observed > maximumEventBytes || int64(len(line))+int64(event.Len()) > maximumEventBytes {
			return result, errStreamProtocol
		}
		if len(line) > 0 {
			if _, writeErr := io.WriteString(w, line); writeErr != nil {
				return result, errStreamWrite
			}
			if result.FirstByte == 0 {
				result.FirstByte = time.Since(started)
			}
			flusher.Flush()
			trimmed := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
			if trimmed == "" {
				if observeUsage {
					if processErr := observeSSEEvent(event.Bytes(), &result); processErr != nil {
						return result, processErr
					}
				}
				event.Reset()
			} else {
				event.WriteString(trimmed)
				event.WriteByte('\n')
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if event.Len() > 0 && observeUsage {
					if processErr := observeSSEEvent(event.Bytes(), &result); processErr != nil {
						return result, processErr
					}
				}
				return result, nil
			}
			return result, err
		}
	}
}

func observeSSEEvent(event []byte, result *streamResult) error {
	var dataLines []string
	for _, line := range strings.Split(strings.TrimSuffix(string(event), "\n"), "\n") {
		if strings.HasPrefix(line, ":") {
			continue
		}
		if line == "data" {
			dataLines = append(dataLines, "")
			continue
		}
		if strings.HasPrefix(line, "data:") {
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
	data := strings.Join(dataLines, "\n")
	if data == "[DONE]" {
		if result.Done {
			return errStreamProtocol
		}
		result.Done = true
		return nil
	}
	var probe struct {
		Usage json.RawMessage `json:"usage"`
	}
	if json.Unmarshal([]byte(data), &probe) != nil {
		return errStreamProtocol
	}
	if len(probe.Usage) == 0 || bytes.Equal(probe.Usage, []byte("null")) {
		return nil
	}
	if result.UsageFound {
		return errStreamProtocol
	}
	usage, err := extractChatUsage([]byte(data))
	if err != nil {
		return errStreamProtocol
	}
	result.Usage, result.UsageFound, result.TerminalDigest = usage, true, sha256.Sum256([]byte(data))
	return nil
}

func streamingUsageRequested(body []byte) (bool, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return false, errStreamProtocol
	}
	count := 0
	var raw json.RawMessage
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return false, errStreamProtocol
		}
		key := keyToken.(string)
		if key == "stream_options" {
			count++
			if decoder.Decode(&raw) != nil {
				return false, errStreamProtocol
			}
		} else {
			var skip json.RawMessage
			if decoder.Decode(&skip) != nil {
				return false, errStreamProtocol
			}
		}
	}
	if _, err = decoder.Token(); err != nil || count != 1 {
		return false, errStreamProtocol
	}
	inner := json.NewDecoder(bytes.NewReader(raw))
	token, err = inner.Token()
	if err != nil || token != json.Delim('{') {
		return false, errStreamProtocol
	}
	includeCount := 0
	include := false
	for inner.More() {
		k, _ := inner.Token()
		if k.(string) == "include_usage" {
			includeCount++
			if inner.Decode(&include) != nil {
				return false, errStreamProtocol
			}
		} else {
			var skip json.RawMessage
			if inner.Decode(&skip) != nil {
				return false, errStreamProtocol
			}
		}
	}
	if _, err = inner.Token(); err != nil || includeCount != 1 || !include {
		return false, errStreamProtocol
	}
	return true, nil
}
