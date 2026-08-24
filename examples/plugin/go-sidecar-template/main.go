package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	runtimev1 "github.com/nativegatewayhq/gateway/plugin-sdk/runtime/v1"
)

const testModeHeader = "X-Native-Gateway-Conformance"
const testCaseHeader = "X-Native-Gateway-Conformance-Case"
const transparentPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M/wHwAF/gL+X8W8WQAAAABJRU5ErkJggg=="

func main() {
	token := os.Getenv("PLUGIN_AUTH_TOKEN")
	if !validToken(token) {
		_, _ = fmt.Fprintln(os.Stderr, "plugin configuration error")
		os.Exit(1)
	}
	address := os.Getenv("PLUGIN_ADDR")
	if address == "" {
		address = "127.0.0.1:8081"
	}
	server := &http.Server{Addr: address, Handler: newHandler(token), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		_, _ = fmt.Fprintln(os.Stderr, "plugin server stopped")
		os.Exit(1)
	}
}

func newHandler(token string) http.Handler {
	mux := http.NewServeMux()
	authenticated := func(next http.HandlerFunc) http.HandlerFunc {
		return func(writer http.ResponseWriter, request *http.Request) {
			expected := []byte("Bearer " + token)
			actual := []byte(request.Header.Get("Authorization"))
			if len(actual) != len(expected) || subtle.ConstantTimeCompare(actual, expected) != 1 {
				writer.WriteHeader(http.StatusUnauthorized)
				return
			}
			next(writer, request)
		}
	}
	mux.HandleFunc("GET /plugin/v1/health", authenticated(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_ = runtimev1.EncodeHealth(writer)
	}))
	mux.HandleFunc("POST /plugin/v1/execute", authenticated(execute))
	return mux
}

func execute(writer http.ResponseWriter, request *http.Request) {
	decoded, err := runtimev1.DecodeRequest(request.Body, 2<<20)
	if err != nil {
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	expected := runtimev1.Expectation{Identity: decoded.Identity(), Protocol: decoded.Protocol, Model: decoded.Model, Output: "base64", MaximumImages: 2}
	if request.Header.Get(testModeHeader) == "runtime/v1" {
		switch request.Header.Get(testCaseHeader) {
		case "error":
			writer.Header().Set("Content-Type", "application/json")
			_ = runtimev1.EncodeResponse(writer, runtimev1.Failure(decoded.Identity(), runtimev1.InvalidRequest("conformance invalid request")), expected)
			return
		case "cancel":
			<-request.Context().Done()
			return
		}
	}
	images := make([]runtimev1.Image, decoded.Input.Images)
	for index := range images {
		images[index] = runtimev1.Image{MIMEType: "image/png", Base64: transparentPNG}
	}
	writer.Header().Set("Content-Type", "application/json")
	_ = runtimev1.EncodeResponse(writer, runtimev1.Success(decoded.Identity(), runtimev1.Result{Images: images, Usage: runtimev1.Usage{Images: len(images)}}), expected)
}

func validToken(token string) bool {
	return len(token) >= 16 && len(token) <= 4096 && strings.TrimSpace(token) == token
}
