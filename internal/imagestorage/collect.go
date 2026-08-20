package imagestorage

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

var (
	ErrFetchRejected  = errors.New("provider asset fetch rejected")
	ErrFetchFailed    = errors.New("provider asset fetch failed")
	ErrInvalidContent = errors.New("invalid provider image content")
	ErrTooLarge       = errors.New("provider image content too large")
)

type resolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

type Collector struct {
	config   Config
	resolver resolver
	dialer   net.Dialer
}

type Collected struct {
	File        *os.File
	Size        int64
	SHA256      [sha256.Size]byte
	ContentType string
	Extension   string
}

func NewCollector(config Config) (*Collector, error) {
	if err := config.Validate(); err != nil || config.Mode != Managed {
		return nil, ErrInvalidConfig
	}
	return &Collector{config: config, resolver: net.DefaultResolver}, nil
}

func (collector *Collector) Fetch(ctx context.Context, provider, rawURL string) (*Collected, error) {
	target, addresses, err := collector.authorize(ctx, provider, rawURL)
	if err != nil {
		return nil, err
	}
	port := target.Port()
	if port == "" {
		port = "443"
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DisableKeepAlives:     true,
		TLSHandshakeTimeout:   collector.config.FetchTimeout,
		ResponseHeaderTimeout: collector.config.FetchTimeout,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var last error
			for _, address := range addresses {
				connection, dialErr := collector.dialer.DialContext(ctx, network, net.JoinHostPort(address.String(), port))
				if dialErr == nil {
					return connection, nil
				}
				last = dialErr
			}
			return nil, last
		},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: collector.config.FetchTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, ErrFetchRejected
	}
	request.Header.Set("Accept", "image/png,image/jpeg,image/webp,image/gif")
	response, err := client.Do(request)
	if err != nil {
		return nil, ErrFetchFailed
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return nil, ErrFetchFailed
	}
	if response.ContentLength > collector.config.MaximumImageBytes {
		return nil, ErrTooLarge
	}
	return collector.collect(response.Body, response.Header.Get("Content-Type"))
}

func (collector *Collector) DecodeBase64(value, declaredContentType string) (*Collected, error) {
	maximumEncoded := base64.StdEncoding.EncodedLen(int(collector.config.MaximumImageBytes))
	if len(value) > maximumEncoded+2 {
		return nil, ErrTooLarge
	}
	return collector.collect(base64.NewDecoder(base64.StdEncoding, strings.NewReader(value)), declaredContentType)
}

func (collector *Collector) collect(reader io.Reader, declaredContentType string) (_ *Collected, returnedErr error) {
	directory := collector.config.TemporaryDirectory
	if directory != "" {
		directory = filepath.Clean(directory)
	}
	file, err := os.CreateTemp(directory, "nativegateway-image-*")
	if err != nil {
		return nil, ErrUnavailable
	}
	defer func() {
		if returnedErr != nil {
			_ = file.Close()
			_ = os.Remove(file.Name())
		}
	}()
	hasher := sha256.New()
	limited := &io.LimitedReader{R: reader, N: collector.config.MaximumImageBytes + 1}
	written, err := io.Copy(io.MultiWriter(file, hasher), limited)
	if err != nil {
		return nil, ErrFetchFailed
	}
	if written > collector.config.MaximumImageBytes {
		return nil, ErrTooLarge
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, ErrUnavailable
	}
	buffered := bufio.NewReader(file)
	header, _ := buffered.Peek(512)
	detected := http.DetectContentType(header)
	contentType, extension, err := validateImageType(declaredContentType, detected)
	if err != nil {
		return nil, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, ErrUnavailable
	}
	var digest [sha256.Size]byte
	copy(digest[:], hasher.Sum(nil))
	return &Collected{File: file, Size: written, SHA256: digest, ContentType: contentType, Extension: extension}, nil
}

func (collector *Collector) authorize(ctx context.Context, provider, raw string) (*url.URL, []netip.Addr, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || net.ParseIP(parsed.Hostname()) != nil {
		return nil, nil, ErrFetchRejected
	}
	origin := parsed.Scheme + "://" + parsed.Host
	allowed := false
	for _, candidate := range collector.config.FetchOrigins[provider] {
		if origin == candidate {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, nil, ErrFetchRejected
	}
	addresses, err := collector.resolver.LookupNetIP(ctx, "ip", parsed.Hostname())
	if err != nil || len(addresses) == 0 {
		return nil, nil, ErrFetchFailed
	}
	for index := range addresses {
		addresses[index] = addresses[index].Unmap()
		if unsafeAddress(addresses[index]) {
			return nil, nil, ErrFetchRejected
		}
	}
	return parsed, addresses, nil
}

func unsafeAddress(address netip.Addr) bool {
	return !address.IsValid() || !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() || address.IsMulticast() || address.IsUnspecified()
}

func validateImageType(declared, detected string) (string, string, error) {
	if declared != "" {
		parsed, _, err := mime.ParseMediaType(declared)
		if err != nil {
			return "", "", ErrInvalidContent
		}
		declared = parsed
	}
	types := map[string]string{"image/png": "png", "image/jpeg": "jpg", "image/webp": "webp", "image/gif": "gif"}
	extension, ok := types[detected]
	if !ok || (declared != "" && declared != detected) {
		return "", "", ErrInvalidContent
	}
	return detected, extension, nil
}

func (collected *Collected) Close() error {
	if collected == nil || collected.File == nil {
		return nil
	}
	name := collected.File.Name()
	closeErr := collected.File.Close()
	removeErr := os.Remove(name)
	if closeErr != nil {
		return closeErr
	}
	if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return fmt.Errorf("remove collected image: %w", removeErr)
	}
	return nil
}
