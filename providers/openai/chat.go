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
)

type ChatRequest struct {
	ChannelID, ContentType, Accept, UserAgent string
	ContentLength                             int64
	Body                                      io.Reader
}
type ChatExecutor struct {
	origin      *url.URL
	client      *http.Client
	credentials *providercredentials.Registry
	timeout     time.Duration
}

func NewChat(credentials *providercredentials.Registry, timeout time.Duration) *ChatExecutor {
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext, ForceAttemptHTTP2: true, MaxIdleConns: 100, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: timeout, ExpectContinueTimeout: time.Second}
	return NewChatWithClient(credentials, timeout, &http.Client{Transport: transport})
}
func NewChatWithClient(credentials *providercredentials.Registry, timeout time.Duration, client *http.Client) *ChatExecutor {
	origin, err := url.Parse("https://api.openai.com")
	if err != nil || origin.Scheme != "https" || origin.Host == "" {
		panic("invalid trusted OpenAI origin")
	}
	if client == nil {
		client = &http.Client{Transport: http.DefaultTransport}
	}
	copy := *client
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &ChatExecutor{origin: origin, client: &copy, credentials: credentials, timeout: timeout}
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
	return response, nil
}

type chatCancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *chatCancelBody) Close() error { err := b.ReadCloser.Close(); b.cancel(); return err }
