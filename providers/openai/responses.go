package openai

import (
	"context"
	"errors"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	"io"
	"net/http"
	"net/url"
	"time"
)

var (
	ErrResponsesTimeout  = errors.New("responses provider request timed out")
	ErrResponsesCanceled = errors.New("responses provider request canceled")
	ErrResponsesUpstream = errors.New("responses provider unavailable")
)

type ResponsesRequest struct {
	ChannelID, ContentType, Accept, UserAgent string
	ContentLength                             int64
	Body                                      io.Reader
}
type ResponsesExecutor struct {
	origin      *url.URL
	client      *http.Client
	credentials *providercredentials.Registry
	timeout     time.Duration
}

func NewResponses(credentials *providercredentials.Registry, timeout time.Duration) *ResponsesExecutor {
	return NewResponsesWithClient(credentials, timeout, &http.Client{Transport: http.DefaultTransport})
}
func NewResponsesWithClient(credentials *providercredentials.Registry, timeout time.Duration, client *http.Client) *ResponsesExecutor {
	origin, _ := url.Parse("https://api.openai.com")
	copy := *client
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &ResponsesExecutor{origin: origin, client: &copy, credentials: credentials, timeout: timeout}
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
	return response, nil
}
