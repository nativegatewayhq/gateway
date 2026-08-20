package job

import (
	"crypto/sha256"
	"encoding/json"
	"strings"
	"testing"
)

func TestTransitionMatrixAndTerminalImmutability(t *testing.T) {
	allowed := [][2]Status{{Pending, Queued}, {Pending, Processing}, {Queued, Processing}, {Queued, Succeeded}, {Processing, Failed}, {Reconciling, Processing}, {Reconciling, Canceled}, {Succeeded, Succeeded}}
	for _, pair := range allowed {
		if !CanTransition(pair[0], pair[1]) {
			t.Fatalf("expected %s -> %s", pair[0], pair[1])
		}
	}
	denied := [][2]Status{{Succeeded, Failed}, {Failed, Processing}, {Canceled, Queued}, {Processing, Pending}}
	for _, pair := range denied {
		if CanTransition(pair[0], pair[1]) {
			t.Fatalf("unexpected %s -> %s", pair[0], pair[1])
		}
	}
}

func TestSnapshotBoundaryAndDigest(t *testing.T) {
	body := []byte(`{"ok":true}`)
	snapshot := Snapshot{Status: 200, Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: body, SHA256: sha256.Sum256(body)}
	if err := ValidateSnapshot(snapshot, 1024); err != nil {
		t.Fatal(err)
	}
	snapshot.Headers["Authorization"] = []string{"secret"}
	if err := ValidateSnapshot(snapshot, 1024); err == nil {
		t.Fatal("secret-bearing header accepted")
	}
}

func TestValidateObservationRequiresTerminalSnapshot(t *testing.T) {
	if err := ValidateObservation(Processing, Observation{Status: Succeeded}, 1024); err == nil {
		t.Fatal("missing snapshot accepted")
	}
	if err := ValidateObservation(Processing, Observation{Status: Canceled}, 1024); err != nil {
		t.Fatal(err)
	}
	if err := ValidateObservation(Succeeded, Observation{Status: Failed}, 1024); err == nil {
		t.Fatal("terminal conflict accepted")
	}
}

func TestPublicJobExcludesProviderAndTenantMetadata(t *testing.T) {
	value := Job{ID: "job_00000000000000000000000000000000", Owner: Owner{OrganizationID: "secret"}, Provider: "secret-provider", ChannelID: "secret-channel", ChargeID: "secret-charge", Status: Succeeded, Snapshot: Snapshot{Status: 200, Body: []byte("result")}}
	public := Public(value)
	if public.ID != value.ID || public.Result == nil || string(public.Result.Body) != "result" {
		t.Fatalf("public=%+v", public)
	}
	encoded, err := json.Marshal(public)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"secret-provider", "secret-channel", "secret-charge", "secret\""} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("public job leaked %q: %s", secret, encoded)
		}
	}
}
