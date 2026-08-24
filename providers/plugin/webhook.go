package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nativegatewayhq/gateway/internal/jobs"
	"github.com/nativegatewayhq/gateway/internal/plugins"
	asyncv1 "github.com/nativegatewayhq/gateway/plugin-sdk/async/v1"
	"github.com/nativegatewayhq/gateway/plugin-sdk/jsonstrict"
)

type CallbackApplier interface {
	ApplyPluginWebhook(context.Context, jobs.WebhookObservation) (bool, error)
}

type ServiceCallbackApplier struct{ Service *jobs.Service }

func (value ServiceCallbackApplier) ApplyPluginWebhook(ctx context.Context, request jobs.WebhookObservation) (bool, error) {
	_, replay, err := value.Service.ApplyWebhook(ctx, request)
	return replay, err
}

type CallbackHandler struct {
	registry         *plugins.Registry
	service          CallbackApplier
	secrets          [][]byte
	now              func() time.Time
	tolerance        time.Duration
	maximumBodyBytes int64
}

func NewCallbackHandler(registry *plugins.Registry, service CallbackApplier, secrets [][]byte, tolerance time.Duration, maximumBodyBytes int64) (*CallbackHandler, error) {
	if registry == nil || service == nil || len(secrets) < 1 || len(secrets) > 2 || tolerance <= 0 || tolerance > 15*time.Minute || maximumBodyBytes < 1 || maximumBodyBytes > 128<<20 {
		return nil, errors.New("invalid plugin callback configuration")
	}
	copySecrets := make([][]byte, len(secrets))
	for index, secret := range secrets {
		if len(secret) != 32 {
			return nil, errors.New("invalid plugin callback configuration")
		}
		copySecrets[index] = append([]byte(nil), secret...)
	}
	return &CallbackHandler{registry: registry, service: service, secrets: copySecrets, now: time.Now, tolerance: tolerance, maximumBodyBytes: maximumBodyBytes}, nil
}

func (handler *CallbackHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writer.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	jobID, token, ok := callbackPath(request.URL.Path)
	if !ok || request.URL.RawQuery != "" {
		writer.WriteHeader(http.StatusNotFound)
		return
	}
	timestampText, deliveryID, signature, ok := callbackHeaders(request.Header)
	if !ok {
		writer.WriteHeader(http.StatusUnauthorized)
		return
	}
	timestamp, err := strconv.ParseInt(timestampText, 10, 64)
	if err != nil || delta(handler.now().Unix(), timestamp) > int64(handler.tolerance/time.Second) {
		writer.WriteHeader(http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, handler.maximumBodyBytes+1))
	if err != nil || len(body) == 0 || int64(len(body)) > handler.maximumBodyBytes || !handler.verify(timestamp, deliveryID, body, signature) {
		writer.WriteHeader(http.StatusUnauthorized)
		return
	}
	var callback asyncv1.Callback
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if jsonstrict.Validate(body) != nil || decoder.Decode(&callback) != nil || decoder.Decode(&struct{}{}) != io.EOF || callback.GatewayJobID != jobID || callback.DeliveryID != deliveryID {
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	binding, found := handler.registry.AsyncIdentity(callback.PluginID, callback.PluginVersion, callback.ManifestDigest, callback.Protocol, callback.Model)
	if !found || !binding.Callback {
		writer.WriteHeader(http.StatusUnauthorized)
		return
	}
	expected := asyncv1.Expectation{Identity: callback.Identity(), Output: binding.Output, MaximumImages: binding.MaximumImages}
	if asyncv1.ValidateCallback(callback, expected) != nil {
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	observation, err := NativeObservation(binding.Protocol, jobID, callback.ProviderJobRef, callback.Observation, "webhook")
	if err != nil {
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	_, err = handler.service.ApplyPluginWebhook(request.Context(), jobs.WebhookObservation{JobID: jobID, Provider: "plugin", DeliveryID: deliveryID, Token: token, ProviderJobID: callback.ProviderJobRef, Observation: observation})
	if errors.Is(err, jobs.ErrWebhookNotReady) {
		writer.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if err != nil {
		writer.WriteHeader(http.StatusConflict)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (handler *CallbackHandler) verify(timestamp int64, delivery string, body []byte, signature string) bool {
	for _, secret := range handler.secrets {
		if asyncv1.VerifyCallbackSignature(secret, timestamp, delivery, body, signature) == nil {
			return true
		}
	}
	return false
}

func callbackPath(path string) (string, string, bool) {
	value := strings.TrimPrefix(path, "/internal/webhooks/plugin/")
	parts := strings.Split(value, "/")
	returnValue := len(parts) == 2 && parts[0] != "" && parts[1] != "" && !strings.ContainsAny(value, "?#")
	if !returnValue {
		return "", "", false
	}
	return parts[0], parts[1], true
}
func callbackHeaders(header http.Header) (string, string, string, bool) {
	timestamps := header.Values(asyncv1.CallbackTimestampHeader)
	deliveries := header.Values(asyncv1.CallbackDeliveryHeader)
	signatures := header.Values(asyncv1.CallbackSignatureHeader)
	if len(timestamps) != 1 || len(deliveries) != 1 || len(signatures) != 1 {
		return "", "", "", false
	}
	return timestamps[0], deliveries[0], signatures[0], true
}
func delta(left, right int64) int64 {
	if left < right {
		return right - left
	}
	return left - right
}
