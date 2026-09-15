package cloudpublisher

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// The sibling of the test below, for the answer that records NOTHING: the
// provider returns a successful empty slice whenever the usage body omits
// the window keys, sets them null, or omits utilization. The initial-account
// probe fires whenever a verified-account key holds zero readings, and only
// a recorded reading ends that — so this answer left the predicate exactly
// as it found it and the probe fired again at every launch, a 5s round trip
// per ranked candidate on the synchronous publish path. (Revi Ra0ac0e.)
func TestNewAccountMeterConvergesWhenTheProviderReportsNoWindow(t *testing.T) {
	f := newPoolFixture(t, credpool.Limits{MaxUSDPerDay: 5})
	withTenantForfait(t, f, "sk-ant-quiet-provider")
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
	f.pub.trust = usagecap.Trust{MaxAge: time.Hour, Window: 2 * time.Hour}
	probes := 0
	f.pub.usageProbe = func(context.Context, []byte) ([]usagecap.Reading, error) {
		probes++
		return nil, nil
	}

	for pass := range 3 {
		until, _ := f.pub.forfaitWindowClosed(t.Context(), usagecap.TenantScope(poolTeam), owner, rec, []byte("payload"))
		if !until.IsZero() {
			t.Fatalf("pass %d: an empty answer says nothing is closed", pass)
		}
	}
	if probes != 1 {
		t.Fatalf("provider probes=%d over three launches, want one — the initial-account probe never converges on an empty answer", probes)
	}

	// The memo is an observation, not a verdict: it expires on the same
	// trust window a recorded reading does, and then the account is asked
	// again rather than assumed answered forever.
	key := usagecap.Key(delegate.BackendClaudeCode, usagecap.TenantScope(poolTeam), rec.Fingerprint)
	f.pub.rememberProbeWithoutReading(key, time.Now().Add(-2*time.Hour))
	if _, _ = f.pub.forfaitWindowClosed(t.Context(), usagecap.TenantScope(poolTeam), owner, rec, []byte("payload")); probes != 2 {
		t.Fatalf("provider probes=%d, want the account re-asked once the memo aged out", probes)
	}
}

// The twin of the test above, for the answer that never ARRIVES — and the one
// that costs more, because an outage is exactly when the probe re-fires
// hardest and each attempt burns a full timeout on the synchronous publish
// path. The memo was written for the successful-but-empty branch only, so the
// likelier branch kept spinning: one test covering its own branch and not its
// sibling. (Revi R780903 / R7094da.)
func TestNewAccountMeterConvergesWhenTheProviderCannotBeReached(t *testing.T) {
	f := newPoolFixture(t, credpool.Limits{MaxUSDPerDay: 5})
	withTenantForfait(t, f, "sk-ant-unreachable-provider")
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
	f.pub.trust = usagecap.Trust{MaxAge: time.Hour, Window: 2 * time.Hour}
	probes := 0
	f.pub.usageProbe = func(context.Context, []byte) ([]usagecap.Reading, error) {
		probes++
		return nil, errors.New("the provider is unreachable")
	}

	for pass := range 3 {
		until, _ := f.pub.forfaitWindowClosed(t.Context(), usagecap.TenantScope(poolTeam), owner, rec, []byte("payload"))
		// The documented posture on an unavailable provider: fail OPEN. The
		// fix must stop the re-probing without turning an outage into a wall.
		if !until.IsZero() {
			t.Fatalf("pass %d: an unreachable provider must not close the window", pass)
		}
	}
	if probes != 1 {
		t.Fatalf("provider probes=%d over three launches, want one — a failed probe re-fires at every launch, %v per ranked candidate, for as long as the outage lasts", probes, usageProbeTimeout)
	}

	// Same as its twin: an observation, not a verdict. Once the memo ages out
	// the account is asked again rather than assumed unreachable forever.
	key := usagecap.Key(delegate.BackendClaudeCode, usagecap.TenantScope(poolTeam), rec.Fingerprint)
	f.pub.rememberProbeWithoutReading(key, time.Now().Add(-2*time.Hour))
	if _, _ = f.pub.forfaitWindowClosed(t.Context(), usagecap.TenantScope(poolTeam), owner, rec, []byte("payload")); probes != 2 {
		t.Fatalf("provider probes=%d, want the account re-asked once the memo aged out", probes)
	}
}

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
