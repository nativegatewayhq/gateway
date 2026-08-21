package videostorage

import (
	"context"
	"crypto/sha256"
	"errors"
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

type resolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

type Collector struct {
	config   Config
	resolver resolver
	dialer   net.Dialer
}

type Collected struct {
	File                   *os.File
	Size                   int64
	SHA256                 [sha256.Size]byte
	ContentType, Extension string
}

func NewCollector(config Config) (*Collector, error) {
	if config.Validate() != nil || config.Mode != Managed {
		return nil, ErrInvalidConfig
	}
	return &Collector{config: config, resolver: net.DefaultResolver}, nil
}

func (collector *Collector) Fetch(ctx context.Context, raw string) (_ *Collected, returnedErr error) {
	target, addresses, err := collector.authorize(ctx, raw)
	if err != nil {
		return nil, err
	}
	port := target.Port()
	if port == "" {
		port = "443"
	}
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, DisableCompression: true, TLSHandshakeTimeout: collector.config.FetchTimeout, ResponseHeaderTimeout: collector.config.FetchTimeout, DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		var last error
		for _, address := range addresses {
			connection, dialErr := collector.dialer.DialContext(ctx, network, net.JoinHostPort(address.String(), port))
			if dialErr == nil {
				return connection, nil
			}
			last = dialErr
		}
		return nil, last
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: collector.config.FetchTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	request.Header.Set("Accept", "video/mp4,video/webm,video/quicktime")
	response, err := client.Do(request)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 || response.ContentLength <= 0 {
		return nil, ErrUnavailable
	}
	if response.ContentLength > collector.config.MaximumVideoBytes {
		return nil, ErrTooLarge
	}
	contentType, extension, err := videoType(response.Header.Get("Content-Type"))
	if err != nil {
		return nil, err
	}
	directory := collector.config.TemporaryDirectory
	if directory != "" {
		directory = filepath.Clean(directory)
	}
	file, err := os.CreateTemp(directory, "nativegateway-video-*")
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
	written, err := io.Copy(io.MultiWriter(file, hasher), io.LimitReader(response.Body, response.ContentLength+1))
	if err != nil || written != response.ContentLength {
		return nil, ErrUnavailable
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return nil, ErrUnavailable
	}
	header := make([]byte, 16)
	read, readErr := io.ReadFull(file, header)
	if readErr != nil && !errors.Is(readErr, io.ErrUnexpectedEOF) {
		return nil, ErrUnavailable
	}
	if !matchesVideoSignature(contentType, header[:read]) {
		return nil, ErrInvalid
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return nil, ErrUnavailable
	}
	var digest [sha256.Size]byte
	copy(digest[:], hasher.Sum(nil))
	return &Collected{File: file, Size: written, SHA256: digest, ContentType: contentType, Extension: extension}, nil
}

func (collector *Collector) authorize(ctx context.Context, raw string) (*url.URL, []netip.Addr, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || parsed.Port() != "" && parsed.Port() != "443" || net.ParseIP(parsed.Hostname()) != nil {
		return nil, nil, ErrFetchRejected
	}
	origin := parsed.Scheme + "://" + parsed.Host
	allowed := false
	for _, candidate := range collector.config.FetchOrigins["runway"] {
		candidateURL, _ := url.Parse(candidate)
		if origin == candidateURL.Scheme+"://"+candidateURL.Host {
			allowed = true
		}
	}
	if !allowed {
		return nil, nil, ErrFetchRejected
	}
	addresses, err := collector.resolver.LookupNetIP(ctx, "ip", parsed.Hostname())
	if err != nil || len(addresses) == 0 {
		return nil, nil, ErrUnavailable
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
	if !address.IsValid() || !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() || address.IsMulticast() || address.IsUnspecified() {
		return true
	}
	for _, raw := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "2001:db8::/32"} {
		if netip.MustParsePrefix(raw).Contains(address) {
			return true
		}
	}
	return false
}

func videoType(value string) (string, string, error) {
	parsed, _, err := mime.ParseMediaType(value)
	if err != nil {
		return "", "", ErrInvalid
	}
	types := map[string]string{"video/mp4": "mp4", "video/webm": "webm", "video/quicktime": "mov"}
	extension, ok := types[parsed]
	if !ok {
		return "", "", ErrInvalid
	}
	return parsed, extension, nil
}

func matchesVideoSignature(contentType string, header []byte) bool {
	switch contentType {
	case "video/mp4", "video/quicktime":
		return len(header) >= 12 && string(header[4:8]) == "ftyp"
	case "video/webm":
		return len(header) >= 4 && header[0] == 0x1a && header[1] == 0x45 && header[2] == 0xdf && header[3] == 0xa3
	default:
		return false
	}
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
		return removeErr
	}
	return nil
}

func validCDNURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil && parsed.Fragment == "" && strings.TrimSpace(value) == value
}
