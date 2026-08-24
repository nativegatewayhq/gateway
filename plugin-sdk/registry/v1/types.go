// Package registry defines the public Native Gateway signed Adapter Registry v1.
package registry

import "time"

const IndexSchema = "nativegateway.adapter-registry/v1"
const TrustSchema = "nativegateway.adapter-trust/v1"
const AdmissionPredicateType = "https://nativegateway.dev/attestations/adapter-admission/v1"
const StatementType = "https://in-toto.io/Statement/v1"
const IndexPayloadType = "application/vnd.nativegateway.adapter-registry.v1+json"
const AdmissionPayloadType = "application/vnd.in-toto+json"
const RuntimeSchema = "nativegateway.plugin-request/v1"
const RuntimeSDK = "runtime/v1"
const AsyncRuntimeSchema = "nativegateway.plugin-async/v1"
const AsyncRuntimeSDK = "async/v1"
const VideoRuntimeSchema = "nativegateway.plugin-video/v1"
const VideoRuntimeSDK = "video/v1"

const MaximumIndexBytes = 4 << 20
const MaximumAdmissionBytes = 1 << 20
const MaximumTrustBytes = 1 << 20
const MaximumEnvelopeBytes = 8 << 20

type Envelope struct {
	PayloadType string      `json:"payloadType"`
	Payload     string      `json:"payload"`
	Signatures  []Signature `json:"signatures"`
}

type Signature struct {
	KeyID string `json:"keyid"`
	Sig   string `json:"sig"`
}

type TrustPolicy struct {
	SchemaVersion   string       `json:"schema_version"`
	Threshold       int          `json:"threshold"`
	MinimumSequence uint64       `json:"minimum_sequence"`
	Keys            []TrustedKey `json:"keys"`
}

type TrustedKey struct {
	KeyID     string    `json:"key_id"`
	Algorithm string    `json:"algorithm"`
	PublicKey string    `json:"public_key"`
	NotBefore time.Time `json:"not_before"`
	NotAfter  time.Time `json:"not_after"`
}

type Index struct {
	SchemaVersion       string    `json:"schema_version"`
	Sequence            uint64    `json:"sequence"`
	CreatedAt           time.Time `json:"created_at"`
	ExpiresAt           time.Time `json:"expires_at"`
	PreviousIndexDigest string    `json:"previous_index_digest,omitempty"`
	Releases            []Release `json:"releases"`
}

type Release struct {
	PluginID      string         `json:"plugin_id"`
	PluginVersion string         `json:"plugin_version"`
	Status        string         `json:"status"`
	YankReason    string         `json:"yank_reason,omitempty"`
	Admissions    []AdmissionRef `json:"admissions"`
}

type AdmissionRef struct {
	Platform       string `json:"platform"`
	EnvelopeDigest string `json:"envelope_digest"`
}

type Statement struct {
	Type          string    `json:"_type"`
	Subject       []Subject `json:"subject"`
	PredicateType string    `json:"predicateType"`
	Predicate     Admission `json:"predicate"`
}

type Subject struct {
	Name   string            `json:"name"`
	Digest map[string]string `json:"digest"`
}

type Admission struct {
	PluginID             string              `json:"plugin_id"`
	PluginVersion        string              `json:"plugin_version"`
	ManifestDigest       string              `json:"manifest_digest"`
	RuntimeSchema        string              `json:"runtime_schema"`
	RuntimeSDK           string              `json:"runtime_sdk"`
	GatewayCompatibility string              `json:"gateway_compatibility"`
	Platform             string              `json:"platform"`
	Artifact             Descriptor          `json:"artifact"`
	Conformance          ConformanceEvidence `json:"conformance"`
	Source               SourceEvidence      `json:"source"`
	Builder              BuilderEvidence     `json:"builder"`
	SBOM                 Descriptor          `json:"sbom"`
	Provenance           Descriptor          `json:"provenance"`
}

type Descriptor struct {
	MediaType string `json:"media_type"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

type ConformanceEvidence struct {
	ReportDigest         string `json:"report_digest"`
	SchemaVersion        string `json:"schema_version"`
	RequiredChecksDigest string `json:"required_checks_digest"`
	Outcome              string `json:"outcome"`
}

type SourceEvidence struct {
	Repository string `json:"repository"`
	Commit     string `json:"commit"`
}

type BuilderEvidence struct {
	ID               string `json:"id"`
	InvocationDigest string `json:"invocation_digest"`
}

type VerifiedIndex struct {
	Index          Index
	EnvelopeDigest string
	PayloadDigest  string
}

type VerifiedAdmission struct {
	Statement      Statement
	EnvelopeDigest string
	PayloadDigest  string
}
