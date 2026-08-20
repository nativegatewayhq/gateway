// Package openaiimages implements the shared trusted transport used by
// providers exposing the OpenAI Images HTTP protocol.
package openaiimages

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/nativegatewayhq/gateway/internal/providercredentials"
)

var (
	ErrTimeout        = errors.New("image provider request timed out")
	ErrCanceled       = errors.New("image provider request canceled")
	ErrUpstream       = errors.New("image provider unavailable")
	ErrInvalidRequest = errors.New("invalid image provider request")
)

type Request struct {
	ContentType string
	Accept      string
	UserAgent   string
	Body        io.Reader
}

type Executor struct {
	origin      *url.URL
	provider    providercredentials.ProviderID
	client      *http.Client
	credentials *providercredentials.Registry
	timeout     time.Duration
}

func New(provider providercredentials.ProviderID, credentials *providercredentials.Registry, timeout time.Duration) *Executor {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: timeout,
		ExpectContinueTimeout: time.Second,
	}
	return NewWithClient(provider, credentials, timeout, &http.Client{Transport: transport})
}

func NewWithClient(provider providercredentials.ProviderID, credentials *providercredentials.Registry, timeout time.Duration, client *http.Client) *Executor {
	origin, validProvider := originForProvider(provider)
	if !validProvider {
		return &Executor{provider: provider, credentials: credentials, timeout: timeout}
	}
	parsedOrigin, err := url.Parse(origin)
	if err != nil || parsedOrigin.Scheme != "https" || parsedOrigin.Host == "" || parsedOrigin.User != nil || parsedOrigin.RawQuery != "" || parsedOrigin.Fragment != "" {
		panic("invalid trusted image provider origin")
	}
	if client == nil {
		client = &http.Client{Transport: http.DefaultTransport}
	}
	clientCopy := *client
	clientCopy.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return &Executor{origin: parsedOrigin, provider: provider, client: &clientCopy, credentials: credentials, timeout: timeout}
}

func originForProvider(provider providercredentials.ProviderID) (string, bool) {
	switch provider {
	case providercredentials.OpenAI:
		return "https://api.openai.com", true
	case providercredentials.XAI:
		return "https://api.x.ai", true
	default:
		return "", false
	}
}

func (executor *Executor) Generate(ctx context.Context, input Request) (*http.Response, error) {
	if executor == nil || executor.origin == nil || (executor.provider != providercredentials.OpenAI && executor.provider != providercredentials.XAI) {
		return nil, ErrInvalidRequest
	}
	requestURL := *executor.origin
	requestURL.User = nil
	requestURL.Fragment = ""
	requestURL.Path = "/v1/images/generations"
	requestURL.RawPath = ""
	requestURL.RawQuery = ""

	requestContext, cancel := context.WithTimeout(ctx, executor.timeout)
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, requestURL.String(), input.Body)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("%w: create request", ErrUpstream)
	}
	request.Host = ""
	request.RequestURI = ""
	if input.ContentType != "" {
		request.Header.Set("Content-Type", input.ContentType)
	}
	if input.Accept != "" {
		request.Header.Set("Accept", input.Accept)
	}
	if input.UserAgent != "" {
		request.Header.Set("User-Agent", input.UserAgent)
	}
	request, err = providercredentials.PrepareOutbound(request, executor.provider, executor.credentials)
	if err != nil {
		cancel()
		return nil, err
	}

	response, err := executor.client.Do(request)
	if err != nil {
		contextError := requestContext.Err()
		cancel()
		switch {
		case errors.Is(err, context.DeadlineExceeded), errors.Is(contextError, context.DeadlineExceeded):
			return nil, ErrTimeout
		case errors.Is(err, context.Canceled), errors.Is(contextError, context.Canceled):
			return nil, ErrCanceled
		default:
			return nil, ErrUpstream
		}
	}
	response.Body = &cancelOnClose{ReadCloser: response.Body, cancel: cancel}
	return response, nil
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
