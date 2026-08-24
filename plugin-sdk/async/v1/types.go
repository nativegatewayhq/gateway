// Package async defines the public Native Gateway asynchronous HTTP sidecar wire v1.
package async

const SubmitRequestSchema = "nativegateway.plugin-async-submit-request/v1"
const ControlRequestSchema = "nativegateway.plugin-async-control-request/v1"
const SubmitResponseSchema = "nativegateway.plugin-async-submit-response/v1"
const ObservationResponseSchema = "nativegateway.plugin-async-observation-response/v1"
const CallbackSchema = "nativegateway.plugin-async-callback/v1"
const ContractVersion = "async/v1"
const RuntimeSchema = "nativegateway.plugin-async/v1"

type Identity struct {
	RequestID, GatewayJobID, PluginID, PluginVersion, ManifestDigest string
}

type ImageInput struct {
	Prompt  string `json:"prompt"`
	Images  int    `json:"images"`
	Size    string `json:"size,omitempty"`
	Quality string `json:"quality,omitempty"`
}

type SubmitRequest struct {
	SchemaVersion  string     `json:"schema_version"`
	RequestID      string     `json:"request_id"`
	GatewayJobID   string     `json:"gateway_job_id"`
	PluginID       string     `json:"plugin_id"`
	PluginVersion  string     `json:"plugin_version"`
	ManifestDigest string     `json:"manifest_digest"`
	Protocol       string     `json:"protocol"`
	Operation      string     `json:"operation"`
	Model          string     `json:"model"`
	Input          ImageInput `json:"input"`
	CallbackURL    string     `json:"callback_url,omitempty"`
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

type Image struct {
	MIMEType string `json:"mime_type"`
	Base64   string `json:"base64,omitempty"`
	URL      string `json:"url,omitempty"`
}

type Usage struct {
	Dimension string `json:"dimension"`
	Unit      string `json:"unit"`
	Quantity  int64  `json:"quantity"`
}

type Result struct {
	Images []Image `json:"images"`
	Usage  Usage   `json:"usage"`
}

type PluginError struct {
	Category  string `json:"category"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

type Observation struct {
	Status string       `json:"status"`
	Result *Result      `json:"result,omitempty"`
	Error  *PluginError `json:"error,omitempty"`
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
	Identity      Identity
	Output        string
	MaximumImages int
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
