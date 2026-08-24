package audioassets

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"
)

var ErrStorage = errors.New("audio asset storage unavailable")

type ObjectStore interface {
	Put(context.Context, string, string, int64, [32]byte, io.Reader) error
	Get(context.Context, string, int64) (io.ReadCloser, error)
	Delete(context.Context, string) error
	Ready(context.Context) error
}

type S3Config struct {
	Endpoint, Region, Bucket, AccessKeyID, SecretAccessKey, ServerSideEncryption string
	UploadTimeout, DownloadTimeout                                               time.Duration
}
type S3Store struct {
	endpoint                             *url.URL
	region, bucket, accessKey, secretKey string
	serverSideEncryption                 string
	uploadClient, downloadClient         *http.Client
	now                                  func() time.Time
}

func NewS3(config S3Config) (*S3Store, error) {
	endpoint, err := url.Parse(strings.TrimSpace(config.Endpoint))
	if err != nil || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || !((endpoint.Scheme == "https") || (endpoint.Scheme == "http" && (endpoint.Hostname() == "127.0.0.1" || endpoint.Hostname() == "localhost" || endpoint.Hostname() == "::1"))) || strings.TrimSpace(config.Region) == "" || !safePart(config.Bucket) || strings.TrimSpace(config.AccessKeyID) == "" || strings.TrimSpace(config.SecretAccessKey) == "" || (config.ServerSideEncryption != "" && config.ServerSideEncryption != "AES256") || config.UploadTimeout <= 0 || config.UploadTimeout > 10*time.Minute || config.DownloadTimeout <= 0 || config.DownloadTimeout > 10*time.Minute {
		return nil, ErrInvalid
	}
	noRedirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &S3Store{endpoint: endpoint, region: config.Region, bucket: config.Bucket, accessKey: config.AccessKeyID, secretKey: config.SecretAccessKey, serverSideEncryption: config.ServerSideEncryption, uploadClient: &http.Client{Timeout: config.UploadTimeout, CheckRedirect: noRedirect}, downloadClient: &http.Client{Timeout: config.DownloadTimeout, CheckRedirect: noRedirect}, now: time.Now}, nil
}
func (store *S3Store) Put(ctx context.Context, key, contentType string, size int64, digest [32]byte, body io.Reader) error {
	if !validObjectKey(key) || !strings.HasPrefix(contentType, "audio/") || size < 1 || digest == ([32]byte{}) || body == nil {
		return ErrInvalid
	}
	request, err := store.request(ctx, http.MethodPut, key, io.LimitReader(body, size), hex.EncodeToString(digest[:]))
	if err != nil {
		return err
	}
	request.ContentLength = size
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("x-amz-meta-sha256", hex.EncodeToString(digest[:]))
	request.Header.Set("If-None-Match", "*")
	if store.serverSideEncryption != "" {
		request.Header.Set("x-amz-server-side-encryption", store.serverSideEncryption)
	}
	store.sign(request, hex.EncodeToString(digest[:]), store.now().UTC())
	response, err := store.uploadClient.Do(request)
	if err != nil {
		return ErrStorage
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode == http.StatusPreconditionFailed {
		existing, verifyErr := store.Get(ctx, key, size)
		if verifyErr != nil {
			return ErrStorage
		}
		defer existing.Close()
		hash := sha256.New()
		read, verifyErr := io.Copy(hash, existing)
		if verifyErr != nil || read != size || !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), hex.EncodeToString(digest[:])) {
			return ErrStorage
		}
		return nil
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return ErrStorage
	}
	return nil
}
func (store *S3Store) Get(ctx context.Context, key string, maximum int64) (io.ReadCloser, error) {
	if !validObjectKey(key) || maximum < 1 {
		return nil, ErrInvalid
	}
	empty := sha256.Sum256(nil)
	request, err := store.request(ctx, http.MethodGet, key, nil, hex.EncodeToString(empty[:]))
	if err != nil {
		return nil, err
	}
	response, err := store.downloadClient.Do(request)
	if err != nil {
		return nil, ErrStorage
	}
	if response.StatusCode < 200 || response.StatusCode > 299 || response.ContentLength != maximum {
		response.Body.Close()
		return nil, ErrStorage
	}
	return response.Body, nil
}
func (store *S3Store) Delete(ctx context.Context, key string) error {
	if !validObjectKey(key) {
		return ErrInvalid
	}
	empty := sha256.Sum256(nil)
	request, err := store.request(ctx, http.MethodDelete, key, nil, hex.EncodeToString(empty[:]))
	if err != nil {
		return err
	}
	return store.do(store.uploadClient, request)
}
func (store *S3Store) Ready(ctx context.Context) error {
	empty := sha256.Sum256(nil)
	request, err := store.request(ctx, http.MethodHead, "", nil, hex.EncodeToString(empty[:]))
	if err != nil {
		return err
	}
	return store.do(store.uploadClient, request)
}
func (store *S3Store) request(ctx context.Context, method, key string, body io.Reader, payloadHash string) (*http.Request, error) {
	target := *store.endpoint
	target.Path = path.Join(store.endpoint.Path, store.bucket, key)
	request, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return nil, ErrStorage
	}
	store.sign(request, payloadHash, store.now().UTC())
	return request, nil
}
func (store *S3Store) do(client *http.Client, request *http.Request) error {
	response, err := client.Do(request)
	if err != nil {
		return ErrStorage
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return ErrStorage
	}
	return nil
}
func (store *S3Store) sign(request *http.Request, payloadHash string, now time.Time) {
	date := now.Format("20060102")
	amzDate := now.Format("20060102T150405Z")
	request.Header.Set("x-amz-content-sha256", payloadHash)
	request.Header.Set("x-amz-date", amzDate)
	host := request.Host
	if host == "" {
		host = request.URL.Host
	}
	headers := map[string]string{"host": host}
	for name, values := range request.Header {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "x-amz-") && lower != "authorization" {
			headers[lower] = strings.Join(values, ",")
		}
	}
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	var canonicalBuilder strings.Builder
	for _, name := range names {
		canonicalBuilder.WriteString(name)
		canonicalBuilder.WriteByte(':')
		canonicalBuilder.WriteString(strings.TrimSpace(headers[name]))
		canonicalBuilder.WriteByte('\n')
	}
	canonicalHeaders := canonicalBuilder.String()
	signedHeaders := strings.Join(names, ";")
	canonical := strings.Join([]string{request.Method, request.URL.EscapedPath(), request.URL.Query().Encode(), canonicalHeaders, signedHeaders, payloadHash}, "\n")
	canonicalHash := sha256.Sum256([]byte(canonical))
	scope := date + "/" + store.region + "/s3/aws4_request"
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(canonicalHash[:])
	dateKey := s3HMAC([]byte("AWS4"+store.secretKey), date)
	regionKey := s3HMAC(dateKey, store.region)
	serviceKey := s3HMAC(regionKey, "s3")
	signature := hex.EncodeToString(s3HMAC(s3HMAC(serviceKey, "aws4_request"), toSign))
	request.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s", store.accessKey, scope, signedHeaders, signature))
}
func s3HMAC(key []byte, value string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = io.WriteString(mac, value)
	return mac.Sum(nil)
}
func safePart(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}
