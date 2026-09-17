package cloudpublisher

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// seedScopedKey stores a key carrying a workload audience.
// The NAME and the SECRET are deliberately unlike each other. A fixture that
// used one string for both would let "the log names the key" pass on a log
// that printed the key — the assertion could not tell the two apart.
func seedScopedKey(t *testing.T, st secrets.ApiKeyStore, sealer secrets.Sealer, teamID string, provider secrets.Provider, name string, bots []string) secrets.ApiKey {
	t.Helper()
	id := secrets.NewApiKeyID()
	plaintext := "PLAINTEXT-MUST-NEVER-APPEAR-" + id
	sealed, err := secrets.SealAPIKey(sealer, id, []byte(plaintext))
	if err != nil {
		t.Fatalf("seal key: %v", err)
	}
	k := secrets.ApiKey{
		ID: id, ScopeTeamID: teamID, Provider: provider, Fingerprint: "fp-" + name,
		Name: name, SealedSecret: sealed, Bots: bots, CreatedAt: time.Now().UTC(),
	}
	if err := st.Create(context.Background(), k); err != nil {
		t.Fatalf("create key: %v", err)
	}
	return k
}

// audiencePublisher is the smallest publisher that can walk the BYOK tier,
// plus the log it wrote.
func audiencePublisher(t *testing.T) (*Publisher, secrets.Sealer, *secrets.MemoryApiKeyStore, *bytes.Buffer) {
	t.Helper()
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	keys := secrets.NewMemoryApiKeyStore()
	var buf bytes.Buffer
	p := &Publisher{apiKeys: keys, runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer,
		logger: iterlog.New(iterlog.LevelInfo, &buf)}
	return p, sealer, keys, &buf
}

func resolveWithBot(t *testing.T, p *Publisher, sealer secrets.Sealer, runID, botID string, pins map[string]string) secrets.RunBundle {
	t.Helper()
	ctx := store.WithTenant(context.Background(), "team1")
	creds, err := p.resolveAndSealCredentials(ctx, runID, "", "team1", "owner1", botID,
		nil, pins, nil, model.ModelOverrides{}, nil)
	if err != nil {
		t.Fatalf("resolveAndSealCredentials: %v", err)
	}
	if creds.secretsRef == "" {
		return secrets.RunBundle{}
	}
	rec, err := p.runSecrets.(*secrets.MemoryRunSecretsStore).Get(ctx, creds.secretsRef)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := secrets.OpenRunBundle(sealer, runID, rec.SealedBundle)
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}

// Narrowing an audience does not stop the excluded run — it walks it down to
// the next key, the org tier, the pool, the platform key, and finally the
// runner pod's ambient env. So the bill moves, and possibly the vendor, while
// the only trace is the absence of a key. Saying it once is the difference
// between an operator who can answer "why is our key not funding this" and one
// who reads an empty slot and concludes the key is gone.
func TestAWithheldKeyIsNamedInTheLog(t *testing.T) {
	p, sealer, keys, buf := audiencePublisher(t)
	seedScopedKey(t, keys, sealer, "team1", secrets.ProviderAnthropic, "audit-only-key", []string{"sec-audit-source"})

	bundle := resolveWithBot(t, p, sealer, "run-withheld", "review-pr", nil)
	if got := bundle.APIKeys[secrets.ProviderAnthropic]; got != "" {
		t.Fatalf("the scoped key funded another bot: %q", got)
	}
	log := buf.String()
	for _, want := range []string{"WITHHELD", "audit-only-key", "sec-audit-source", "review-pr"} {
		if !strings.Contains(log, want) {
			t.Fatalf("the log does not name %q — a withheld key is indistinguishable from an absent one.\nlog:\n%s", want, log)
		}
	}
	if strings.Contains(log, "PLAINTEXT-MUST-NEVER-APPEAR") {
		t.Fatalf("the diagnostic printed the sealed secret:\n%s", log)
	}
}

// The same key is offered and refused several times per launch — every tier
// re-resolves the same providers and every restore walks them again. Four
// lines about one key read as four keys, which is how a diagnostic becomes
// noise nobody reads.
func TestAWithheldKeyIsNamedOnce(t *testing.T) {
	p, sealer, keys, buf := audiencePublisher(t)
	seedScopedKey(t, keys, sealer, "team1", secrets.ProviderAnthropic, "audit-only-key", []string{"sec-audit-source"})

	resolveWithBot(t, p, sealer, "run-once", "review-pr", nil)
	if n := strings.Count(buf.String(), "WITHHELD"); n != 1 {
		t.Fatalf("the withholding was reported %d times, want exactly 1:\n%s", n, buf.String())
	}
}

// A pin is honoured over the evidence predicate BY DESIGN, and the audience is
// read before that exemption — so a pinned key its audience excludes is the
// one case where the operator's explicit choice loses. Losing silently is what
// TestResolve_pinnedKeyWithFreshRefusalsWarns exists to prevent; the audience
// must not re-create that silence under a new cause.
func TestAPinTheAudienceRefusesIsNamed(t *testing.T) {
	p, sealer, keys, buf := audiencePublisher(t)
	scoped := seedScopedKey(t, keys, sealer, "team1", secrets.ProviderAnthropic, "audit-only-key", []string{"sec-audit-source"})

	bundle := resolveWithBot(t, p, sealer, "run-pin-refused", "review-pr",
		map[string]string{string(secrets.ProviderAnthropic): scoped.ID})
	if got := bundle.APIKeys[secrets.ProviderAnthropic]; got != "" {
		t.Fatalf("pinning lifted the audience: %q", got)
	}
	log := buf.String()
	if !strings.Contains(log, "pinned api key") || !strings.Contains(log, scoped.ID) {
		t.Fatalf("a pin refused by its audience left no trace naming it.\nlog:\n%s", log)
	}
	if !strings.Contains(log, "does NOT lift an audience") {
		t.Fatalf("the log does not say why the pin lost.\nlog:\n%s", log)
	}
}

// botregistry treats `sec_audit_source`, `Sec Audit Source` and
// `sec-audit-source` as one bundle, and the launch path carries whichever
// spelling the requester typed. An audience that compared those as different
// bots would stop funding the moment someone typed it differently — and,
// per the test above, the bill would move to the next tier.
//
// The predicate stays exact; the fold happens once here, before the walk. Both
// halves matter: this test would pass on a predicate that folded internally,
// which is why the exactness table lives beside it in pkg/secrets.
func TestThePublisherFoldsTheBotIDBeforeTheWalk(t *testing.T) {
	for _, spelling := range []string{"sec-audit-source", "Sec-Audit-Source", "sec_audit_source", "SEC AUDIT SOURCE", "  sec-audit-source  "} {
		p, sealer, keys, _ := audiencePublisher(t)
		seedScopedKey(t, keys, sealer, "team1", secrets.ProviderAnthropic, "audit-only-key", []string{"sec-audit-source"})

		bundle := resolveWithBot(t, p, sealer, "run-"+spelling, spelling, nil)
		if bundle.APIKeys[secrets.ProviderAnthropic] == "" {
			t.Errorf("bot id %q did not reach its own key", spelling)
		}
	}
}

// The fold must not become a second, looser matcher: canonicalising is about
// spelling, never about widening. A different bot stays a different bot.
func TestTheFoldDoesNotAdmitADifferentBot(t *testing.T) {
	p, sealer, keys, _ := audiencePublisher(t)
	seedScopedKey(t, keys, sealer, "team1", secrets.ProviderAnthropic, "audit-only-key", []string{"sec-audit-source"})

	for _, other := range []string{"sec-audit-deps", "sec-audit", "review-pr", ""} {
		bundle := resolveWithBot(t, p, sealer, "run-other", other, nil)
		if got := bundle.APIKeys[secrets.ProviderAnthropic]; got != "" {
			t.Errorf("bot id %q was admitted by an audience naming sec-audit-source: %q", other, got)
		}
	}
}

// An open key is the common case and must stay untouched by all of the above:
// no withholding, no log line, funded for every spelling and for no bot at all.
func TestAnOpenKeyIsNeitherWithheldNorAnnounced(t *testing.T) {
	p, sealer, keys, buf := audiencePublisher(t)
	seedScopedKey(t, keys, sealer, "team1", secrets.ProviderAnthropic, "open-key", nil)

	for _, bot := range []string{"review-pr", "Sec_Audit Source", ""} {
		bundle := resolveWithBot(t, p, sealer, "run-open", bot, nil)
		if bundle.APIKeys[secrets.ProviderAnthropic] == "" {
			t.Fatalf("bot %q: an unscoped key stopped funding", bot)
		}
	}
	if strings.Contains(buf.String(), "WITHHELD") {
		t.Fatalf("an unscoped key was reported as withheld:\n%s", buf.String())
	}
}
