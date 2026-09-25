package cloudpublisher

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
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
		nil, pins, nil, model.ModelOverrides{}, nil, store.RunTrustDefault, nil)
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

// The audience's canonicalisation must not reach the OTHER readers of the same
// bot id. Two stores match it EXACTLY and have no folding write edge: bot
// secret bindings (stored raw from the route's path value) and credpool
// pledges (rows written before canonicalBotIDs existed). A stored bot may
// legitimately be named `my_bot` — botsource.ValidSlug admits `_` — so folding
// the shared variable made a REQUIRED secret resolve to nothing and blocked the
// launch, and let an `optional: true` one run unauthenticated.
//
// This is a REGRESSION test: it passed before the fold was introduced, failed
// with it, and passes again now that the fold belongs to the audience alone.
func TestTheAudienceFoldDoesNotReachBotSecretBindings(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	const underscoreBot = "my_bot"

	generic := secrets.NewMemoryGenericSecretStore()
	secretID := secrets.NewGenericSecretID()
	sealedSecret, err := secrets.SealGenericSecret(sealer, secretID, []byte("bound-token"))
	if err != nil {
		t.Fatalf("SealGenericSecret: %v", err)
	}
	if err := generic.Create(context.Background(), secrets.GenericSecret{
		ID: secretID, ScopeTeamID: "team1", Name: "org_forge_token",
		SealedSecret: sealedSecret, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("generic Create: %v", err)
	}
	bindings := secrets.NewMemoryBotSecretBindingStore()
	if err := bindings.Create(context.Background(), secrets.BotSecretBinding{
		ID: "binding-1", TenantID: "team1", BotID: underscoreBot,
		SecretID: secretID, SecretNameForWorkflow: "forge_token",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("binding Create: %v", err)
	}

	p, _, _, _ := audiencePublisher(t)
	p.genericSecrets = generic
	p.botBindings = bindings
	wf := &ir.Workflow{Name: "w", Secrets: map[string]*ir.Secret{"forge_token": {}}}

	ctx := store.WithTenant(context.Background(), "team1")
	if _, err := p.resolveAndSealCredentials(ctx, "run-binding", "", "team1", "owner1", underscoreBot,
		wf, nil, nil, model.ModelOverrides{}, nil, store.RunTrustDefault, nil); err != nil {
		t.Fatalf("a binding on a bot named %q stopped resolving — the audience's fold reached a store that matches exactly: %v", underscoreBot, err)
	}
}

// One withheld key cannot tell "once per key" from "once, full stop". Two keys
// can, and the difference matters: a team that scopes several keys would be
// told about one of them and left to wonder about the rest.
func TestEveryWithheldKeyIsNamed(t *testing.T) {
	p, sealer, keys, buf := audiencePublisher(t)
	seedScopedKey(t, keys, sealer, "team1", secrets.ProviderAnthropic, "anthropic-audit-only", []string{"sec-audit-source"})
	seedScopedKey(t, keys, sealer, "team1", secrets.ProviderOpenAI, "openai-audit-only", []string{"sec-audit-source"})

	resolveWithBot(t, p, sealer, "run-two", "review-pr", nil)
	log := buf.String()
	for _, want := range []string{"anthropic-audit-only", "openai-audit-only"} {
		if !strings.Contains(log, want) {
			t.Fatalf("the log does not name %q — only the first withholding was reported.\nlog:\n%s", want, log)
		}
	}
	if n := strings.Count(log, "WITHHELD"); n != 2 {
		t.Fatalf("WITHHELD appears %d times, want 2 (one per key, no more, no fewer):\n%s", n, log)
	}
}

// The pin diagnostic must name the pin that its OWN audience refused, never a
// pin that happens to coexist with some other withheld key. A false, confident
// line pointing at the wrong cause is worse than no line: it sends the operator
// to edit an audience that was never involved.
func TestAHealthyPinIsNotBlamedOnAnotherKeysAudience(t *testing.T) {
	p, sealer, keys, buf := audiencePublisher(t)
	// Withheld, and NOT the pinned one.
	seedScopedKey(t, keys, sealer, "team1", secrets.ProviderOpenAI, "openai-audit-only", []string{"sec-audit-source"})
	open := seedScopedKey(t, keys, sealer, "team1", secrets.ProviderAnthropic, "anthropic-open", nil)

	bundle := resolveWithBot(t, p, sealer, "run-healthy-pin", "review-pr",
		map[string]string{string(secrets.ProviderAnthropic): open.ID})
	if bundle.APIKeys[secrets.ProviderAnthropic] == "" {
		t.Fatalf("the healthy pinned key did not fund the run")
	}
	log := buf.String()
	if strings.Contains(log, "does NOT lift an audience") {
		t.Fatalf("a healthy pin was reported as refused by an audience it never had:\n%s", log)
	}
	if !strings.Contains(log, "openai-audit-only") {
		t.Fatalf("the genuinely withheld key went unreported:\n%s", log)
	}
}
