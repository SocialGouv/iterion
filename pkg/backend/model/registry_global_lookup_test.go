package model

import (
	"context"
	"testing"
)

// credentialsLookupFn and oauthDirLookupFn are process globals: pkg/runner
// installs a reader at boot, and a test that installs a double has to give the
// previous one back or every test running after it in the same binary resolves
// through the double. "Idempotent, the latest call wins" is not a restore
// contract — it is the clobber, stated.
//
// Measured at b06f9b290: 30 install sites across 9 test files and 3 packages,
// 17 of them followed by a hand-written restore that reinstalls a HARDCODED
// no-op instead of the previous reader, and 13 followed by nothing at all
// (pkg/supervise/model_hint_test.go:251 was one of them). The two setters now
// return the restore the way modelspecs.SetDefault already does, which is the
// chokepoint: one line per site, restoring what was actually there, and
// impossible to forget because dropping the result is what the guard below
// refuses.

// readCredential reads the installed lookup the way ResolveWithContext does.
func readCredential(t *testing.T, ctx context.Context, provider string) (string, bool) {
	t.Helper()
	resolve, ok := credentialsLookup(ctx)
	if !ok || resolve == nil {
		return "", false
	}
	return resolve(provider), true
}

// One scenario per global, isolated: a restore gives back the reader that was
// there, not a no-op that happens to look like one on an empty binary. The
// nesting is the half a single install cannot show — with the old hand-written
// restore, the inner cleanup destroyed the OUTER install.
func TestTheCredentialsLookupRestoresThePreviousReader(t *testing.T) {
	ctx := context.Background()
	if _, ok := credentialsLookup(ctx); ok {
		t.Fatal("a credentials lookup is already installed in this binary — an earlier test leaked one, which is exactly the clobber under test")
	}

	restoreOuter := SetCredentialsLookup(func(context.Context) (func(string) string, bool) {
		return func(string) string { return "outer" }, true
	})
	if got, ok := readCredential(t, ctx, "anthropic"); !ok || got != "outer" {
		t.Fatalf("after the first install the lookup answers (%q, %v), want (%q, true)", got, ok, "outer")
	}

	restoreInner := SetCredentialsLookup(func(context.Context) (func(string) string, bool) {
		return func(string) string { return "inner" }, true
	})
	if got, ok := readCredential(t, ctx, "anthropic"); !ok || got != "inner" {
		t.Fatalf("after the second install the lookup answers (%q, %v), want (%q, true)", got, ok, "inner")
	}

	restoreInner()
	if got, ok := readCredential(t, ctx, "anthropic"); !ok || got != "outer" {
		t.Fatalf("the second restore left (%q, %v); want the reader the FIRST install put there (%q) — a restore that reinstalls a hardcoded no-op destroys whatever was underneath", got, ok, "outer")
	}

	restoreOuter()
	if _, ok := credentialsLookup(ctx); ok {
		t.Fatal("the first restore left a lookup installed — the install outlives the test that wanted it, and every later test resolves through it")
	}
}

// The same scenario for the second global, isolated from the first: two guards
// that cover each other are two guards neither of which is tested.
func TestTheOAuthDirLookupRestoresThePreviousReader(t *testing.T) {
	ctx := context.Background()
	if got := oauthDirLookup(ctx, "claude"); got != "" {
		t.Fatalf("an OAuth-dir lookup is already installed in this binary (answers %q) — an earlier test leaked one", got)
	}

	restoreOuter := SetOAuthDirLookup(func(context.Context) (func(string) string, bool) {
		return func(string) string { return "/outer" }, true
	})
	if got := oauthDirLookup(ctx, "claude"); got != "/outer" {
		t.Fatalf("after the first install the lookup answers %q, want %q", got, "/outer")
	}

	restoreInner := SetOAuthDirLookup(func(context.Context) (func(string) string, bool) {
		return func(string) string { return "/inner" }, true
	})
	if got := oauthDirLookup(ctx, "claude"); got != "/inner" {
		t.Fatalf("after the second install the lookup answers %q, want %q", got, "/inner")
	}

	restoreInner()
	if got := oauthDirLookup(ctx, "claude"); got != "/outer" {
		t.Fatalf("the second restore left %q; want the reader the FIRST install put there (%q)", got, "/outer")
	}

	restoreOuter()
	if got := oauthDirLookup(ctx, "claude"); got != "" {
		t.Fatalf("the first restore left %q installed — the install outlives the test that wanted it", got)
	}
}
