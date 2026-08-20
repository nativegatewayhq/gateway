//go:build sdkconformance

package openai

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
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	"github.com/nativegatewayhq/gateway/internal/providerhealth"
	chatoperation "github.com/nativegatewayhq/gateway/operations/chat"
	responsesoperation "github.com/nativegatewayhq/gateway/operations/responses"
	openaiProvider "github.com/nativegatewayhq/gateway/providers/openai"
)

func TestOfficialOpenAISDKsUseOnlyBaseURLAndKey(t *testing.T) {
	handler := chatHandler(t, authFunc(func(context.Context, string) (apikey.Principal, error) { return apikey.Principal{}, nil }), chatExecutorFunc(func(context.Context, openaiProvider.ChatRequest) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"chatcmpl_sdk","object":"chat.completion","created":1,"model":"gpt-4.1","choices":[{"index":0,"message":{"role":"assistant","content":"gateway ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`))}, nil
	}), 4096)
	server := httptest.NewServer(handler)
	defer server.Close()
	python := `from openai import OpenAI
c=OpenAI(api_key="service-key",base_url="` + server.URL + `/v1")
r=c.chat.completions.create(model="gpt-4.1",messages=[{"role":"user","content":"hello"}])
assert r.choices[0].message.content == "gateway ok"`
	command := exec.Command("python3", "-c", python)
	command.Env = append(os.Environ(), "PYTHONPATH=/private/tmp/openai-sdk-python")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Python SDK: %v: %s", err, output)
	}
	javascript := `const OpenAI=require("openai").default; const c=new OpenAI({apiKey:"service-key",baseURL:"` + server.URL + `/v1"}); c.chat.completions.create({model:"gpt-4.1",messages:[{role:"user",content:"hello"}]}).then(r=>{if(r.choices[0].message.content!=="gateway ok")process.exit(2)}).catch(e=>{console.error(e);process.exit(1)});`
	command = exec.Command("node", "-e", javascript)
	command.Env = append(os.Environ(), "NODE_PATH=/private/tmp/openai-sdk-node/node_modules")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("JavaScript SDK: %v: %s", err, output)
	}
}

func TestOfficialOpenAIStreamingSDKs(t *testing.T) {
	stream := "data: {\"id\":\"chatcmpl_stream\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-4.1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"chatcmpl_stream\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-4.1\",\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\ndata: [DONE]\n\n"
	handler := chatHandler(t, authFunc(func(context.Context, string) (apikey.Principal, error) { return apikey.Principal{}, nil }), chatExecutorFunc(func(_ context.Context, request openaiProvider.ChatRequest) (*http.Response, error) {
		body, _ := io.ReadAll(request.Body)
		if !strings.Contains(string(body), `"stream":true`) {
			t.Fatalf("request=%s", body)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}, nil
	}), 4096)
	server := httptest.NewServer(handler)
	defer server.Close()
	python := `from openai import OpenAI
c=OpenAI(api_key="service-key",base_url="` + server.URL + `/v1")
s=c.chat.completions.create(model="gpt-4.1",messages=[{"role":"user","content":"hello"}],stream=True,stream_options={"include_usage":True})
chunks=list(s)
assert "".join((x.choices[0].delta.content or "") for x in chunks if x.choices) == "hi"
assert chunks[-1].usage.total_tokens == 2`
	command := exec.Command("python3", "-c", python)
	command.Env = append(os.Environ(), "PYTHONPATH=/private/tmp/openai-sdk-python")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Python streaming SDK: %v: %s", err, output)
	}
	javascript := `const OpenAI=require("openai").default; (async()=>{const c=new OpenAI({apiKey:"service-key",baseURL:"` + server.URL + `/v1"});const controller=new AbortController();const s=await c.chat.completions.create({model:"gpt-4.1",messages:[{role:"user",content:"hello"}],stream:true,stream_options:{include_usage:true}},{signal:controller.signal});let text="",usage;for await(const x of s){if(x.choices.length)text+=x.choices[0].delta.content||"";if(x.usage)usage=x.usage}if(text!=="hi"||usage.total_tokens!==2)process.exit(2)})().catch(e=>{console.error(e);process.exit(1)});`
	command = exec.Command("node", "-e", javascript)
	command.Env = append(os.Environ(), "NODE_PATH=/private/tmp/openai-sdk-node/node_modules")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("JavaScript streaming SDK: %v: %s", err, output)
	}
}

func TestOfficialOpenAISDKsUseLogicalModelThroughXAICompatibleRoute(t *testing.T) {
	registry, err := chatoperation.NewRouteRegistry([]chatoperation.Route{{Model: "logical-chat", Owner: "gateway", Policy: chatoperation.Priority, Candidates: []chatoperation.Candidate{{ID: "candidate_xai", Provider: providercredentials.XAI, ProviderModel: "grok-provider", ChannelID: "channel_00000000000000000000000000000002", Enabled: true, Capabilities: chatoperation.Capabilities{Streaming: true, Tools: true, JSONMode: true}}}}})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewRoutedChatHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), authFunc(func(context.Context, string) (apikey.Principal, error) { return apikey.Principal{}, nil }), registry, map[providercredentials.ProviderID]ChatExecutor{providercredentials.XAI: chatExecutorFunc(func(_ context.Context, request openaiProvider.ChatRequest) (*http.Response, error) {
		body, _ := io.ReadAll(request.Body)
		if !strings.Contains(string(body), `"model":"grok-provider"`) || !strings.Contains(string(body), `"tools"`) {
			t.Fatalf("routed SDK request=%s", body)
		}
		if request.Streaming {
			stream := "data: {\"id\":\"chatcmpl_route_stream\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"grok-provider\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"routed stream\"},\"finish_reason\":null}]}\n\ndata: [DONE]\n\n"
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}, nil
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"chatcmpl_route","object":"chat.completion","created":1,"model":"grok-provider","choices":[{"index":0,"message":{"role":"assistant","content":"routed ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`))}, nil
	})}, channelAvailability{"channel_00000000000000000000000000000002": true}, providerhealth.NoopGate{}, 8192)
	server := httptest.NewServer(handler)
	defer server.Close()
	python := `from openai import OpenAI
c=OpenAI(api_key="service-key",base_url="` + server.URL + `/v1")
r=c.chat.completions.create(model="logical-chat",messages=[{"role":"user","content":"hello"}],tools=[{"type":"function","function":{"name":"lookup","description":"lookup","parameters":{"type":"object","properties":{}}}}])
assert r.choices[0].message.content == "routed ok"`
	command := exec.Command("python3", "-c", python)
	command.Env = append(os.Environ(), "PYTHONPATH=/private/tmp/openai-sdk-python")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Python routed SDK: %v: %s", err, output)
	}
	javascript := `const OpenAI=require("openai").default;const c=new OpenAI({apiKey:"service-key",baseURL:"` + server.URL + `/v1"});c.chat.completions.create({model:"logical-chat",messages:[{role:"user",content:"hello"}],tools:[{type:"function",function:{name:"lookup",description:"lookup",parameters:{type:"object",properties:{}}}}]}).then(r=>{if(r.choices[0].message.content!=="routed ok")process.exit(2)}).catch(e=>{console.error(e);process.exit(1)});`
	command = exec.Command("node", "-e", javascript)
	command.Env = append(os.Environ(), "NODE_PATH=/private/tmp/openai-sdk-node/node_modules")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("JavaScript routed SDK: %v: %s", err, output)
	}
	pythonStream := `from openai import OpenAI
c=OpenAI(api_key="service-key",base_url="` + server.URL + `/v1")
s=c.chat.completions.create(model="logical-chat",messages=[{"role":"user","content":"hello"}],tools=[{"type":"function","function":{"name":"lookup","description":"lookup","parameters":{"type":"object","properties":{}}}}],stream=True)
assert "".join((x.choices[0].delta.content or "") for x in s if x.choices) == "routed stream"`
	command = exec.Command("python3", "-c", pythonStream)
	command.Env = append(os.Environ(), "PYTHONPATH=/private/tmp/openai-sdk-python")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Python routed streaming SDK: %v: %s", err, output)
	}
	javascriptStream := `const OpenAI=require("openai").default;(async()=>{const c=new OpenAI({apiKey:"service-key",baseURL:"` + server.URL + `/v1"});const s=await c.chat.completions.create({model:"logical-chat",messages:[{role:"user",content:"hello"}],tools:[{type:"function",function:{name:"lookup",description:"lookup",parameters:{type:"object",properties:{}}}}],stream:true});let text="";for await(const x of s){if(x.choices.length)text+=x.choices[0].delta.content||""}if(text!=="routed stream")process.exit(2)})().catch(e=>{console.error(e);process.exit(1)});`
	command = exec.Command("node", "-e", javascriptStream)
	command.Env = append(os.Environ(), "NODE_PATH=/private/tmp/openai-sdk-node/node_modules")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("JavaScript routed streaming SDK: %v: %s", err, output)
	}
}

func TestOfficialOpenAIResponsesSDKs(t *testing.T) {
	registry, _ := responsesoperation.NewRegistry([]string{"gpt-4.1"})
	handler := NewResponsesHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), authFunc(func(context.Context, string) (apikey.Principal, error) { return apikey.Principal{}, nil }), registry, responsesExecutorFunc(func(context.Context, openaiProvider.ResponsesRequest) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"resp_sdk","object":"response","created_at":1,"status":"completed","model":"gpt-4.1","output":[{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"gateway ok","annotations":[]}]}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`))}, nil
	}), channelAvailability{"channel_00000000000000000000000000000001": true}, 4096)
	server := httptest.NewServer(handler)
	defer server.Close()
	python := `from openai import OpenAI
c=OpenAI(api_key="service-key",base_url="` + server.URL + `/v1")
r=c.responses.create(model="gpt-4.1",input="hello")
assert r.output_text == "gateway ok"`
	command := exec.Command("python3", "-c", python)
	command.Env = append(os.Environ(), "PYTHONPATH=/private/tmp/openai-sdk-python")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Python Responses SDK: %v: %s", err, output)
	}
	javascript := `const OpenAI=require("openai").default;const c=new OpenAI({apiKey:"service-key",baseURL:"` + server.URL + `/v1"});c.responses.create({model:"gpt-4.1",input:"hello"}).then(r=>{if(r.output_text!=="gateway ok")process.exit(2)}).catch(e=>{console.error(e);process.exit(1)});`
	command = exec.Command("node", "-e", javascript)
	command.Env = append(os.Environ(), "NODE_PATH=/private/tmp/openai-sdk-node/node_modules")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("JavaScript Responses SDK: %v: %s", err, output)
	}
}

func TestOfficialOpenAIResponsesStreamingSDKs(t *testing.T) {
	registry, _ := responsesoperation.NewRegistry([]string{"gpt-4.1"})
	stream := "event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_sdk\",\"object\":\"response\",\"created_at\":1,\"status\":\"in_progress\",\"model\":\"gpt-4.1\",\"output\":[]}}\n\n" +
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"sequence_number\":1,\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"gateway ok\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"sequence_number\":2,\"response\":{\"id\":\"resp_sdk\",\"object\":\"response\",\"created_at\":1,\"status\":\"completed\",\"model\":\"gpt-4.1\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":2,\"total_tokens\":3}}}\n\n"
	handler := NewResponsesHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), authFunc(func(context.Context, string) (apikey.Principal, error) { return apikey.Principal{}, nil }), registry, responsesExecutorFunc(func(_ context.Context, request openaiProvider.ResponsesRequest) (*http.Response, error) {
		if !request.Streaming {
			t.Fatal("streaming request flag missing")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}, nil
	}), channelAvailability{"channel_00000000000000000000000000000001": true}, 8192)
	server := httptest.NewServer(handler)
	defer server.Close()
	python := `from openai import OpenAI
c=OpenAI(api_key="service-key",base_url="` + server.URL + `/v1")
s=c.responses.create(model="gpt-4.1",input="hello",stream=True)
events=list(s)
assert any(e.type == "response.output_text.delta" and e.delta == "gateway ok" for e in events)
assert events[-1].type == "response.completed"`
	command := exec.Command("python3", "-c", python)
	command.Env = append(os.Environ(), "PYTHONPATH=/private/tmp/openai-sdk-python")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Python Responses streaming SDK: %v: %s", err, output)
	}
	pythonAsync := `import asyncio
from openai import AsyncOpenAI
async def main():
    c=AsyncOpenAI(api_key="service-key",base_url="` + server.URL + `/v1")
    s=await c.responses.create(model="gpt-4.1",input="hello",stream=True)
    text=""
    terminal=""
    async for e in s:
        if e.type == "response.output_text.delta": text += e.delta
        if e.type == "response.completed": terminal=e.type
    assert text == "gateway ok" and terminal == "response.completed"
asyncio.run(main())`
	command = exec.Command("python3", "-c", pythonAsync)
	command.Env = append(os.Environ(), "PYTHONPATH=/private/tmp/openai-sdk-python")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Python async Responses streaming SDK: %v: %s", err, output)
	}
	javascript := `const OpenAI=require("openai").default;(async()=>{const c=new OpenAI({apiKey:"service-key",baseURL:"` + server.URL + `/v1"});const controller=new AbortController();const s=await c.responses.create({model:"gpt-4.1",input:"hello",stream:true},{signal:controller.signal});let text="",terminal="";for await(const e of s){if(e.type==="response.output_text.delta")text+=e.delta;if(e.type.startsWith("response.")&&(e.type.endsWith("completed")||e.type.endsWith("failed")||e.type.endsWith("incomplete")))terminal=e.type}if(text!=="gateway ok"||terminal!=="response.completed")process.exit(2)})().catch(e=>{console.error(e);process.exit(1)});`
	command = exec.Command("node", "-e", javascript)
	command.Env = append(os.Environ(), "NODE_PATH=/private/tmp/openai-sdk-node/node_modules")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("JavaScript Responses streaming SDK: %v: %s", err, output)
	}
}
