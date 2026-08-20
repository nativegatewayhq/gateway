package imagestorage

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
	"strings"
	"time"
)

type S3Store struct {
	endpoint, cdn        *url.URL
	region, bucket       string
	accessKey, secretKey string
	client               *http.Client
	now                  func() time.Time
	uploadTimeout        time.Duration
}

func NewS3(config Config) (*S3Store, error) {
	if err := config.Validate(); err != nil || config.Mode != Managed {
		return nil, ErrInvalidConfig
	}
	endpoint, _ := url.Parse(config.Endpoint)
	cdn, _ := url.Parse(config.CDNBaseURL)
	return &S3Store{endpoint: endpoint, cdn: cdn, region: config.Region, bucket: config.Bucket, accessKey: config.AccessKeyID, secretKey: config.SecretAccessKey, client: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, now: time.Now, uploadTimeout: config.UploadTimeout}, nil
}

func (store *S3Store) Put(ctx context.Context, object Object, body io.Reader) (StoredObject, error) {
	if object.Key == "" || strings.HasPrefix(object.Key, "/") || strings.Contains(object.Key, "..") || object.Size < 0 || strings.TrimSpace(object.ContentType) == "" || body == nil {
		return StoredObject{}, ErrInvalidObject
	}
	ctx, cancel := context.WithTimeout(ctx, store.uploadTimeout)
	defer cancel()
	target := *store.endpoint
	target.Path = path.Join(store.endpoint.Path, store.bucket, object.Key)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, target.String(), io.LimitReader(body, object.Size))
	if err != nil {
		return StoredObject{}, ErrUnavailable
	}
	req.ContentLength = object.Size
	req.Header.Set("Content-Type", object.ContentType)
	req.Header.Set("x-amz-meta-sha256", hex.EncodeToString(object.SHA256[:]))
	store.sign(req, hex.EncodeToString(object.SHA256[:]), store.now().UTC())
	response, err := store.client.Do(req)
	if err != nil {
		return StoredObject{}, ErrUnavailable
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return StoredObject{}, ErrUnavailable
	}
	public, err := publicURL(store.cdn.String(), object.Key)
	if err != nil {
		return StoredObject{}, err
	}
	return StoredObject{Key: object.Key, URL: public, ContentType: object.ContentType, Size: object.Size, SHA256: object.SHA256}, nil
}

func (store *S3Store) Ready(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, store.uploadTimeout)
	defer cancel()
	target := *store.endpoint
	target.Path = path.Join(store.endpoint.Path, store.bucket)
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, target.String(), nil)
	if err != nil {
		return ErrUnavailable
	}
	empty := sha256.Sum256(nil)
	store.sign(req, hex.EncodeToString(empty[:]), store.now().UTC())
	response, err := store.client.Do(req)
	if err != nil {
		return ErrUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return ErrUnavailable
	}
	return nil
}

func (store *S3Store) sign(request *http.Request, payloadHash string, now time.Time) {
	date := now.Format("20060102")
	amzDate := now.Format("20060102T150405Z")
	request.Header.Set("x-amz-content-sha256", payloadHash)
	request.Header.Set("x-amz-date", amzDate)
	canonicalURI := request.URL.EscapedPath()
	host := request.Host
	if host == "" {
		host = request.URL.Host
	}
	canonicalHeaders := "host:" + host + "\n" + "x-amz-content-sha256:" + payloadHash + "\n" + "x-amz-date:" + amzDate + "\n"
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonical := strings.Join([]string{request.Method, canonicalURI, request.URL.Query().Encode(), canonicalHeaders, signedHeaders, payloadHash}, "\n")
	canonicalHash := sha256.Sum256([]byte(canonical))
	scope := date + "/" + store.region + "/s3/aws4_request"
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(canonicalHash[:])
	dateKey := hmacSHA256([]byte("AWS4"+store.secretKey), date)
	regionKey := hmacSHA256(dateKey, store.region)
	serviceKey := hmacSHA256(regionKey, "s3")
	signingKey := hmacSHA256(serviceKey, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(signingKey, toSign))
	request.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s", store.accessKey, scope, signedHeaders, signature))
}

func hmacSHA256(key []byte, value string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = io.WriteString(mac, value)
	return mac.Sum(nil)
}

func IsUnavailable(err error) bool { return errors.Is(err, ErrUnavailable) }
