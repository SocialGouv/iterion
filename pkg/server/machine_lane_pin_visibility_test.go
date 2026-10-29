package server

import (
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// #2644, the half no carrier decision touches: a credential pin a MACHINE
// lane can never spend.
//
// A webhook fires under webhookActor(cfg.ID) and the trigger spine under
// triggerSpineActor — synthetic identities no BYOK key is ever scoped to. So
// secrets.Resolve's visibility walk never lists a USER-scoped key as a
// candidate for them, pinned or not ("Other users' user-scoped keys never
// apply", keyRank == 99), and the launch seals the team's default instead.
// Nothing downstream can report it: onWithheld answers for the bot and
// repository audiences, which an invisible key never reaches. The reachable
// path is the studio's own pin dialog — GET /api/teams/{id}/api-keys returns
// "all team-wide keys + their own user-scoped keys" to an admin, and the
// dialog groups by provider only — so the pin an admin picks for their own
// key was accepted at write and dropped at launch, silently.

func seedVendorKey(t *testing.T, keys secrets.ApiKeyStore, tenant, user, name string, isDefault bool) secrets.ApiKey {
	t.Helper()
	k := secrets.ApiKey{
		ID: secrets.NewApiKeyID(), TenantID: tenant, ScopeTeamID: tenant, ScopeUserID: user,
		Provider: secrets.ProviderAnthropic, Name: name, IsDefault: isDefault,
		Fingerprint: "fp-" + name, CreatedAt: time.Now().UTC(),
	}
	if err := keys.Create(store.WithTenant(t.Context(), tenant), k); err != nil {
		t.Fatal(err)
	}
	return k
}

// The write refusal. A pin the lane's actor cannot spend is refused where it
// is written, exactly like the gateway-provider pin next to it and like the
// secret twin below — the operator's explicit choice is never replaced in
// silence.
func TestValidateKeyOverrides_RefusesAPinThisWebhookCanNeverSpend(t *testing.T) {
	keys := secrets.NewMemoryApiKeyStore()
	personal := seedVendorKey(t, keys, "t1", "alice", "alice-personal", false)
	org := seedVendorKey(t, keys, "t1", "", "org-default", true)
	s := &Server{apiKeys: keys}
	ctx := store.WithTenant(t.Context(), "t1")
	actor := webhookActor("wh1")

	err := s.validateKeyOverrides(ctx, "t1", actor, map[string]string{"anthropic": personal.ID})
	if err == nil || !strings.Contains(err.Error(), "not spendable by this webhook") {
		t.Fatalf("a pin on a user-scoped key = %v, want a refusal naming the unspendable pin", err)
	}
	if !strings.Contains(err.Error(), actor) {
		t.Errorf("the refusal %q does not name the identity that cannot spend the key (%q) — an operator cannot act on it", err, actor)
	}

	// The guard is about VISIBILITY, not about pinning: the same team's
	// org-scoped key still pins.
	if err := s.validateKeyOverrides(ctx, "t1", actor, map[string]string{"anthropic": org.ID}); err != nil {
		t.Fatalf("an org-scoped pin refused: %v", err)
	}
	// And it fails CLOSED on an actor it does not know: an empty identity
	// matches no key's scope, so an unknown lane is judged like a machine
	// one instead of skipping the question.
	if err := s.validateKeyOverrides(ctx, "t1", "", map[string]string{"anthropic": personal.ID}); err == nil {
		t.Error("an actor-less call accepted a user-scoped pin — the arm is skippable")
	}
	if err := s.validateKeyOverrides(ctx, "t1", "", map[string]string{"anthropic": org.ID}); err != nil {
		t.Errorf("an actor-less call refused an ORG-scoped pin: %v — failing closed must not mean refusing everything", err)
	}
}

// The two halves of ONE request must answer the same way: the asymmetry is
// what the defect was. Two separate scenarios — one per validator — because
// two guards that cover each other each look tested and neither is.
func TestAWebhooksKeyAndSecretPinsRefuseTheSamePersonalShape(t *testing.T) {
	t.Run("key half", func(t *testing.T) {
		keys := secrets.NewMemoryApiKeyStore()
		personal := seedVendorKey(t, keys, "t1", "alice", "alice-personal", false)
		s := &Server{apiKeys: keys}
		err := s.validateKeyOverrides(store.WithTenant(t.Context(), "t1"), "t1", webhookActor("wh1"), map[string]string{"anthropic": personal.ID})
		if err == nil {
			t.Fatal("key_overrides accepted a user-scoped reference its secret twin refuses")
		}
	})
	t.Run("secret half", func(t *testing.T) {
		gen := secrets.NewMemoryGenericSecretStore()
		ctx := store.WithTenant(t.Context(), "t1")
		sec := secrets.GenericSecret{
			ID: "sec-personal", TenantID: "t1", ScopeTeamID: "t1", ScopeUserID: "alice",
			Name: "forge_token", CreatedAt: time.Now().UTC(),
		}
		if err := gen.Create(ctx, sec); err != nil {
			t.Fatal(err)
		}
		s := &Server{genericSecrets: gen}
		err := s.validateSecretOverrides(ctx, "t1", map[string]string{"forge_token": sec.ID})
		if err == nil {
			t.Fatal("secret_overrides accepted a user-scoped reference")
		}
	})
}

// The EFFECT, read off the resolution rather than off a log line: under a
// machine actor the pin yields the TEAM key's fingerprint, and no refusal is
// reported. This is the fact that makes the write the only possible layer —
// and it is deliberately coupled to the guard above: whoever lifts the
// visibility restriction for pinned keys (giving a webhook authority over a
// member's personal key — a product decision, ARB-90 option B) reddens here
// and is told to lift the write refusal with it.
//
// What it gates, said exactly: the GUARANTEE, not either site. Two
// independent layers hold it — the store's scope filter (ListByTeam drops a
// foreign scope_user) and keyRank's "never applies" rank — and each one
// alone keeps this test green with the other removed (measured: both
// single-layer mutants survive). Removing BOTH reddens it on the
// fingerprint. That is the intended altitude for a promise about what a run
// may spend; a per-layer witness belongs with each layer, and neither has
// one today.
func TestAPersonalPinIsInertUnderAMachineActor(t *testing.T) {
	keys := secrets.NewMemoryApiKeyStore()
	personal := seedVendorKey(t, keys, "t1", "alice", "alice-personal", false)
	org := seedVendorKey(t, keys, "t1", "", "org-default", true)
	ctx := store.WithTenant(t.Context(), "t1")
	pin := map[secrets.Provider]string{secrets.ProviderAnthropic: personal.ID}

	for _, actor := range []string{webhookActor("wh1"), triggerSpineActor} {
		withheld := 0
		got, err := secrets.Resolve(ctx, keys, "t1", actor, "probe",
			[]secrets.Provider{secrets.ProviderAnthropic}, pin, nil, nil,
			func(secrets.ApiKey, secrets.AudienceRefusal) { withheld++ })
		if err != nil {
			t.Fatalf("Resolve under %q: %v", actor, err)
		}
		res, ok := got[secrets.ProviderAnthropic]
		if !ok {
			t.Fatalf("under %q the walk resolved no anthropic key at all", actor)
		}
		if res.Fingerprint != org.Fingerprint {
			t.Errorf("under %q the pin resolved to fingerprint %q, want the team default's %q", actor, res.Fingerprint, org.Fingerprint)
		}
		if res.Fingerprint == personal.Fingerprint {
			t.Errorf("under %q a user-scoped pin was honoured — the write guard must be lifted with it", actor)
		}
		if withheld != 0 {
			t.Errorf("under %q the walk reported %d withheld key(s); an invisible pin reaches no refusal channel, which is why the write must refuse it", actor, withheld)
		}
	}

	// Control: the mechanism works, the ACTOR is the variable. Under the
	// key's own owner the identical pin wins — so the refusal above is not
	// a broken pin path being papered over.
	got, err := secrets.Resolve(ctx, keys, "t1", "alice", "probe",
		[]secrets.Provider{secrets.ProviderAnthropic}, pin, nil, nil, nil)
	if err != nil {
		t.Fatalf("Resolve under the key's owner: %v", err)
	}
	if got[secrets.ProviderAnthropic].Fingerprint != personal.Fingerprint {
		t.Fatalf("under alice the pin resolved to %q, want her own key %q", got[secrets.ProviderAnthropic].Fingerprint, personal.Fingerprint)
	}
}

// The actor helper produces the LITERAL three readers outside this function
// parse to classify a machine-owned run: the run list's source kind
// (pkg/runview/service_runs.go:deriveSourceKind), the audit actor kind
// (auditWrite) and store.RunSourceKindWebhook's own documentation. Renaming
// the four call sites onto one helper made that literal a single point;
// pinning it is what keeps the point from moving. Declared limit: this is a
// FORM gate on the PRODUCER — the reader half is pinned separately against
// the same literal (pkg/runview/source_kind_test.go), and no test spans the
// two, which is why dropping the prefix reddened nothing before this one.
func TestTheWebhookActorCarriesThePrefixEveryReaderClassifiesOn(t *testing.T) {
	got := webhookActor("wh1")
	if !strings.HasPrefix(got, "webhook:") {
		t.Fatalf("webhookActor = %q, want the %q prefix the run list and the audit log classify on", got, "webhook:")
	}
	if got != "webhook:wh1" {
		t.Errorf("webhookActor(\"wh1\") = %q, want %q", got, "webhook:wh1")
	}
	// A real key id is never this shape, which is the property the
	// spendability guard rests on.
	if secrets.SpendableBy(secrets.ApiKey{ScopeTeamID: "t1", ScopeUserID: got}, "t1", "alice") {
		t.Error("a key scoped to the webhook actor is spendable by a person — the guard's premise is that these identity spaces do not meet")
	}
}
