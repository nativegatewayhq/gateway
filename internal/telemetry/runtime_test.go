package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestRuntimeExportsBothSignalsAndShutsDown(t *testing.T) {
	var lock sync.Mutex
	paths := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer exporter-secret" || request.Header.Get("Content-Type") != "application/x-protobuf" {
			t.Errorf("headers=%v", request.Header)
		}
		lock.Lock()
		paths[request.URL.Path]++
		lock.Unlock()
		writer.Header().Set("Content-Type", "application/x-protobuf")
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	config := DefaultConfig()
	config.Mode, config.Endpoint, config.Authorization, config.SampleRatio = Optional, server.URL+"/collector", "Bearer exporter-secret", 1
	config.ExportInterval, config.ExportTimeout, config.ShutdownTimeout = time.Second, time.Second, 3*time.Second
	runtime, err := New(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	handler := runtime.Recorder.Middleware(runtime.Propagator, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusOK) }))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/health/live", nil))
	if err := runtime.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	lock.Lock()
	defer lock.Unlock()
	if paths["/collector/v1/traces"] == 0 || paths["/collector/v1/metrics"] == 0 {
		t.Fatalf("paths=%v", paths)
	}
}
