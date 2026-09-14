package cloudpublisher

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

func TestNewAccountMeterChecksProviderBeforeFirstAdmission(t *testing.T) {
	f := newPoolFixture(t, credpool.Limits{MaxUSDPerDay: 5})
	withTenantForfait(t, f, "sk-ant-already-exhausted")
	owner := secrets.OrgOwnerKey(poolTeam)
	rec, err := f.pub.oauthForfait.Get(t.Context(), owner, secrets.OAuthKindClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	rec.Fingerprint = (secrets.OAuthAccount{ID: "c7f30bce-7e8b-4bb7-8199-afc172f3d580", OrganizationID: "0d05ce64-6b67-4368-909c-0d26f5a5870a"}).Fingerprint()
	if err := f.pub.oauthForfait.Upsert(t.Context(), rec); err != nil {
		t.Fatal(err)
	}
	f.pub.usageCaps = usagecap.NewMemStore()
	probes := 0
	f.pub.usageProbe = func(context.Context, []byte) ([]usagecap.Reading, error) {
		probes++
		return []usagecap.Reading{{Window: usagecap.WindowSevenDay, Status: usagecap.StatusRejected, Utilization: 1, ObservedAt: time.Now(), ResetsAt: time.Now().Add(time.Hour)}}, nil
	}
	if token := bundleToken(t, f, "account-first"); contains(token, "sk-ant-already-exhausted") || !contains(token, "sk-ant-donated") {
		t.Fatalf("fresh account meter admitted the exhausted subscription: %q", token)
	}
	// The first run holds the donor's capacity lease. Check the same account
	// independently of pool availability: its learned refusal must be reused.
	if until, _ := f.pub.forfaitWindowClosed(t.Context(), usagecap.TenantScope(poolTeam), owner, rec, []byte("unused because a fresh reading exists")); until.IsZero() {
		t.Fatal("second admission forgot the account's provider refusal")
	}
	if probes != 1 {
		t.Fatalf("provider probes=%d, want one; the second launch must reuse its reading", probes)
	}
}

func TestPlatformForfaitTriesNextRankThenRestoresWhenAllClosed(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	oauth := secrets.NewMemoryOAuthStore()
	seedOAuth(t, oauth, sealer, secrets.PlatformOwnerKey, "sk-ant-platform-primary")
	first, _ := oauth.Get(t.Context(), secrets.PlatformOwnerKey, secrets.OAuthKindClaudeCode)
	first.Fingerprint = "platform-primary"
	if err := oauth.Upsert(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.ID, second.Rank, second.Fingerprint = "", 1, "platform-fallback"
	second.SealedPayload, err = secrets.SealOAuthPayload(sealer, second.UserID, second.Kind, []byte(`{"claudeAiOauth":{"accessToken":"sk-ant-platform-fallback"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := oauth.Upsert(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	meter := usagecap.NewMemStore()
	closeAccount := func(fp string) {
		t.Helper()
		if err := meter.Record(t.Context(), usagecap.Key("claude_code", usagecap.ScopePlatform, fp), usagecap.Reading{Window: usagecap.WindowSevenDay, Status: usagecap.StatusRejected, Utilization: 1, ObservedAt: time.Now(), ResetsAt: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	closeAccount(first.Fingerprint)
	rs := secrets.NewMemoryRunSecretsStore()
	p := &Publisher{oauthForfait: oauth, runSecrets: rs, sealer: sealer, logger: testLogger(), usageCaps: meter}
	b := resolveBundle(t, p, rs, sealer, "platform-rank-1", "team1", "webhook:config")
	if b.OAuthFingerprints["claude_code"] != second.Fingerprint || !b.PlatformSourced["claude_code"] {
		t.Fatalf("platform did not try its healthy fallback: fingerprints=%v platform=%v", b.OAuthFingerprints, b.PlatformSourced)
	}
	closeAccount(second.Fingerprint)
	b = resolveBundle(t, p, rs, sealer, "platform-all-closed", "team1", "webhook:config")
	if b.OAuthFingerprints["claude_code"] != first.Fingerprint || !b.PlatformSourced["claude_code"] {
		t.Fatalf("all-closed restore lost the primary/provenance: fingerprints=%v platform=%v", b.OAuthFingerprints, b.PlatformSourced)
	}
}
