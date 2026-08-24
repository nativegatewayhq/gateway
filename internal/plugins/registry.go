// Package plugins binds validated Provider manifests to immutable runtime routes.
package plugins

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	imageoperation "github.com/nativegatewayhq/gateway/operations/image"
	videooperation "github.com/nativegatewayhq/gateway/operations/video"
	manifest "github.com/nativegatewayhq/gateway/plugin-sdk/manifest/v1"
	registryv1 "github.com/nativegatewayhq/gateway/plugin-sdk/registry/v1"
)

var ErrInvalidConfiguration = errors.New("invalid plugin configuration")

type Config struct {
	EndpointOrigins      map[string]string
	AuthSecrets          map[string]string
	ResultOrigins        map[string][]string
	Timeout              time.Duration
	MaximumRequestBytes  int64
	MaximumResponseBytes int64
	MaximumConcurrency   int
}

type Binding struct {
	PluginID, Version, Model, Protocol, ChannelID, CandidateID   string
	ManifestDigest                                               [32]byte
	Origin                                                       *url.URL
	BearerToken                                                  string
	MaximumImages                                                int
	Output                                                       string
	Async, Callback                                              bool
	Video, TextToVideo, ImageToVideo, Audio                      bool
	MaximumDurationSeconds                                       int
	Ratios                                                       map[string]struct{}
	ResultOrigins                                                map[string]struct{}
	RegistrySequence                                             uint64
	RegistryIndexDigest, RegistryEnvelopeDigest, AdmissionDigest [32]byte
}

type RegistryIndexEvidence struct {
	Sequence                                    uint64
	IndexDigest, EnvelopeDigest, PreviousDigest [32]byte
	CreatedAt, ExpiresAt                        time.Time
}

type Registry struct {
	bindings      map[string]Binding
	routes        []imageoperation.ModelRoute
	videoRoutes   []videooperation.Route
	config        Config
	indexEvidence *RegistryIndexEvidence
}

func NewRegistry(validated []manifest.Validated, config Config) (*Registry, error) {
	return newRegistry(validated, config, nil)
}

func NewAdmittedRegistry(validated []manifest.Validated, config Config, snapshot registryv1.Snapshot) (*Registry, error) {
	if len(snapshot.Admissions) != len(validated) {
		return nil, ErrInvalidConfiguration
	}
	return newRegistry(validated, config, &snapshot)
}

func newRegistry(validated []manifest.Validated, config Config, snapshot *registryv1.Snapshot) (*Registry, error) {
	if config.Timeout <= 0 || config.Timeout > 5*time.Minute || config.MaximumRequestBytes < 1 || config.MaximumRequestBytes > 64<<20 || config.MaximumResponseBytes < 1 || config.MaximumResponseBytes > 128<<20 || config.MaximumConcurrency < 1 || config.MaximumConcurrency > 4096 {
		return nil, ErrInvalidConfiguration
	}
	result := &Registry{bindings: make(map[string]Binding), config: config}
	if snapshot != nil {
		indexDigest, ok1 := decodeSHA256(snapshot.Index.PayloadDigest)
		envelopeDigest, ok2 := decodeSHA256(snapshot.Index.EnvelopeDigest)
		previousDigest, ok3 := decodeOptionalSHA256(snapshot.Index.Index.PreviousIndexDigest)
		if !ok1 || !ok2 || !ok3 || snapshot.Index.Index.Sequence < 1 {
			return nil, ErrInvalidConfiguration
		}
		result.indexEvidence = &RegistryIndexEvidence{Sequence: snapshot.Index.Index.Sequence, IndexDigest: indexDigest, EnvelopeDigest: envelopeDigest, PreviousDigest: previousDigest, CreatedAt: snapshot.Index.Index.CreatedAt, ExpiresAt: snapshot.Index.Index.ExpiresAt}
	}
	seenModels := map[string]bool{"openai\x00gpt-image-1": true, "openai\x00grok-imagine-image-quality": true, "gemini\x00gemini-image": true}
	for _, item := range validated {
		var admissionDigest [32]byte
		if snapshot != nil {
			admission, exists := snapshot.Admissions[item.Manifest.ID+"@"+item.Manifest.Version]
			var valid bool
			admissionDigest, valid = decodeSHA256(admission.EnvelopeDigest)
			if !exists || !valid {
				return nil, ErrInvalidConfiguration
			}
		}
		originValue, endpointOK := config.EndpointOrigins[item.Manifest.Transport.EndpointRef]
		bearerToken, secretOK := config.AuthSecrets[item.Manifest.Transport.AuthSecretRef]
		if !endpointOK || !secretOK || bearerToken == "" || len(bearerToken) > 4096 || strings.TrimSpace(bearerToken) != bearerToken {
			return nil, ErrInvalidConfiguration
		}
		origin, err := parseOrigin(originValue)
		if err != nil {
			return nil, ErrInvalidConfiguration
		}
		resultOrigins := make(map[string]struct{}, len(config.ResultOrigins[item.Manifest.ID]))
		for _, rawResultOrigin := range config.ResultOrigins[item.Manifest.ID] {
			parsedResultOrigin, parseErr := parseHTTPSOrigin(rawResultOrigin)
			if parseErr != nil {
				return nil, ErrInvalidConfiguration
			}
			resultOrigins[parsedResultOrigin] = struct{}{}
		}
		for _, model := range item.Manifest.Models {
			for _, protocol := range model.Protocols {
				modelKey := protocol + "\x00" + model.ID
				if seenModels[modelKey] {
					return nil, ErrInvalidConfiguration
				}
				seenModels[modelKey] = true
				channelMaterial := item.Manifest.ID + "\x00" + item.Manifest.Version + "\x00" + model.ID + "\x00" + protocol + "\x00" + hex.EncodeToString(item.Digest[:])
				if snapshot != nil {
					channelMaterial += "\x00" + hex.EncodeToString(admissionDigest[:])
				}
				digest := sha256.Sum256([]byte(channelMaterial))
				channelID := "channel_" + hex.EncodeToString(digest[:16])
				candidateID := "candidate_plugin_" + hex.EncodeToString(digest[:8])
				if channelID == "channel_00000000000000000000000000000001" || channelID == "channel_00000000000000000000000000000002" || channelID == "channel_00000000000000000000000000000003" {
					return nil, ErrInvalidConfiguration
				}
				if model.Capabilities.Output[0] == "url" && len(resultOrigins) == 0 {
					return nil, ErrInvalidConfiguration
				}
				binding := Binding{PluginID: item.Manifest.ID, Version: item.Manifest.Version, Model: model.ID, Protocol: protocol, ChannelID: channelID, CandidateID: candidateID, ManifestDigest: item.Digest, Origin: origin, BearerToken: bearerToken, MaximumImages: model.Capabilities.MaximumImages, Output: model.Capabilities.Output[0], ResultOrigins: resultOrigins, Async: model.Async != nil}
				if model.Async != nil {
					binding.Callback = model.Async.Callback
				}
				if snapshot != nil {
					binding.RegistrySequence = result.indexEvidence.Sequence
					binding.RegistryIndexDigest = result.indexEvidence.IndexDigest
					binding.RegistryEnvelopeDigest = result.indexEvidence.EnvelopeDigest
					binding.AdmissionDigest = admissionDigest
				}
				if _, duplicate := result.bindings[channelID]; duplicate {
					return nil, ErrInvalidConfiguration
				}
				result.bindings[channelID] = binding
				result.routes = append(result.routes, imageoperation.ModelRoute{Protocol: protocol, Model: model.ID, Owner: item.Manifest.ID, Capabilities: []imageoperation.Capability{{Operation: imageoperation.Generate, MediaType: imageoperation.JSON}}, Policy: imageoperation.Fixed, FixedCandidateID: candidateID, Candidates: []imageoperation.ChannelCandidate{{ID: candidateID, Provider: providercredentials.Plugin, ProviderModel: model.ID, ChannelID: channelID, Enabled: true}}, Usage: imageoperation.UsageCapability{Dimension: "output", Unit: "image", DefaultQuantity: 1, MaximumQuantity: int64(model.Capabilities.MaximumImages), RequestExtractor: protocol + "-plugin-image-count-v1", ResultExtractor: "plugin-image-output-v1"}})
			}
		}
		for _, model := range item.Manifest.VideoModels {
			modelKey := "runway\x00" + model.ID
			if seenModels[modelKey] {
				return nil, ErrInvalidConfiguration
			}
			seenModels[modelKey] = true
			if len(resultOrigins) == 0 {
				return nil, ErrInvalidConfiguration
			}
			channelMaterial := item.Manifest.ID + "\x00" + item.Manifest.Version + "\x00" + model.ID + "\x00runway\x00" + hex.EncodeToString(item.Digest[:])
			if snapshot != nil {
				channelMaterial += "\x00" + hex.EncodeToString(admissionDigest[:])
			}
			digest := sha256.Sum256([]byte(channelMaterial))
			channelID := "channel_" + hex.EncodeToString(digest[:16])
			candidateID := "candidate_plugin_" + hex.EncodeToString(digest[:8])
			ratios := make(map[string]struct{}, len(model.Capabilities.Ratios))
			for _, ratio := range model.Capabilities.Ratios {
				ratios[ratio] = struct{}{}
			}
			binding := Binding{PluginID: item.Manifest.ID, Version: item.Manifest.Version, Model: model.ID, Protocol: "runway", ChannelID: channelID, CandidateID: candidateID, ManifestDigest: item.Digest, Origin: origin, BearerToken: bearerToken, Output: "url", Async: true, Callback: model.Async.Callback, Video: true, TextToVideo: model.Capabilities.TextToVideo, ImageToVideo: model.Capabilities.ImageToVideo, Audio: model.Capabilities.Audio, MaximumDurationSeconds: model.Capabilities.MaximumDurationSeconds, Ratios: ratios, ResultOrigins: resultOrigins}
			if snapshot != nil {
				binding.RegistrySequence = result.indexEvidence.Sequence
				binding.RegistryIndexDigest = result.indexEvidence.IndexDigest
				binding.RegistryEnvelopeDigest = result.indexEvidence.EnvelopeDigest
				binding.AdmissionDigest = admissionDigest
			}
			if _, duplicate := result.bindings[channelID]; duplicate {
				return nil, ErrInvalidConfiguration
			}
			result.bindings[channelID] = binding
			result.videoRoutes = append(result.videoRoutes, videooperation.Route{Model: model.ID, ProviderModel: model.ID, Provider: providercredentials.Plugin, ChannelID: channelID, TextToVideo: model.Capabilities.TextToVideo, ImageToVideo: model.Capabilities.ImageToVideo})
		}
	}
	sort.Slice(result.routes, func(i, j int) bool {
		if result.routes[i].Protocol == result.routes[j].Protocol {
			return result.routes[i].Model < result.routes[j].Model
		}
		return result.routes[i].Protocol < result.routes[j].Protocol
	})
	return result, nil
}

func (registry *Registry) VideoRoutes() []videooperation.Route {
	if registry == nil {
		return nil
	}
	return append([]videooperation.Route(nil), registry.videoRoutes...)
}
func (registry *Registry) SupportsVideo() bool {
	return registry != nil && len(registry.videoRoutes) > 0
}

func (registry *Registry) IndexEvidence() (RegistryIndexEvidence, bool) {
	if registry == nil || registry.indexEvidence == nil {
		return RegistryIndexEvidence{}, false
	}
	return *registry.indexEvidence, true
}

func parseOrigin(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" && parsed.Path != "/" {
		return nil, ErrInvalidConfiguration
	}
	host := strings.ToLower(parsed.Hostname())
	loopback := host == "localhost" || host == "127.0.0.1" || host == "::1"
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && loopback) {
		return nil, ErrInvalidConfiguration
	}
	parsed.Path = ""
	return parsed, nil
}

func parseHTTPSOrigin(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Path != "" && parsed.Path != "/" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", ErrInvalidConfiguration
	}
	return parsed.Scheme + "://" + parsed.Host, nil
}

func (registry *Registry) Routes() []imageoperation.ModelRoute {
	if registry == nil {
		return nil
	}
	values := make([]imageoperation.ModelRoute, len(registry.routes))
	for index, route := range registry.routes {
		values[index] = cloneRoute(route)
	}
	return values
}

func (registry *Registry) Binding(channelID string) (Binding, bool) {
	if registry == nil {
		return Binding{}, false
	}
	value, ok := registry.bindings[channelID]
	return cloneBinding(value), ok
}

func (registry *Registry) Bindings() []Binding {
	if registry == nil {
		return nil
	}
	values := make([]Binding, 0, len(registry.bindings))
	for _, binding := range registry.bindings {
		values = append(values, cloneBinding(binding))
	}
	sort.Slice(values, func(i, j int) bool { return values[i].ChannelID < values[j].ChannelID })
	return values
}

func (registry *Registry) ConfiguredChannel(channelID string) bool {
	_, ok := registry.Binding(channelID)
	return ok
}

func (registry *Registry) SupportsAsyncProtocol(protocol string) bool {
	if registry == nil {
		return false
	}
	for _, binding := range registry.bindings {
		if binding.Async && binding.Protocol == protocol {
			return true
		}
	}
	return false
}

func (registry *Registry) SupportsCallbacks() bool {
	if registry == nil {
		return false
	}
	for _, binding := range registry.bindings {
		if binding.Async && binding.Callback {
			return true
		}
	}
	return false
}

func (registry *Registry) AsyncIdentity(pluginID, version, digest, protocol, model string) (Binding, bool) {
	if registry == nil {
		return Binding{}, false
	}
	var found Binding
	matched := false
	for _, binding := range registry.bindings {
		if binding.Async && binding.PluginID == pluginID && binding.Version == version && binding.DigestHex() == digest && binding.Protocol == protocol && binding.Model == model {
			found, matched = binding, true
		}
	}
	return cloneBinding(found), matched
}

func cloneRoute(route imageoperation.ModelRoute) imageoperation.ModelRoute {
	route.Capabilities = append([]imageoperation.Capability(nil), route.Capabilities...)
	route.Candidates = append([]imageoperation.ChannelCandidate(nil), route.Candidates...)
	return route
}
func cloneBinding(binding Binding) Binding {
	if binding.Origin != nil {
		origin := *binding.Origin
		binding.Origin = &origin
	}
	binding.ResultOrigins = maps.Clone(binding.ResultOrigins)
	binding.Ratios = maps.Clone(binding.Ratios)
	return binding
}

func (binding Binding) DigestHex() string { return hex.EncodeToString(binding.ManifestDigest[:]) }

func (binding Binding) String() string {
	return fmt.Sprintf("%s@%s/%s", binding.PluginID, binding.Version, binding.Model)
}

func decodeSHA256(value string) ([32]byte, bool) {
	decoded, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	if err != nil || len(decoded) != 32 || !strings.HasPrefix(value, "sha256:") {
		return [32]byte{}, false
	}
	var result [32]byte
	copy(result[:], decoded)
	return result, true
}

func decodeOptionalSHA256(value string) ([32]byte, bool) {
	if value == "" {
		return [32]byte{}, true
	}
	return decodeSHA256(value)
}
