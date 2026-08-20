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
	ErrResponsesTimeout    = errors.New("responses provider request timed out")
	ErrResponsesCanceled   = errors.New("responses provider request canceled")
	ErrResponsesUpstream   = errors.New("responses provider unavailable")
	ErrResponsesStreamIdle = errors.New("responses provider stream idle timeout")
)

type ResponsesRequest struct {
	ChannelID, ContentType, Accept, UserAgent string
	ContentLength                             int64
	Body                                      io.Reader
	Streaming                                 bool
}
type ResponsesExecutor struct {
	origin            *url.URL
	client            *http.Client
	credentials       *providercredentials.Registry
	timeout           time.Duration
	streamIdleTimeout time.Duration
}

func NewResponses(credentials *providercredentials.Registry, timeout time.Duration, streamIdle ...time.Duration) *ResponsesExecutor {
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext, ForceAttemptHTTP2: true, MaxIdleConns: 100, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: timeout, ExpectContinueTimeout: time.Second}
	executor := NewResponsesWithClient(credentials, timeout, &http.Client{Transport: transport})
	if len(streamIdle) > 0 {
		executor.streamIdleTimeout = streamIdle[0]
	}
	return executor
}
func NewResponsesWithClient(credentials *providercredentials.Registry, timeout time.Duration, client *http.Client) *ResponsesExecutor {
	origin, _ := url.Parse("https://api.openai.com")
	copy := *client
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &ResponsesExecutor{origin: origin, client: &copy, credentials: credentials, timeout: timeout, streamIdleTimeout: 30 * time.Second}
}
func (e *ResponsesExecutor) Create(ctx context.Context, input ResponsesRequest) (*http.Response, error) {
	if e == nil || e.timeout <= 0 {
		return nil, ErrResponsesUpstream
	}
	target := *e.origin
	target.Path = "/v1/responses"
	target.RawQuery = ""
	requestCtx, cancel := context.WithTimeout(ctx, e.timeout)
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, target.String(), input.Body)
	if err != nil {
		cancel()
		return nil, ErrResponsesUpstream
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
		cause := requestCtx.Err()
		cancel()
		if errors.Is(cause, context.DeadlineExceeded) {
			return nil, ErrResponsesTimeout
		}
		if errors.Is(cause, context.Canceled) {
			return nil, ErrResponsesCanceled
		}
		return nil, ErrResponsesUpstream
	}
	response.Body = &chatCancelBody{ReadCloser: response.Body, cancel: cancel}
	if input.Streaming {
		response.Body = &responsesIdleReadCloser{ReadCloser: response.Body, timeout: e.streamIdleTimeout}
	}
	return response, nil
}

type responsesIdleReadCloser struct {
	io.ReadCloser
	timeout time.Duration
}

func (reader *responsesIdleReadCloser) Read(buffer []byte) (int, error) {
	if reader.timeout <= 0 {
		return reader.ReadCloser.Read(buffer)
	}
	result := make(chan idleReadResult, 1)
	go func() { n, err := reader.ReadCloser.Read(buffer); result <- idleReadResult{n: n, err: err} }()
	timer := time.NewTimer(reader.timeout)
	defer timer.Stop()
	select {
	case value := <-result:
		return value.n, value.err
	case <-timer.C:
		_ = reader.ReadCloser.Close()
		<-result
		return 0, ErrResponsesStreamIdle
	}
}
