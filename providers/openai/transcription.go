package openai

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/nativegatewayhq/gateway/internal/providercredentials"
)

var (
	ErrTranscriptionTimeout  = errors.New("transcription provider request timed out")
	ErrTranscriptionCanceled = errors.New("transcription provider request canceled")
	ErrTranscriptionUpstream = errors.New("transcription provider unavailable")
	ErrInvalidTranscription  = errors.New("invalid transcription provider request")
)

type TranscriptionRequest struct {
	ChannelID, ContentType, Accept, UserAgent string
	ContentLength                             int64
	Body                                      io.Reader
}
type TranscriptionExecutor struct {
	origin               *url.URL
	client               *http.Client
	credentials          *providercredentials.Registry
	timeout, idleTimeout time.Duration
}

func NewTranscription(credentials *providercredentials.Registry, timeout, idleTimeout time.Duration) *TranscriptionExecutor {
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext, ForceAttemptHTTP2: true, MaxIdleConns: 100, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: timeout, ExpectContinueTimeout: time.Second}
	return NewTranscriptionWithClient(credentials, timeout, idleTimeout, &http.Client{Transport: transport})
}
func NewTranscriptionWithClient(credentials *providercredentials.Registry, timeout, idleTimeout time.Duration, client *http.Client) *TranscriptionExecutor {
	origin, _ := url.Parse("https://api.openai.com")
	return &TranscriptionExecutor{origin: origin, client: client, credentials: credentials, timeout: timeout, idleTimeout: idleTimeout}
}
func (e *TranscriptionExecutor) Create(ctx context.Context, input TranscriptionRequest) (*http.Response, error) {
	if e == nil || e.origin == nil || e.client == nil || e.credentials == nil || e.timeout <= 0 || e.idleTimeout <= 0 || input.ChannelID == "" || input.Body == nil || input.ContentLength < 0 {
		return nil, ErrInvalidTranscription
	}
	target := *e.origin
	target.Path = "/v1/audio/transcriptions"
	target.RawPath, target.RawQuery, target.Fragment = "", "", ""
	requestContext, cancel := context.WithTimeout(ctx, e.timeout)
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, target.String(), input.Body)
	if err != nil {
		cancel()
		return nil, ErrTranscriptionUpstream
	}
	request.ContentLength = input.ContentLength
	request.Header.Set("Content-Type", input.ContentType)
	if input.Accept != "" {
		request.Header.Set("Accept", input.Accept)
	}
	if input.UserAgent != "" {
		request.Header.Set("User-Agent", input.UserAgent)
	}
	request, err = providercredentials.PrepareOutboundChannel(request, input.ChannelID, providercredentials.OpenAI, e.credentials)
	if err != nil {
		cancel()
		return nil, err
	}
	response, err := e.client.Do(request)
	providercredentials.ClearApplied(request)
	if err != nil {
		cause := requestContext.Err()
		cancel()
		switch {
		case errors.Is(err, context.DeadlineExceeded) || errors.Is(cause, context.DeadlineExceeded):
			return nil, ErrTranscriptionTimeout
		case errors.Is(err, context.Canceled) || errors.Is(cause, context.Canceled):
			return nil, ErrTranscriptionCanceled
		default:
			return nil, ErrTranscriptionUpstream
		}
	}
	response.Body = &chatCancelBody{ReadCloser: &idleReadCloser{ReadCloser: response.Body, timeout: e.idleTimeout}, cancel: cancel}
	return response, nil
}
