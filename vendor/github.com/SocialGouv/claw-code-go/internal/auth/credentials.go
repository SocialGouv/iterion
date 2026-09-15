// Package auth — multi-provider credential storage.
// Credentials are persisted to ~/.claw-code/credentials.json (mode 0600).
package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ProviderCredentials holds authentication credentials for a single AI provider.
type ProviderCredentials struct {
	AuthMethod string     `json:"auth_method"` // "api_key" or "oauth"
	APIKey     string     `json:"api_key,omitempty"`
	OAuth      *TokenData `json:"oauth,omitempty"`
}

// CredentialStore holds credentials for all configured providers plus the active selection.
type CredentialStore struct {
	ActiveProvider string                          `json:"active_provider"`
	Providers      map[string]*ProviderCredentials `json:"providers"`
}

// credentialsFilePath returns the path to ~/.claw-code/credentials.json.
func credentialsFilePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claw-code", "credentials.json"), nil
}

// LoadCredentialStore reads the credential store from disk.
// Returns an empty (non-nil) store if the file does not exist.
func LoadCredentialStore() (*CredentialStore, error) {
	path, err := credentialsFilePath()
	if err != nil {
		return emptyCredentialStore(), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return emptyCredentialStore(), nil
		}
		return nil, err
	}
	var store CredentialStore
	if err := json.Unmarshal(data, &store); err != nil {
		return emptyCredentialStore(), nil
	}
	if store.Providers == nil {
		store.Providers = map[string]*ProviderCredentials{}
	}
	return &store, nil
}

// SaveCredentialStore writes the credential store to disk with mode 0600.
func SaveCredentialStore(store *CredentialStore) error {
	path, err := credentialsFilePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// SetProviderAPIKey saves an API key for the given provider and marks it as active.
func SetProviderAPIKey(provider, apiKey string) error {
	store, err := LoadCredentialStore()
	if err != nil {
		return err
	}
	store.Providers[provider] = &ProviderCredentials{
		AuthMethod: "api_key",
		APIKey:     apiKey,
	}
	store.ActiveProvider = provider
	return SaveCredentialStore(store)
}

// SetProviderOAuth saves OAuth tokens for the given provider and marks it as active.
func SetProviderOAuth(provider string, td *TokenData) error {
	store, err := LoadCredentialStore()
	if err != nil {
		return err
	}
	store.Providers[provider] = &ProviderCredentials{
		AuthMethod: "oauth",
		OAuth:      td,
	}
	store.ActiveProvider = provider
	return SaveCredentialStore(store)
}

// GetActiveProvider returns the active provider name from the credential store.
// Falls back to "anthropic" if no store exists or no provider is set.
func GetActiveProvider() string {
	store, err := LoadCredentialStore()
	if err != nil || store.ActiveProvider == "" {
		return "anthropic"
	}
	return store.ActiveProvider
}

// ResolveCredentials returns the provider, credential token, and auth method for the
// currently active provider.
//
// Resolution order:
//  1. ANTHROPIC_API_KEY env var  → provider "anthropic" / method "api_key"
//  2. OPENAI_API_KEY env var     → provider "openai"    / method "api_key"
//  3. Active provider in ~/.claw-code/credentials.json
//  4. Legacy ~/.claw-code/auth.json (Anthropic OAuth from Phase 3 flows)
func ResolveCredentials() (provider, token, method string, err error) {
	// Env-var overrides take precedence over any stored state.
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		return "anthropic", key, "api_key", nil
	}
	if key := os.Getenv("OPENAI_API_KEY"); key != "" {
		return "openai", key, "api_key", nil
	}

	// Try the credentials store.
	store, _ := LoadCredentialStore()
	activeProv := store.ActiveProvider
	if activeProv == "" {
		activeProv = "anthropic"
	}

	if cred, ok := store.Providers[activeProv]; ok && cred != nil {
		switch cred.AuthMethod {
		case "api_key":
			if cred.APIKey != "" {
				return activeProv, cred.APIKey, "api_key", nil
			}
		case "oauth":
			if cred.OAuth != nil {
				td := cred.OAuth
				if IsExpired(td) {
					if td.RefreshToken != "" {
						td, err = RefreshToken(td.RefreshToken)
						if err != nil {
							return "", "", "", fmt.Errorf("refresh oauth token: %w", err)
						}
						_ = SetProviderOAuth(activeProv, td)
					} else {
						return "", "", "", fmt.Errorf("oauth token expired; run /login")
					}
				}
				return activeProv, td.AccessToken, "oauth", nil
			}
		}
	}

	// Legacy fallback: ~/.claw-code/auth.json written by the original /auth login flow.
	if activeProv == "anthropic" {
		td, legErr := LoadTokens()
		if legErr == nil && !IsExpired(td) {
			return "anthropic", td.AccessToken, "oauth", nil
		}
	}

	return "", "", "", fmt.Errorf("no credentials found — run /login to authenticate")
}

func emptyCredentialStore() *CredentialStore {
	return &CredentialStore{
		Providers: map[string]*ProviderCredentials{},
	}
}
