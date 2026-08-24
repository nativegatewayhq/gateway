package plugin

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nativegatewayhq/gateway/internal/jobs"
	"github.com/nativegatewayhq/gateway/internal/plugins"
	"github.com/nativegatewayhq/gateway/plugin-sdk/jsonstrict"
	videov1 "github.com/nativegatewayhq/gateway/plugin-sdk/video/v1"
)

type VideoCallbackHandler struct {
	registry         *plugins.Registry
	service          CallbackApplier
	secrets          [][]byte
	now              func() time.Time
	tolerance        time.Duration
	maximumBodyBytes int64
}

func NewVideoCallbackHandler(registry *plugins.Registry, service CallbackApplier, secrets [][]byte, tolerance time.Duration, maximumBodyBytes int64) (*VideoCallbackHandler, error) {
	if registry == nil || service == nil || len(secrets) < 1 || len(secrets) > 2 || tolerance <= 0 || tolerance > 15*time.Minute || maximumBodyBytes < 1 || maximumBodyBytes > 128<<20 {
		return nil, errors.New("invalid plugin video callback configuration")
	}
	copySecrets := make([][]byte, len(secrets))
	for index, secret := range secrets {
		if len(secret) != 32 {
			return nil, errors.New("invalid plugin video callback configuration")
		}
		copySecrets[index] = append([]byte(nil), secret...)
	}
	return &VideoCallbackHandler{registry: registry, service: service, secrets: copySecrets, now: time.Now, tolerance: tolerance, maximumBodyBytes: maximumBodyBytes}, nil
}

func (handler *VideoCallbackHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writer.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	jobID, token, ok := videoCallbackPath(request.URL.Path)
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
	var callback videov1.Callback
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if jsonstrict.Validate(body) != nil || decoder.Decode(&callback) != nil || decoder.Decode(&struct{}{}) != io.EOF || callback.GatewayJobID != jobID || callback.DeliveryID != deliveryID {
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	binding, found := handler.registry.AsyncIdentity(callback.PluginID, callback.PluginVersion, callback.ManifestDigest, callback.Protocol, callback.Model)
	if !found || !binding.Video || !binding.Callback {
		writer.WriteHeader(http.StatusUnauthorized)
		return
	}
	expected := videoExpectationForCallback(binding, callback.Identity())
	if videov1.ValidateCallback(callback, expected) != nil {
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	observation, err := videoObservation(callback.Observation, jobID, callback.ProviderJobRef, "webhook")
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

func videoExpectationForCallback(binding plugins.Binding, identity videov1.Identity) videov1.Expectation {
	ratios := map[string]bool{}
	for ratio := range binding.Ratios {
		ratios[ratio] = true
	}
	origins := map[string]bool{}
	for origin := range binding.ResultOrigins {
		origins[origin] = true
	}
	return videov1.Expectation{Identity: identity, MaximumDurationSeconds: binding.MaximumDurationSeconds, Ratios: ratios, Audio: binding.Audio, TextToVideo: binding.TextToVideo, ImageToVideo: binding.ImageToVideo, ResultOrigins: origins}
}
func (handler *VideoCallbackHandler) verify(timestamp int64, delivery string, body []byte, signature string) bool {
	for _, secret := range handler.secrets {
		if videov1.VerifyCallbackSignature(secret, timestamp, delivery, body, signature) == nil {
			return true
		}
	}
	return false
}
func videoCallbackPath(path string) (string, string, bool) {
	value := strings.TrimPrefix(path, "/internal/webhooks/plugin-video/")
	parts := strings.Split(value, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.ContainsAny(value, "?#") {
		return "", "", false
	}
	return parts[0], parts[1], true
}
