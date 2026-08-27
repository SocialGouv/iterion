package cloudpublisher

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// Every tier that fills an OAuth slot must also stamp WHICH credential
// filled it.
//
// The usage-cap meter key composes that identity (usagecap.Key). A slot
// left without one silently falls back to the historical slot-shaped key
// `claude_code|<scope>` — where a fresh credential posted over an
// exhausted one inherits the replaced account's seven-day reading and is
// parked by it until that account's own reset instant, days out. That is
// the lived failure this whole change exists to close, so closing it on
// one tier and not the others just moves it.
//
// This test is the ratchet: it walks the bundle the runner would actually
// receive, per tier, and fails on any OAuth slot with no identity.

// seedOAuthStamped seals a claude_code forfait under ownerKey and stamps
// the record exactly as the connect handler (sealOAuthRecord) does.
// Returns the identity the tier is expected to carry into the bundle.
func seedOAuthStamped(t *testing.T, st secrets.OAuthStore, sealer secrets.Sealer, ownerKey, refreshToken string) string {
	t.Helper()
	blob := []byte(`{"claudeAiOauth":{"accessToken":"sk-ant-access-000000000000","refreshToken":"` + refreshToken + `"}}`)
	sealed, err := secrets.SealOAuthPayload(sealer, ownerKey, secrets.OAuthKindClaudeCode, blob)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	fp := secrets.OAuthIdentityFingerprint(secrets.OAuthKindClaudeCode, blob)
	if err := st.Upsert(context.Background(), secrets.OAuthRecord{
		UserID:        ownerKey,
		Kind:          secrets.OAuthKindClaudeCode,
		SealedPayload: sealed,
		Fingerprint:   fp,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	return fp
}

func assertOAuthSlotsIdentified(t *testing.T, b secrets.RunBundle, tier string) {
	t.Helper()
	if len(b.OAuthCredentials) == 0 {
		t.Fatalf("%s: no oauth credential resolved — the fixture does not exercise this tier, so it proves nothing", tier)
	}
	for kind := range b.OAuthCredentials {
		if b.OAuthFingerprints[kind] == "" {
			t.Errorf("%s: oauth slot %q carries no meter identity — it falls back to the slot-shaped key, "+
				"where a rotated credential inherits the readings of the account it replaced", tier, kind)
		}
	}
}

func TestOAuthMeterIdentity_EveryTierStampsTheSlotItFills(t *testing.T) {
	newPub := func(t *testing.T) (*Publisher, *secrets.MemoryRunSecretsStore, secrets.Sealer, secrets.OAuthStore) {
		t.Helper()
		sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
		if err != nil {
			t.Fatalf("sealer: %v", err)
		}
		oauth := secrets.NewMemoryOAuthStore()
		rs := secrets.NewMemoryRunSecretsStore()
		return &Publisher{
			oauthForfait: oauth,
			runSecrets:   rs,
			sealer:       sealer,
			logger:       testLogger(),
		}, rs, sealer, oauth
	}

	// Tier 3, the run owner's personal forfait.
	t.Run("user", func(t *testing.T) {
		p, rs, sealer, oauth := newPub(t)
		want := seedOAuthStamped(t, oauth, sealer, "alice", "rt-alice")
		b := resolveBundle(t, p, rs, sealer, "run-1", "team1", "alice")
		assertOAuthSlotsIdentified(t, b, "user tier")
		if got := b.OAuthFingerprints["claude_code"]; got != want {
			t.Errorf("identity = %q, want the record's own %q", got, want)
		}
	})

	// Tier 3's org fallback — the credential that powers automated runs,
	// whose owner is a synthetic identity with no personal forfait.
	t.Run("org fallback", func(t *testing.T) {
		p, rs, sealer, oauth := newPub(t)
		want := seedOAuthStamped(t, oauth, sealer, secrets.OrgOwnerKey("team1"), "rt-org")
		b := resolveBundle(t, p, rs, sealer, "run-1", "team1", "webhook:cfg-1")
		assertOAuthSlotsIdentified(t, b, "org tier")
		if got := b.OAuthFingerprints["claude_code"]; got != want {
			t.Errorf("identity = %q, want the record's own %q", got, want)
		}
	})

	// Tier 5, the deployment's own forfait — the one a super-admin rotates
	// with a single `iterion remote admin llm` call, which is exactly when
	// an inherited reading would park the WHOLE deployment.
	t.Run("platform", func(t *testing.T) {
		p, rs, sealer, oauth := newPub(t)
		want := seedOAuthStamped(t, oauth, sealer, secrets.PlatformOwnerKey, "rt-platform")
		b := resolveBundle(t, p, rs, sealer, "run-1", "team1", "webhook:cfg-1")
		assertOAuthSlotsIdentified(t, b, "platform tier")
		if !b.PlatformSourced["claude_code"] {
			t.Fatal("the fixture did not go through the platform tier")
		}
		if got := b.OAuthFingerprints["claude_code"]; got != want {
			t.Errorf("identity = %q, want the record's own %q", got, want)
		}

		// Rotate it: the same slot, the same shared platform scope, a
		// different subscription — and therefore a different meter.
		rotated := seedOAuthStamped(t, oauth, sealer, secrets.PlatformOwnerKey, "rt-platform-NEW")
		after := resolveBundle(t, p, rs, sealer, "run-2", "team1", "webhook:cfg-1")
		if got := after.OAuthFingerprints["claude_code"]; got != rotated || got == want {
			t.Errorf("after rotation identity = %q, want the new record's %q (was %q) — "+
				"a rotated platform forfait must open a fresh meter", got, rotated, want)
		}
	})

	// Tier 4, a lent subscription. Two donors serving one tenant at
	// different times must not share a meter: donor A's measured
	// exhaustion has no bearing on the run donor B is funding.
	t.Run("pool grant", func(t *testing.T) {
		f := newPoolFixture(t, credpool.Limits{})
		b, res := f.resolve(t, "run-1", nil)
		if res.grant == nil {
			t.Fatal("the fixture did not go through the pool tier")
		}
		assertOAuthSlotsIdentified(t, b, "pool tier")
		if got, other := b.OAuthFingerprints["claude_code"], secrets.FingerprintSHA256("credpool-pledge:other-pledge"); got == other {
			t.Error("the grant's identity does not distinguish pledges")
		}
	})
}
