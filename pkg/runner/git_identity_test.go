package runner

import (
	"context"
	"testing"
)

// resolveForgeCommitterIdentity turns the push token into the canonical GitHub
// noreply identity so a pushed commit is attributed to the token's owner — not
// the stray "iterion" account the old fallback email mapped to. The happy path
// hits api.github.com (hardcoded), so here we assert the no-network
// short-circuits; the network path is exercised live.
func TestResolveForgeCommitterIdentity_ShortCircuits(t *testing.T) {
	if _, _, ok := resolveForgeCommitterIdentity(context.Background(), "https://github.com/acme/api", ""); ok {
		t.Error("empty token must not resolve")
	}
	if _, _, ok := resolveForgeCommitterIdentity(context.Background(), "https://gitlab.com/acme/api", "tok"); ok {
		t.Error("non-github host must fall back (ok=false)")
	}
	if _, _, ok := resolveForgeCommitterIdentity(context.Background(), "://bad", "tok"); ok {
		t.Error("unparseable repo url must fall back")
	}
}

// The fallback identity must never be a resolvable GitHub account: an
// `.invalid` domain (RFC 2606) guarantees it maps to no user.
func TestGitAuthorFallbackIsUnattributable(t *testing.T) {
	t.Setenv("ITERION_GIT_AUTHOR_EMAIL", "")
	t.Setenv("ITERION_GIT_AUTHOR_NAME", "")
	if email := gitAuthorEmail(); email == "iterion@users.noreply.github.com" {
		t.Fatal("fallback still maps to the real 'iterion' account")
	}
	if got := gitAuthorEmail(); got != "iterion-runner@bot.iterion.invalid" {
		t.Errorf("fallback email = %q", got)
	}
}
