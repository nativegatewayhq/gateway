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
	ErrChatTimeout        = errors.New("chat provider request timed out")
	ErrChatCanceled       = errors.New("chat provider request canceled")
	ErrChatUpstream       = errors.New("chat provider unavailable")
	ErrInvalidChatRequest = errors.New("invalid chat provider request")
	ErrChatStreamIdle     = errors.New("chat provider stream idle timeout")
)

type ChatRequest struct {
	ChannelID, ContentType, Accept, UserAgent string
	ContentLength                             int64
	Body                                      io.Reader
	Streaming                                 bool
}
type ChatExecutor struct {
	origin            *url.URL
	provider          providercredentials.ProviderID
	client            *http.Client
	credentials       *providercredentials.Registry
	timeout           time.Duration
	streamIdleTimeout time.Duration
}

func NewChat(credentials *providercredentials.Registry, timeout time.Duration, streamIdle ...time.Duration) *ChatExecutor {
	return NewChatForProvider(providercredentials.OpenAI, credentials, timeout, streamIdle...)
}
func NewChatForProvider(provider providercredentials.ProviderID, credentials *providercredentials.Registry, timeout time.Duration, streamIdle ...time.Duration) *ChatExecutor {
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext, ForceAttemptHTTP2: true, MaxIdleConns: 100, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: timeout, ExpectContinueTimeout: time.Second}
	executor := NewChatWithClientForProvider(provider, credentials, timeout, &http.Client{Transport: transport})
	if len(streamIdle) > 0 {
		executor.streamIdleTimeout = streamIdle[0]
	}
	return executor
}
func NewChatWithClient(credentials *providercredentials.Registry, timeout time.Duration, client *http.Client) *ChatExecutor {
	return NewChatWithClientForProvider(providercredentials.OpenAI, credentials, timeout, client)
}
func NewChatWithClientForProvider(provider providercredentials.ProviderID, credentials *providercredentials.Registry, timeout time.Duration, client *http.Client) *ChatExecutor {
	originText := "https://api.openai.com"
	if provider == providercredentials.XAI {
		originText = "https://api.x.ai"
	} else if provider != providercredentials.OpenAI {
		panic("invalid OpenAI-protocol Chat provider")
	}
	origin, err := url.Parse(originText)
	if err != nil || origin.Scheme != "https" || origin.Host == "" {
		panic("invalid trusted OpenAI origin")
	}
	if client == nil {
		client = &http.Client{Transport: http.DefaultTransport}
	}
	copy := *client
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &ChatExecutor{origin: origin, provider: provider, client: &copy, credentials: credentials, timeout: timeout, streamIdleTimeout: 30 * time.Second}
}
func (e *ChatExecutor) Complete(ctx context.Context, input ChatRequest) (*http.Response, error) {
	if e == nil || e.origin == nil || e.timeout <= 0 {
		return nil, ErrInvalidChatRequest
	}
	target := *e.origin
	target.Path = "/v1/chat/completions"
	target.RawPath = ""
	target.RawQuery = ""
	target.Fragment = ""
	requestCtx, cancel := context.WithTimeout(ctx, e.timeout)
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, target.String(), input.Body)
	if err != nil {
		cancel()
		return nil, ErrChatUpstream
	}
	if input.ContentLength > 0 {
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
	request, err = providercredentials.PrepareOutboundChannel(request, input.ChannelID, e.provider, e.credentials)
	if err != nil {
		cancel()
		return nil, err
	}
	response, err := e.client.Do(request)
	providercredentials.ClearApplied(request)
	if err != nil {
		cause := requestCtx.Err()
		cancel()
		switch {
		case errors.Is(err, context.DeadlineExceeded) || errors.Is(cause, context.DeadlineExceeded):
			return nil, ErrChatTimeout
		case errors.Is(err, context.Canceled) || errors.Is(cause, context.Canceled):
			return nil, ErrChatCanceled
		default:
			return nil, ErrChatUpstream
		}
	}
	response.Body = &chatCancelBody{ReadCloser: response.Body, cancel: cancel}
	if input.Streaming {
		response.Body = &idleReadCloser{ReadCloser: response.Body, timeout: e.streamIdleTimeout}
	}
	return response, nil
}

type chatCancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *chatCancelBody) Close() error { err := b.ReadCloser.Close(); b.cancel(); return err }

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
	go func() { n, err := reader.ReadCloser.Read(buffer); result <- idleReadResult{n: n, err: err} }()
	timer := time.NewTimer(reader.timeout)
	defer timer.Stop()
	select {
	case value := <-result:
		return value.n, value.err
	case <-timer.C:
		_ = reader.ReadCloser.Close()
		<-result
		return 0, ErrChatStreamIdle
	}
}
