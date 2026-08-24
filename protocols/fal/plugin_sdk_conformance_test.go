//go:build sdkconformance

package fal

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"

	"github.com/nativegatewayhq/gateway/internal/apikey"
)

func TestOfficialFalJavaScriptSDKUsesPluginRouteWithOnlyBaseURLAndKey(t *testing.T) {
	service := &jobsStub{}
	principal := apikey.Principal{APIKeyID: "key", ProjectID: "project", OrganizationID: "org"}
	handler := NewHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), authStub{principal}, pluginModelsStub{}, service, nil, nil, 1<<20, "https://gateway.example")
	server := httptest.NewServer(handler)
	defer server.Close()
	javascript := `const {fal}=require("@fal-ai/client");(async()=>{fal.config({credentials:"service-key",proxyUrl:{url:"` + server.URL + `/fal/proxy",when:"always"}});const q=await fal.queue.submit("fal-ai/example-async-image-v1",{input:{prompt:"draw a cat"}});if(q.request_id!=="job_00000000000000000000000000000000")process.exit(2);const s=await fal.queue.status("fal-ai/example-async-image-v1",{requestId:q.request_id,logs:false});if(!s.status)process.exit(3)})().catch(e=>{console.error(e);process.exit(1)})`
	command := exec.Command("node", "-e", javascript)
	command.Env = append(os.Environ(), "NODE_PATH=/private/tmp/fal-sdk-node/node_modules")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("fal JavaScript plugin route: %v: %s", err, output)
	}
	if service.request.Provider != "plugin" {
		t.Fatalf("provider=%s", service.request.Provider)
	}
}
