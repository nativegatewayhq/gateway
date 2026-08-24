//go:build sdkconformance

package replicate

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"

	"github.com/nativegatewayhq/gateway/internal/apikey"
)

func TestOfficialReplicatePythonSDKUsesPluginRouteWithOnlyBaseURLAndKey(t *testing.T) {
	service := &jobsStub{}
	handler := NewHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), authStub{principal: apikey.Principal{APIKeyID: "key", ProjectID: "project", OrganizationID: "org"}}, pluginModelsStub{}, service, nil, nil, 1<<20, "https://gateway.example")
	server := httptest.NewServer(handler)
	defer server.Close()
	python := `from replicate.client import Client
c=Client(api_token="service-key",base_url="` + server.URL + `")
p=c.predictions.create(version="example-async-image-v1",input={"prompt":"draw a cat"})
assert p.id=="job_00000000000000000000000000000000"
assert c.predictions.get(p.id).id==p.id`
	command := exec.Command("python3", "-c", python)
	command.Env = append(os.Environ(), "PYTHONPATH=/private/tmp/replicate-sdk-python")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Replicate Python plugin route: %v: %s", err, output)
	}
	if service.request.Provider != "plugin" {
		t.Fatalf("provider=%s", service.request.Provider)
	}
}
