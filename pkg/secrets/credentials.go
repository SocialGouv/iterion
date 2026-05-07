package secrets

import (
	"context"
	"errors"
)

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
	// CLI subprocess. Empty when no OAuth-forfait is in play.
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

// ErrOAuthForfaitInThirdParty is the sentinel error guarding the
// claw backend (and any other in-process LLM SDK consumer) from
// using a Claude Pro/Max OAuth bearer token. Reusing the forfait
// outside the official Claude Code CLI surface violates Anthropic's
// Consumer Terms — see memory feedback_no_anthropic_oauth_in_third_party.
//
// Callers should invoke GuardThirdPartyOAuth right before consuming
// a credential for an in-process LLM call. The delegate backends
// (claude_code, codex) which spawn the upstream CLI are exempt: the
// CLI itself remains the authorised consumer in that path.
var ErrOAuthForfaitInThirdParty = errors.New("secrets: refusing to use Claude Code OAuth-forfait via third-party SDK (CGU violation)")

// GuardThirdPartyOAuth returns ErrOAuthForfaitInThirdParty when the
// given ctx has an OAuth-forfait connection for kind but no
// matching API key for provider — i.e. the only available
// credential is the forfait, which is forbidden in this code path.
//
// Returns nil when the ctx has no credentials, when an API key IS
// available, or when no OAuth credential of that kind is present.
func GuardThirdPartyOAuth(ctx context.Context, provider Provider, kind OAuthKind) error {
	creds, ok := CredentialsFromContext(ctx)
	if !ok {
		return nil
	}
	if creds.APIKey(provider) != "" {
		return nil
	}
	if creds.OAuthDir(string(kind)) == "" {
		return nil
	}
	return ErrOAuthForfaitInThirdParty
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
