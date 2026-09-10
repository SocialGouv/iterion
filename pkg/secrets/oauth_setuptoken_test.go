package secrets

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

const testSetupToken = "sk-ant-oat01-" + "AAAABBBBCCCCDDDDEEEEFFFF"

// The hazard this pins: wrapping a setup token stamps a computed expiry
// into the payload, so fingerprinting the PAYLOAD would hand the same
// credential a different identity on every upload — a fresh usage meter and
// a dropped account label each time, deterministically, which is worse than
// the blob-hash gap it would sit on top of.
//
// Identity must therefore depend on the token and nothing else. Two
// normalizations a year apart are the falsifying pair.
func TestNormalizeAnthropicBlob_IdentityIsTheTokenNotTheWrapper(t *testing.T) {
	t0 := time.Date(2026, 9, 8, 7, 0, 0, 0, time.UTC)
	t1 := t0.Add(365 * 24 * time.Hour)

	a, err := NormalizeAnthropicBlob([]byte(testSetupToken), t0)
	if err != nil {
		t.Fatalf("normalize at t0: %v", err)
	}
	b, err := NormalizeAnthropicBlob([]byte(testSetupToken), t1)
	if err != nil {
		t.Fatalf("normalize at t1: %v", err)
	}
	if !a.Wrapped || !b.Wrapped {
		t.Fatalf("a bare setup token was not recognised as one (wrapped=%v/%v)", a.Wrapped, b.Wrapped)
	}
	if !bytes.Equal(a.Identity, b.Identity) {
		t.Fatalf("identity moved with the clock: %q vs %q — every upload would open a new usage meter", a.Identity, b.Identity)
	}
	if string(a.Identity) != testSetupToken {
		t.Fatalf("identity = %q, want the token itself", a.Identity)
	}
	if bytes.Equal(a.Payload, b.Payload) {
		t.Fatal("payload did not move with the clock — the assumed expiry is not being stamped, so the record would read as 'Not logged in'")
	}
}

// Surrounding whitespace is what a token read from a file carries. It must
// not change the credential's identity, and it must not reach the token
// itself (a `Bearer <token>\n` header is refused by the provider with an
// error that names nothing useful).
func TestNormalizeAnthropicBlob_WhitespaceDoesNotChangeIdentity(t *testing.T) {
	now := time.Date(2026, 9, 8, 7, 0, 0, 0, time.UTC)
	clean, err := NormalizeAnthropicBlob([]byte(testSetupToken), now)
	if err != nil {
		t.Fatalf("normalize clean: %v", err)
	}
	fromFile, err := NormalizeAnthropicBlob([]byte("  "+testSetupToken+"\n"), now)
	if err != nil {
		t.Fatalf("normalize with whitespace: %v", err)
	}
	if !bytes.Equal(clean.Identity, fromFile.Identity) {
		t.Fatalf("a trailing newline forked the identity: %q vs %q", clean.Identity, fromFile.Identity)
	}
	if !bytes.Equal(clean.Payload, fromFile.Payload) {
		t.Fatalf("a trailing newline reached the payload:\n%s\n%s", clean.Payload, fromFile.Payload)
	}
}

// A wrapped token must satisfy the SAME ingestion gate a pasted
// credentials.json does — an expiry and at least one scope, absent which the
// Claude CLI reads the record as "Not logged in" and it never serves a run.
func TestNormalizeAnthropicBlob_WrappedTokenCarriesWhatTheCLIRequires(t *testing.T) {
	now := time.Date(2026, 9, 8, 7, 0, 0, 0, time.UTC)
	nb, err := NormalizeAnthropicBlob([]byte(testSetupToken), now)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	v, err := ParseAnthropicView(nb.Payload)
	if err != nil {
		t.Fatalf("the wrapper is not parseable as credentials.json: %v", err)
	}
	if v.ClaudeAIOauth.AccessToken != testSetupToken {
		t.Fatalf("accessToken = %q, want the token", v.ClaudeAIOauth.AccessToken)
	}
	if want := now.Add(SetupTokenAssumedLifetime).UnixMilli(); v.ClaudeAIOauth.ExpiresAt != want {
		t.Fatalf("expiresAt = %d, want %d (now + %s)", v.ClaudeAIOauth.ExpiresAt, want, SetupTokenAssumedLifetime)
	}
	if len(v.ClaudeAIOauth.Scopes) == 0 {
		t.Fatal("no scope: the CLI reads that as 'Not logged in'")
	}
	if v.ClaudeAIOauth.RefreshToken != "" {
		t.Fatalf("refreshToken = %q, want empty — a setup token cannot be renewed and the record must say so", v.ClaudeAIOauth.RefreshToken)
	}
}

// Everything that is NOT a setup token must reach the existing parser
// untouched, so a credentials.json keeps its byte-for-byte identity and a
// pasted transcript keeps earning its own typed refusal rather than a
// vaguer one from the new path.
func TestNormalizeAnthropicBlob_LeavesEverythingElseAlone(t *testing.T) {
	now := time.Date(2026, 9, 8, 7, 0, 0, 0, time.UTC)
	for _, in := range []string{
		`{"claudeAiOauth":{"accessToken":"sk-ant-oat01-x","expiresAt":4102444800000,"scopes":["user:inference"]}}`,
		"  " + `{"claudeAiOauth":{}}`,
		"Welcome to Claude Code\n> /login\nsk-ant-oat01-x",
		"",
		"sk-ant-api03-not-an-oauth-token",
	} {
		nb, err := NormalizeAnthropicBlob([]byte(in), now)
		if err != nil {
			t.Fatalf("normalize(%.30q): unexpected error %v", in, err)
		}
		if nb.Wrapped {
			t.Fatalf("normalize(%.30q) wrapped a blob that is not a bare setup token", in)
		}
		if !bytes.Equal(nb.Payload, []byte(in)) || !bytes.Equal(nb.Identity, []byte(in)) {
			t.Fatalf("normalize(%.30q) altered a blob it should have passed through", in)
		}
	}
}

// A token that picked up an inner space or control character is a paste
// accident. It must be refused HERE, typed, rather than sealed and handed to
// a fleet of runs that all die on "Header has invalid value".
func TestNormalizeAnthropicBlob_RefusesAMangledToken(t *testing.T) {
	now := time.Date(2026, 9, 8, 7, 0, 0, 0, time.UTC)
	for _, in := range []string{
		"sk-ant-oat01-aaa bbb",
		// A zero-width space, escaped: written literally it is invisible in
		// the source too, which is how it reaches a token in the first place.
		"sk-ant-oat01-aaa" + "\u200b" + "bbb",
	} {
		if _, err := NormalizeAnthropicBlob([]byte(in), now); err == nil {
			t.Fatalf("normalize(%q) accepted a mangled token", in)
		} else if !strings.Contains(err.Error(), "setup token") {
			t.Fatalf("refusal does not name the field: %v", err)
		}
	}
}
