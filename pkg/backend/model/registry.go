// Package model provides the ModelRegistry and claw-based NodeExecutor
// for resolving "provider/model-id" specs and executing LLM nodes.
package model

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	anthropicprovider "github.com/SocialGouv/claw-code-go/pkg/api/providers/anthropic"
	bedrockprovider "github.com/SocialGouv/claw-code-go/pkg/api/providers/bedrock"
	foundryprovider "github.com/SocialGouv/claw-code-go/pkg/api/providers/foundry"
	openaiprovider "github.com/SocialGouv/claw-code-go/pkg/api/providers/openai"
	vertexprovider "github.com/SocialGouv/claw-code-go/pkg/api/providers/vertex"
)

// ProviderFactory creates an APIClient for a given model ID.
// The factory is called once per unique model ID; results are cached.
type ProviderFactory func(modelID string) (api.APIClient, error)

// KeyedProviderFactory builds an APIClient given an explicit API key.
// Used by ResolveWithContext when a per-run BYOK plaintext is
// available — the result is NOT cached (multi-tenant safety).
type KeyedProviderFactory func(modelID, apiKey string) (api.APIClient, error)

// cacheEntry holds a per-key sync.Once so concurrent resolves for the same
// spec only invoke the factory once, without holding the registry lock for
// the duration of slow factory I/O (e.g. AWS IMDS, Google ADC).
type cacheEntry struct {
	once   sync.Once
	client api.APIClient
	err    error
}

// Registry resolves model specs of the form "provider/model-id" to
// APIClient instances. It caches resolved clients for reuse.
type Registry struct {
	mu               sync.Mutex
	providers        map[string]ProviderFactory
	providersWithKey map[string]KeyedProviderFactory
	cache            map[string]*cacheEntry
}

// NewRegistry creates a model registry pre-loaded with built-in providers.
func NewRegistry() *Registry {
	r := &Registry{
		providers:        make(map[string]ProviderFactory),
		providersWithKey: make(map[string]KeyedProviderFactory),
		cache:            make(map[string]*cacheEntry),
	}
	r.registerDefaults()
	return r
}

// registerDefaults registers the built-in provider factories. Each maps a
// "<name>/" prefix in a model spec to a claw-code-go provider; auth comes
// from the standard env vars / SDK credential chains documented in each
// provider package.
func (r *Registry) registerDefaults() {
	r.providers["anthropic"] = func(modelID string) (api.APIClient, error) {
		p := anthropicprovider.New()
		return p.NewClient(api.ProviderConfig{
			APIKey: os.Getenv("ANTHROPIC_API_KEY"),
			Model:  modelID,
		})
	}
	r.providersWithKey["anthropic"] = func(modelID, apiKey string) (api.APIClient, error) {
		p := anthropicprovider.New()
		return p.NewClient(api.ProviderConfig{APIKey: apiKey, Model: modelID})
	}
	r.providers["openai"] = func(modelID string) (api.APIClient, error) {
		p := openaiprovider.New()
		return p.NewClient(api.ProviderConfig{
			APIKey: os.Getenv("OPENAI_API_KEY"),
			Model:  modelID,
		})
	}
	r.providersWithKey["openai"] = func(modelID, apiKey string) (api.APIClient, error) {
		p := openaiprovider.New()
		return p.NewClient(api.ProviderConfig{APIKey: apiKey, Model: modelID})
	}
	// AWS Bedrock — auth via aws-sdk-go-v2 standard credential chain
	// (AWS_REGION, AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY, profile,
	// EC2/ECS metadata, etc.). cfg.APIKey is ignored; Bedrock doesn't
	// use API keys.
	r.providers["bedrock"] = func(modelID string) (api.APIClient, error) {
		p := bedrockprovider.New()
		return p.NewClient(api.ProviderConfig{Model: modelID})
	}
	// GCP Vertex AI — auth via Google ADC. Requires GOOGLE_CLOUD_PROJECT;
	// GOOGLE_CLOUD_REGION defaults to us-east5.
	r.providers["vertex"] = func(modelID string) (api.APIClient, error) {
		p := vertexprovider.New()
		return p.NewClient(api.ProviderConfig{Model: modelID})
	}
	// Azure Foundry (Azure OpenAI Service) — auth via AZURE_OPENAI_API_KEY
	// or azidentity DefaultAzureCredential. Requires AZURE_OPENAI_ENDPOINT
	// and AZURE_OPENAI_DEPLOYMENT (or modelID is treated as the deployment).
	r.providers["foundry"] = func(modelID string) (api.APIClient, error) {
		p := foundryprovider.New()
		return p.NewClient(api.ProviderConfig{
			APIKey: os.Getenv("AZURE_OPENAI_API_KEY"),
			Model:  modelID,
		})
	}
	r.providersWithKey["foundry"] = func(modelID, apiKey string) (api.APIClient, error) {
		p := foundryprovider.New()
		return p.NewClient(api.ProviderConfig{APIKey: apiKey, Model: modelID})
	}
}

// Register adds a provider factory under the given name.
// Calling Register with an already-registered name replaces the factory and
// invalidates any previously cached entries for that provider, so subsequent
// Resolve calls go through the new factory.
func (r *Registry) Register(providerName string, factory ProviderFactory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[providerName] = factory

	// Invalidate any cached entries created by the previous factory for this
	// provider. The prefix is "<providerName>/" — see cacheKey construction
	// in Resolve.
	prefix := providerName + "/"
	for key := range r.cache {
		if strings.HasPrefix(key, prefix) {
			delete(r.cache, key)
		}
	}
}

// Resolve parses a model spec ("provider/model-id") and returns the
// corresponding APIClient, creating it via the provider factory if
// not already cached.
//
// Concurrency: the registry mutex is only held while looking up or creating
// the per-key cache entry — never during the factory call. Concurrent
// Resolve calls for the same spec rendezvous on the entry's sync.Once so the
// factory runs exactly once; concurrent calls for different specs run their
// factories in parallel.
func (r *Registry) Resolve(spec string) (api.APIClient, error) {
	providerName, modelID, err := ParseModelSpec(spec)
	if err != nil {
		return nil, err
	}

	cacheKey := providerName + "/" + modelID

	// Get-or-create the cache entry under the lock (fast — no I/O).
	r.mu.Lock()
	entry, ok := r.cache[cacheKey]
	if !ok {
		entry = &cacheEntry{}
		r.cache[cacheKey] = entry
	}
	factory, hasFactory := r.providers[providerName]
	r.mu.Unlock()

	if !hasFactory {
		// Drop the just-created empty entry so a later Register can succeed
		// without being shadowed by a permanently-failed once.
		r.mu.Lock()
		if cached, ok := r.cache[cacheKey]; ok && cached == entry {
			delete(r.cache, cacheKey)
		}
		r.mu.Unlock()
		return nil, fmt.Errorf("model: unknown provider %q (spec: %q)", providerName, spec)
	}

	// Run the factory exactly once per key, without holding the registry lock.
	entry.once.Do(func() {
		client, ferr := factory(modelID)
		if ferr != nil {
			entry.err = fmt.Errorf("model: provider %q failed to create model %q: %w", providerName, modelID, ferr)
			return
		}
		entry.client = client
	})
	if entry.err != nil {
		return nil, entry.err
	}
	return entry.client, nil
}

// Capabilities returns the capabilities of the model identified by spec.
// Capabilities are derived from a static table keyed by provider and model family.
func (r *Registry) Capabilities(spec string) (ModelCapabilities, error) {
	providerName, modelID, err := ParseModelSpec(spec)
	if err != nil {
		return ModelCapabilities{}, err
	}

	// Resolve to validate the provider exists and cache the client.
	if _, err := r.Resolve(spec); err != nil {
		return ModelCapabilities{}, err
	}

	return capabilitiesForModel(providerName, modelID), nil
}

// ParseModelSpec splits "provider/model-id" into its components.
func ParseModelSpec(spec string) (providerName, modelID string, err error) {
	idx := strings.Index(spec, "/")
	if idx <= 0 || idx == len(spec)-1 {
		return "", "", fmt.Errorf("model: invalid spec %q (expected \"provider/model-id\")", spec)
	}
	return spec[:idx], spec[idx+1:], nil
}

// ResolveWithContext is the BYOK-aware variant of Resolve.
//
// When ctx carries per-run secrets.Credentials with a non-empty key
// for the requested provider, this method bypasses the cache and
// constructs a fresh APIClient using the override key. This is
// crucial in cloud mode: a single runner pod serially handles runs
// for many tenants, so caching one tenant's API key against a
// modelID would leak it across tenants.
//
// When ctx has no credentials (local mode, or no BYOK configured),
// this falls through to Resolve(spec) which uses the env-var fallback
// + per-process cache.
func (r *Registry) ResolveWithContext(ctx context.Context, spec string) (api.APIClient, error) {
	providerName, modelID, err := ParseModelSpec(spec)
	if err != nil {
		return nil, err
	}
	creds, hasCreds := credentialsLookup(ctx)
	if !hasCreds {
		return r.Resolve(spec)
	}
	overrideKey := creds(providerName)
	if overrideKey == "" {
		// No tenant-scoped key for this provider — fall back to the
		// shared resolver (env vars + cache).
		return r.Resolve(spec)
	}
	r.mu.Lock()
	factory, hasFactory := r.providersWithKey[providerName]
	r.mu.Unlock()
	if !hasFactory {
		// Provider doesn't expose a BYOK factory; fall back.
		return r.Resolve(spec)
	}
	return factory(modelID, overrideKey)
}

// credentialsLookup returns a closure that maps a provider name to
// its per-run plaintext API key, or an empty string when ctx carries
// no credentials. Defined as a function so the model package doesn't
// import pkg/secrets directly (the credentials lookup is wired by
// the runner via SetCredentialsLookup).
type credentialsResolver func(provider string) string

// credentialsLookupFn is the indirection that lets pkg/runner inject
// a Credentials reader at boot. Default: no-op (no per-run keys).
var credentialsLookupFn = func(ctx context.Context) (credentialsResolver, bool) {
	return nil, false
}

// SetCredentialsLookup wires a per-ctx credentials lookup. The
// runner calls this at boot once with a closure that reads from
// secrets.CredentialsFromContext and returns the per-provider key.
// Idempotent; the latest call wins.
func SetCredentialsLookup(fn func(ctx context.Context) (func(provider string) string, bool)) {
	credentialsLookupFn = func(ctx context.Context) (credentialsResolver, bool) {
		f, ok := fn(ctx)
		return credentialsResolver(f), ok
	}
}

func credentialsLookup(ctx context.Context) (credentialsResolver, bool) {
	return credentialsLookupFn(ctx)
}
