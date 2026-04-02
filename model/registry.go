// Package model provides the ModelRegistry and goai-based NodeExecutor
// for resolving "provider/model-id" specs and executing LLM nodes via goai.
package model

import (
	"fmt"
	"strings"
	"sync"

	"github.com/zendev-sh/goai/provider"
	"github.com/zendev-sh/goai/provider/anthropic"
	"github.com/zendev-sh/goai/provider/openai"
)

// ProviderFactory creates a LanguageModel for a given model ID.
// The factory is called once per unique model ID; results are cached.
type ProviderFactory func(modelID string) (provider.LanguageModel, error)

// Registry resolves model specs of the form "provider/model-id" to
// goai LanguageModel instances. It caches resolved models for reuse.
type Registry struct {
	mu        sync.RWMutex
	providers map[string]ProviderFactory
	cache     map[string]provider.LanguageModel
}

// NewRegistry creates a model registry pre-loaded with built-in providers.
func NewRegistry() *Registry {
	r := &Registry{
		providers: make(map[string]ProviderFactory),
		cache:     make(map[string]provider.LanguageModel),
	}
	r.registerDefaults()
	return r
}

// registerDefaults registers the built-in provider factories.
func (r *Registry) registerDefaults() {
	r.providers["anthropic"] = func(modelID string) (provider.LanguageModel, error) {
		return anthropic.Chat(modelID), nil
	}
	r.providers["openai"] = func(modelID string) (provider.LanguageModel, error) {
		return openai.Chat(modelID), nil
	}
}

// Register adds a provider factory under the given name.
// Calling Register with an already-registered name replaces the factory.
func (r *Registry) Register(providerName string, factory ProviderFactory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[providerName] = factory
}

// Resolve parses a model spec ("provider/model-id") and returns the
// corresponding LanguageModel, creating it via the provider factory if
// not already cached.
func (r *Registry) Resolve(spec string) (provider.LanguageModel, error) {
	providerName, modelID, err := ParseModelSpec(spec)
	if err != nil {
		return nil, err
	}

	cacheKey := providerName + "/" + modelID

	// Fast path: check cache.
	r.mu.RLock()
	if m, ok := r.cache[cacheKey]; ok {
		r.mu.RUnlock()
		return m, nil
	}
	r.mu.RUnlock()

	// Slow path: create model.
	r.mu.Lock()
	defer r.mu.Unlock()

	// Double-check after acquiring write lock.
	if m, ok := r.cache[cacheKey]; ok {
		return m, nil
	}

	factory, ok := r.providers[providerName]
	if !ok {
		return nil, fmt.Errorf("model: unknown provider %q (spec: %q)", providerName, spec)
	}

	m, err := factory(modelID)
	if err != nil {
		return nil, fmt.Errorf("model: provider %q failed to create model %q: %w", providerName, modelID, err)
	}

	r.cache[cacheKey] = m
	return m, nil
}

// Capabilities returns the capabilities of the model identified by spec.
func (r *Registry) Capabilities(spec string) (provider.ModelCapabilities, error) {
	m, err := r.Resolve(spec)
	if err != nil {
		return provider.ModelCapabilities{}, err
	}
	return provider.ModelCapabilitiesOf(m), nil
}

// ParseModelSpec splits "provider/model-id" into its components.
func ParseModelSpec(spec string) (providerName, modelID string, err error) {
	idx := strings.Index(spec, "/")
	if idx <= 0 || idx == len(spec)-1 {
		return "", "", fmt.Errorf("model: invalid spec %q (expected \"provider/model-id\")", spec)
	}
	return spec[:idx], spec[idx+1:], nil
}
