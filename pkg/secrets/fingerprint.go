package secrets

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// fingerprintHex returns the first 8 bytes of SHA-256 hex-encoded.
// 64 bits is enough to distinguish credentials in audit logs while
// keeping the value short. It is an IDENTITY, and one that now travels
// further than a log line: the usage-cap meter composes it into its
// store key (usagecap.Key), so a collision would merge two credentials'
// meters — a wrong reading, never a disclosure, since the fingerprint is
// no more a handle on the secret than it ever was. The exported form is
// FingerprintSHA256 (sealer.go).
func fingerprintHex(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:8])
}

// OAuthIdentityFingerprint returns the audit identity of the SUBSCRIPTION a
// forfait payload authenticates — the value OAuthRecord.Fingerprint carries
// and the usage-cap meter keys on.
//
// It reads the most stable identifier the blob actually carries, in
// descending order of how well each survives the same account being
// connected twice:
//
//  1. codex `tokens.account_id` — the ChatGPT account itself (what the
//     backend receives in the ChatGPT-Account-ID header). Two logins to one
//     account agree on it.
//  2. the refresh token — one OAuth grant. Anthropic's credentials.json
//     carries no account field at all, so this is the best available there,
//     and it is what makes the shared-forfait pattern meter correctly: one
//     login whose credentials.json is COPIED (pasted by several teammates,
//     or connected both personally and at org level) yields ONE identity.
//     Under a whole-blob hash those became N meters and N times the
//     operator's ceiling.
//  3. the access token — for a token-only paste, which has no refresh token
//     and therefore never rotates: still better than the blob, which also
//     moves with reserialisation and the expiry stamp.
//  4. the whole payload — last resort, for a shape we cannot read.
//
// Each input is domain-separated by kind and field so two credentials
// cannot collide by sharing a literal across different roles. The fallbacks
// identify BYTES, not an account: a separate login to the same account
// mints a distinct grant and still opens its own meter. That residue fails
// OPEN (a fresh meter starts empty) with the mid-run guard behind it —
// the same trade the rotation case makes deliberately.
func OAuthIdentityFingerprint(kind OAuthKind, payload []byte) string {
	id := func(field, value string) string {
		return fingerprintHex(string(kind) + "|" + field + "|" + value)
	}
	switch kind {
	case OAuthKindCodex:
		if v, err := ParseCodexView(payload); err == nil {
			if acct := strings.TrimSpace(v.Tokens.AccountID); acct != "" {
				return id("account_id", acct)
			}
			if rt := strings.TrimSpace(v.Tokens.RefreshToken); rt != "" {
				return id("refresh_token", rt)
			}
			if at := strings.TrimSpace(v.Tokens.AccessToken); at != "" {
				return id("access_token", at)
			}
		}
	case OAuthKindClaudeCode:
		if v, err := ParseAnthropicView(payload); err == nil {
			if rt := strings.TrimSpace(v.ClaudeAIOauth.RefreshToken); rt != "" {
				return id("refresh_token", rt)
			}
			if at := strings.TrimSpace(v.ClaudeAIOauth.AccessToken); at != "" {
				return id("access_token", at)
			}
		}
	}
	return id("payload", string(payload))
}
