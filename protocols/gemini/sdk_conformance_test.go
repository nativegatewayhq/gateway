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

func TestOfficialGeminiLLMGenerateContentSDKsManagedSettlement(t *testing.T) {
	models, err := geminioperation.NewRegistryWithLimits([]string{"gemini-2.5-pro"}, map[string]geminioperation.Limits{"gemini-2.5-pro": {MaximumInputTokens: 8192, MaximumOutputTokens: 100}})
	if err != nil {
		t.Fatal(err)
	}
	executor := &sdkExecutor{response: `{"candidates":[{"content":{"role":"model","parts":[{"text":"gateway ok"}]}}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":2,"thoughtsTokenCount":1,"totalTokenCount":4}}`}
	tokenBilling := &geminiLLMBillingFake{}
	principal := apikey.Principal{OrganizationID: "org_sdk", ProjectID: "project_sdk", APIKeyID: "key_sdk"}
	handler := NewBillableHandlerWithLLMTokenBilling(slog.New(slog.NewTextHandler(io.Discard, nil)), sdkAuthenticator{principal: principal}, nil, executor, 8192, &geminiBillingFake{}, tokenBilling, nil, nil, models)
	server := httptest.NewServer(handler)
	defer server.Close()

	python := `from google import genai
from google.genai import types
c=genai.Client(api_key="service-key",http_options=types.HttpOptions(base_url="` + server.URL + `"))
r=c.models.generate_content(model="gemini-2.5-pro",contents="hello",config=types.GenerateContentConfig(max_output_tokens=20))
assert r.text == "gateway ok"`
	command := exec.Command("python3", "-c", python)
	command.Env = append(os.Environ(), "PYTHONPATH=/private/tmp/google-genai-sdk-python")
	if output, runErr := command.CombinedOutput(); runErr != nil {
		t.Fatalf("Python managed Gemini SDK: %v: %s", runErr, output)
	}

	javascript := `const {GoogleGenAI}=require("@google/genai");const c=new GoogleGenAI({apiKey:"service-key",httpOptions:{baseUrl:"` + server.URL + `"}});c.models.generateContent({model:"gemini-2.5-pro",contents:"hello",config:{maxOutputTokens:20}}).then(r=>{if(r.text!=="gateway ok")process.exit(2)}).catch(e=>{console.error(e);process.exit(1)});`
	command = exec.Command("node", "-e", javascript)
	command.Env = append(os.Environ(), "NODE_PATH=/private/tmp/google-genai-sdk-node/node_modules")
	if output, runErr := command.CombinedOutput(); runErr != nil {
		t.Fatalf("JavaScript managed Gemini SDK: %v: %s", runErr, output)
	}
	if tokenBilling.beginCalls != 2 || tokenBilling.beginRequest.Protocol != "gemini" || tokenBilling.beginRequest.MaximumOutputTokens != 20 || tokenBilling.usage.ThoughtsTokens != 1 {
		t.Fatalf("begin=%+v calls=%d usage=%+v", tokenBilling.beginRequest, tokenBilling.beginCalls, tokenBilling.usage)
	}
}

func TestOfficialGeminiLLMStreamingSDKs(t *testing.T) {
	models, err := geminioperation.NewRegistryWithLimits([]string{"gemini-2.5-pro"}, map[string]geminioperation.Limits{"gemini-2.5-pro": {MaximumInputTokens: 8192, MaximumOutputTokens: 100}})
	if err != nil {
		t.Fatal(err)
	}
	stream := "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"gateway \"}]},\"index\":0}],\"usageMetadata\":{\"promptTokenCount\":1,\"candidatesTokenCount\":1,\"totalTokenCount\":2}}\n\n" +
		"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\",\"index\":0}],\"usageMetadata\":{\"promptTokenCount\":1,\"candidatesTokenCount\":2,\"thoughtsTokenCount\":1,\"totalTokenCount\":4}}\n\n"
	executor := &sdkExecutor{streamResponse: stream}
	tokenBilling := &geminiLLMBillingFake{}
	principal := apikey.Principal{OrganizationID: "org_sdk", ProjectID: "project_sdk", APIKeyID: "key_sdk"}
	handler := NewBillableHandlerWithLLMTokenBilling(slog.New(slog.NewTextHandler(io.Discard, nil)), sdkAuthenticator{principal: principal}, nil, executor, 8192, &geminiBillingFake{}, tokenBilling, nil, nil, models)
	server := httptest.NewServer(handler)
	defer server.Close()

	python := `from google import genai
from google.genai import types
c=genai.Client(api_key="service-key",http_options=types.HttpOptions(base_url="` + server.URL + `"))
text=""; usage=None
for chunk in c.models.generate_content_stream(model="gemini-2.5-pro",contents="hello",config=types.GenerateContentConfig(max_output_tokens=20)):
    text += chunk.text or ""
    usage = chunk.usage_metadata or usage
assert text == "gateway ok" and usage.total_token_count == 4`
	command := exec.Command("python3", "-c", python)
	command.Env = append(os.Environ(), "PYTHONPATH=/private/tmp/google-genai-sdk-python")
	if output, runErr := command.CombinedOutput(); runErr != nil {
		t.Fatalf("Python Gemini streaming SDK: %v: %s", runErr, output)
	}

	pythonAsync := `import asyncio
from google import genai
from google.genai import types
async def main():
    c=genai.Client(api_key="service-key",http_options=types.HttpOptions(base_url="` + server.URL + `"))
    text=""; usage=None
    async for chunk in await c.aio.models.generate_content_stream(model="gemini-2.5-pro",contents="hello",config=types.GenerateContentConfig(max_output_tokens=20)):
        text += chunk.text or ""
        usage = chunk.usage_metadata or usage
    assert text == "gateway ok" and usage.total_token_count == 4
asyncio.run(main())`
	command = exec.Command("python3", "-c", pythonAsync)
	command.Env = append(os.Environ(), "PYTHONPATH=/private/tmp/google-genai-sdk-python")
	if output, runErr := command.CombinedOutput(); runErr != nil {
		t.Fatalf("Python async Gemini streaming SDK: %v: %s", runErr, output)
	}

	javascript := `const {GoogleGenAI}=require("@google/genai");(async()=>{const c=new GoogleGenAI({apiKey:"service-key",httpOptions:{baseUrl:"` + server.URL + `"}});const controller=new AbortController();const s=await c.models.generateContentStream({model:"gemini-2.5-pro",contents:"hello",config:{maxOutputTokens:20,abortSignal:controller.signal}});let text="",usage;for await(const chunk of s){text+=chunk.text||"";usage=chunk.usageMetadata||usage}if(text!=="gateway ok"||usage.totalTokenCount!==4)process.exit(2)})().catch(e=>{console.error(e);process.exit(1)});`
	command = exec.Command("node", "-e", javascript)
	command.Env = append(os.Environ(), "NODE_PATH=/private/tmp/google-genai-sdk-node/node_modules")
	if output, runErr := command.CombinedOutput(); runErr != nil {
		t.Fatalf("JavaScript Gemini streaming SDK: %v: %s", runErr, output)
	}
	if executor.calls != 3 || tokenBilling.beginCalls != 3 || !tokenBilling.streamComplete || tokenBilling.usage.ThoughtsTokens != 1 {
		t.Fatalf("calls=%d begin=%d billing=%+v", executor.calls, tokenBilling.beginCalls, tokenBilling)
	}
}

type sdkAuthenticator struct{ principal apikey.Principal }

func (auth sdkAuthenticator) Authenticate(context.Context, string) (apikey.Principal, error) {
	return auth.principal, nil
}

type sdkExecutor struct {
	response       string
	streamResponse string
	bodies         []string
	calls          int
}

func (executor *sdkExecutor) GenerateContent(_ context.Context, request google.GenerateContentRequest) (*http.Response, error) {
	body, _ := io.ReadAll(request.Body)
	executor.calls++
	executor.bodies = append(executor.bodies, string(body))
	if request.Streaming {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(executor.streamResponse))}, nil
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(executor.response))}, nil
}
