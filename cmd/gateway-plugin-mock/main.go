package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	runtimev1 "github.com/nativegatewayhq/gateway/plugin-sdk/runtime/v1"
)

const conformanceHeader = "X-Native-Gateway-Conformance"
const conformanceCaseHeader = "X-Native-Gateway-Conformance-Case"

const transparentPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M/wHwAF/gL+X8W8WQAAAABJRU5ErkJggg=="

func main() {
	token := os.Getenv("GATEWAY_PLUGIN_MOCK_TOKEN")
	if token == "" || len(token) > 4096 || strings.TrimSpace(token) != token {
		_, _ = fmt.Fprintln(os.Stderr, "mock plugin configuration error")
		os.Exit(1)
	}
	addr := os.Getenv("GATEWAY_PLUGIN_MOCK_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8081"
	}
	mux := http.NewServeMux()
	auth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+token {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next(w, r)
		}
	}
	mux.HandleFunc("GET /plugin/v1/health", auth(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = runtimev1.EncodeHealth(w)
	}))
	mux.HandleFunc("POST /plugin/v1/execute", auth(execute))
	server := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	if err := server.ListenAndServe(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "mock plugin stopped")
		os.Exit(1)
	}
}

func execute(w http.ResponseWriter, r *http.Request) {
	request, err := runtimev1.DecodeRequest(r.Body, 2<<20)
	if err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if r.Header.Get(conformanceHeader) == "runtime/v1" {
		switch r.Header.Get(conformanceCaseHeader) {
		case "error":
			expected := expectation(request)
			response := runtimev1.Failure(request.Identity(), runtimev1.InvalidRequest("conformance invalid request"))
			w.Header().Set("Content-Type", "application/json")
			_ = runtimev1.EncodeResponse(w, response, expected)
			return
		case "cancel":
			<-r.Context().Done()
			if !errorsIsCancellation(r.Context().Err()) {
				http.Error(w, "canceled", http.StatusRequestTimeout)
			}
			return
		}
	}
	images := make([]runtimev1.Image, request.Input.Images)
	for index := range images {
		images[index] = runtimev1.Image{MIMEType: "image/png", Base64: base64.StdEncoding.EncodeToString(mustDecodePNG())}
	}
	expected := expectation(request)
	response := runtimev1.Success(request.Identity(), runtimev1.Result{Images: images, Usage: runtimev1.Usage{Images: len(images)}})
	w.Header().Set("Content-Type", "application/json")
	_ = runtimev1.EncodeResponse(w, response, expected)
}

func expectation(request runtimev1.ExecuteRequest) runtimev1.Expectation {
	return runtimev1.Expectation{Identity: request.Identity(), Protocol: request.Protocol, Model: request.Model, Output: "base64", MaximumImages: 10}
}

func errorsIsCancellation(err error) bool {
	return err == context.Canceled || err == context.DeadlineExceeded
}
func mustDecodePNG() []byte {
	value, err := base64.StdEncoding.DecodeString(transparentPNG)
	if err != nil {
		panic("invalid embedded png")
	}
	return value
}
