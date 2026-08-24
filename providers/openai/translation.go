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
	ErrTranslationTimeout  = errors.New("translation provider request timed out")
	ErrTranslationCanceled = errors.New("translation provider request canceled")
	ErrTranslationUpstream = errors.New("translation provider unavailable")
	ErrInvalidTranslation  = errors.New("invalid translation provider request")
)

type TranslationRequest struct {
	ChannelID, ContentType, Accept, UserAgent string
	ContentLength                             int64
	Body                                      io.Reader
}

type TranslationExecutor struct {
	origin      *url.URL
	client      *http.Client
	credentials *providercredentials.Registry
	timeout     time.Duration
}

func NewTranslation(credentials *providercredentials.Registry, timeout time.Duration) *TranslationExecutor {
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext, ForceAttemptHTTP2: true, MaxIdleConns: 100, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: timeout, ExpectContinueTimeout: time.Second}
	return NewTranslationWithClient(credentials, timeout, &http.Client{Transport: transport})
}

func NewTranslationWithClient(credentials *providercredentials.Registry, timeout time.Duration, client *http.Client) *TranslationExecutor {
	origin, _ := url.Parse("https://api.openai.com")
	return &TranslationExecutor{origin: origin, client: client, credentials: credentials, timeout: timeout}
}

func (executor *TranslationExecutor) Create(ctx context.Context, input TranslationRequest) (*http.Response, error) {
	if executor == nil || executor.origin == nil || executor.client == nil || executor.credentials == nil || executor.timeout <= 0 || input.ChannelID == "" || input.Body == nil || input.ContentLength < 0 {
		return nil, ErrInvalidTranslation
	}
	target := *executor.origin
	target.Path = "/v1/audio/translations"
	target.RawPath, target.RawQuery, target.Fragment = "", "", ""
	requestContext, cancel := context.WithTimeout(ctx, executor.timeout)
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, target.String(), input.Body)
	if err != nil {
		cancel()
		return nil, ErrTranslationUpstream
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
			return nil, ErrTranslationTimeout
		case errors.Is(err, context.Canceled) || errors.Is(cause, context.Canceled):
			return nil, ErrTranslationCanceled
		default:
			return nil, ErrTranslationUpstream
		}
	}
	response.Body = &chatCancelBody{ReadCloser: response.Body, cancel: cancel}
	return response, nil
}
