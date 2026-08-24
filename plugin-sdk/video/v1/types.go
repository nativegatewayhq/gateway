// Package video defines the public Native Gateway asynchronous video sidecar wire v1.
package video

const SubmitRequestSchema = "nativegateway.plugin-video-submit-request/v1"
const ControlRequestSchema = "nativegateway.plugin-video-control-request/v1"
const SubmitResponseSchema = "nativegateway.plugin-video-submit-response/v1"
const ObservationResponseSchema = "nativegateway.plugin-video-observation-response/v1"
const CallbackSchema = "nativegateway.plugin-video-callback/v1"
const ContractVersion = "video/v1"
const RuntimeSchema = "nativegateway.plugin-video/v1"

type Identity struct{ RequestID, GatewayJobID, PluginID, PluginVersion, ManifestDigest string }
type SourceAsset struct {
	URI         string `json:"uri"`
	ContentType string `json:"content_type"`
}
type Input struct {
	Kind            string       `json:"kind"`
	Prompt          string       `json:"prompt"`
	DurationSeconds int          `json:"duration_seconds"`
	Ratio           string       `json:"ratio"`
	Audio           bool         `json:"audio"`
	Seed            *int64       `json:"seed,omitempty"`
	Source          *SourceAsset `json:"source,omitempty"`
}
type SubmitRequest struct {
	SchemaVersion  string `json:"schema_version"`
	RequestID      string `json:"request_id"`
	GatewayJobID   string `json:"gateway_job_id"`
	PluginID       string `json:"plugin_id"`
	PluginVersion  string `json:"plugin_version"`
	ManifestDigest string `json:"manifest_digest"`
	Protocol       string `json:"protocol"`
	Operation      string `json:"operation"`
	Model          string `json:"model"`
	Input          Input  `json:"input"`
	CallbackURL    string `json:"callback_url,omitempty"`
}
type ControlRequest struct {
	SchemaVersion  string `json:"schema_version"`
	RequestID      string `json:"request_id"`
	GatewayJobID   string `json:"gateway_job_id"`
	PluginID       string `json:"plugin_id"`
	PluginVersion  string `json:"plugin_version"`
	ManifestDigest string `json:"manifest_digest"`
	Action         string `json:"action"`
	ProviderJobRef string `json:"provider_job_ref"`
}
type Usage struct {
	Dimension string `json:"dimension"`
	Unit      string `json:"unit"`
	Quantity  int64  `json:"quantity"`
}
type Result struct {
	URL             string `json:"url"`
	ContentType     string `json:"content_type"`
	DurationSeconds int    `json:"duration_seconds"`
}
type PluginError struct {
	Category  string `json:"category"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}
type Observation struct {
	Status   string       `json:"status"`
	Progress *int         `json:"progress,omitempty"`
	Result   *Result      `json:"result,omitempty"`
	Usage    *Usage       `json:"usage,omitempty"`
	Error    *PluginError `json:"error,omitempty"`
}
type SubmitResponse struct {
	SchemaVersion  string      `json:"schema_version"`
	RequestID      string      `json:"request_id"`
	GatewayJobID   string      `json:"gateway_job_id"`
	PluginID       string      `json:"plugin_id"`
	PluginVersion  string      `json:"plugin_version"`
	ManifestDigest string      `json:"manifest_digest"`
	ProviderJobRef string      `json:"provider_job_ref"`
	Observation    Observation `json:"observation"`
}
type ObservationResponse struct {
	SchemaVersion  string      `json:"schema_version"`
	RequestID      string      `json:"request_id"`
	GatewayJobID   string      `json:"gateway_job_id"`
	PluginID       string      `json:"plugin_id"`
	PluginVersion  string      `json:"plugin_version"`
	ManifestDigest string      `json:"manifest_digest"`
	Observation    Observation `json:"observation"`
}
type Callback struct {
	SchemaVersion  string      `json:"schema_version"`
	DeliveryID     string      `json:"delivery_id"`
	RequestID      string      `json:"request_id"`
	GatewayJobID   string      `json:"gateway_job_id"`
	PluginID       string      `json:"plugin_id"`
	PluginVersion  string      `json:"plugin_version"`
	ManifestDigest string      `json:"manifest_digest"`
	Protocol       string      `json:"protocol"`
	Operation      string      `json:"operation"`
	Model          string      `json:"model"`
	ProviderJobRef string      `json:"provider_job_ref"`
	Observation    Observation `json:"observation"`
}
type Expectation struct {
	Identity               Identity
	MaximumDurationSeconds int
	Ratios                 map[string]bool
	Audio                  bool
	TextToVideo            bool
	ImageToVideo           bool
	ResultOrigins          map[string]bool
}

func (value SubmitRequest) Identity() Identity {
	return Identity{value.RequestID, value.GatewayJobID, value.PluginID, value.PluginVersion, value.ManifestDigest}
}
func (value ControlRequest) Identity() Identity {
	return Identity{value.RequestID, value.GatewayJobID, value.PluginID, value.PluginVersion, value.ManifestDigest}
}
func (value SubmitResponse) Identity() Identity {
	return Identity{value.RequestID, value.GatewayJobID, value.PluginID, value.PluginVersion, value.ManifestDigest}
}
func (value ObservationResponse) Identity() Identity {
	return Identity{value.RequestID, value.GatewayJobID, value.PluginID, value.PluginVersion, value.ManifestDigest}
}
func (value Callback) Identity() Identity {
	return Identity{value.RequestID, value.GatewayJobID, value.PluginID, value.PluginVersion, value.ManifestDigest}
}
