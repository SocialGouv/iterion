package cloudpublisher

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// seedRankedKey seeds a key with an EXPLICIT CreatedAt, because the priority
// walk orders equal-rank keys by it (secrets.OrderedAPIKeyCandidates) and the
// memory store lists rows in map order: a witness about "the key the walk
// prefers" is only a witness when that preference is a fact.
func seedRankedKey(t *testing.T, p *Publisher, teamID string, provider secrets.Provider, plaintext, fp string, createdAt time.Time) {
	t.Helper()
	id := secrets.NewApiKeyID()
	sealed, err := secrets.SealAPIKey(p.sealer, id, []byte(plaintext))
	if err != nil {
		t.Fatalf("seal key: %v", err)
	}
	if err := p.apiKeys.Create(context.Background(), secrets.ApiKey{
		ID: id, ScopeTeamID: teamID, Provider: provider, Fingerprint: fp,
		Name: plaintext, SealedSecret: sealed, CreatedAt: createdAt,
	}); err != nil {
		t.Fatalf("create key: %v", err)
	}
}

// #2368 — the walk remembered ONE candidate per slot, so a candidate a
// reservation holds masked the dead sibling behind it: the restore refused
// the reserved one (rightly) and the sibling had never been written down, so
// the run was published with an empty wire, to die on a no-credential auth
// error nothing retries instead of parking on a durable usage-window retry.
func TestCredWalk_AReservedCandidateNoLongerMasksItsDeadSibling(t *testing.T) {
	// (A2) Two keys of one provider in the SAME tenant tier: the one the
	// unfiltered walk prefers is held by the reserve, its sibling is dead.
	t.Run("a reserved tenant key does not mask a dead tenant sibling", func(t *testing.T) {
		p := credWalkPublisher(t, true)
		older := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		// Older ⇒ preferred: this is the candidate the fallback lookup used
		// to record, and the only one it recorded.
		seedRankedKey(t, p, "team1", secrets.ProviderAnthropic, "sk-reserved", "fp-reserved", older)
		seedRankedKey(t, p, "team1", secrets.ProviderAnthropic, "sk-dead", "fp-dead", older.Add(time.Hour))
		recordHeld(t, p, usagecap.TenantScope("team1"), "fp-reserved")
		recordRefusal(t, p.usageCaps, usagecap.TenantScope("team1"), "fp-dead")

		switch got := walkAndPreview(t, p, "run-a2", "feature-dev").APIKeys[secrets.ProviderAnthropic]; got {
		case "sk-dead": // right: the run parks on the dead key's own retry
		case "sk-reserved":
			t.Fatal("the RESERVED key was handed to an unreserved bot — the reserve leaked through the restore")
		default:
			t.Fatalf("anthropic wire = %q, want sk-dead: the reserved key the walk preferred masked its dead sibling, which was never remembered, and the run goes out with an empty wire", got)
		}
	})

	// (A3) The sibling lives one tier DOWN: the platform key was dropped at
	// record time because the provider had already been "seen" by the tenant
	// tier — whose entry the restore then refused.
	t.Run("a reserved tenant key does not mask a dead platform key", func(t *testing.T) {
		p := credWalkPublisher(t, true)
		at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		seedRankedKey(t, p, "team1", secrets.ProviderAnthropic, "sk-tenant-reserved", "fp-tenant", at)
		seedRankedKey(t, p, secrets.PlatformTenantID, secrets.ProviderAnthropic, "sk-platform-dead", "fp-platform", at)
		recordHeld(t, p, usagecap.TenantScope("team1"), "fp-tenant")
		recordRefusal(t, p.usageCaps, usagecap.ScopePlatform, "fp-platform")

		got := walkAndPreview(t, p, "run-a3", "feature-dev").APIKeys[secrets.ProviderAnthropic]
		if got == "sk-tenant-reserved" {
			t.Fatal("the RESERVED tenant key was handed to an unreserved bot — the reserve leaked through the restore")
		}
		if got != "sk-platform-dead" {
			t.Fatalf("anthropic wire = %q, want sk-platform-dead: the tenant candidate the reserve holds masked the platform sibling at RECORD time, so the restore had nothing left to hand back", got)
		}
	})

	// (A4, same tier) Two subscriptions of ONE kind inside the tenant tier —
	// a member's own and the team's, the fill order the walk applies. The
	// member's is held by the reserve, the team's window is closed: one
	// remembered candidate per kind meant the team's was never written down.
	t.Run("a reserved personal forfait does not mask the team's dead one", func(t *testing.T) {
		p := credWalkPublisher(t, true)
		oauth := secrets.NewMemoryOAuthStore()
		seedOAuth(t, oauth, p.sealer, "owner1", "sk-ant-personal-reserved")
		seedOAuth(t, oauth, p.sealer, secrets.OrgOwnerKey("team1"), "sk-ant-team-dead")
		p.oauthForfait = oauth
		recordHeld(t, p, usagecap.TenantScope("team1"), seededFP("owner1"))
		recordRefusal(t, p.usageCaps, usagecap.TenantScope("team1"), seededFP(secrets.OrgOwnerKey("team1")))

		got := string(walkAndPreview(t, p, "run-a4-same-tier", "feature-dev").OAuthCredentials["claude_code"])
		if contains(got, "sk-ant-personal-reserved") {
			t.Fatal("the RESERVED personal forfait was handed to an unreserved bot — the reserve leaked through the restore")
		}
		if !contains(got, "sk-ant-team-dead") {
			t.Fatalf("claude_code blob = %q, want the team's dead forfait: the member's forfait the reserve holds was the ONE candidate remembered for the kind, so the restore had nothing left to hand back", got)
		}
	})

	// (A4) The forfait half of the same class: one remembered candidate per
	// KIND, and the shared tier declining to record its own once the kind was
	// seen. A reserved tenant subscription therefore masked the platform
	// subscription behind it.
	t.Run("a reserved tenant forfait does not mask a dead platform forfait", func(t *testing.T) {
		p := credWalkPublisher(t, true)
		oauth := secrets.NewMemoryOAuthStore()
		seedOAuth(t, oauth, p.sealer, secrets.OrgOwnerKey("team1"), "sk-ant-tenant-reserved")
		seedOAuth(t, oauth, p.sealer, secrets.PlatformOwnerKey, "sk-ant-platform-dead")
		p.oauthForfait = oauth
		recordHeld(t, p, usagecap.TenantScope("team1"), seededFP(secrets.OrgOwnerKey("team1")))
		recordRefusal(t, p.usageCaps, usagecap.ScopePlatform, seededFP(secrets.PlatformOwnerKey))

		got := string(walkAndPreview(t, p, "run-a4", "feature-dev").OAuthCredentials["claude_code"])
		if contains(got, "sk-ant-tenant-reserved") {
			t.Fatal("the RESERVED tenant forfait was handed to an unreserved bot — the reserve leaked through the restore")
		}
		if !contains(got, "sk-ant-platform-dead") {
			t.Fatalf("claude_code blob = %q, want the platform forfait: the tenant candidate the reserve holds masked it at RECORD time ('one skip per kind'), so the restore had nothing left to hand back", got)
		}
	})
}

// The memory itself, built in BOTH fill orders: the restore asks per TIER, so
// which tier's candidate comes back is a property of the tier and not of the
// order the candidates arrived in. A witness that can only redden in one
// order is flaky, not a witness.
func TestSkippedCreds_TheRestoreOrderIsTheTierNotTheArrival(t *testing.T) {
	tenant := skippedAPIKey{plaintext: "sk-tenant", fingerprint: "fp-tenant"}
	org := skippedAPIKey{plaintext: "sk-org", fingerprint: "fp-org", org: true}
	platform := skippedAPIKey{plaintext: "sk-platform", fingerprint: "fp-platform", platform: true}

	for _, order := range [][]skippedAPIKey{
		{tenant, org, platform},
		{platform, org, tenant},
	} {
		m := skippedAPIKeys{}
		for _, sk := range order {
			m.remember(secrets.ProviderAnthropic, sk)
		}
		// Recording the same fingerprint twice must not offer it twice: two
		// tiers that can both see one record own one candidate.
		for _, sk := range order {
			m.remember(secrets.ProviderAnthropic, sk)
		}
		for tier, want := range map[restoreTier]string{
			restoreTierTenant:   "sk-tenant",
			restoreTierOrg:      "sk-org",
			restoreTierPlatform: "sk-platform",
		} {
			got := m.candidatesAt(secrets.ProviderAnthropic, tier)
			if len(got) != 1 {
				t.Fatalf("arrival order %v, tier %v: %d candidates, want exactly 1 — a duplicate fingerprint was remembered twice, or a tier's candidate was lost", order, tier, len(got))
			}
			if got[0].plaintext != want {
				t.Errorf("arrival order %v, tier %v: candidate = %q, want %q — the restore is reading the arrival order, not the tier", order, tier, got[0].plaintext, want)
			}
		}
	}

	// The forfait twin, including the kind order the restore walks: ranging
	// the map would make "the first kind wins" a property of Go's map seed.
	f := skippedForfaits{}
	f.remember("codex", skippedForfait{fp: "fp-codex-platform", platform: true})
	f.remember("claude_code", skippedForfait{fp: "fp-cc-tenant"})
	f.remember("claude_code", skippedForfait{fp: "fp-cc-tenant"})
	if got := f.kinds(); len(got) != 2 || got[0] != "claude_code" || got[1] != "codex" {
		t.Errorf("kinds() = %v, want [claude_code codex] — the restore's order must be a fact, not a map's", got)
	}
	if got := f.candidatesAt("claude_code", restoreTierTenant); len(got) != 1 || got[0].fp != "fp-cc-tenant" {
		t.Errorf("claude_code tenant candidates = %v, want exactly fp-cc-tenant once", got)
	}
	if got := f.candidatesAt("claude_code", restoreTierPlatform); len(got) != 0 {
		t.Errorf("claude_code platform candidates = %v, want none: the tenant record is not the platform's", got)
	}
}
