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
	manifest "github.com/nativegatewayhq/gateway/plugin-sdk/manifest/v1"
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
	PluginID, Version, Model, Protocol, ChannelID, CandidateID string
	ManifestDigest                                             [32]byte
	Origin                                                     *url.URL
	BearerToken                                                string
	MaximumImages                                              int
	Output                                                     string
	ResultOrigins                                              map[string]struct{}
}

type Registry struct {
	bindings map[string]Binding
	routes   []imageoperation.ModelRoute
	config   Config
}

func NewRegistry(validated []manifest.Validated, config Config) (*Registry, error) {
	if config.Timeout <= 0 || config.Timeout > 5*time.Minute || config.MaximumRequestBytes < 1 || config.MaximumRequestBytes > 64<<20 || config.MaximumResponseBytes < 1 || config.MaximumResponseBytes > 128<<20 || config.MaximumConcurrency < 1 || config.MaximumConcurrency > 4096 {
		return nil, ErrInvalidConfiguration
	}
	result := &Registry{bindings: make(map[string]Binding), config: config}
	seenModels := map[string]bool{"openai\x00gpt-image-1": true, "openai\x00grok-imagine-image-quality": true, "gemini\x00gemini-image": true}
	for _, item := range validated {
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
				digest := sha256.Sum256([]byte(item.Manifest.ID + "\x00" + item.Manifest.Version + "\x00" + model.ID + "\x00" + protocol + "\x00" + hex.EncodeToString(item.Digest[:])))
				channelID := "channel_" + hex.EncodeToString(digest[:16])
				candidateID := "candidate_plugin_" + hex.EncodeToString(digest[:8])
				if channelID == "channel_00000000000000000000000000000001" || channelID == "channel_00000000000000000000000000000002" || channelID == "channel_00000000000000000000000000000003" {
					return nil, ErrInvalidConfiguration
				}
				if model.Capabilities.Output[0] == "url" && len(resultOrigins) == 0 {
					return nil, ErrInvalidConfiguration
				}
				binding := Binding{PluginID: item.Manifest.ID, Version: item.Manifest.Version, Model: model.ID, Protocol: protocol, ChannelID: channelID, CandidateID: candidateID, ManifestDigest: item.Digest, Origin: origin, BearerToken: bearerToken, MaximumImages: model.Capabilities.MaximumImages, Output: model.Capabilities.Output[0], ResultOrigins: resultOrigins}
				if _, duplicate := result.bindings[channelID]; duplicate {
					return nil, ErrInvalidConfiguration
				}
				result.bindings[channelID] = binding
				result.routes = append(result.routes, imageoperation.ModelRoute{Protocol: protocol, Model: model.ID, Owner: item.Manifest.ID, Capabilities: []imageoperation.Capability{{Operation: imageoperation.Generate, MediaType: imageoperation.JSON}}, Policy: imageoperation.Fixed, FixedCandidateID: candidateID, Candidates: []imageoperation.ChannelCandidate{{ID: candidateID, Provider: providercredentials.Plugin, ProviderModel: model.ID, ChannelID: channelID, Enabled: true}}, Usage: imageoperation.UsageCapability{Dimension: "output", Unit: "image", DefaultQuantity: 1, MaximumQuantity: int64(model.Capabilities.MaximumImages), RequestExtractor: protocol + "-plugin-image-count-v1", ResultExtractor: "plugin-image-output-v1"}})
			}
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
	return binding
}

func (binding Binding) DigestHex() string { return hex.EncodeToString(binding.ManifestDigest[:]) }

func (binding Binding) String() string {
	return fmt.Sprintf("%s@%s/%s", binding.PluginID, binding.Version, binding.Model)
}
