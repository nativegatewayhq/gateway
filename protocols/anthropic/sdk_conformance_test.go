//go:build sdkconformance

package anthropic

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
	operation "github.com/nativegatewayhq/gateway/operations/anthropic"
	provider "github.com/nativegatewayhq/gateway/providers/anthropic"
)

func TestOfficialAnthropicMessagesSDKs(t *testing.T) {
	models, _ := operation.NewRegistry([]string{"claude-test"})
	handler := NewHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), sdkAuth{}, models, sdkExecutor{}, sdkAvailable{}, nil, 8192, false)
	server := httptest.NewServer(handler)
	defer server.Close()
	python := `from anthropic import Anthropic
c=Anthropic(api_key="service-key",base_url="` + server.URL + `")
r=c.messages.create(model="claude-test",max_tokens=16,messages=[{"role":"user","content":"hello"}])
assert r.content[0].text == "gateway ok" and r.usage.output_tokens == 2`
	command := exec.Command("python3", "-c", python)
	command.Env = append(os.Environ(), "PYTHONPATH=/private/tmp/anthropic-sdk-python")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Python SDK: %v: %s", err, output)
	}
	pythonAsync := `import asyncio
from anthropic import AsyncAnthropic
async def main():
 c=AsyncAnthropic(api_key="service-key",base_url="` + server.URL + `")
 r=await c.messages.create(model="claude-test",max_tokens=16,messages=[{"role":"user","content":"hello"}])
 assert r.content[0].text == "gateway ok"
asyncio.run(main())`
	command = exec.Command("python3", "-c", pythonAsync)
	command.Env = append(os.Environ(), "PYTHONPATH=/private/tmp/anthropic-sdk-python")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Python async SDK: %v: %s", err, output)
	}
	javascript := `const Anthropic=require('@anthropic-ai/sdk').default;const c=new Anthropic({apiKey:'service-key',baseURL:'` + server.URL + `'});c.messages.create({model:'claude-test',max_tokens:16,messages:[{role:'user',content:'hello'}]}).then(r=>{if(r.content[0].text!=='gateway ok')process.exit(2)}).catch(e=>{console.error(e);process.exit(1)});`
	command = exec.Command("node", "-e", javascript)
	command.Env = append(os.Environ(), "NODE_PATH=/private/tmp/anthropic-sdk-node/node_modules")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("TypeScript SDK: %v: %s", err, output)
	}
}

func TestOfficialAnthropicManagedMessagesSDKs(t *testing.T) {
	models, _ := operation.NewRegistryWithLimits([]string{"claude-test"}, map[string]operation.Limits{"claude-test": {MaximumInputTokens: 8192, MaximumOutputTokens: 100}})
	charges := &billingStub{}
	handler := NewBillableHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), sdkAuth{}, models, &managedExecutor{}, sdkAvailable{}, nil, 8192, charges)
	server := httptest.NewServer(handler)
	defer server.Close()
	python := `from anthropic import Anthropic
c=Anthropic(api_key="service-key",base_url="` + server.URL + `")
r=c.messages.create(model="claude-test",max_tokens=16,messages=[{"role":"user","content":"hello"}])
assert r.content[0].text == "ok"`
	command := exec.Command("python3", "-c", python)
	command.Env = append(os.Environ(), "PYTHONPATH=/private/tmp/anthropic-sdk-python")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Python managed SDK: %v: %s", err, output)
	}
	javascript := `const Anthropic=require('@anthropic-ai/sdk').default;const c=new Anthropic({apiKey:'service-key',baseURL:'` + server.URL + `'});c.messages.create({model:'claude-test',max_tokens:16,messages:[{role:'user',content:'hello'}]}).then(r=>{if(r.content[0].text!=='ok')process.exit(2)}).catch(e=>{console.error(e);process.exit(1)});`
	command = exec.Command("node", "-e", javascript)
	command.Env = append(os.Environ(), "NODE_PATH=/private/tmp/anthropic-sdk-node/node_modules")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("TypeScript managed SDK: %v: %s", err, output)
	}
	if charges.complete != 2 || charges.usage.CacheWriteTokens != 2 || charges.usage.CachedInputTokens != 3 {
		t.Fatalf("billing=%+v", charges)
	}
}

func TestOfficialAnthropicStreamingSDKs(t *testing.T) {
	models, _ := operation.NewRegistryWithLimits([]string{"claude-test"}, map[string]operation.Limits{"claude-test": {MaximumInputTokens: 8192, MaximumOutputTokens: 100}})
	charges := &billingStub{}
	handler := NewBillableHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), sdkAuth{}, models, sdkStreamExecutor{}, sdkAvailable{}, nil, 8192, charges)
	server := httptest.NewServer(handler)
	defer server.Close()
	python := `from anthropic import Anthropic
c=Anthropic(api_key="service-key",base_url="` + server.URL + `")
with c.messages.stream(model="claude-test",max_tokens=16,messages=[{"role":"user","content":"hello"}]) as s:
 assert s.get_final_text() == "gateway ok"`
	command := exec.Command("python3", "-c", python)
	command.Env = append(os.Environ(), "PYTHONPATH=/private/tmp/anthropic-sdk-python")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Python streaming SDK: %v: %s", err, output)
	}
	pythonAsync := `import asyncio
from anthropic import AsyncAnthropic
async def main():
 c=AsyncAnthropic(api_key="service-key",base_url="` + server.URL + `")
 async with c.messages.stream(model="claude-test",max_tokens=16,messages=[{"role":"user","content":"hello"}]) as s:
  assert await s.get_final_text() == "gateway ok"
asyncio.run(main())`
	command = exec.Command("python3", "-c", pythonAsync)
	command.Env = append(os.Environ(), "PYTHONPATH=/private/tmp/anthropic-sdk-python")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Python async streaming SDK: %v: %s", err, output)
	}
	javascript := `const Anthropic=require('@anthropic-ai/sdk').default;(async()=>{const c=new Anthropic({apiKey:'service-key',baseURL:'` + server.URL + `'});const s=c.messages.stream({model:'claude-test',max_tokens:16,messages:[{role:'user',content:'hello'}]});const m=await s.finalMessage();if(m.content[0].text!=='gateway ok')process.exit(2)})().catch(e=>{console.error(e);process.exit(1)});`
	command = exec.Command("node", "-e", javascript)
	command.Env = append(os.Environ(), "NODE_PATH=/private/tmp/anthropic-sdk-node/node_modules")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("TypeScript streaming SDK: %v: %s", err, output)
	}
	if charges.complete != 3 || charges.usage.CompletionTokens != 4 {
		t.Fatalf("billing=%+v", charges)
	}
}

type sdkStreamExecutor struct{}

func (sdkStreamExecutor) CreateMessage(_ context.Context, request provider.MessagesRequest) (*http.Response, error) {
	if !request.Streaming {
		return nil, provider.ErrInvalidRequest
	}
	stream := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_stream\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-test\",\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":5,\"cache_creation_input_tokens\":2,\"cache_read_input_tokens\":3,\"output_tokens\":0}}}\n\n" + "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" + "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"gateway ok\"}}\n\n" + "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" + "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":4}}\n\n" + "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}, nil
}

type sdkAuth struct{}

func (sdkAuth) Authenticate(context.Context, string) (apikey.Principal, error) {
	return apikey.Principal{}, nil
}

type sdkAvailable struct{}

func (sdkAvailable) ConfiguredChannel(context.Context, string, providercredentials.ProviderID) bool {
	return true
}

type sdkExecutor struct{}

func (sdkExecutor) CreateMessage(context.Context, provider.MessagesRequest) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"msg_sdk","type":"message","role":"assistant","model":"claude-test","content":[{"type":"text","text":"gateway ok"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":2}}`))}, nil
}
