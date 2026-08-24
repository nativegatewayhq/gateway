package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/nativegatewayhq/gateway/internal/plugins"
	manifest "github.com/nativegatewayhq/gateway/plugin-sdk/manifest/v1"
)

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
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	}))
	mux.HandleFunc("POST /plugin/v1/execute", auth(execute))
	server := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	if err := server.ListenAndServe(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "mock plugin stopped")
		os.Exit(1)
	}
}

func execute(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 2<<20+1))
	if err != nil || len(body) > 2<<20 || manifest.HasDuplicateKeys(body) {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	var request plugins.ExecuteRequest
	if decoder.Decode(&request) != nil || decoder.Decode(&struct{}{}) != io.EOF || request.SchemaVersion != plugins.RequestSchema || request.Operation != "image.generate" || request.RequestID == "" || request.PluginID == "" || request.PluginVersion == "" || request.ManifestDigest == "" || request.Model == "" || request.Input.Prompt == "" || request.Input.Images < 1 || request.Input.Images > 10 {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	images := make([]plugins.Image, request.Input.Images)
	for index := range images {
		images[index] = plugins.Image{MIMEType: "image/png", Base64: base64.StdEncoding.EncodeToString(mustDecodePNG())}
	}
	response := plugins.ExecuteResponse{SchemaVersion: plugins.ResponseSchema, RequestID: request.RequestID, PluginID: request.PluginID, PluginVersion: request.PluginVersion, ManifestDigest: request.ManifestDigest, Result: &plugins.Result{Images: images, Usage: plugins.Usage{Images: len(images)}}}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}
func mustDecodePNG() []byte {
	value, err := base64.StdEncoding.DecodeString(transparentPNG)
	if err != nil {
		panic("invalid embedded png")
	}
	return value
}
