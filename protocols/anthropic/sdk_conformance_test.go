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
