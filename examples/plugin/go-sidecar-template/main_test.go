package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	runtimev1 "github.com/nativegatewayhq/gateway/plugin-sdk/runtime/v1"
)

const token = "0123456789abcdef-template"

func TestHealthAuthAndExecute(t *testing.T) {
	handler := newHandler(token)
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/plugin/v1/health", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.Code)
	}
	health := request(t, handler, http.MethodGet, "/plugin/v1/health", nil, "")
	if health.Code != http.StatusOK {
		t.Fatalf("health status = %d", health.Code)
	}
	value := executeRequest()
	body, _ := runtimev1.CanonicalRequest(value)
	success := request(t, handler, http.MethodPost, "/plugin/v1/execute", body, "success")
	expected := runtimev1.Expectation{Identity: value.Identity(), Protocol: value.Protocol, Model: value.Model, Output: "base64", MaximumImages: 2}
	if _, err := runtimev1.DecodeResponse(success.Body, 1<<20, expected); err != nil {
		t.Fatal(err)
	}
	failure := request(t, handler, http.MethodPost, "/plugin/v1/execute", body, "error")
	decoded, err := runtimev1.DecodeResponse(failure.Body, 1<<20, expected)
	if err != nil || decoded.Error == nil || decoded.Error.Category != "invalid_request" {
		t.Fatalf("error response = %#v, %v", decoded, err)
	}
}

func TestCancellationReturns(t *testing.T) {
	handler := newHandler(token)
	body, _ := runtimev1.CanonicalRequest(executeRequest())
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "/plugin/v1/execute", bytes.NewReader(body)).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set(testModeHeader, "runtime/v1")
	request.Header.Set(testCaseHeader, "cancel")
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(httptest.NewRecorder(), request)
		close(done)
	}()
	cancel()
	<-done
}

func request(t *testing.T, handler http.Handler, method, path string, body []byte, testCase string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	if testCase != "" {
		request.Header.Set(testModeHeader, "runtime/v1")
		request.Header.Set(testCaseHeader, testCase)
	}
	handler.ServeHTTP(recorder, request)
	return recorder
}

func executeRequest() runtimev1.ExecuteRequest {
	return runtimev1.ExecuteRequest{SchemaVersion: runtimev1.RequestSchema, RequestID: "req_template", PluginID: "provider.example", PluginVersion: "1.0.0", ManifestDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Operation: "image.generate", Protocol: "openai", Model: "example-image-v1", Input: runtimev1.ImageInput{Prompt: "fixture", Images: 1}}
}
