//go:build sdkconformance

package gemini

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/nativegatewayhq/gateway/internal/apikey"
	geminioperation "github.com/nativegatewayhq/gateway/operations/gemini"
	"github.com/nativegatewayhq/gateway/providers/google"
)

func TestOfficialGeminiLLMGenerateContentSDKs(t *testing.T) {
	models, err := geminioperation.NewRegistry([]string{"gemini-2.5-pro"})
	if err != nil {
		t.Fatal(err)
	}
	executor := &sdkExecutor{response: `{"candidates":[{"content":{"role":"model","parts":[{"text":"gateway ok"}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":2,"totalTokenCount":3}}`}
	handler := NewHandlerWithLLMModels(slog.New(slog.NewTextHandler(io.Discard, nil)), sdkAuthenticator{}, executor, 8192, nil, models)
	server := httptest.NewServer(handler)
	defer server.Close()

	python := `from google import genai
from google.genai import types
c=genai.Client(api_key="service-key",http_options=types.HttpOptions(base_url="` + server.URL + `"))
r=c.models.generate_content(model="gemini-2.5-pro",contents="hello",config=types.GenerateContentConfig(system_instruction="system"))
assert r.text == "gateway ok"`
	command := exec.Command("python3", "-c", python)
	command.Env = append(os.Environ(), "PYTHONPATH=/private/tmp/google-genai-sdk-python")
	if output, runErr := command.CombinedOutput(); runErr != nil {
		t.Fatalf("Python Gemini SDK: %v: %s", runErr, output)
	}

	javascript := `const {GoogleGenAI}=require("@google/genai");const c=new GoogleGenAI({apiKey:"service-key",httpOptions:{baseUrl:"` + server.URL + `"}});c.models.generateContent({model:"gemini-2.5-pro",contents:"hello",config:{systemInstruction:"system"}}).then(r=>{if(r.text!=="gateway ok")process.exit(2)}).catch(e=>{console.error(e);process.exit(1)});`
	command = exec.Command("node", "-e", javascript)
	command.Env = append(os.Environ(), "NODE_PATH=/private/tmp/google-genai-sdk-node/node_modules")
	if output, runErr := command.CombinedOutput(); runErr != nil {
		t.Fatalf("JavaScript Gemini SDK: %v: %s", runErr, output)
	}
	if executor.calls != 2 || !strings.Contains(executor.bodies[0], "systemInstruction") || !strings.Contains(executor.bodies[1], "systemInstruction") {
		t.Fatalf("calls=%d bodies=%v", executor.calls, executor.bodies)
	}
}

type sdkAuthenticator struct{}

func (sdkAuthenticator) Authenticate(context.Context, string) (apikey.Principal, error) {
	return apikey.Principal{}, nil
}

type sdkExecutor struct {
	response string
	bodies   []string
	calls    int
}

func (executor *sdkExecutor) GenerateContent(_ context.Context, request google.GenerateContentRequest) (*http.Response, error) {
	body, _ := io.ReadAll(request.Body)
	executor.calls++
	executor.bodies = append(executor.bodies, string(body))
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(executor.response))}, nil
}
