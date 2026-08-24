package asyncfixtures

import (
	"os"
	"testing"

	asyncv1 "github.com/nativegatewayhq/gateway/plugin-sdk/async/v1"
)

func TestFixtureCorpus(t *testing.T) {
	submit, err := os.Open("valid-submit.json")
	if err != nil {
		t.Fatal(err)
	}
	defer submit.Close()
	if _, err = asyncv1.DecodeSubmitRequest(submit, 1<<20); err != nil {
		t.Fatal(err)
	}
	poll, err := os.Open("valid-poll.json")
	if err != nil {
		t.Fatal(err)
	}
	defer poll.Close()
	if _, err = asyncv1.DecodeControlRequest(poll, 1<<20); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"invalid-control-ref.json", "invalid-duplicate.json"} {
		file, openErr := os.Open(name)
		if openErr != nil {
			t.Fatal(openErr)
		}
		_, decodeErr := asyncv1.DecodeControlRequest(file, 1<<20)
		file.Close()
		if decodeErr == nil {
			t.Fatalf("accepted %s", name)
		}
	}
}
