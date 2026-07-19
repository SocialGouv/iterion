// Package model provides the ModelRegistry and claw-based NodeExecutor
// for resolving "provider/model-id" specs and executing LLM nodes.
package model

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	anthropicprovider "github.com/SocialGouv/claw-code-go/pkg/api/providers/anthropic"
	bedrockprovider "github.com/SocialGouv/claw-code-go/pkg/api/providers/bedrock"
	foundryprovider "github.com/SocialGouv/claw-code-go/pkg/api/providers/foundry"
	openaiprovider "github.com/SocialGouv/claw-code-go/pkg/api/providers/openai"
	vertexprovider "github.com/SocialGouv/claw-code-go/pkg/api/providers/vertex"

	"github.com/SocialGouv/iterion/pkg/secrets"
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
// claudeForfaitWarnOnce guards the one-time stderr warning emitted when the
// claw backend is wired to the Claude Code subscription forfait (dev-purpose;
// see the anthropic provider factory below).
var claudeForfaitWarnOnce sync.Once

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
		// ANTHROPIC_BASE_URL forwards to claw the same redirect the
		// Anthropic SDK and the Claude Code CLI already honour. This is
		// what enables `backend: claw` workflows to reach z.ai's
		// Anthropic-compatible endpoint (GLM-4.5/4.6 via the Coding
		// Plan): set ANTHROPIC_BASE_URL=https://api.z.ai/api/anthropic
		// and ANTHROPIC_AUTH_TOKEN (claw's Anthropic provider treats
		// either ANTHROPIC_API_KEY or ANTHROPIC_AUTH_TOKEN as auth).
		//
		// ZAI_API_KEY env-fallback: when no Anthropic env auth is
		// present but ZAI_API_KEY is, treat the request as targeting
		// z.ai and synthesise the bearer + base URL automatically.
		// Mirrors the Claude-Code delegate's anthropicCredOptsForCLI
		// so `backend: claw` and `backend: claude_code` behave the
		// same way for desktop users who just dropped a ZAI_API_KEY
		// line in ~/.iterion/env.
		apiKey := os.Getenv("ANTHROPIC_API_KEY")
		baseURL := os.Getenv("ANTHROPIC_BASE_URL")
		authToken := os.Getenv("ANTHROPIC_AUTH_TOKEN")
		if apiKey == "" && authToken == "" {
			if zai := os.Getenv("ZAI_API_KEY"); zai != "" {
				apiKey = zai
				if baseURL == "" {
					baseURL = secrets.ZAIDefaultBaseURL
				}
			}
		}
		cfg := api.ProviderConfig{
			APIKey:  apiKey,
			Model:   modelID,
			BaseURL: baseURL,
		}
		// Claude Code subscription "forfait": when only ANTHROPIC_AUTH_TOKEN is
		// set (the OAuth access token from ~/.claude/.credentials.json) against
		// real api.anthropic.com — not a z.ai/bigmodel BYOK base URL, which uses
		// the token as an x-api-key-style key — pass it as the OAuth bearer so
		// claw sends `Authorization: Bearer` + the `anthropic-beta: oauth-2025-04-20`
		// header the API requires (see claw client.go).
		//
		// KNOWN LIMITATION — effectively unusable, dev-purpose only. The token
		// authenticates, but the forfait's rate limiter throttles non-Claude-Code
		// clients to ~zero: a claw request 429s immediately (rate_limit_error, no
		// Retry-After), even with fresh daily quota, while the official `claude`
		// CLI on the same token+model works fine. Anthropic scopes the forfait to
		// Claude Code (Consumer Terms). What claw "lacks" is the Claude Code client
		// identity (User-Agent / client headers) — reproducing it would be spoofing,
		// not a fix, so we don't. Use ANTHROPIC_API_KEY, z.ai (ZAI_API_KEY), or
		// another provider for real runs. A one-time stderr warning fires below.
		lowBase := strings.ToLower(baseURL)
		isZAI := strings.Contains(lowBase, "z.ai") || strings.Contains(lowBase, "bigmodel")
		if apiKey == "" && authToken != "" && !isZAI {
			cfg.OAuthToken = authToken
			claudeForfaitWarnOnce.Do(func() {
				fmt.Fprintln(os.Stderr, "⚠️  claw: using the Claude Code subscription forfait "+
					"(ANTHROPIC_AUTH_TOKEN, no ANTHROPIC_API_KEY) for an anthropic/* model. This is "+
					"DEV-PURPOSE ONLY and effectively unusable: the forfait throttles non-Claude-Code "+
					"clients, so requests 429 immediately (Consumer Terms scope it to Claude Code). "+
					"Use ANTHROPIC_API_KEY, z.ai (ZAI_API_KEY), or another provider for real runs.")
			})
		}
		return p.NewClient(withClientIdentity(cfg))
	}
	r.providersWithKey["anthropic"] = func(modelID, apiKey string) (api.APIClient, error) {
		p := anthropicprovider.New()
		// BYOK path still honours ANTHROPIC_BASE_URL for now. A
		// per-tenant base-URL override should ride alongside the BYOK
		// key record once the cloud-side z.ai BYOK lands — see
		// .plans/zai-glm-oauth.md.
		return p.NewClient(withClientIdentity(api.ProviderConfig{
			APIKey:  apiKey,
			Model:   modelID,
			BaseURL: os.Getenv("ANTHROPIC_BASE_URL"),
		}))
	}
	r.providers["openai"] = func(modelID string) (api.APIClient, error) {
		p := openaiprovider.New()
		// OPENAI_BASE_URL forwards for OpenRouter / Ollama / vLLM / any
		// other OpenAI-shaped backend (same shape as ANTHROPIC_BASE_URL
		// above). When set, it also disables the ChatGPT-OAuth path so
		// we don't send masquerading codex_cli_rs headers to a third
		// party.
		cfg := api.ProviderConfig{
			Model:   modelID,
			BaseURL: os.Getenv("OPENAI_BASE_URL"),
		}
		// Resolution: an explicit OPENAI_API_KEY wins by default — it's
		// the standard surface for both CI and BYOK setups, and treating
		// a user-set env var as deliberate avoids silently spending
		// someone else's ChatGPT subscription. ChatGPT-forfait OAuth
		// (sourced from Codex CLI's auth.json) is used when:
		//   - OPENAI_API_KEY is unset, OR
		//   - ITERION_OPENAI_USE_OAUTH=1 forces the forfait path even
		//     when a key is present.
		// ITERION_OPENAI_USE_OAUTH=0 disables OAuth entirely.
		// An explicit OPENAI_BASE_URL also disables OAuth so we never
		// masquerade Codex headers to a third-party endpoint.
		//
		// Known limitations (track separately, not blocking v1):
		//   - Stale token: the resolved client is cached for the life of
		//     the process; long-running daemons (studio, dispatcher)
		//     keep using the access_token captured at first resolve.
		//     Codex CLI rotates tokens ~hourly; restart the daemon to
		//     refresh, or set a tight max_duration on dispatcher jobs.
		//   - This disk factory is the process-env / desktop path. Cloud
		//     mode instead resolves the tenant's per-run codex forfait via
		//     ResolveWithContext → openAIFromCtxForfait (reads
		//     Credentials.OAuthDir("codex")), so a runner with no ~/.codex
		//     still uses the connected ChatGPT-forfait.
		apiKey := os.Getenv("OPENAI_API_KEY")
		oauthPref := os.Getenv("ITERION_OPENAI_USE_OAUTH")
		oauthDisabled := oauthPref == "0" || cfg.BaseURL != ""
		oauthForced := oauthPref == "1"
		shouldTryOAuth := !oauthDisabled && (apiKey == "" || oauthForced)
		if shouldTryOAuth {
			if view, err := secrets.LoadCodexCredentialsFromDisk(); err == nil && view.IsChatGPTMode() {
				applyCodexOAuth(&cfg, view)
				return p.NewClient(withClientIdentity(cfg))
			}
		}
		cfg.APIKey = apiKey
		return p.NewClient(withClientIdentity(cfg))
	}
	r.providersWithKey["openai"] = func(modelID, apiKey string) (api.APIClient, error) {
		p := openaiprovider.New()
		return p.NewClient(withClientIdentity(api.ProviderConfig{
			APIKey:  apiKey,
			Model:   modelID,
			BaseURL: os.Getenv("OPENAI_BASE_URL"),
		}))
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
		return p.NewClient(withClientIdentity(api.ProviderConfig{Model: modelID}))
	}
	// Azure Foundry (Azure OpenAI Service) — auth via AZURE_OPENAI_API_KEY
	// or azidentity DefaultAzureCredential. Requires AZURE_OPENAI_ENDPOINT
	// and AZURE_OPENAI_DEPLOYMENT (or modelID is treated as the deployment).
	r.providers["foundry"] = func(modelID string) (api.APIClient, error) {
		p := foundryprovider.New()
		return p.NewClient(withClientIdentity(api.ProviderConfig{
			APIKey: os.Getenv("AZURE_OPENAI_API_KEY"),
			Model:  modelID,
		}))
	}
	r.providersWithKey["foundry"] = func(modelID, apiKey string) (api.APIClient, error) {
		p := foundryprovider.New()
		return p.NewClient(withClientIdentity(api.ProviderConfig{APIKey: apiKey, Model: modelID}))
	}
	// xAI Grok — OpenAI-compatible chat completions at api.x.ai.
	// Auth via XAI_API_KEY (or cloud BYOK under provider "xai"). Model
	// specs look like `xai/grok-3`, `xai/grok-3-mini`, `xai/grok-4`.
	// claw-code-go reuses the openai provider for xAI (SelectProvider
	// maps "xai" → openaiprovider); we do the same so StreamResponse,
	// tool calling, and reasoning-model detection stay shared.
	r.providers["xai"] = func(modelID string) (api.APIClient, error) {
		p := openaiprovider.New()
		return p.NewClient(withClientIdentity(api.ProviderConfig{
			APIKey:  os.Getenv("XAI_API_KEY"),
			Model:   modelID,
			BaseURL: xaiBaseURL(),
		}))
	}
	r.providersWithKey["xai"] = func(modelID, apiKey string) (api.APIClient, error) {
		p := openaiprovider.New()
		return p.NewClient(withClientIdentity(api.ProviderConfig{
			APIKey:  apiKey,
			Model:   modelID,
			BaseURL: xaiBaseURL(),
		}))
	}
}

// xaiBaseURL resolves the OpenAI-compatible host for xAI. Prefer an
// explicit XAI_BASE_URL (operator / proxy override); otherwise the
// published api.x.ai host. Trailing `/v1` is stripped so a user who
// pastes the SDK-style base URL does not end up posting to
// `…/v1/v1/chat/completions` (the openai client always appends
// `/v1/chat/completions`).
func xaiBaseURL() string {
	base := strings.TrimSpace(os.Getenv("XAI_BASE_URL"))
	if base == "" {
		base = secrets.XAIDefaultBaseURL
	}
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(strings.ToLower(base), "/v1") {
		base = strings.TrimRight(base[:len(base)-3], "/")
	}
	return base
}

// withClientIdentity injects ITERION_LLM_USER_AGENT (the iterion-branded
// User-Agent override) into a claw ProviderConfig; claw resolves the rest of
// the precedence chain. See docs/backends.md § Client identity.
func withClientIdentity(cfg api.ProviderConfig) api.ProviderConfig {
	cfg.UserAgent = os.Getenv("ITERION_LLM_USER_AGENT")
	return cfg
}

// codexCLIVersion resolves the Codex CLI version string to send in the
// `version:` HTTP header when claw operates in ChatGPT-OAuth mode. OpenAI's
// backend gates model availability on this value (e.g. gpt-5.5 requires
// codex-cli >= 0.130). Resolution precedence:
//  1. ITERION_CODEX_VERSION env var (operator override; lets a fresh-but-
//     binary-stale environment claim newer model access)
//  2. `codex --version` parsed at most once per process (cached)
//  3. "" — claw-code-go falls back to its baked-in version string
var (
	codexVersionOnce   sync.Once
	codexVersionCached string
)

func codexCLIVersion() string {
	if v := os.Getenv("ITERION_CODEX_VERSION"); v != "" {
		return v
	}
	codexVersionOnce.Do(func() {
		// Timeout guards against a wedged codex binary stalling every
		// LLM call forever (sync.Once would cache the hang).
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "codex", "--version").Output()
		if err != nil {
			return
		}
		// Output format: "codex-cli 0.130.0\n"
		fields := strings.Fields(strings.TrimSpace(string(out)))
		if len(fields) == 0 {
			return
		}
		codexVersionCached = fields[len(fields)-1]
	})
	return codexVersionCached
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
		// No tenant-scoped BYOK key. For openai, try the tenant's RESOLVED
		// codex ChatGPT-forfait before falling back: in cloud mode the disk
		// factory (r.Resolve) reads the pod's ~/.codex, which is empty — the
		// per-run forfait lives in Credentials.OAuthDir("codex"). Closes the
		// documented "factory learns to consume creds.OAuthDir(codex)" gap.
		if providerName == "openai" {
			if client, ok, err := r.openAIFromCtxForfait(ctx, modelID); ok {
				return client, err
			}
		}
		// No tenant-scoped credential for this provider — fall back to the
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

// applyCodexOAuth stamps a Codex ChatGPT-forfait view onto a provider config
// (Bearer OAuth token + account id + client version header). Shared by the
// disk-based factory and the ctx-resolved (cloud) path so both build an
// identical ChatGPT-mode client.
func applyCodexOAuth(cfg *api.ProviderConfig, view secrets.CodexCredentialsView) {
	cfg.OAuthToken = view.Tokens.AccessToken
	cfg.OpenAIChatGPTAccountID = view.Tokens.AccountID
	cfg.OpenAIClientVersion = codexCLIVersion()
}

// openAIFromCtxForfait builds an OpenAI ChatGPT-forfait client from the
// tenant's per-run-materialised codex credentials (Credentials.OAuthDir),
// honouring the same disable knobs as the disk factory. Returns ok=false when
// OAuth is disabled, no codex dir is resolved, or the dir has no ChatGPT-mode
// auth.json — the caller then falls back to the shared resolver.
func (r *Registry) openAIFromCtxForfait(ctx context.Context, modelID string) (api.APIClient, bool, error) {
	if os.Getenv("ITERION_OPENAI_USE_OAUTH") == "0" || os.Getenv("OPENAI_BASE_URL") != "" {
		return nil, false, nil
	}
	dir := oauthDirLookup(ctx, "codex")
	if dir == "" {
		return nil, false, nil
	}
	view, err := secrets.LoadCodexCredentialsFrom(dir)
	if err != nil || !view.IsChatGPTMode() {
		return nil, false, nil
	}
	cfg := api.ProviderConfig{Model: modelID, BaseURL: os.Getenv("OPENAI_BASE_URL")}
	applyCodexOAuth(&cfg, view)
	client, cerr := openaiprovider.New().NewClient(withClientIdentity(cfg))
	return client, true, cerr
}

// oauthDirResolver maps an OAuth kind ("codex" / "claude_code") to its
// per-run materialised credentials dir, or "" when absent.
type oauthDirResolver func(kind string) string

// oauthDirLookupFn is the indirection that lets pkg/runner inject a reader of
// Credentials.OAuthDir from ctx, keeping this package's coupling to the runner
// injection-based (mirrors credentialsLookupFn). Default: no-op.
var oauthDirLookupFn = func(ctx context.Context) (oauthDirResolver, bool) {
	return nil, false
}

// SetOAuthDirLookup wires a per-ctx OAuth-dir reader. The runner calls this at
// boot with a closure over secrets.CredentialsFromContext(ctx).OAuthDir.
// Idempotent; latest call wins.
func SetOAuthDirLookup(fn func(ctx context.Context) (func(kind string) string, bool)) {
	oauthDirLookupFn = func(ctx context.Context) (oauthDirResolver, bool) {
		f, ok := fn(ctx)
		return oauthDirResolver(f), ok
	}
}

func oauthDirLookup(ctx context.Context, kind string) string {
	f, ok := oauthDirLookupFn(ctx)
	if !ok || f == nil {
		return ""
	}
	return f(kind)
}
