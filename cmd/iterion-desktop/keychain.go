//go:build desktop

package main

import (
	"github.com/zalando/go-keyring"
)

// keychainService is the OS-level service identifier under which all
// secrets are stored. Conflicts with another product using the same name
// would mix entries, so keep this stable and unique.
const keychainService = "io.iterion.desktop"

// KnownAPIKeys is the canonical set of LLM provider API keys the desktop
// app knows how to surface in Settings UI and inject into the env. Adding
// to this list propagates to the Settings tab and onboarding wizard
// automatically.
var KnownAPIKeys = []string{
	"ANTHROPIC_API_KEY",
	"OPENAI_API_KEY",
	"OPENROUTER_API_KEY",
	"GROQ_API_KEY",
}

type secretStore interface {
	Get(key string) (string, error)
	Set(key, value string) error
	Delete(key string) error
}

// Keychain wraps go-keyring with a fixed service name and centralised
// error handling.
//
// Important security invariant: this package never logs secret values.
// Callers (bindings.go) must also treat returned values as opaque — never
// reflect them back to JS, never write to disk.
type Keychain struct{}

// NewKeychain returns a stateless Keychain wrapper. Errors surface only on
// individual operations.
func NewKeychain() *Keychain { return &Keychain{} }

// Get returns the stored secret for `key`, or an error. A missing key is
// returned as the underlying go-keyring "secret not found" error — callers
// typically treat this as an empty value (no shadow injection).
func (*Keychain) Get(key string) (string, error) {
	return keyring.Get(keychainService, key)
}

// Set stores `value` under `key`. Overwrites silently.
func (*Keychain) Set(key, value string) error {
	return keyring.Set(keychainService, key, value)
}

// Delete removes the entry. Idempotent: deleting a missing key is treated
// as success.
func (*Keychain) Delete(key string) error {
	if err := keyring.Delete(keychainService, key); err != nil {
		// go-keyring returns ErrNotFound when the entry doesn't exist;
		// the caller doesn't care about that distinction.
		if err == keyring.ErrNotFound {
			return nil
		}
		return err
	}
	return nil
}
