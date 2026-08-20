package fal

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nativegatewayhq/gateway/internal/jobs"
	"github.com/nativegatewayhq/gateway/internal/telemetry"
	joboperation "github.com/nativegatewayhq/gateway/operations/job"
)

var (
	ErrWebhookSignature = errors.New("invalid fal webhook signature")
	ErrJWKSUnavailable  = errors.New("fal JWKS unavailable")
)

type JWKSConfig struct {
	URL, ExpectedURL  string
	Timeout, CacheTTL time.Duration
	RefreshCooldown   time.Duration
	MaximumBodyBytes  int64
}

type FalJWKSVerifier struct {
	url                  *url.URL
	http                 *http.Client
	cacheTTL, cooldown   time.Duration
	maximumBodyBytes     int64
	now                  func() time.Time
	mu                   sync.Mutex
	keys                 []ed25519.PublicKey
	expires, lastRefresh time.Time
	fetching             chan struct{}
}

func NewFalJWKSVerifier(config JWKSConfig) (*FalJWKSVerifier, error) {
	parsed, err := url.Parse(strings.TrimSpace(config.URL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "/.well-known/jwks.json" || config.Timeout <= 0 || config.Timeout > time.Minute || config.CacheTTL <= 0 || config.CacheTTL > 24*time.Hour || config.RefreshCooldown <= 0 || config.RefreshCooldown > time.Hour || config.MaximumBodyBytes < 1 || config.MaximumBodyBytes > 64*1024 {
		return nil, ErrJWKSUnavailable
	}
	loopback := parsed.Hostname() == "localhost" || parsed.Hostname() == "127.0.0.1" || parsed.Hostname() == "::1"
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && loopback) {
		return nil, ErrJWKSUnavailable
	}
	if config.ExpectedURL != "" && config.URL != config.ExpectedURL && !loopback {
		return nil, ErrJWKSUnavailable
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	return &FalJWKSVerifier{url: parsed, http: &http.Client{Timeout: config.Timeout, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, cacheTTL: config.CacheTTL, cooldown: config.RefreshCooldown, maximumBodyBytes: config.MaximumBodyBytes, now: time.Now}, nil
}

func (verifier *FalJWKSVerifier) Verify(ctx context.Context, requestID, userID, timestamp, signatureHex string, body []byte) error {
	if verifier == nil || requestID == "" || userID == "" || len(requestID) > 200 || len(userID) > 200 || strings.ContainsAny(requestID+userID, "\r\n") {
		return ErrWebhookSignature
	}
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || absDuration(verifier.now().UTC().Sub(time.Unix(seconds, 0).UTC())) > 5*time.Minute {
		return ErrWebhookSignature
	}
	signature, err := hex.DecodeString(signatureHex)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return ErrWebhookSignature
	}
	digest := sha256.Sum256(body)
	message := []byte(requestID + "\n" + userID + "\n" + timestamp + "\n" + hex.EncodeToString(digest[:]))
	keys, err := verifier.load(ctx, false)
	if err != nil {
		return err
	}
	if verifyAny(keys, message, signature) {
		return nil
	}
	keys, err = verifier.load(ctx, true)
	if err != nil {
		return err
	}
	if verifyAny(keys, message, signature) {
		return nil
	}
	return ErrWebhookSignature
}

func (verifier *FalJWKSVerifier) load(ctx context.Context, force bool) ([]ed25519.PublicKey, error) {
	for {
		verifier.mu.Lock()
		now := verifier.now().UTC()
		if len(verifier.keys) > 0 && !now.After(verifier.expires) && (!force || now.Sub(verifier.lastRefresh) < verifier.cooldown) {
			keys := cloneKeys(verifier.keys)
			verifier.mu.Unlock()
			return keys, nil
		}
		if verifier.fetching != nil {
			wait := verifier.fetching
			verifier.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ErrJWKSUnavailable
			case <-wait:
				continue
			}
		}
		verifier.fetching = make(chan struct{})
		wait := verifier.fetching
		verifier.mu.Unlock()

		keys, ttl, err := verifier.fetch(ctx)
		verifier.mu.Lock()
		if err == nil {
			verifier.keys = keys
			verifier.lastRefresh = verifier.now().UTC()
			verifier.expires = verifier.lastRefresh.Add(ttl)
		}
		close(wait)
		verifier.fetching = nil
		cached := cloneKeys(verifier.keys)
		valid := len(cached) > 0 && !verifier.now().UTC().After(verifier.expires)
		verifier.mu.Unlock()
		if err != nil && !valid {
			return nil, ErrJWKSUnavailable
		}
		return cached, nil
	}
}

func (verifier *FalJWKSVerifier) fetch(ctx context.Context) ([]ed25519.PublicKey, time.Duration, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, verifier.url.String(), nil)
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Accept", "application/json")
	response, err := verifier.http.Do(request)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, 0, ErrJWKSUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, verifier.maximumBodyBytes+1))
	if err != nil || int64(len(body)) > verifier.maximumBodyBytes {
		return nil, 0, ErrJWKSUnavailable
	}
	var envelope struct {
		Keys []struct{ KTY, CRV, X, KID string }
	}
	if json.Unmarshal(body, &envelope) != nil || len(envelope.Keys) == 0 || len(envelope.Keys) > 32 {
		return nil, 0, ErrJWKSUnavailable
	}
	seen := map[string]struct{}{}
	keys := make([]ed25519.PublicKey, 0, len(envelope.Keys))
	for _, item := range envelope.Keys {
		if item.KTY != "OKP" || item.CRV != "Ed25519" || item.X == "" || len(item.KID) > 200 {
			return nil, 0, ErrJWKSUnavailable
		}
		if item.KID != "" {
			if _, exists := seen[item.KID]; exists {
				return nil, 0, ErrJWKSUnavailable
			}
			seen[item.KID] = struct{}{}
		}
		decoded, err := base64.RawURLEncoding.DecodeString(item.X)
		if err != nil || len(decoded) != ed25519.PublicKeySize {
			return nil, 0, ErrJWKSUnavailable
		}
		keys = append(keys, ed25519.PublicKey(append([]byte(nil), decoded...)))
	}
	ttl := verifier.cacheTTL
	for _, directive := range strings.Split(response.Header.Get("Cache-Control"), ",") {
		directive = strings.TrimSpace(directive)
		if strings.HasPrefix(directive, "max-age=") {
			seconds, err := strconv.Atoi(strings.TrimPrefix(directive, "max-age="))
			if err == nil && seconds > 0 && time.Duration(seconds)*time.Second < ttl {
				ttl = time.Duration(seconds) * time.Second
			}
		}
	}
	return keys, ttl, nil
}

func verifyAny(keys []ed25519.PublicKey, message, signature []byte) bool {
	for _, key := range keys {
		if ed25519.Verify(key, message, signature) {
			return true
		}
	}
	return false
}
func cloneKeys(values []ed25519.PublicKey) []ed25519.PublicKey {
	result := make([]ed25519.PublicKey, len(values))
	for index, key := range values {
		result[index] = append(ed25519.PublicKey(nil), key...)
	}
	return result
}
func absDuration(value time.Duration) time.Duration {
	if value < 0 {
		return -value
	}
	return value
}

type FalWebhookAdapter interface {
	WebhookObservation(string, []byte) (string, joboperation.Observation, error)
}
type FalWebhookService interface {
	ApplyWebhook(context.Context, jobs.WebhookObservation) (joboperation.Job, bool, error)
}

type WebhookHandler struct {
	logger           *slog.Logger
	verifier         *FalJWKSVerifier
	service          FalWebhookService
	adapter          FalWebhookAdapter
	maximumBodyBytes int64
	telemetry        *telemetry.Recorder
}

func NewWebhookHandler(logger *slog.Logger, verifier *FalJWKSVerifier, service FalWebhookService, adapter FalWebhookAdapter, maximumBodyBytes int64) (*WebhookHandler, error) {
	if logger == nil || verifier == nil || service == nil || adapter == nil || maximumBodyBytes < 1 || maximumBodyBytes > 256*1024*1024 {
		return nil, errors.New("invalid fal webhook handler")
	}
	return &WebhookHandler{logger: logger, verifier: verifier, service: service, adapter: adapter, maximumBodyBytes: maximumBodyBytes}, nil
}
func (handler *WebhookHandler) SetTelemetry(recorder *telemetry.Recorder) {
	handler.telemetry = recorder
}

func (handler *WebhookHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		methodNotAllowed(writer, http.MethodPost)
		return
	}
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" || request.Header.Get("Content-Encoding") != "" {
		writeError(writer, http.StatusBadRequest, "Invalid webhook request")
		return
	}
	names := []string{"X-Fal-Webhook-Request-Id", "X-Fal-Webhook-User-Id", "X-Fal-Webhook-Timestamp", "X-Fal-Webhook-Signature"}
	values := make([]string, len(names))
	for index, name := range names {
		entries := request.Header.Values(name)
		if len(entries) != 1 {
			handler.record(request.Context(), "failure", joboperation.Failed)
			writeError(writer, http.StatusUnauthorized, "Invalid webhook signature")
			return
		}
		values[index] = entries[0]
	}
	if request.ContentLength > handler.maximumBodyBytes {
		writeError(writer, http.StatusRequestEntityTooLarge, "Webhook body is too large")
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, handler.maximumBodyBytes+1))
	if err != nil || int64(len(body)) > handler.maximumBodyBytes {
		writeError(writer, http.StatusRequestEntityTooLarge, "Webhook body is too large")
		return
	}
	if err := handler.verifier.Verify(request.Context(), values[0], values[1], values[2], values[3], body); err != nil {
		if errors.Is(err, ErrJWKSUnavailable) {
			handler.record(request.Context(), "retried", joboperation.Reconciling)
			writeError(writer, http.StatusServiceUnavailable, "Webhook verification unavailable")
		} else {
			handler.record(request.Context(), "failure", joboperation.Failed)
			writeError(writer, http.StatusUnauthorized, "Invalid webhook signature")
		}
		return
	}
	jobID, token, ok := falWebhookPath(request.URL.Path)
	if !ok {
		writeError(writer, http.StatusNotFound, "Not found")
		return
	}
	providerID, observation, err := handler.adapter.WebhookObservation(jobID, body)
	if err != nil || providerID != values[0] {
		handler.record(request.Context(), "failure", joboperation.Failed)
		writeError(writer, http.StatusBadRequest, "Invalid webhook payload")
		return
	}
	_, _, err = handler.service.ApplyWebhook(request.Context(), jobs.WebhookObservation{JobID: jobID, Provider: "fal", DeliveryID: values[0], Token: token, ProviderJobID: providerID, Observation: observation})
	switch {
	case err == nil, errors.Is(err, joboperation.ErrConflict):
		handler.record(request.Context(), "success", observation.Status)
		writer.WriteHeader(http.StatusNoContent)
	case errors.Is(err, jobs.ErrWebhookNotReady):
		handler.record(request.Context(), "retried", joboperation.Reconciling)
		writeError(writer, http.StatusServiceUnavailable, "Webhook processing unavailable")
	case errors.Is(err, jobs.ErrWebhookRejected), errors.Is(err, joboperation.ErrInvalid), errors.Is(err, joboperation.ErrNotFound):
		handler.record(request.Context(), "failure", joboperation.Failed)
		writeError(writer, http.StatusBadRequest, "Invalid webhook payload")
	default:
		handler.logger.Warn("fal webhook application failed", "category", "webhook_storage_failed")
		handler.record(request.Context(), "retried", joboperation.Reconciling)
		writeError(writer, http.StatusServiceUnavailable, "Webhook processing unavailable")
	}
}

func falWebhookPath(path string) (string, string, bool) {
	const prefix = "/internal/webhooks/fal/"
	parts := strings.Split(strings.TrimPrefix(path, prefix), "/")
	if !strings.HasPrefix(path, prefix) || len(parts) != 2 || !joboperation.ValidID(parts[0]) || !strings.HasPrefix(parts[1], "whk_") || len(parts[1]) != 36 || strings.ContainsAny(parts[1], ".%") {
		return "", "", false
	}
	return parts[0], parts[1], true
}
func (handler *WebhookHandler) record(ctx context.Context, outcome string, status joboperation.Status) {
	if handler.telemetry != nil {
		handler.telemetry.Job(ctx, telemetry.JobRecord{Protocol: "fal", Stage: "webhook", Status: string(status), Outcome: outcome})
	}
}
