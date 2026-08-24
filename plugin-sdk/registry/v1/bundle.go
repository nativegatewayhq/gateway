package registry

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	asyncconformance "github.com/nativegatewayhq/gateway/plugin-sdk/conformance/async/v1"
	conformance "github.com/nativegatewayhq/gateway/plugin-sdk/conformance/v1"
	manifest "github.com/nativegatewayhq/gateway/plugin-sdk/manifest/v1"
)

const MaximumConformanceReportBytes = 1 << 20

type BundleConfig struct {
	TrustPolicyFile, IndexEnvelopeFile, AdmissionDirectory string
	GatewayVersion, Platform                               string
	MinimumSequence                                        uint64
	LastSequence                                           uint64
	LastIndexDigest                                        string
	Now                                                    time.Time
}

// VerifyReportDirectory verifies a complete, content-addressed conformance
// report corpus for the admissions selected in snapshot. Files must be named
// after the exact report SHA-256 referenced by each signed admission.
func VerifyReportDirectory(snapshot Snapshot, directory string) error {
	if len(snapshot.Admissions) < 1 {
		return ErrInvalid
	}
	files, err := loadDigestFiles(directory, ".json", MaximumConformanceReportBytes)
	if err != nil || len(files) != len(snapshot.Admissions) {
		return ErrInvalid
	}
	used := make(map[string]bool, len(files))
	for _, admission := range snapshot.Admissions {
		digest := admission.Statement.Predicate.Conformance.ReportDigest
		body, ok := files[digest]
		if !ok || Digest(body) != digest {
			return ErrInvalid
		}
		switch admission.Statement.Predicate.Conformance.SchemaVersion {
		case conformance.ReportSchema:
			report, decodeErr := conformance.DecodeReport(bytes.NewReader(body), MaximumConformanceReportBytes)
			if decodeErr != nil || VerifyConformanceReport(admission, report) != nil {
				return ErrInvalid
			}
		case asyncconformance.ReportSchema:
			report, decodeErr := asyncconformance.DecodeReport(bytes.NewReader(body), MaximumConformanceReportBytes)
			if decodeErr != nil || VerifyAsyncConformanceReport(admission, report) != nil {
				return ErrInvalid
			}
		default:
			return ErrInvalid
		}
		used[digest] = true
	}
	if len(used) != len(files) {
		return ErrInvalid
	}
	return nil
}

type Snapshot struct {
	Index      VerifiedIndex
	Trust      TrustPolicy
	Admissions map[string]VerifiedAdmission
}

func LoadSnapshot(config BundleConfig, manifests []manifest.Validated) (Snapshot, error) {
	if config.Platform == "" {
		config.Platform = runtime.GOOS + "/" + runtime.GOARCH
	}
	if !validPlatform(config.Platform) || !versionPattern.MatchString(config.GatewayVersion) || config.MinimumSequence < 1 || config.Now.IsZero() || (config.LastSequence == 0) != (config.LastIndexDigest == "") || (config.LastIndexDigest != "" && !sha256Pattern.MatchString(config.LastIndexDigest)) {
		return Snapshot{}, ErrInvalid
	}
	trustBody, err := readSafeFile(config.TrustPolicyFile, MaximumTrustBytes)
	if err != nil {
		return Snapshot{}, ErrInvalid
	}
	trust, err := DecodeTrustPolicy(bytes.NewReader(trustBody), MaximumTrustBytes)
	if err != nil {
		return Snapshot{}, ErrInvalid
	}
	indexBody, err := readSafeFile(config.IndexEnvelopeFile, MaximumEnvelopeBytes)
	if err != nil {
		return Snapshot{}, ErrInvalid
	}
	indexEnvelope, err := DecodeEnvelope(bytes.NewReader(indexBody), MaximumEnvelopeBytes)
	if err != nil {
		return Snapshot{}, ErrInvalid
	}
	verifiedIndex, err := VerifyIndex(indexEnvelope, trust, config.Now.UTC().Truncate(time.Second), config.MinimumSequence)
	if err != nil {
		return Snapshot{}, ErrInvalid
	}
	if config.LastSequence > 0 {
		switch {
		case verifiedIndex.Index.Sequence < config.LastSequence:
			return Snapshot{}, ErrInvalid
		case verifiedIndex.Index.Sequence == config.LastSequence && verifiedIndex.PayloadDigest != config.LastIndexDigest:
			return Snapshot{}, ErrInvalid
		case verifiedIndex.Index.Sequence > config.LastSequence && (verifiedIndex.Index.Sequence != config.LastSequence+1 || verifiedIndex.Index.PreviousIndexDigest != config.LastIndexDigest):
			return Snapshot{}, ErrInvalid
		}
	}
	manifestByRelease := make(map[string]manifest.Validated, len(manifests))
	for _, item := range manifests {
		key := item.Manifest.ID + "@" + item.Manifest.Version
		if _, duplicate := manifestByRelease[key]; duplicate {
			return Snapshot{}, ErrInvalid
		}
		manifestByRelease[key] = item
	}
	admissionFiles, err := loadAdmissionFiles(config.AdmissionDirectory)
	if err != nil {
		return Snapshot{}, ErrInvalid
	}
	admissions := make(map[string]VerifiedAdmission, len(manifests))
	usedDigests := map[string]bool{}
	for _, release := range verifiedIndex.Index.Releases {
		key := release.PluginID + "@" + release.PluginVersion
		item, wanted := manifestByRelease[key]
		if !wanted {
			continue
		}
		if release.Status != "active" {
			return Snapshot{}, ErrInvalid
		}
		var reference AdmissionRef
		found := false
		for _, candidate := range release.Admissions {
			if candidate.Platform == config.Platform {
				reference, found = candidate, true
			}
		}
		if !found {
			return Snapshot{}, ErrInvalid
		}
		envelope, ok := admissionFiles[reference.EnvelopeDigest]
		if !ok {
			return Snapshot{}, ErrInvalid
		}
		verified, verifyErr := VerifyAdmission(envelope, trust, verifiedIndex.Index.CreatedAt, AdmissionExpectation{PluginID: release.PluginID, PluginVersion: release.PluginVersion, Platform: config.Platform, EnvelopeDigest: reference.EnvelopeDigest, GatewayVersion: config.GatewayVersion, Manifest: item})
		if verifyErr != nil {
			return Snapshot{}, ErrInvalid
		}
		admissions[key] = verified
		usedDigests[reference.EnvelopeDigest] = true
	}
	if len(admissions) != len(manifests) || len(usedDigests) != len(admissionFiles) {
		return Snapshot{}, ErrInvalid
	}
	return Snapshot{Index: verifiedIndex, Trust: trust, Admissions: admissions}, nil
}

func readSafeFile(path string, maximum int64) ([]byte, error) {
	if !filepath.IsAbs(path) || strings.TrimSpace(path) != path || maximum < 1 {
		return nil, ErrInvalid
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 || info.Size() < 1 || info.Size() > maximum {
		return nil, ErrInvalid
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, ErrInvalid
	}
	body, readErr := io.ReadAll(io.LimitReader(file, maximum+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || int64(len(body)) > maximum {
		return nil, ErrInvalid
	}
	return body, nil
}

func loadAdmissionFiles(directory string) (map[string]Envelope, error) {
	files, err := loadDigestFiles(directory, ".dsse.json", MaximumEnvelopeBytes)
	if err != nil {
		return nil, err
	}
	result := make(map[string]Envelope, len(files))
	for digest, body := range files {
		envelope, decodeErr := DecodeEnvelope(bytes.NewReader(body), MaximumEnvelopeBytes)
		if decodeErr != nil || envelope.PayloadType != AdmissionPayloadType {
			return nil, ErrInvalid
		}
		canonical, canonicalErr := CanonicalEnvelope(envelope)
		if canonicalErr != nil || !bytes.Equal(canonical, body) || Digest(canonical) != digest {
			return nil, ErrInvalid
		}
		result[digest] = envelope
	}
	return result, nil
}

func loadDigestFiles(directory, suffix string, maximum int64) (map[string][]byte, error) {
	if !filepath.IsAbs(directory) || strings.TrimSpace(directory) != directory {
		return nil, ErrInvalid
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 {
		return nil, ErrInvalid
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) < 1 || len(entries) > 4096 {
		return nil, ErrInvalid
	}
	sort.Slice(entries, func(left, right int) bool { return entries[left].Name() < entries[right].Name() })
	result := make(map[string][]byte, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || len(name) != 64+len(suffix) || !hexDigestPattern.MatchString(strings.TrimSuffix(name, suffix)) {
			return nil, ErrInvalid
		}
		body, readErr := readSafeFile(filepath.Join(directory, name), maximum)
		if readErr != nil {
			return nil, ErrInvalid
		}
		digest := "sha256:" + strings.TrimSuffix(name, suffix)
		if Digest(body) != digest {
			return nil, ErrInvalid
		}
		result[digest] = body
	}
	return result, nil
}
