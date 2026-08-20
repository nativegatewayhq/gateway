// Package google implements the trusted Gemini Developer API transport.
package google

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nativegatewayhq/gateway/internal/providercredentials"
)

const defaultOrigin = "https://generativelanguage.googleapis.com"

var (
	ErrTimeout        = errors.New("google request timed out")
	ErrCanceled       = errors.New("google request canceled")
	ErrUpstream       = errors.New("google upstream unavailable")
	ErrInvalidRequest = errors.New("invalid google request")
	ErrStreamIdle     = errors.New("google stream idle timeout")
)

type GenerateContentRequest struct {
	Model       string
	ChannelID   string
	Action      string
	Streaming   bool
	Query       url.Values
	ContentType string
	Accept      string
	UserAgent   string
	APIClient   string
	Body        io.Reader
}

type Executor struct {
	origin      *url.URL
	client      *http.Client
	credentials *providercredentials.Registry
	timeout     time.Duration
	streamIdle  time.Duration
}

func New(credentials *providercredentials.Registry, timeout time.Duration, streamIdle ...time.Duration) *Executor {
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
	executor := NewWithClient(credentials, timeout, &http.Client{Transport: transport})
	if len(streamIdle) > 0 {
		executor.streamIdle = streamIdle[0]
	}
	return executor
}

// NewWithClient preserves the fixed production origin while allowing an
// instrumented transport for tests and deployment-level network policy.
func NewWithClient(credentials *providercredentials.Registry, timeout time.Duration, client *http.Client) *Executor {
	origin, _ := url.Parse(defaultOrigin)
	if client == nil {
		client = &http.Client{Transport: http.DefaultTransport}
	}
	clientCopy := *client
	clientCopy.CheckRedirect = rejectRedirect
	return newExecutor(origin, &clientCopy, credentials, timeout)
}

func newExecutor(origin *url.URL, client *http.Client, credentials *providercredentials.Registry, timeout time.Duration) *Executor {
	return &Executor{origin: origin, client: client, credentials: credentials, timeout: timeout, streamIdle: 30 * time.Second}
}

func rejectRedirect(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }

func (executor *Executor) GenerateContent(ctx context.Context, input GenerateContentRequest) (*http.Response, error) {
	if !validModel(input.Model) {
		return nil, ErrInvalidRequest
	}
	requestURL := *executor.origin
	requestURL.User = nil
	requestURL.Fragment = ""
	action := input.Action
	if action == "" {
		action = "generateContent"
	}
	if action != "generateContent" && action != "streamGenerateContent" {
		return nil, ErrInvalidRequest
	}
	requestURL.Path = strings.TrimSuffix(executor.origin.Path, "/") + "/v1beta/models/" + input.Model + ":" + action
	requestURL.RawPath = ""
	requestURL.RawQuery = cloneQuery(input.Query).Encode()

	var requestContext context.Context
	var cancel context.CancelFunc
	var headerTimer *time.Timer
	var headerTimedOut atomic.Bool
	if input.Streaming {
		requestContext, cancel = context.WithCancel(ctx)
		headerTimer = time.AfterFunc(executor.timeout, func() {
			headerTimedOut.Store(true)
			cancel()
		})
	} else {
		requestContext, cancel = context.WithTimeout(ctx, executor.timeout)
	}
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, requestURL.String(), input.Body)
	if err != nil {
		if headerTimer != nil {
			headerTimer.Stop()
		}
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
	if input.APIClient != "" {
		request.Header.Set("x-goog-api-client", input.APIClient)
	}
	if input.ChannelID == "" {
		request, err = providercredentials.PrepareOutbound(request, providercredentials.Google, executor.credentials)
	} else {
		request, err = providercredentials.PrepareOutboundChannel(request, input.ChannelID, providercredentials.Google, executor.credentials)
	}
	if err != nil {
		if headerTimer != nil {
			headerTimer.Stop()
		}
		cancel()
		return nil, err
	}

	response, err := executor.client.Do(request)
	if headerTimer != nil {
		headerTimer.Stop()
	}
	providercredentials.ClearApplied(request)
	if err != nil {
		contextError := requestContext.Err()
		cancel()
		switch {
		case headerTimedOut.Load(), errors.Is(err, context.DeadlineExceeded), errors.Is(contextError, context.DeadlineExceeded):
			return nil, ErrTimeout
		case errors.Is(err, context.Canceled), errors.Is(contextError, context.Canceled):
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

func validModel(model string) bool {
	if model == "" || len(model) > 200 {
		return false
	}
	for _, character := range model {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '.' || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}

func cloneQuery(input url.Values) url.Values {
	output := make(url.Values, len(input))
	for key, values := range input {
		output[key] = append([]string(nil), values...)
	}
	return output
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

type idleReadCloser struct {
	io.ReadCloser
	timeout time.Duration
}

type idleReadResult struct {
	n   int
	err error
}

func (body *idleReadCloser) Read(buffer []byte) (int, error) {
	if body.timeout <= 0 {
		return body.ReadCloser.Read(buffer)
	}
	result := make(chan idleReadResult, 1)
	go func() {
		n, err := body.ReadCloser.Read(buffer)
		result <- idleReadResult{n: n, err: err}
	}()
	timer := time.NewTimer(body.timeout)
	defer timer.Stop()
	select {
	case value := <-result:
		return value.n, value.err
	case <-timer.C:
		_ = body.ReadCloser.Close()
		<-result
		return 0, ErrStreamIdle
	}
}
