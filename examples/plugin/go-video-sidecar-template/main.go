package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	runtimev1 "github.com/nativegatewayhq/gateway/plugin-sdk/runtime/v1"
	videov1 "github.com/nativegatewayhq/gateway/plugin-sdk/video/v1"
)

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
	mux.HandleFunc("POST /plugin/video/v1/submit", auth(server.submit))
	mux.HandleFunc("POST /plugin/video/v1/poll", auth(server.control))
	mux.HandleFunc("POST /plugin/video/v1/cancel", auth(server.control))
	mux.HandleFunc("GET /plugin/v1/health", auth(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_ = runtimev1.EncodeHealth(writer)
	}))
	return mux
}
func (server *server) submit(w http.ResponseWriter, r *http.Request) {
	// A production adapter derives this identity from a strict preliminary
	// envelope decode. This deterministic template accepts the conformance
	// identity fixed by its manifest and validates every capability field.
	var raw videov1.SubmitRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&raw) != nil {
		w.WriteHeader(400)
		return
	}
	expected := expectation(raw.Identity())
	body, _ := json.Marshal(raw)
	request, err := videov1.DecodeSubmitRequest(bytes.NewReader(body), 2<<20, expected)
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
	response := videov1.SubmitResponse{SchemaVersion: videov1.SubmitResponseSchema, RequestID: request.RequestID, GatewayJobID: request.GatewayJobID, PluginID: request.PluginID, PluginVersion: request.PluginVersion, ManifestDigest: request.ManifestDigest, ProviderJobRef: ref, Observation: videov1.Observation{Status: "QUEUED"}}
	body, err = videov1.CanonicalSubmitResponse(response, expected)
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
	request, err := videov1.DecodeControlRequest(r.Body, 2<<20)
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
	observation := videov1.Observation{Status: status}
	if status == "SUCCEEDED" {
		observation.Result = &videov1.Result{URL: "https://assets.example.com/conformance.mp4", ContentType: "video/mp4", DurationSeconds: 5}
		observation.Usage = &videov1.Usage{Dimension: "provider_credit", Unit: "microcredit", Quantity: 1_000_000}
	}
	response := videov1.ObservationResponse{SchemaVersion: videov1.ObservationResponseSchema, RequestID: request.RequestID, GatewayJobID: request.GatewayJobID, PluginID: request.PluginID, PluginVersion: request.PluginVersion, ManifestDigest: request.ManifestDigest, Observation: observation}
	expected := expectation(request.Identity())
	body, err := videov1.CanonicalObservationResponse(response, expected)
	if err != nil {
		w.WriteHeader(500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}
func (server *server) callback(request videov1.SubmitRequest, ref string) {
	time.Sleep(20 * time.Millisecond)
	delivery := "delivery_templatecallback1"
	callback := videov1.Callback{SchemaVersion: videov1.CallbackSchema, DeliveryID: delivery, RequestID: request.RequestID, GatewayJobID: request.GatewayJobID, PluginID: request.PluginID, PluginVersion: request.PluginVersion, ManifestDigest: request.ManifestDigest, Protocol: request.Protocol, Operation: request.Operation, Model: request.Model, ProviderJobRef: ref, Observation: videov1.Observation{Status: "SUCCEEDED", Result: &videov1.Result{URL: "https://assets.example.com/conformance.mp4", ContentType: "video/mp4", DurationSeconds: request.Input.DurationSeconds}, Usage: &videov1.Usage{Dimension: "provider_credit", Unit: "microcredit", Quantity: 1_000_000}}}
	body, err := videov1.CanonicalCallback(callback, expectation(request.Identity()))
	if err != nil {
		return
	}
	timestamp := time.Now().Unix()
	signature, err := videov1.SignCallback(server.callbackKey, timestamp, delivery, body)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	outbound, err := http.NewRequestWithContext(ctx, http.MethodPost, request.CallbackURL, strings.NewReader(string(body)))
	if err != nil {
		return
	}
	outbound.Header.Set(videov1.CallbackTimestampHeader, fmt.Sprint(timestamp))
	outbound.Header.Set(videov1.CallbackDeliveryHeader, delivery)
	outbound.Header.Set(videov1.CallbackSignatureHeader, signature)
	outbound.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(outbound)
	if err == nil {
		response.Body.Close()
	}
}

func expectation(identity videov1.Identity) videov1.Expectation {
	return videov1.Expectation{Identity: identity, MaximumDurationSeconds: 60, Ratios: map[string]bool{"16:9": true, "9:16": true}, TextToVideo: true, ImageToVideo: true, ResultOrigins: map[string]bool{"https://assets.example.com": true}}
}
