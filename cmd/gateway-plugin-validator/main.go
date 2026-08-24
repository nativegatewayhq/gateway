package main

import (
	"flag"
	"fmt"
	"os"

	manifest "github.com/nativegatewayhq/gateway/plugin-sdk/manifest/v1"
)

func main() {
	directory := flag.String("manifest-dir", "", "absolute directory containing Provider Manifest v1 JSON files")
	gatewayVersion := flag.String("gateway-version", "0.1.0", "Gateway semantic version used for compatibility checks")
	flag.Parse()
	if *directory == "" {
		_, _ = fmt.Fprintln(os.Stderr, "manifest validation failed: -manifest-dir is required")
		os.Exit(2)
	}
	validated, err := manifest.LoadDirectory(*directory, *gatewayVersion)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "manifest validation failed")
		os.Exit(1)
	}
	for _, item := range validated {
		_, _ = fmt.Fprintf(os.Stdout, "%s@%s sha256:%x models:%d\n", item.Manifest.ID, item.Manifest.Version, item.Digest, len(item.Manifest.Models))
	}
}
