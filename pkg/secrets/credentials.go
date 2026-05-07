package secrets

import "context"

// Credentials carries the resolved per-run BYOK plaintext keyed by
// provider. Stamped into context by the runner right before the
// engine starts, consumed by pkg/backend/model/registry.go and the
// claude_code/codex delegate backends.
//
// Plaintexts here are sensitive: never log them, never include them
// in events. The runner zeroes the slice after the run completes
// (best effort — Go does not give us secure-erase guarantees, but
// the bundle's TTL bounds exposure on the wire and at rest).
type Credentials struct {
	APIKeys map[Provider]string
	// OAuthCredentialFiles maps "claude_code" / "codex" → the
	// absolute path of a temp directory holding the materialised
	// credentials.json or auth.json. The delegate backends pass
	// this directory via CLAUDE_CONFIG_DIR / CODEX_HOME to the
	// CLI subprocess. Phase D wires this; Phase C leaves it nil.
	OAuthCredentialFiles map[string]string
}

// APIKey returns the plaintext API key for the requested provider
// (or "" when none is configured for the run).
func (c Credentials) APIKey(p Provider) string {
	if c.APIKeys == nil {
		return ""
	}
	return c.APIKeys[p]
}

// OAuthDir returns the temp dir holding sealed credentials for kind
// (claude_code / codex), or "" when no OAuth bundle was injected.
func (c Credentials) OAuthDir(kind string) string {
	if c.OAuthCredentialFiles == nil {
		return ""
	}
	return c.OAuthCredentialFiles[kind]
}

type credentialsCtxKey struct{}

// WithCredentials returns a child ctx carrying the resolved
// credentials. Empty / zero-value Credentials are still stored so
// callers can detect "we are inside a per-run scope with no keys"
// vs "no credentials ctx at all" (env fallback).
func WithCredentials(parent context.Context, c Credentials) context.Context {
	return context.WithValue(parent, credentialsCtxKey{}, c)
}

// CredentialsFromContext returns the resolved credentials and a flag
// indicating whether a per-run scope was active at all.
func CredentialsFromContext(ctx context.Context) (Credentials, bool) {
	if ctx == nil {
		return Credentials{}, false
	}
	c, ok := ctx.Value(credentialsCtxKey{}).(Credentials)
	return c, ok
}
