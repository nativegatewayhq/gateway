package job

import (
	"crypto/sha256"
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
