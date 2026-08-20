//go:build sdkconformance

package runway

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/nativegatewayhq/gateway/internal/apikey"
	joboperation "github.com/nativegatewayhq/gateway/operations/job"
	videooperation "github.com/nativegatewayhq/gateway/operations/video"
)

func TestOfficialRunwaySDKsUseOnlyBaseURLAndKey(t *testing.T) {
	models, _ := videooperation.NewRegistry([]string{"gen4_turbo"})
	principal := apikey.Principal{OrganizationID: "org", ProjectID: "project", APIKeyID: "key", ModelAccessMode: apikey.ModelAccessAll}
	service := &jobsStub{job: joboperation.Job{ID: "job_0123456789abcdef0123456789abcdef", Protocol: "runway", Model: "gen4_turbo", Status: joboperation.Processing, CreatedAt: time.Now().UTC()}}
	handler := NewHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), authStub{principal}, models, service, 1<<20)
	server := httptest.NewServer(handler)
	defer server.Close()
	python := `import asyncio
from runwayml import RunwayML, AsyncRunwayML
c=RunwayML(api_key="service-key",base_url="` + server.URL + `",max_retries=0)
a=c.text_to_video.create(model="gen4_turbo",prompt_text="hello",ratio="1280:720",duration=5)
b=c.image_to_video.create(model="gen4_turbo",prompt_image="https://example.com/input.png",ratio="1280:720",prompt_text="hello")
assert a.id.startswith("job_") and b.id.startswith("job_")
task=c.tasks.retrieve(a.id)
assert task.id==a.id and task.status=="RUNNING"
c.tasks.delete(a.id)
async def main():
 c=AsyncRunwayML(api_key="service-key",base_url="` + server.URL + `",max_retries=0)
 a=await c.text_to_video.create(model="gen4_turbo",prompt_text="hello",ratio="1280:720",duration=5)
 assert a.id.startswith("job_")
 await c.close()
asyncio.run(main())`
	command := exec.Command("python3", "-c", python)
	command.Env = append(os.Environ(), "PYTHONPATH=/private/tmp/runway-sdk-python-deps")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Python SDK: %v: %s", err, output)
	}
	javascript := `const RunwayML=require('@runwayml/sdk').default;(async()=>{const c=new RunwayML({apiKey:'service-key',baseURL:'` + server.URL + `',maxRetries:0});const a=await c.textToVideo.create({model:'gen4_turbo',promptText:'hello',ratio:'1280:720',duration:5});const b=await c.imageToVideo.create({model:'gen4_turbo',promptImage:'https://example.com/input.png',ratio:'1280:720',promptText:'hello'});if(!a.id.startsWith('job_')||!b.id.startsWith('job_'))process.exit(2);const task=await c.tasks.retrieve(a.id);if(task.status!=='RUNNING'||task.id!==a.id)process.exit(3);await c.tasks.delete(a.id)})().catch(e=>{console.error(e);process.exit(1)});`
	command = exec.Command("node", "-e", javascript)
	command.Env = append(os.Environ(), "NODE_PATH=/private/tmp/runway-sdk-node/node_modules")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("JavaScript SDK: %v: %s", err, output)
	}
	if service.submitted != 5 {
		t.Fatalf("provider submissions=%d", service.submitted)
	}
}
