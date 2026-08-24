package plugins

import (
	"context"

	"github.com/nativegatewayhq/gateway/internal/providercredentials"
)

type ChannelAvailability interface {
	ConfiguredProviders() []providercredentials.ProviderID
	ConfiguredChannel(context.Context, string, providercredentials.ProviderID) bool
}

type Availability struct {
	base     ChannelAvailability
	registry *Registry
}

func NewAvailability(base ChannelAvailability, registry *Registry) *Availability {
	return &Availability{base: base, registry: registry}
}

func (availability *Availability) ConfiguredProviders() []providercredentials.ProviderID {
	values := availability.base.ConfiguredProviders()
	if availability.registry != nil && len(availability.registry.routes) > 0 {
		values = append(values, providercredentials.Plugin)
	}
	return values
}

func (availability *Availability) ConfiguredChannel(ctx context.Context, channelID string, provider providercredentials.ProviderID) bool {
	if provider == providercredentials.Plugin {
		return availability.registry != nil && availability.registry.ConfiguredChannel(channelID)
	}
	return availability.base.ConfiguredChannel(ctx, channelID, provider)
}
