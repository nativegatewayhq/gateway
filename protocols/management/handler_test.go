package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nativegatewayhq/gateway/internal/apikey"
	"github.com/nativegatewayhq/gateway/internal/jobs"
	joboperation "github.com/nativegatewayhq/gateway/operations/job"
)

type authStub struct {
	principal apikey.Principal
	err       error
}

func (a authStub) Authenticate(_ context.Context, raw string) (apikey.Principal, error) {
	if raw != "secret" && a.err == nil {
		return apikey.Principal{}, errors.New("bad key")
	}
	return a.principal, a.err
}

type repoStub struct {
	list   jobs.ManagementListRequest
	detail jobs.ManagementDetail
	err    error
}

func (s *repoStub) ListManagement(_ context.Context, r jobs.ManagementListRequest) ([]jobs.ManagementJob, bool, error) {
	s.list = r
	return []jobs.ManagementJob{{ID: "job_00000000000000000000000000000001", CreatedAt: time.Unix(10, 0)}}, true, s.err
}
func (s *repoStub) GetManagement(_ context.Context, _ joboperation.Owner, _ string, allow func(string, string, string) bool) (jobs.ManagementDetail, error) {
	if !allow("fal", "image.generate", "model-a") {
		return jobs.ManagementDetail{}, joboperation.ErrNotFound
	}
	return s.detail, s.err
}

func TestCursorBindingRotationAndTamper(t *testing.T) {
	old := []byte(strings.Repeat("o", 32))
	active := []byte(strings.Repeat("a", 32))
	payload := CursorPayload{CreatedAt: time.Unix(10, 0), ID: "job_00000000000000000000000000000001"}
	oldCodec, _ := NewCursorCodec([][]byte{old})
	token, _ := oldCodec.Encode(payload, "binding")
	rotated, _ := NewCursorCodec([][]byte{active, old})
	if _, err := rotated.Decode(token, "binding", time.Unix(20, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := rotated.Decode(token+"x", "binding", time.Unix(20, 0)); err == nil {
		t.Fatal("tampered cursor accepted")
	}
	if _, err := rotated.Decode(token, "different", time.Unix(20, 0)); err == nil {
		t.Fatal("cross-binding cursor accepted")
	}
}

func TestHandlerListBindsOwnerAndModelPolicy(t *testing.T) {
	repo := &repoStub{}
	principal := apikey.Principal{OrganizationID: "org", ProjectID: "project", APIKeyID: "key", ModelAccessMode: apikey.ModelAccessAllowlist, ModelPermissions: []apikey.ModelPermission{{Protocol: "fal", Operation: "image.generate", Model: "model-a"}, {Protocol: "openai", Operation: "image.generate", Model: "hidden"}}}
	h, err := NewHandler(authStub{principal: principal}, repo, [][]byte{[]byte(strings.Repeat("s", 32))})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/gateway/v1/jobs?protocol=fal&limit=10", nil)
	r.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if repo.list.Owner.APIKeyID != "key" || len(repo.list.AllowedModels) != 1 || repo.list.AllowedModels[0].Model != "model-a" {
		t.Fatalf("unsafe repository request: %#v", repo.list)
	}
	var body map[string]any
	if json.Unmarshal(w.Body.Bytes(), &body) != nil || body["next_cursor"] == "" {
		t.Fatal("missing cursor")
	}
}

func TestHandlerRejectsDuplicateQueryAndHidesUnauthorizedDetail(t *testing.T) {
	principal := apikey.Principal{OrganizationID: "org", ProjectID: "project", APIKeyID: "key", ModelAccessMode: apikey.ModelAccessAllowlist, ModelPermissions: []apikey.ModelPermission{{Protocol: "fal", Operation: "image.generate", Model: "other"}}}
	h, _ := NewHandler(authStub{principal: principal}, &repoStub{}, [][]byte{[]byte(strings.Repeat("s", 32))})
	for _, target := range []string{"/gateway/v1/jobs?limit=1&limit=2", "/gateway/v1/jobs/job_00000000000000000000000000000001"} {
		r := httptest.NewRequest(http.MethodGet, target, nil)
		r.Header.Set("Authorization", "Bearer secret")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		want := http.StatusBadRequest
		if strings.Contains(target, "/job_") {
			want = http.StatusNotFound
		}
		if w.Code != want {
			t.Fatalf("%s status=%d", target, w.Code)
		}
	}
}
