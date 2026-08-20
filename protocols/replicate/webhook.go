package replicate

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nativegatewayhq/gateway/internal/jobs"
	"github.com/nativegatewayhq/gateway/internal/telemetry"
	joboperation "github.com/nativegatewayhq/gateway/operations/job"
)

type WebhookAdapter interface {
	WebhookObservation(string, []byte) (string, joboperation.Observation, error)
}

type WebhookService interface {
	ApplyWebhook(context.Context, jobs.WebhookObservation) (joboperation.Job, bool, error)
}

type SignatureVerifier struct {
	secrets   [][]byte
	tolerance time.Duration
	now       func() time.Time
}

func NewSignatureVerifier(values []string, tolerance time.Duration) (*SignatureVerifier, error) {
	if len(values) < 1 || len(values) > 2 || tolerance < time.Minute || tolerance > 15*time.Minute {
		return nil, errors.New("invalid Replicate webhook verifier configuration")
	}
	verifier := &SignatureVerifier{tolerance: tolerance, now: time.Now}
	for _, value := range values {
		if !strings.HasPrefix(value, "whsec_") {
			return nil, errors.New("invalid Replicate webhook verifier configuration")
		}
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(value, "whsec_"))
		if err != nil || len(decoded) < 16 || len(decoded) > 128 {
			return nil, errors.New("invalid Replicate webhook verifier configuration")
		}
		verifier.secrets = append(verifier.secrets, decoded)
	}
	return verifier, nil
}

func (verifier *SignatureVerifier) Verify(deliveryID, timestamp, signature string, body []byte) bool {
	if verifier == nil || deliveryID == "" || len(deliveryID) > 200 || strings.ContainsAny(deliveryID, "\r\n") || timestamp == "" || signature == "" {
		return false
	}
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}
	delta := verifier.now().UTC().Sub(time.Unix(seconds, 0).UTC())
	if delta < -verifier.tolerance || delta > verifier.tolerance {
		return false
	}
	message := append([]byte(deliveryID+"."+timestamp+"."), body...)
	var candidates [][]byte
	for _, field := range strings.Fields(signature) {
		parts := strings.SplitN(field, ",", 2)
		if len(parts) != 2 || parts[0] != "v1" {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(parts[1])
		if err == nil && len(decoded) == sha256.Size {
			candidates = append(candidates, decoded)
		}
	}
	matched := 0
	for _, secret := range verifier.secrets {
		mac := hmac.New(sha256.New, secret)
		_, _ = mac.Write(message)
		expected := mac.Sum(nil)
		for _, candidate := range candidates {
			matched |= subtle.ConstantTimeCompare(expected, candidate)
		}
	}
	return matched == 1
}

type WebhookHandler struct {
	logger           *slog.Logger
	verifier         *SignatureVerifier
	service          WebhookService
	adapter          WebhookAdapter
	maximumBodyBytes int64
	telemetry        *telemetry.Recorder
}

func NewWebhookHandler(logger *slog.Logger, verifier *SignatureVerifier, service WebhookService, adapter WebhookAdapter, maximumBodyBytes int64) (*WebhookHandler, error) {
	if logger == nil || verifier == nil || service == nil || adapter == nil || maximumBodyBytes < 1 || maximumBodyBytes > 256*1024*1024 {
		return nil, errors.New("invalid Replicate webhook handler configuration")
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
		writeDetail(writer, http.StatusBadRequest, "Invalid webhook request")
		return
	}
	deliveryValues, timestampValues, signatureValues := request.Header.Values("Webhook-Id"), request.Header.Values("Webhook-Timestamp"), request.Header.Values("Webhook-Signature")
	if len(deliveryValues) != 1 || len(timestampValues) != 1 || len(signatureValues) != 1 || request.ContentLength > handler.maximumBodyBytes {
		handler.record(request.Context(), "failure", string(joboperation.Failed))
		writeDetail(writer, http.StatusUnauthorized, "Invalid webhook signature")
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, handler.maximumBodyBytes+1))
	if err != nil || int64(len(body)) > handler.maximumBodyBytes {
		writeDetail(writer, http.StatusRequestEntityTooLarge, "Webhook body is too large")
		return
	}
	if !handler.verifier.Verify(deliveryValues[0], timestampValues[0], signatureValues[0], body) {
		handler.record(request.Context(), "failure", string(joboperation.Failed))
		writeDetail(writer, http.StatusUnauthorized, "Invalid webhook signature")
		return
	}
	jobID, token, ok := webhookPath(request.URL.Path)
	if !ok {
		writeDetail(writer, http.StatusNotFound, "Not found")
		return
	}
	providerJobID, observation, err := handler.adapter.WebhookObservation(jobID, body)
	if err != nil {
		handler.record(request.Context(), "failure", string(joboperation.Failed))
		writeDetail(writer, http.StatusBadRequest, "Invalid webhook payload")
		return
	}
	_, _, err = handler.service.ApplyWebhook(request.Context(), jobs.WebhookObservation{JobID: jobID, Provider: "replicate", DeliveryID: deliveryValues[0], Token: token, ProviderJobID: providerJobID, Observation: observation})
	switch {
	case err == nil, errors.Is(err, joboperation.ErrConflict):
		handler.record(request.Context(), "success", string(observation.Status))
		writer.WriteHeader(http.StatusNoContent)
	case errors.Is(err, jobs.ErrWebhookRejected), errors.Is(err, joboperation.ErrInvalid), errors.Is(err, joboperation.ErrNotFound):
		handler.record(request.Context(), "failure", string(joboperation.Failed))
		writeDetail(writer, http.StatusBadRequest, "Invalid webhook payload")
	case errors.Is(err, jobs.ErrWebhookNotReady):
		handler.record(request.Context(), "retried", string(joboperation.Reconciling))
		writeDetail(writer, http.StatusServiceUnavailable, "Webhook processing unavailable")
	default:
		handler.record(request.Context(), "retried", string(joboperation.Reconciling))
		handler.logger.Warn("Replicate webhook application failed", "category", "webhook_storage_failed")
		writeDetail(writer, http.StatusServiceUnavailable, "Webhook processing unavailable")
	}
}

func (handler *WebhookHandler) record(ctx context.Context, outcome, status string) {
	if handler.telemetry != nil {
		handler.telemetry.Job(ctx, telemetry.JobRecord{Protocol: "replicate", Stage: "webhook", Status: status, Outcome: outcome})
	}
}

func webhookPath(path string) (string, string, bool) {
	const prefix = "/internal/webhooks/replicate/"
	if !strings.HasPrefix(path, prefix) {
		return "", "", false
	}
	parts := strings.Split(strings.TrimPrefix(path, prefix), "/")
	if len(parts) != 2 || !joboperation.ValidID(parts[0]) || !strings.HasPrefix(parts[1], "whk_") || len(parts[1]) != 36 || strings.ContainsAny(parts[1], ".%") {
		return "", "", false
	}
	return parts[0], parts[1], true
}
