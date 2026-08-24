package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	asyncconformance "github.com/nativegatewayhq/gateway/plugin-sdk/conformance/async/v1"
	conformance "github.com/nativegatewayhq/gateway/plugin-sdk/conformance/v1"
	"github.com/nativegatewayhq/gateway/plugin-sdk/jsonstrict"
	manifest "github.com/nativegatewayhq/gateway/plugin-sdk/manifest/v1"
)

const maximumMappingBytes = 1 << 20

var refPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)
var envPattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
var errConfiguration = errors.New("invalid conformance configuration")

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func run(arguments []string, getenv func(string) string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("gateway-plugin-conformance", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	manifestDirectory := flags.String("manifest-dir", "", "absolute Provider Manifest directory")
	pluginID := flags.String("plugin-id", "", "Provider Manifest plugin ID")
	gatewayVersion := flags.String("gateway-version", "0.1.0", "Gateway semantic version")
	endpointsJSON := flags.String("endpoints-json", "", "JSON mapping of endpoint refs to origins")
	secretEnvJSON := flags.String("auth-secret-env-json", "{}", "JSON mapping of secret refs to environment variable names")
	secretFileJSON := flags.String("auth-secret-file-json", "{}", "JSON mapping of secret refs to absolute files")
	jsonOutput := flags.Bool("json", false, "write the versioned JSON report")
	timeout := flags.Duration("timeout", 10*time.Second, "per-check timeout")
	maximumRequest := flags.Int64("maximum-request-bytes", 2<<20, "maximum sidecar request bytes")
	maximumResponse := flags.Int64("maximum-response-bytes", 64<<20, "maximum sidecar response bytes")
	profile := flags.String("profile", "runtime-v1", "conformance profile: runtime-v1 or async-v1")
	callbackSecretEnv := flags.String("callback-secret-env", "", "environment variable containing a base64 32-byte async callback key")
	if flags.Parse(arguments) != nil || flags.NArg() != 0 {
		return configurationFailure(stderr)
	}
	validated, endpoint, secret, err := resolve(*manifestDirectory, *pluginID, *gatewayVersion, *endpointsJSON, *secretEnvJSON, *secretFileJSON, getenv)
	if err != nil {
		return configurationFailure(stderr)
	}
	defer clear(secret)
	if *profile == "async-v1" {
		encoded := getenv(*callbackSecretEnv)
		callbackSecret, decodeErr := base64.StdEncoding.DecodeString(encoded)
		if *callbackSecretEnv == "" || decodeErr != nil || len(callbackSecret) != 32 {
			return configurationFailure(stderr)
		}
		defer clear(callbackSecret)
		runner, newErr := asyncconformance.New(asyncconformance.Config{Manifest: validated, Endpoint: endpoint, Secret: secret, CallbackSecret: callbackSecret, Timeout: *timeout, MaximumRequestBytes: *maximumRequest, MaximumResponseBytes: *maximumResponse})
		if newErr != nil {
			return configurationFailure(stderr)
		}
		report, runErr := runner.Run(context.Background())
		if runErr != nil {
			return configurationFailure(stderr)
		}
		if *jsonOutput {
			if asyncconformance.EncodeReport(stdout, report) != nil {
				return configurationFailure(stderr)
			}
		} else {
			_, _ = fmt.Fprintf(stdout, "%s@%s async conformance %s (%d checks)\n", report.PluginID, report.PluginVersion, report.Outcome, len(report.Checks))
			for _, check := range report.Checks {
				if check.Category == "" {
					_, _ = fmt.Fprintf(stdout, "%s %s\n", check.Outcome, check.ID)
				} else {
					_, _ = fmt.Fprintf(stdout, "%s %s [%s]\n", check.Outcome, check.ID, check.Category)
				}
			}
		}
		if report.Outcome != "pass" {
			return 1
		}
		return 0
	}
	if *profile != "runtime-v1" {
		return configurationFailure(stderr)
	}
	runner, err := conformance.New(conformance.Config{Manifest: validated, Endpoint: endpoint, Secret: secret, Timeout: *timeout, MaximumRequestBytes: *maximumRequest, MaximumResponseBytes: *maximumResponse})
	if err != nil {
		return configurationFailure(stderr)
	}
	report, err := runner.Run(context.Background())
	if err != nil {
		return configurationFailure(stderr)
	}
	if *jsonOutput {
		if conformance.EncodeReport(stdout, report) != nil {
			return configurationFailure(stderr)
		}
	} else {
		_, _ = fmt.Fprintf(stdout, "%s@%s conformance %s (%d checks)\n", report.PluginID, report.PluginVersion, report.Outcome, len(report.Checks))
		for _, check := range report.Checks {
			if check.Category == "" {
				_, _ = fmt.Fprintf(stdout, "%s %s\n", check.Outcome, check.ID)
			} else {
				_, _ = fmt.Fprintf(stdout, "%s %s [%s]\n", check.Outcome, check.ID, check.Category)
			}
		}
	}
	if report.Outcome != "pass" {
		return 1
	}
	return 0
}

func resolve(directory, pluginID, gatewayVersion, endpointsRaw, envRaw, fileRaw string, getenv func(string) string) (manifest.Validated, string, []byte, error) {
	if directory == "" || pluginID == "" || getenv == nil {
		return manifest.Validated{}, "", nil, errConfiguration
	}
	items, err := manifest.LoadDirectory(directory, gatewayVersion)
	if err != nil {
		return manifest.Validated{}, "", nil, errConfiguration
	}
	var selected manifest.Validated
	found := false
	for _, item := range items {
		if item.Manifest.ID == pluginID {
			selected, found = item, true
		}
	}
	if !found {
		return manifest.Validated{}, "", nil, errConfiguration
	}
	endpoints, err := decodeMapping(endpointsRaw)
	if err != nil {
		return manifest.Validated{}, "", nil, errConfiguration
	}
	environment, err := decodeMapping(envRaw)
	if err != nil {
		return manifest.Validated{}, "", nil, errConfiguration
	}
	files, err := decodeMapping(fileRaw)
	if err != nil {
		return manifest.Validated{}, "", nil, errConfiguration
	}
	endpoint, ok := endpoints[selected.Manifest.Transport.EndpointRef]
	if !ok || endpoint == "" {
		return manifest.Validated{}, "", nil, errConfiguration
	}
	secretRef := selected.Manifest.Transport.AuthSecretRef
	envName, fromEnv := environment[secretRef]
	fileName, fromFile := files[secretRef]
	if fromEnv == fromFile {
		return manifest.Validated{}, "", nil, errConfiguration
	}
	var secret []byte
	if fromEnv {
		if !envPattern.MatchString(envName) {
			return manifest.Validated{}, "", nil, errConfiguration
		}
		secret = []byte(getenv(envName))
	} else {
		secret, err = readSecretFile(fileName)
		if err != nil {
			return manifest.Validated{}, "", nil, errConfiguration
		}
	}
	if len(secret) < 16 || len(secret) > 4096 || strings.TrimSpace(string(secret)) != string(secret) {
		clear(secret)
		return manifest.Validated{}, "", nil, errConfiguration
	}
	return selected, endpoint, secret, nil
}

func decodeMapping(raw string) (map[string]string, error) {
	if len(raw) < 2 || len(raw) > maximumMappingBytes || jsonstrict.Validate([]byte(raw)) != nil {
		return nil, errConfiguration
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	var values map[string]string
	if decoder.Decode(&values) != nil || decoder.Decode(&struct{}{}) != io.EOF || values == nil || len(values) > 256 {
		return nil, errConfiguration
	}
	for key, value := range values {
		if !refPattern.MatchString(key) || len(value) < 1 || len(value) > 4096 || strings.TrimSpace(value) != value {
			return nil, errConfiguration
		}
	}
	return values, nil
}

func readSecretFile(path string) ([]byte, error) {
	if !filepath.IsAbs(path) || strings.TrimSpace(path) != path {
		return nil, errConfiguration
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 || info.Size() < 16 || info.Size() > 4096 {
		return nil, errConfiguration
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errConfiguration
	}
	body, readErr := io.ReadAll(io.LimitReader(file, 4097))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || len(body) > 4096 || bytes.IndexByte(body, 0) >= 0 {
		clear(body)
		return nil, errConfiguration
	}
	return body, nil
}

func configurationFailure(stderr io.Writer) int {
	_, _ = fmt.Fprintln(stderr, "plugin conformance configuration failed")
	return 2
}

func clear(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
