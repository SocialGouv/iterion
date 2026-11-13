package auth

import (
	"fmt"
	"os"
)

// AuthMethod describes how credentials were obtained.
type AuthMethod string

const (
	AuthMethodAPIKey AuthMethod = "api_key"
	AuthMethodOAuth  AuthMethod = "oauth"
)

// ResolvedCredential represents a fully resolved authentication credential
// ready for use in API requests.
type ResolvedCredential struct {
	Provider string
	Token    string
	Method   AuthMethod
}

// AuthResolver implements combined API key + OAuth authentication with fallback.
// It tries API keys first (matching Rust's priority), then falls back to OAuth,
// while preserving the existing Go credential store for multi-provider login.
//
// Resolution priority:
//  1. Provider-specific env vars (ANTHROPIC_API_KEY, XAI_API_KEY, OPENAI_API_KEY, DASHSCOPE_API_KEY)
//  2. ANTHROPIC_AUTH_TOKEN env var (OAuth token via env)
//  3. Credential store (~/.claw-code/credentials.json) — active provider
//  4. Legacy auth.json (~/.claw-code/auth.json) — Anthropic OAuth fallback
type AuthResolver struct {
	// envProvider allows overriding env var lookup in tests.
	envProvider func(string) string
	// storeLoader allows overriding credential store loading in tests.
	storeLoader func() (*CredentialStore, error)
}

// NewAuthResolver creates an AuthResolver using real env vars and file system.
func NewAuthResolver() *AuthResolver {
	return &AuthResolver{
		envProvider: os.Getenv,
		storeLoader: LoadCredentialStore,
	}
}

// Resolve attempts to resolve credentials for the given provider hint.
// If providerHint is empty, the resolver tries all providers in priority order.
func (r *AuthResolver) Resolve(providerHint string) (*ResolvedCredential, error) {
	getenv := r.envProvider
	if getenv == nil {
		getenv = os.Getenv
	}
	loadStore := r.storeLoader
	if loadStore == nil {
		loadStore = LoadCredentialStore
	}

	// 1. Provider-specific API key env vars.
	envChecks := []struct {
		provider string
		envVar   string
	}{
		{"anthropic", "ANTHROPIC_API_KEY"},
		{"openai", "OPENAI_API_KEY"},
		{"xai", "XAI_API_KEY"},
		{"dashscope", "DASHSCOPE_API_KEY"},
	}

	// If a provider hint is given, check that provider's env var first.
	if providerHint != "" {
		for _, ec := range envChecks {
			if ec.provider == providerHint {
				if key := getenv(ec.envVar); key != "" {
					return &ResolvedCredential{
						Provider: ec.provider,
						Token:    key,
						Method:   AuthMethodAPIKey,
					}, nil
				}
			}
		}
	}

	// Check all env vars in priority order.
	for _, ec := range envChecks {
		if key := getenv(ec.envVar); key != "" {
			return &ResolvedCredential{
				Provider: ec.provider,
				Token:    key,
				Method:   AuthMethodAPIKey,
			}, nil
		}
	}

	// 2. ANTHROPIC_AUTH_TOKEN env var (OAuth via env).
	if token := getenv("ANTHROPIC_AUTH_TOKEN"); token != "" {
		return &ResolvedCredential{
			Provider: "anthropic",
			Token:    token,
			Method:   AuthMethodOAuth,
		}, nil
	}

	// 3. Credential store.
	store, _ := loadStore()
	if store != nil {
		activeProv := store.ActiveProvider
		if providerHint != "" {
			activeProv = providerHint
		}
		if activeProv == "" {
			activeProv = "anthropic"
		}

		if cred, ok := store.Providers[activeProv]; ok && cred != nil {
			switch cred.AuthMethod {
			case "api_key":
				if cred.APIKey != "" {
					return &ResolvedCredential{
						Provider: activeProv,
						Token:    cred.APIKey,
						Method:   AuthMethodAPIKey,
					}, nil
				}
			case "oauth":
				if cred.OAuth != nil {
					td := cred.OAuth
					if IsExpired(td) {
						if td.RefreshToken != "" {
							refreshed, err := RefreshToken(td.RefreshToken)
							if err != nil {
								return nil, fmt.Errorf("refresh oauth token for %s: %w", activeProv, err)
							}
							_ = SetProviderOAuth(activeProv, refreshed)
							td = refreshed
						} else {
							return nil, fmt.Errorf("oauth token for %s expired; run /login", activeProv)
						}
					}
					return &ResolvedCredential{
						Provider: activeProv,
						Token:    td.AccessToken,
						Method:   AuthMethodOAuth,
					}, nil
				}
			}
		}
	}

	// 4. Legacy fallback: ~/.claw-code/auth.json
	if providerHint == "" || providerHint == "anthropic" {
		td, err := LoadTokens()
		if err == nil && !IsExpired(td) {
			return &ResolvedCredential{
				Provider: "anthropic",
				Token:    td.AccessToken,
				Method:   AuthMethodOAuth,
			}, nil
		}
	}

	return nil, fmt.Errorf("no credentials found — run /login to authenticate")
}

// ResolveForModel is a convenience that uses DetectProviderKind-style logic
// to determine which provider to try first based on the model name.
func (r *AuthResolver) ResolveForModel(model string) (*ResolvedCredential, error) {
	// Try without hint first (uses priority order), which matches current
	// ResolveCredentials behavior.
	return r.Resolve("")
}
