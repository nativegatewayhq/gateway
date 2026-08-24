package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"

	manifest "github.com/nativegatewayhq/gateway/plugin-sdk/manifest/v1"
	registry "github.com/nativegatewayhq/gateway/plugin-sdk/registry/v1"
)

func main() { os.Exit(run(os.Args[1:], time.Now, os.Stdout, os.Stderr)) }

func run(arguments []string, now func() time.Time, stdout, stderr io.Writer) int {
	if len(arguments) == 0 || (arguments[0] != "verify" && arguments[0] != "matrix") || now == nil {
		return configurationFailure(stderr)
	}
	command := arguments[0]
	flags := flag.NewFlagSet("gateway-plugin-registry "+command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	trustFile := flags.String("trust-file", "", "absolute local Adapter trust policy file")
	indexFile := flags.String("index-file", "", "absolute local signed Registry index envelope")
	admissionDirectory := flags.String("admission-dir", "", "absolute local signed admission envelope directory")
	manifestDirectory := flags.String("manifest-dir", "", "absolute trusted Provider Manifest directory")
	reportDirectory := flags.String("report-dir", "", "absolute local conformance report directory")
	gatewayVersion := flags.String("gateway-version", "0.1.0", "Gateway semantic version")
	platform := flags.String("platform", runtime.GOOS+"/"+runtime.GOARCH, "exact linux/amd64 or linux/arm64 platform")
	minimumSequence := flags.Uint64("minimum-sequence", 1, "operator rollback floor")
	lastSequence := flags.Uint64("last-sequence", 0, "last accepted Registry sequence")
	lastIndexDigest := flags.String("last-index-digest", "", "last accepted canonical index SHA-256")
	jsonOutput := flags.Bool("json", false, "write strict machine-readable JSON")
	if flags.Parse(arguments[1:]) != nil || flags.NArg() != 0 || *trustFile == "" || *indexFile == "" || *admissionDirectory == "" || *manifestDirectory == "" || *reportDirectory == "" || strings.TrimSpace(*lastIndexDigest) != *lastIndexDigest {
		return configurationFailure(stderr)
	}
	manifests, err := manifest.LoadDirectory(*manifestDirectory, *gatewayVersion)
	if err != nil || len(manifests) == 0 {
		return verificationFailure(stderr)
	}
	snapshot, err := registry.LoadSnapshot(registry.BundleConfig{TrustPolicyFile: *trustFile, IndexEnvelopeFile: *indexFile, AdmissionDirectory: *admissionDirectory, GatewayVersion: *gatewayVersion, Platform: *platform, MinimumSequence: *minimumSequence, LastSequence: *lastSequence, LastIndexDigest: *lastIndexDigest, Now: now().UTC().Truncate(time.Second)}, manifests)
	if err != nil {
		return verificationFailure(stderr)
	}
	if registry.VerifyReportDirectory(snapshot, *reportDirectory) != nil {
		return verificationFailure(stderr)
	}
	matrix, err := registry.BuildMatrix(snapshot)
	if err != nil {
		return verificationFailure(stderr)
	}
	if command == "verify" && !*jsonOutput {
		_, _ = fmt.Fprintf(stdout, "adapter registry verified sequence %d (%d releases)\n", matrix.IndexSequence, len(matrix.Entries))
		return 0
	}
	var body []byte
	if *jsonOutput {
		body, err = registry.CanonicalMatrix(matrix)
		if err == nil {
			body = append(body, '\n')
		}
	} else {
		body, err = registry.RenderMatrixMarkdown(matrix)
	}
	if err != nil {
		return verificationFailure(stderr)
	}
	_, _ = stdout.Write(body)
	return 0
}

func configurationFailure(stderr io.Writer) int {
	_, _ = fmt.Fprintln(stderr, "adapter registry configuration failed")
	return 2
}

func verificationFailure(stderr io.Writer) int {
	_, _ = fmt.Fprintln(stderr, "adapter registry verification failed")
	return 1
}
