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
	ErrSpeechTimeout  = errors.New("speech provider request timed out")
	ErrSpeechCanceled = errors.New("speech provider request canceled")
	ErrSpeechUpstream = errors.New("speech provider unavailable")
	ErrInvalidSpeech  = errors.New("invalid speech provider request")
)

type SpeechRequest struct {
	ChannelID, ContentType, Accept, UserAgent string
	ContentLength                             int64
	Body                                      io.Reader
}

type SpeechExecutor struct {
	origin      *url.URL
	client      *http.Client
	credentials *providercredentials.Registry
	timeout     time.Duration
	idleTimeout time.Duration
}

func NewSpeech(credentials *providercredentials.Registry, timeout, idleTimeout time.Duration) *SpeechExecutor {
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext, ForceAttemptHTTP2: true, MaxIdleConns: 100, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: timeout, ExpectContinueTimeout: time.Second}
	return NewSpeechWithClient(credentials, timeout, idleTimeout, &http.Client{Transport: transport})
}

func NewSpeechWithClient(credentials *providercredentials.Registry, timeout, idleTimeout time.Duration, client *http.Client) *SpeechExecutor {
	origin, _ := url.Parse("https://api.openai.com")
	return &SpeechExecutor{origin: origin, client: client, credentials: credentials, timeout: timeout, idleTimeout: idleTimeout}
}

func (executor *SpeechExecutor) Create(ctx context.Context, input SpeechRequest) (*http.Response, error) {
	if executor == nil || executor.origin == nil || executor.client == nil || executor.credentials == nil || executor.timeout <= 0 || executor.idleTimeout <= 0 || input.ChannelID == "" || input.Body == nil {
		return nil, ErrInvalidSpeech
	}
	target := *executor.origin
	target.Path = "/v1/audio/speech"
	target.RawPath, target.RawQuery, target.Fragment = "", "", ""
	requestContext, cancel := context.WithTimeout(ctx, executor.timeout)
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, target.String(), input.Body)
	if err != nil {
		cancel()
		return nil, ErrSpeechUpstream
	}
	request.ContentLength = input.ContentLength
	request.Header.Set("Content-Type", input.ContentType)
	if input.Accept != "" {
		request.Header.Set("Accept", input.Accept)
	}
	if input.UserAgent != "" {
		request.Header.Set("User-Agent", input.UserAgent)
	}
	request, err = providercredentials.PrepareOutboundChannel(request, input.ChannelID, providercredentials.OpenAI, executor.credentials)
	if err != nil {
		cancel()
		return nil, err
	}
	response, err := executor.client.Do(request)
	providercredentials.ClearApplied(request)
	if err != nil {
		cause := requestContext.Err()
		cancel()
		switch {
		case errors.Is(err, context.DeadlineExceeded) || errors.Is(cause, context.DeadlineExceeded):
			return nil, ErrSpeechTimeout
		case errors.Is(err, context.Canceled) || errors.Is(cause, context.Canceled):
			return nil, ErrSpeechCanceled
		default:
			return nil, ErrSpeechUpstream
		}
	}
	response.Body = &chatCancelBody{ReadCloser: &idleReadCloser{ReadCloser: response.Body, timeout: executor.idleTimeout}, cancel: cancel}
	return response, nil
}
