package cloudpublisher

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// A forfait slot names the store record it was read from, so the runner can
// FOLLOW the record the refresh worker rotates instead of exchanging the
// refresh token itself — an exchange that revokes the token every other
// holder of the record still uses. Every tier fills its slot through one
// helper; these tests pin that each of them hands the id over.

// rotatedClaude is a server whose refresh worker rotates claude_code
// records — the default deployment.
var rotatedClaude = map[string]bool{string(secrets.OAuthKindClaudeCode): true}

func TestResolveOAuth_UserAndTeamSlotsNameTheirRecord(t *testing.T) {
	sealer, _ := secrets.NewAESGCMSealer(make([]byte, 32))
	oauth := secrets.NewMemoryOAuthStore()
	seedOAuth(t, oauth, sealer, "alice", "sk-ant-personal")
	seedOAuth(t, oauth, sealer, secrets.OrgOwnerKey("team1"), "sk-ant-team")
	rs := secrets.NewMemoryRunSecretsStore()
	p := &Publisher{oauthForfait: oauth, runSecrets: rs, sealer: sealer, logger: testLogger(), rotatedOAuthKinds: rotatedClaude}

	for owner, want := range map[string]string{
		"alice":         secrets.OAuthRecordID("alice", secrets.OAuthKindClaudeCode, 0),
		"webhook:cfg-1": secrets.OAuthRecordID(secrets.OrgOwnerKey("team1"), secrets.OAuthKindClaudeCode, 0),
	} {
		b := resolveBundle(t, p, rs, sealer, "run-"+owner, "team1", owner)
		if got := b.OAuthRecordRefs["claude_code"]; got != want {
			t.Errorf("owner %s: OAuthRecordRefs[claude_code] = %q, want %q", owner, got, want)
		}
	}
}

func TestPlatformTier_oauthSlotNamesItsRecord(t *testing.T) {
	sealer, _ := secrets.NewAESGCMSealer(make([]byte, 32))
	oauth := secrets.NewMemoryOAuthStore()
	seedOAuth(t, oauth, sealer, secrets.PlatformOwnerKey, "sk-ant-platform")
	rs := secrets.NewMemoryRunSecretsStore()
	p := &Publisher{oauthForfait: oauth, runSecrets: rs, sealer: sealer, logger: testLogger(), rotatedOAuthKinds: rotatedClaude}

	b := resolveBundle(t, p, rs, sealer, "run-platform", "team1", "webhook:cfg-1")
	want := secrets.OAuthRecordID(secrets.PlatformOwnerKey, secrets.OAuthKindClaudeCode, 0)
	if got := b.OAuthRecordRefs["claude_code"]; got != want {
		t.Errorf("OAuthRecordRefs[claude_code] = %q, want the platform record %q", got, want)
	}
}

func TestOrgTier_oauthSlotNamesItsRecord(t *testing.T) {
	const orgID = "org-1"
	sealer, _ := secrets.NewAESGCMSealer(make([]byte, 32))
	oauth := secrets.NewMemoryOAuthStore()
	seedOAuth(t, oauth, sealer, secrets.OrgTierOwnerKey(orgID), "sk-ant-org")
	p := &Publisher{
		oauthForfait:      oauth,
		runSecrets:        secrets.NewMemoryRunSecretsStore(),
		sealer:            sealer,
		logger:            testLogger(),
		rotatedOAuthKinds: rotatedClaude,
		identity: &fakeTeamResolver{
			orgs:    map[string]string{"team-in": orgID},
			orgDocs: map[string]identity.Org{orgID: {ID: orgID, CredentialAudience: identity.CredentialAudience{Teams: []string{"team-in"}}}},
		},
	}
	b := resolveBundleForOrg(t, p, "run-org", orgID, "team-in", "webhook:cfg")
	if !b.OrgSourced["claude_code"] {
		t.Fatalf("the org forfait did not fill the slot: %+v", b.OrgSourced)
	}
	want := secrets.OAuthRecordID(secrets.OrgTierOwnerKey(orgID), secrets.OAuthKindClaudeCode, 0)
	if got := b.OAuthRecordRefs["claude_code"]; got != want {
		t.Errorf("OAuthRecordRefs[claude_code] = %q, want the org record %q", got, want)
	}
}

// A lent subscription is the donor's record: a borrower that exchanged its
// refresh token would revoke the token the donor's record, and every run
// spending it, still holds. The slot names the donor's record so the
// borrower's runner follows it instead.
func TestPoolTier_grantNamesTheDonorsRecord(t *testing.T) {
	f := newPoolFixture(t, credpool.Limits{MaxUSDPerDay: 5})
	bundle, creds := f.resolve(t, "run-1", nil)
	if creds.grant == nil {
		t.Fatal("no grant — the tier is not wired")
	}
	want := secrets.OAuthRecordID("donor", secrets.OAuthKindClaudeCode, 0)
	if got := bundle.OAuthRecordRefs["claude_code"]; got != want {
		t.Errorf("OAuthRecordRefs[claude_code] = %q for a pool grant, want the donor's record %q", got, want)
	}
}

// A slot of a kind this server's refresh worker does not rotate names no
// record: a run following a record nobody rotates would never renew its
// token. The runner then refreshes its own copy.
func TestResolveOAuth_aKindNoWorkerRotatesNamesNoRecord(t *testing.T) {
	sealer, _ := secrets.NewAESGCMSealer(make([]byte, 32))
	oauth := secrets.NewMemoryOAuthStore()
	seedOAuth(t, oauth, sealer, "alice", "sk-ant-personal")
	rs := secrets.NewMemoryRunSecretsStore()
	p := &Publisher{oauthForfait: oauth, runSecrets: rs, sealer: sealer, logger: testLogger()}
	b := resolveBundle(t, p, rs, sealer, "run-unrotated", "team1", "alice")
	if len(b.OAuthCredentials["claude_code"]) == 0 {
		t.Fatal("the forfait did not fill the slot — this proves nothing")
	}
	if got, ok := b.OAuthRecordRefs["claude_code"]; ok {
		t.Fatalf("OAuthRecordRefs[claude_code] = %q on a server that does not rotate claude_code records, want none", got)
	}
}
