package openai

import (
	"errors"
	"io"
	"testing"
	"time"
)

func TestStreamingBodyIdleTimeoutClosesBlockedRead(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	body := &idleReadCloser{ReadCloser: reader, timeout: 10 * time.Millisecond}
	started := time.Now()
	_, err := body.Read(make([]byte, 1))
	if !errors.Is(err, ErrChatStreamIdle) {
		t.Fatalf("err=%v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("idle timeout did not unblock")
	}
}
