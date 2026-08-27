package secrets

import "testing"

// The meter identity must name the ACCOUNT, not the bytes that carry it.
// One forfait connected twice — a teammate pasting the same
// credentials.json, or the same login connected personally AND at org
// level — has to land on ONE meter: two would hand the deployment twice
// the ceiling its ITERION_USAGE_CAP_* percentages describe, which is the
// direction that fails open.
func TestOAuthIdentityFingerprint_SameGrantOneIdentity(t *testing.T) {
	// Byte-for-byte different renderings of one Anthropic grant: a
	// re-serialisation with different key order, whitespace, and a moved
	// expiry stamp. The refresh token is the grant.
	a := []byte(`{"claudeAiOauth":{"accessToken":"sk-ant-oat-aaaaaaaaaaaa","refreshToken":"sk-ant-ort-shared-1","expiresAt":1000}}`)
	b := []byte("{\n  \"claudeAiOauth\": {\n    \"expiresAt\": 9999,\n    \"refreshToken\": \"sk-ant-ort-shared-1\",\n    \"accessToken\": \"sk-ant-oat-bbbbbbbbbbbb\"\n  }\n}")
	if fpA, fpB := OAuthIdentityFingerprint(OAuthKindClaudeCode, a), OAuthIdentityFingerprint(OAuthKindClaudeCode, b); fpA != fpB {
		t.Errorf("one grant, two renderings = %q / %q: the shared forfait would meter twice", fpA, fpB)
	}

	// A genuinely different subscription — the lived rotation case — must
	// still open its own meter.
	other := []byte(`{"claudeAiOauth":{"accessToken":"sk-ant-oat-aaaaaaaaaaaa","refreshToken":"sk-ant-ort-other-2"}}`)
	if OAuthIdentityFingerprint(OAuthKindClaudeCode, a) == OAuthIdentityFingerprint(OAuthKindClaudeCode, other) {
		t.Error("two different grants share an identity: a rotated credential would inherit the old account's readings")
	}
}

// Codex carries the account itself, so even two SEPARATE logins to one
// ChatGPT account agree — the strongest identity available anywhere here.
func TestOAuthIdentityFingerprint_CodexPrefersTheAccountID(t *testing.T) {
	login1 := []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"at-1","refresh_token":"rt-1","account_id":"acct-42"}}`)
	login2 := []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"at-2","refresh_token":"rt-2","account_id":"acct-42"}}`)
	if a, b := OAuthIdentityFingerprint(OAuthKindCodex, login1), OAuthIdentityFingerprint(OAuthKindCodex, login2); a != b {
		t.Errorf("one ChatGPT account, two logins = %q / %q: account_id must win over the tokens", a, b)
	}
	other := []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"at-1","refresh_token":"rt-1","account_id":"acct-99"}}`)
	if OAuthIdentityFingerprint(OAuthKindCodex, login1) == OAuthIdentityFingerprint(OAuthKindCodex, other) {
		t.Error("two ChatGPT accounts share an identity")
	}
}

// Degradation, in order — and never an empty string, which would silently
// collapse the meter back onto the slot-shaped key for everyone.
func TestOAuthIdentityFingerprint_FallsBackAndNeverEmpty(t *testing.T) {
	cases := []struct {
		name    string
		kind    OAuthKind
		payload []byte
	}{
		{"token-only anthropic paste", OAuthKindClaudeCode, []byte(`{"claudeAiOauth":{"accessToken":"sk-ant-oat-only-1"}}`)},
		{"codex without an account id", OAuthKindCodex, []byte(`{"tokens":{"refresh_token":"rt-1"}}`)},
		{"unparseable payload", OAuthKindClaudeCode, []byte(`not json at all`)},
		{"empty payload", OAuthKindClaudeCode, nil},
		{"unknown kind", OAuthKind("future_cli"), []byte(`{"whatever":1}`)},
	}
	seen := map[string]string{}
	for _, tc := range cases {
		fp := OAuthIdentityFingerprint(tc.kind, tc.payload)
		if fp == "" {
			t.Errorf("%s: empty fingerprint", tc.name)
		}
		if prev, dup := seen[fp]; dup {
			t.Errorf("%s collides with %s (%q)", tc.name, prev, fp)
		}
		seen[fp] = tc.name
	}

	// The kind is part of the domain, so the same literal in two different
	// credentials cannot merge their meters.
	blob := []byte(`{"tokens":{"refresh_token":"same-literal"}}`)
	if OAuthIdentityFingerprint(OAuthKindCodex, blob) == OAuthIdentityFingerprint(OAuthKindClaudeCode, blob) {
		t.Error("kind is not part of the fingerprint domain")
	}
}
