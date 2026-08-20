// Package anthropic implements the trusted Claude API Messages transport.
package anthropic

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

const defaultOrigin = "https://api.anthropic.com"

var (
	ErrTimeout        = errors.New("Anthropic request timed out")
	ErrCanceled       = errors.New("Anthropic request canceled")
	ErrUpstream       = errors.New("Anthropic upstream unavailable")
	ErrInvalidRequest = errors.New("invalid Anthropic request")
	ErrStreamIdle     = errors.New("Anthropic stream idle timeout")
)

type MessagesRequest struct {
	ChannelID, ContentType, Accept, UserAgent, Version string
	Beta                                               []string
	ContentLength                                      int64
	Body                                               io.Reader
	Streaming                                          bool
}

type Executor struct {
	origin      *url.URL
	client      *http.Client
	credentials *providercredentials.Registry
	timeout     time.Duration
	streamIdle  time.Duration
}

func New(credentials *providercredentials.Registry, timeout time.Duration, streamIdle ...time.Duration) *Executor {
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext, ForceAttemptHTTP2: true, MaxIdleConns: 100, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: timeout, ExpectContinueTimeout: time.Second}
	executor := NewWithClient(credentials, timeout, &http.Client{Transport: transport})
	if len(streamIdle) > 0 {
		executor.streamIdle = streamIdle[0]
	}
	return executor
}

func NewWithClient(credentials *providercredentials.Registry, timeout time.Duration, client *http.Client) *Executor {
	origin, _ := url.Parse(defaultOrigin)
	if client == nil {
		client = &http.Client{Transport: http.DefaultTransport}
	}
	copy := *client
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return newExecutor(origin, &copy, credentials, timeout)
}

func newExecutor(origin *url.URL, client *http.Client, credentials *providercredentials.Registry, timeout time.Duration) *Executor {
	return &Executor{origin: origin, client: client, credentials: credentials, timeout: timeout, streamIdle: 30 * time.Second}
}

func (executor *Executor) CreateMessage(ctx context.Context, input MessagesRequest) (*http.Response, error) {
	if executor == nil || executor.origin == nil || executor.client == nil || executor.timeout <= 0 || input.ChannelID == "" || input.Version == "" {
		return nil, ErrInvalidRequest
	}
	target := *executor.origin
	target.User, target.Fragment, target.RawQuery, target.RawPath = nil, "", "", ""
	target.Path = "/v1/messages"
	var requestContext context.Context
	var cancel context.CancelFunc
	if input.Streaming {
		requestContext, cancel = context.WithCancel(ctx)
	} else {
		requestContext, cancel = context.WithTimeout(ctx, executor.timeout)
	}
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, target.String(), input.Body)
	if err != nil {
		cancel()
		return nil, ErrUpstream
	}
	if input.ContentLength >= 0 {
		request.ContentLength = input.ContentLength
	}
	if input.ContentType != "" {
		request.Header.Set("Content-Type", input.ContentType)
	}
	if input.Accept != "" {
		request.Header.Set("Accept", input.Accept)
	}
	if input.UserAgent != "" {
		request.Header.Set("User-Agent", input.UserAgent)
	}
	request.Header.Set("anthropic-version", input.Version)
	for _, value := range input.Beta {
		request.Header.Add("anthropic-beta", value)
	}
	request, err = providercredentials.PrepareOutboundChannel(request, input.ChannelID, providercredentials.Anthropic, executor.credentials)
	if err != nil {
		cancel()
		return nil, err
	}
	response, err := executor.client.Do(request)
	providercredentials.ClearApplied(request)
	if err != nil {
		cause := requestContext.Err()
		cancel()
		var networkError net.Error
		switch {
		case errors.Is(err, context.DeadlineExceeded), errors.Is(cause, context.DeadlineExceeded), errors.As(err, &networkError) && networkError.Timeout():
			return nil, ErrTimeout
		case errors.Is(err, context.Canceled), errors.Is(cause, context.Canceled):
			return nil, ErrCanceled
		default:
			return nil, ErrUpstream
		}
	}
	response.Body = &cancelOnClose{ReadCloser: response.Body, cancel: cancel}
	if input.Streaming {
		response.Body = &idleReadCloser{ReadCloser: response.Body, timeout: executor.streamIdle}
	}
	return response, nil
}

type idleReadCloser struct {
	io.ReadCloser
	timeout time.Duration
}
type idleReadResult struct {
	n   int
	err error
}

func (reader *idleReadCloser) Read(buffer []byte) (int, error) {
	if reader.timeout <= 0 {
		return reader.ReadCloser.Read(buffer)
	}
	result := make(chan idleReadResult, 1)
	go func() { n, err := reader.ReadCloser.Read(buffer); result <- idleReadResult{n, err} }()
	timer := time.NewTimer(reader.timeout)
	defer timer.Stop()
	select {
	case value := <-result:
		return value.n, value.err
	case <-timer.C:
		_ = reader.ReadCloser.Close()
		<-result
		return 0, ErrStreamIdle
	}
}

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (body *cancelOnClose) Close() error {
	err := body.ReadCloser.Close()
	body.cancel()
	return err
}
