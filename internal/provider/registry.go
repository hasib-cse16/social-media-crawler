// Package provider wires platform implementations together behind one lookup.
package provider

import (
	"context"
	"fmt"

	"github.com/foodibd/socialstats/internal/domain"
)

// Registry resolves a URL to the provider that owns it.
type Registry struct {
	providers []domain.Provider
	byName    map[domain.Platform]domain.Provider
}

func NewRegistry(providers ...domain.Provider) *Registry {
	r := &Registry{
		providers: make([]domain.Provider, 0, len(providers)),
		byName:    make(map[domain.Platform]domain.Provider, len(providers)),
	}
	for _, p := range providers {
		if p == nil {
			continue
		}
		r.providers = append(r.providers, p)
		r.byName[p.Platform()] = p
	}
	return r
}

// For returns the provider that claims rawURL.
func (r *Registry) For(rawURL string) (domain.Provider, error) {
	for _, p := range r.providers {
		if p.Match(rawURL) {
			return p, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", domain.ErrUnsupported, rawURL)
}

// ByPlatform returns a provider by platform name.
func (r *Registry) ByPlatform(p domain.Platform) (domain.Provider, error) {
	if prov, ok := r.byName[p]; ok {
		return prov, nil
	}
	return nil, fmt.Errorf("%w: %s", domain.ErrUnsupported, p)
}

// Platforms lists the registered platforms, in registration order.
func (r *Registry) Platforms() []domain.Platform {
	out := make([]domain.Platform, 0, len(r.providers))
	for _, p := range r.providers {
		out = append(out, p.Platform())
	}
	return out
}

// Stats is the single entry point used by the service layer.
func (r *Registry) Stats(ctx context.Context, rawURL string) (*domain.VideoStats, error) {
	p, err := r.For(rawURL)
	if err != nil {
		return nil, err
	}
	return p.Stats(ctx, rawURL)
}
