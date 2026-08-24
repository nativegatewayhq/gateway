package main

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	asyncv1 "github.com/nativegatewayhq/gateway/plugin-sdk/async/v1"
	runtimev1 "github.com/nativegatewayhq/gateway/plugin-sdk/runtime/v1"
)

const transparentPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M/wHwAF/gL+X8W8WQAAAABJRU5ErkJggg=="

type server struct {
	token       string
	callbackKey []byte
	mutex       sync.Mutex
	jobs        map[string]int
}

func main() {
	token := os.Getenv("PLUGIN_AUTH_TOKEN")
	key, _ := base64.StdEncoding.DecodeString(os.Getenv("PLUGIN_CALLBACK_SECRET"))
	if len(token) < 16 || strings.TrimSpace(token) != token || len(key) != 32 {
		fmt.Fprintln(os.Stderr, "plugin configuration error")
		os.Exit(1)
	}
	address := os.Getenv("PLUGIN_ADDR")
	if address == "" {
		address = "127.0.0.1:8082"
	}
	value := &server{token: token, callbackKey: key, jobs: map[string]int{}}
	httpServer := &http.Server{Addr: address, Handler: value.handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		os.Exit(1)
	}
}

func (server *server) handler() http.Handler {
	mux := http.NewServeMux()
	auth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			expected := []byte("Bearer " + server.token)
			actual := []byte(r.Header.Get("Authorization"))
			if len(actual) != len(expected) || subtle.ConstantTimeCompare(actual, expected) != 1 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			next(w, r)
		}
	}
	mux.HandleFunc("POST /plugin/async/v1/submit", auth(server.submit))
	mux.HandleFunc("POST /plugin/async/v1/poll", auth(server.control))
	mux.HandleFunc("POST /plugin/async/v1/cancel", auth(server.control))
	mux.HandleFunc("GET /plugin/v1/health", auth(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_ = runtimev1.EncodeHealth(writer)
	}))
	return mux
}
func (server *server) submit(w http.ResponseWriter, r *http.Request) {
	request, err := asyncv1.DecodeSubmitRequest(r.Body, 2<<20)
	if err != nil {
		w.WriteHeader(400)
		return
	}
	if r.Header.Get("X-Native-Gateway-Conformance-Case") == "cancel" {
		<-r.Context().Done()
		return
	}
	ref := "conformance:job-1"
	server.mutex.Lock()
	server.jobs[ref] = 0
	server.mutex.Unlock()
	response := asyncv1.SubmitResponse{SchemaVersion: asyncv1.SubmitResponseSchema, RequestID: request.RequestID, GatewayJobID: request.GatewayJobID, PluginID: request.PluginID, PluginVersion: request.PluginVersion, ManifestDigest: request.ManifestDigest, ProviderJobRef: ref, Observation: asyncv1.Observation{Status: "QUEUED"}}
	expected := asyncv1.Expectation{Identity: request.Identity(), Output: "base64", MaximumImages: 2}
	body, err := asyncv1.CanonicalSubmitResponse(response, expected)
	if err != nil {
		w.WriteHeader(500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
	if request.CallbackURL != "" {
		go server.callback(request, ref)
	}
}
func (server *server) control(w http.ResponseWriter, r *http.Request) {
	request, err := asyncv1.DecodeControlRequest(r.Body, 2<<20)
	if err != nil {
		w.WriteHeader(400)
		return
	}
	status := "PROCESSING"
	test := r.Header.Get("X-Native-Gateway-Conformance-Case")
	if request.Action == "cancel" {
		status = "CANCELED"
	} else if test == "success" {
		status = "SUCCEEDED"
	}
	observation := asyncv1.Observation{Status: status}
	if status == "SUCCEEDED" {
		observation.Result = &asyncv1.Result{Images: []asyncv1.Image{{MIMEType: "image/png", Base64: transparentPNG}}, Usage: asyncv1.Usage{Dimension: "output", Unit: "image", Quantity: 1}}
	}
	response := asyncv1.ObservationResponse{SchemaVersion: asyncv1.ObservationResponseSchema, RequestID: request.RequestID, GatewayJobID: request.GatewayJobID, PluginID: request.PluginID, PluginVersion: request.PluginVersion, ManifestDigest: request.ManifestDigest, Observation: observation}
	expected := asyncv1.Expectation{Identity: request.Identity(), Output: "base64", MaximumImages: 2}
	body, err := asyncv1.CanonicalObservationResponse(response, expected)
	if err != nil {
		w.WriteHeader(500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}
func (server *server) callback(request asyncv1.SubmitRequest, ref string) {
	time.Sleep(20 * time.Millisecond)
	delivery := "delivery_templatecallback1"
	callback := asyncv1.Callback{SchemaVersion: asyncv1.CallbackSchema, DeliveryID: delivery, RequestID: request.RequestID, GatewayJobID: request.GatewayJobID, PluginID: request.PluginID, PluginVersion: request.PluginVersion, ManifestDigest: request.ManifestDigest, Protocol: request.Protocol, Operation: request.Operation, Model: request.Model, ProviderJobRef: ref, Observation: asyncv1.Observation{Status: "SUCCEEDED", Result: &asyncv1.Result{Images: []asyncv1.Image{{MIMEType: "image/png", Base64: transparentPNG}}, Usage: asyncv1.Usage{Dimension: "output", Unit: "image", Quantity: 1}}}}
	body, err := asyncv1.CanonicalCallback(callback, asyncv1.Expectation{Identity: request.Identity(), Output: "base64", MaximumImages: 2})
	if err != nil {
		return
	}
	timestamp := time.Now().Unix()
	signature, err := asyncv1.SignCallback(server.callbackKey, timestamp, delivery, body)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	outbound, err := http.NewRequestWithContext(ctx, http.MethodPost, request.CallbackURL, strings.NewReader(string(body)))
	if err != nil {
		return
	}
	outbound.Header.Set(asyncv1.CallbackTimestampHeader, fmt.Sprint(timestamp))
	outbound.Header.Set(asyncv1.CallbackDeliveryHeader, delivery)
	outbound.Header.Set(asyncv1.CallbackSignatureHeader, signature)
	outbound.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(outbound)
	if err == nil {
		response.Body.Close()
	}
}
