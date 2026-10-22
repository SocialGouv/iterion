// Epic #2000 delivery 1's acceptance line, as a test (ADR-129 §7): a team
// funding only its own `openai_compatible` endpoint, an INTEGRATION-level
// cell carrying `providers: [openai_compatible]` and pinning Revi's six LLM
// slots to `claw` + `openai_compatible/<id>`, and the run resolves no
// `claude_code` route and no `zai` route, is classified env-funded and
// acquires no LLM credential — asserted through the public entry
// (SubmitLaunch) over the resolved chain, never over the `.bot` text. Then
// the refusals: a slot whose own `provider:` hint names a provider outside
// the list is REFUSED naming provider, node and level (an override pins, it
// never drops the author's hint — "refused, never re-routed"), and a slot
// the pins leave unreadable is refused naming node and field (under a list,
// readability is compulsory).
//
// THE SHAPE THE GREEN HALF HOLDS FOR, said out loud: a program whose
// routing fields are LITERALS and whose pinned slots carry no `provider:`
// hint of their own. The catalog this repo ships does NOT have that shape —
// 34 of its bots template at least one routing field on an env dial, and
// most of those slots keep a `provider:` hint too. The Revi pair is the
// measured instance (all six slots carry both), so under the acceptance's
// own cell every shipped Revi run is refused ON A DEPLOYMENT THAT SETS NONE
// OF THOSE DIALS, which TestReviIntegrationAcceptance_TheShippedSlotsAreRefused
// asserts verbatim (its freeze is what fixes that deployment).
//
// The qualification is the whole shape of the remaining delivery work,
// because a dial's reading belongs to the deployment: `${ITERION_*}` is
// answered by the platform's stored `bot_vars` first (ir.SetEnvOverlay,
// installed at cmd/iterion/server.go) and then by the pod environment —
// `ITERION_VIBE_*` is not a credential-shaped name, so the cloud process-env
// policy passes it. Two roads therefore reach the acceptance line: de-dial
// those routing fields and drop their `provider:` hints in the `.bot` texts,
// per ADR-129 §7-8 ("a literal field is worth more than an env dial now",
// which today covers only revi-converse's `backend:`); or point the three
// `ITERION_VIBE_PROVIDER_*` dials at the tenant's own gateway, which reaches
// it without editing a `.bot` — and gives the tenant's answer to every bot
// reading those dials, not to Revi alone.
//
// Mutants: the policy's override cells dropped at the fold
// (runview.policyOverrideEntries) → the launch no longer resolves the
// gateway and is refused; the allowlist dropped (policyProviders) → the
// z.ai rescue returns to the pinned set and the platform's z.ai key seals;
// the primary refusal derived from a non-narrow-safe walk
// (providerAccumulator.endRoute) → the hinted slot is no longer refused;
// the unreadable-route refusal dropped (publisher.go's FirstUnreadable arm)
// → the shipped slots are no longer refused.

package cloudpublisher

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/llmroute"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/platformcfg"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// The team's own gateway model id — the `openai_compatible/<id>` an
// integration pins its slots to (#2005's shape).
const reviGatewayModel = "openai_compatible/gw-team1/m"

// reviSlots are the six LLM slots of the Revi pair, in the kinds they
// carry: `agent converge` and `agent converse_agent`, and the four `judge`
// reviewer slots the adaptive policy will never route (fallback_apply
// skips non-agent nodes), which is why a typed per-integration pin is the
// only routing contract they have.
var reviSlots = []string{
	"converge",
	"reviewer_claude",
	"reviewer_claude_glance",
	"reviewer_gpt",
	"reviewer_gpt_glance",
	"converse_agent",
}

// reviShapedBot is the Revi pair's routing shape on fakes: six slots, the
// claude family on `claude_code` with a `zai` rescue route each (the
// provider the sovereignty lock exists to exclude), the gpt family on
// `claw` behind an `openai/` prefix.
//
// The routing fields are LITERALS, which is the shape ADR-129 §8
// prescribes: a `${…}` dial is read through the EXPANDING process's own
// environment, so it answers for whatever deployment reads it rather than
// for the text — unreadable where the span carries no default, its
// default's own route where it carries one. `converseHint` is
// the one variable: "" leaves `converse_agent` with no `provider:` chain of
// its own (the pin decides the route), "anthropic" gives it the real bot's
// load-bearing hint (the pin cannot drop it, so the launch is refused).
func reviShapedBot(converseHint string) string {
	rescue := "  fallbacks:\n    glm:\n      model: \"glm-5.3\"\n      provider: \"zai\"\n      on: [usage_window, auth, unavailable]\n"
	hint := ""
	if converseHint != "" {
		hint = "  provider: \"" + converseHint + "\"\n"
	}
	return "agent converge:\n  model: \"claude-opus-5-5\"\n  backend: \"claude_code\"\n" + rescue +
		"\njudge reviewer_claude:\n  model: \"claude-opus-5-5\"\n  backend: \"claude_code\"\n" + rescue +
		"\njudge reviewer_claude_glance:\n  model: \"claude-opus-5-5\"\n  backend: \"claude_code\"\n" + rescue +
		"\njudge reviewer_gpt:\n  model: \"openai/gpt-6-astra\"\n  backend: \"claw\"\n" +
		"\njudge reviewer_gpt_glance:\n  model: \"openai/gpt-6-luna\"\n  backend: \"claw\"\n" +
		"\nagent converse_agent:\n  model: \"claude-opus-5-5\"\n  backend: \"claude_code\"\n" + hint + rescue +
		"\nworkflow main:\n  converge -> reviewer_claude\n  reviewer_claude -> reviewer_claude_glance\n" +
		"  reviewer_claude_glance -> reviewer_gpt\n  reviewer_gpt -> reviewer_gpt_glance\n" +
		"  reviewer_gpt_glance -> converse_agent\n  converse_agent -> done\n"
}

// integrationRoutingPolicy is the run's frozen policy as a launch lane
// resolves it from an INTEGRATION-level cell: the webhook/subscription
// record's own routing block, folded and validated by the production
// fold and the production validator, so the fixture cannot carry a cell
// no write surface would accept.
//
// `pinned` names the slots the integration re-points; every one of them is
// pinned to `claw` + the team's gateway model, which is the only spelling
// `llmroute.Validate` accepts for that harness — claw derives the provider
// from the model prefix and refuses a `provider:` cell it would ignore.
func integrationRoutingPolicy(t *testing.T, pinned ...string) *store.RunLLMRoutePolicy {
	t.Helper()
	cell := llmroute.Policy{Providers: []string{"openai_compatible"}}
	for _, slot := range pinned {
		cell.Overrides = append(cell.Overrides, llmroute.Override{
			Selector: slot, Backend: llmroute.HarnessClaw, Model: reviGatewayModel,
		})
	}
	if err := llmroute.Validate(cell); err != nil {
		t.Fatalf("the integration's cell is not a policy a write surface would accept: %v", err)
	}
	resolved, sources := llmroute.Resolve(llmroute.Layer{Source: llmroute.SourceWebhook, Policy: cell})
	out := &store.RunLLMRoutePolicy{
		PairOrder: resolved.PairOrder,
		Triggers:  resolved.Triggers,
		Providers: resolved.Providers,
		Sources:   sources,
	}
	for _, o := range resolved.Overrides {
		out.Overrides = append(out.Overrides, store.RunLLMRouteOverride{
			Selector: o.Selector, Backend: o.Backend, Model: o.Model, Provider: o.Provider, Effort: o.Effort,
		})
	}
	if got := out.Sources[llmroute.FieldProviders]; got != llmroute.SourceWebhook {
		t.Fatalf("providers provenance = %q, want %q — the witness must be driven from an integration-level cell", got, llmroute.SourceWebhook)
	}
	return out
}

// TestReviIntegrationAcceptance_AClawCellPinsNoProviderHint is the reason
// the pins above carry `backend` + `model` and no `provider` cell: claw
// derives the provider from the model prefix and acts on no other hint at
// its routing chokepoint, so the write surface REFUSES a `claw` +
// `openai_compatible` cell rather than let the gate certify a pin the
// harness ignores. The gateway spelling an operator can actually write for
// that harness is the model prefix — which is what the acceptance fixture
// writes.
func TestReviIntegrationAcceptance_AClawCellPinsNoProviderHint(t *testing.T) {
	refused := llmroute.Policy{Overrides: []llmroute.Override{{
		Selector: "converge", Backend: llmroute.HarnessClaw,
		Model: reviGatewayModel, Provider: "openai_compatible",
	}}}
	if err := llmroute.Validate(refused); err == nil {
		t.Fatal("llmroute.Validate accepted a claw cell pinning an openai_compatible provider hint — the gate would certify a pin claw ignores")
	}
	accepted := llmroute.Policy{Overrides: []llmroute.Override{{
		Selector: "converge", Backend: llmroute.HarnessClaw, Model: reviGatewayModel,
	}}}
	if err := llmroute.Validate(accepted); err != nil {
		t.Fatalf("llmroute.Validate = %v, want nil — backend + the gateway model prefix is the spelling the acceptance pins use", err)
	}
}

// reviAcceptanceFixture is the publisher the acceptance line is asserted
// against: the real sealing stack, the shared PLATFORM tier holding exactly
// the two credentials the tenant forbids (an Anthropic key and a z.ai key —
// a sealed one can only be the leak), the team holding no credential of its
// own (the gateway is funded by the runner's environment, not by a BYOK key:
// `secrets.ParseProvider` refuses `openai_compatible` by name), and
// `requireLLMCredential` armed, which is the strictest deployment the
// refusal has to survive.
type reviAcceptanceFixture struct {
	pub       *Publisher
	runs      *store.FilesystemRunStore
	logs      *bytes.Buffer
	published []*queue.RunMessage
}

func newReviAcceptanceFixture(t *testing.T) *reviAcceptanceFixture {
	t.Helper()
	sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	keys := secrets.NewMemoryApiKeyStore()
	seedKeyFP(t, keys, sealer, secrets.PlatformTenantID, secrets.ProviderZAI, "sk-zai-platform", "fp-zai")
	seedKeyFP(t, keys, sealer, secrets.PlatformTenantID, secrets.ProviderAnthropic, "sk-ant-platform", "fp-ant")
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	facade := string(platformcfg.FacadeAuto)
	f := &reviAcceptanceFixture{runs: rs, logs: &bytes.Buffer{}}
	f.pub = &Publisher{
		store:                rs,
		apiKeys:              keys,
		oauthForfait:         secrets.NewMemoryOAuthStore(),
		runSecrets:           secrets.NewMemoryRunSecretsStore(),
		genericSecrets:       secrets.NewMemoryGenericSecretStore(),
		usageCaps:            usagecap.NewMemStore(),
		sealer:               sealer,
		requireLLMCredential: true,
		platformAudience:     audienceResolver(&platformcfg.PlatformCredentials{FacadeDefault: &facade}, nil),
		logger:               iterlog.New(iterlog.LevelInfo, f.logs),
		publishRun: func(_ context.Context, m *queue.RunMessage) error {
			cp := *m
			f.published = append(f.published, &cp)
			return nil
		},
	}
	return f
}

// launch drives the PUBLIC entry on `src` under `pol`, with the policy's
// override cells folded under the launch's own rows through the production
// fold — the one every launch lane applies before it reaches the publisher
// (runview.foldAndScreen, witnessed at that seam by
// pkg/runview/service_launch_policy_fold_test.go).
func (f *reviAcceptanceFixture) launch(t *testing.T, runID, src string, pol *store.RunLLMRoutePolicy) error {
	t.Helper()
	wf := compileTestSource(t, src)
	spec := runview.LaunchSpec{
		FilePath:       "revi.bot",
		Source:         src,
		BotID:          "review-pr",
		LLMRoutePolicy: pol,
		ModelOverrides: runview.EffectiveModelOverrides(nil, pol),
	}
	ctx := store.WithIdentity(context.Background(), "team1", "webhook:cfg-1")
	_, err := f.pub.SubmitLaunch(ctx, runID, spec, wf, &runview.CompiledSource{Hash: "hash"})
	return err
}

// TestReviIntegrationAcceptance_EveryPinnedSlotRidesTheTeamGateway is epic
// #2000 delivery 1's acceptance sentence.
func TestReviIntegrationAcceptance_EveryPinnedSlotRidesTheTeamGateway(t *testing.T) {
	f := newReviAcceptanceFixture(t)
	pol := integrationRoutingPolicy(t, reviSlots...)
	if err := f.launch(t, "run-revi-ok", reviShapedBot(""), pol); err != nil {
		t.Fatalf("SubmitLaunch = %v, want nil — every slot is pinned to a route the allowlist admits", err)
	}
	if len(f.published) != 1 {
		t.Fatalf("published %d messages, want 1", len(f.published))
	}
	msg := f.published[0]

	// The resolved chain: all six slots on claw + the team's gateway model.
	// No `claude_code` harness survives, and no row names a forbidden
	// provider — this is the run the runner dispatches, not the `.bot` text.
	if len(msg.ModelOverrides) != len(reviSlots) {
		t.Fatalf("wire model_overrides = %+v, want one row per Revi slot", msg.ModelOverrides)
	}
	for _, row := range msg.ModelOverrides {
		if !slices.Contains(reviSlots, row.Selector) {
			t.Errorf("wire row %+v names no Revi slot", row)
		}
		if row.Backend != llmroute.HarnessClaw {
			t.Errorf("slot %q dispatches on backend %q, want %q — a claude_code route survived the integration's pin", row.Selector, row.Backend, llmroute.HarnessClaw)
		}
		if row.Model != reviGatewayModel {
			t.Errorf("slot %q dispatches model %q, want the team's gateway id %q", row.Selector, row.Model, reviGatewayModel)
		}
	}
	// The frozen allowlist rides the wire, so the claiming runner drops
	// every chain element resolving outside it: the z.ai rescue burns no
	// retry budget on the pod either.
	if !slices.Equal(msg.LLMProviders, []string{"openai_compatible"}) {
		t.Errorf("wire llm_providers = %v, want the frozen [openai_compatible]", msg.LLMProviders)
	}

	// NO LLM CREDENTIAL ACQUIRED — the acceptance's own property, read off
	// the run doc's credential stamp, which carries API-key AND OAuth
	// fingerprints alike and names the tiers that funded the run. The
	// platform tier holds both an Anthropic key and a z.ai key, so a
	// non-empty stamp here can only be the leak.
	ctx := store.WithIdentity(context.Background(), "team1", "webhook:cfg-1")
	run, err := f.runs.LoadRun(ctx, "run-revi-ok")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if len(run.CredFingerprints) != 0 || len(run.CredentialTiers) != 0 {
		t.Errorf("run cred_fingerprints = %v, credential_tiers = %v, want neither — an env-funded run acquires no LLM credential", run.CredFingerprints, run.CredentialTiers)
	}
	for _, forbidden := range []string{"anthropic", "zai", "openai"} {
		if slices.Contains(run.PinnedProviders, forbidden) {
			t.Errorf("run pinned_providers = %v, want no %q — no route of this run may spend it", run.PinnedProviders, forbidden)
		}
	}
	// No bundle at all is this FIXTURE's corollary, not the property: its
	// program declares no `secrets:`, and the generic tier is the one tier
	// an env-funded run still walks (publisher.go's envFunded arm). A
	// shipped Revi program declares `secrets: forge_token`, so it seals a
	// bundle for that token while still acquiring no LLM credential.
	if msg.SecretsRef != "" || msg.BundleDEK != nil {
		t.Errorf("wire secrets_ref = %q, dek = %v, want neither — this program declares no `secrets:`, so there is nothing to seal", msg.SecretsRef, msg.BundleDEK)
	}

	// The classification and the prune both leave an artifact an operator
	// can read: the run is env-funded, and the z.ai routes were pruned by
	// name with their sites, not dropped in silence.
	logs := f.logs.String()
	if !strings.Contains(logs, "env-funded") {
		t.Errorf("the launch did not classify the run env-funded: %s", logs)
	}
	if !strings.Contains(logs, "\"zai\" outside this run's providers allowlist") {
		t.Errorf("the launch did not name the pruned z.ai routes: %s", logs)
	}
}

// TestReviIntegrationAcceptance_AHintedSlotIsRefusedNeverReRouted: the same
// program, the same six pins, but `converse_agent` keeps the real bot's
// load-bearing `provider: "anthropic"` hint. An override pins a field, it
// never drops the author's — so the hint still IS that route, it resolves
// outside the tenant's list, and the launch is REFUSED naming the provider,
// the node and the level that set the list. Nothing is persisted and
// nothing is published: the refusal precedes the run.
func TestReviIntegrationAcceptance_AHintedSlotIsRefusedNeverReRouted(t *testing.T) {
	f := newReviAcceptanceFixture(t)
	pol := integrationRoutingPolicy(t, reviSlots...)
	err := f.launch(t, "run-revi-hint", reviShapedBot("anthropic"), pol)
	if !errors.Is(err, model.ErrProviderOutsideAllowlist) {
		t.Fatalf("SubmitLaunch = %v, want the outside-allowlist refusal", err)
	}
	var outside *model.ProviderOutsideError
	if !errors.As(err, &outside) {
		t.Fatalf("error %v does not carry the refusal's provider/site/level", err)
	}
	if outside.Site != "converse_agent" {
		t.Errorf("refusal names site %q, want converse_agent", outside.Site)
	}
	if outside.Provider != "anthropic" {
		t.Errorf("refusal names provider %q, want anthropic", outside.Provider)
	}
	if outside.Level != llmroute.SourceWebhook {
		t.Errorf("refusal names level %q, want %q — the integration level that set the list", outside.Level, llmroute.SourceWebhook)
	}
	if len(f.published) != 0 {
		t.Errorf("the refused launch published %d messages, want 0", len(f.published))
	}
	if _, err := f.runs.LoadRun(store.WithIdentity(context.Background(), "team1", "webhook:cfg-1"), "run-revi-hint"); err == nil {
		t.Error("the refused launch left a run document behind")
	}
}

// reviShippedBot carries the routing field texts of the Revi pair this repo
// SHIPS, verbatim: bots/review-pr/lib/nodes.bot (converge, reviewer_claude,
// reviewer_claude_glance, reviewer_gpt, reviewer_gpt_glance) and
// bots/revi-converse/main.bot (converse_agent). Every slot templates its
// routing on an `${ITERION_VIBE_*}` dial and keeps a `provider:` hint of its
// own — the two shapes §4 rule 1 and §5 make load-bearing.
const reviShippedBot = "agent converge:\n" +
	"  model: \"${ITERION_VIBE_MODEL_EMIT:-claude-opus-5-5}\"\n" +
	"  backend: \"${ITERION_VIBE_BACKEND_EMIT:-claude_code}\"\n" +
	"  provider: \"${ITERION_VIBE_PROVIDER_EMIT:-anthropic}\"\n" +
	"\njudge reviewer_claude:\n" +
	"  model: \"${ITERION_VIBE_MODEL_CLAUDE:-claude-opus-5-5}\"\n" +
	"  backend: \"${ITERION_VIBE_BACKEND_CLAUDE:-claude_code}\"\n" +
	"  provider: \"${ITERION_VIBE_PROVIDER_CLAUDE:-anthropic}\"\n" +
	"\njudge reviewer_claude_glance:\n" +
	"  model: \"${ITERION_VIBE_MODEL_CLAUDE_GLANCE:-claude-opus-5-5}\"\n" +
	"  backend: \"${ITERION_VIBE_BACKEND_CLAUDE_GLANCE:-claude_code}\"\n" +
	"  provider: \"${ITERION_VIBE_PROVIDER_CLAUDE:-anthropic}\"\n" +
	"\njudge reviewer_gpt:\n" +
	"  model: \"${ITERION_VIBE_MODEL_GPT:-openai/gpt-6-astra}\"\n" +
	"  backend: \"${ITERION_VIBE_BACKEND_GPT:-claw}\"\n" +
	"  provider: \"${ITERION_VIBE_PROVIDER_GPT:-}\"\n" +
	"\njudge reviewer_gpt_glance:\n" +
	"  model: \"${ITERION_VIBE_MODEL_GPT_GLANCE:-openai/gpt-6-luna}\"\n" +
	"  backend: \"${ITERION_VIBE_BACKEND_GPT_GLANCE:-claw}\"\n" +
	"  provider: \"${ITERION_VIBE_PROVIDER_GPT:-}\"\n" +
	"\nagent converse_agent:\n" +
	"  model: \"${ITERION_VIBE_MODEL_CLAUDE:-claude-opus-5-5}\"\n" +
	"  backend: \"claude_code\"\n" +
	"  provider: \"${ITERION_VIBE_PROVIDER_CLAUDE:-anthropic}\"\n" +
	"\nworkflow main:\n  converge -> reviewer_claude\n  reviewer_claude -> reviewer_claude_glance\n" +
	"  reviewer_claude_glance -> reviewer_gpt\n  reviewer_gpt -> reviewer_gpt_glance\n" +
	"  reviewer_gpt_glance -> converse_agent\n  converse_agent -> done\n"

// freezeHostDials blanks every environment name the source references
// through a `${…}` span, so the walk below reads the FIXTURE's own defaults
// whatever the shell the suite runs in. Load-bearing, not hygiene: these are
// this repo's own Revi dials, and an operator who has one exported
// (`ITERION_VIBE_PROVIDER_GPT=openai_compatible` is enough) would otherwise
// read a routing regression where the verdict only changed shell.
//
// The names come from the source text, never from a list beside it: a dial
// added to the fixture is frozen with the fixture, where a list would drift
// and hand the verdict back to the host. A blank value is what `:-` treats
// as unset (`ir.ExpandWithDefault`), so each span reads its own default.
//
// The scanner is one half; assertNoHostAnsweredDial is the other, and the
// one that makes the guarantee hold for a form the scanner does not know.
func freezeHostDials(t *testing.T, source string) {
	t.Helper()
	names := envDialNames(source)
	if len(names) == 0 {
		t.Fatalf("the source references no ${…} dial — freezeHostDials is pointed at the wrong text")
	}
	for _, n := range names {
		t.Setenv(n, "")
	}
	assertNoHostAnsweredDial(t, source)
}

// assertNoHostAnsweredDial is the freeze's post-condition, and it asks the
// READER the walk uses rather than the scanner that produced the names: a
// text the freeze covers reads the same through the production expander as
// through an expander that answers nothing.
//
// Anchored on the producer because a scanner is a model of its grammar and
// this one is narrower than the expander it neutralises: `envDialNames`
// scans `${`, while `ir.ExpandEnvWithDefault` runs with `bracedOnly` false
// and resolves a bare `$NAME` too (`ir.ExpandBracedWithDefault` is the
// braced-only twin). A bare dial therefore leaves the scanner's set empty
// of it and the emptiness guard silent — the rest of the text still carries
// braced spans — and the verdict goes back to the host. With the
// post-condition, a reference form no freeze covered REFUSES the test on
// the very deployment that would have changed its answer.
func assertNoHostAnsweredDial(t *testing.T, texts ...string) {
	t.Helper()
	for _, s := range texts {
		got, want := ir.ExpandEnvWithDefault(s), ir.ExpandWithDefault(s, func(string) string { return "" })
		if got != want {
			t.Fatalf("this host still answers a reference the text carries: the production reader gives %q where an unset environment gives %q — freeze that dial", got, want)
		}
	}
}

// envDialNames lists the environment names a source references through
// `${NAME}` or `${NAME:-default}`, deduplicated, in first-seen order.
func envDialNames(source string) []string {
	var names []string
	seen := map[string]bool{}
	for rest := source; ; {
		i := strings.Index(rest, "${")
		if i < 0 {
			return names
		}
		rest = rest[i+2:]
		end := 0
		for end < len(rest) && (rest[end] == '_' || rest[end] >= 'A' && rest[end] <= 'Z' ||
			rest[end] >= 'a' && rest[end] <= 'z' || rest[end] >= '0' && rest[end] <= '9') {
			end++
		}
		if name := rest[:end]; name != "" && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
		rest = rest[end:]
	}
}

// TestReviIntegrationAcceptance_TheShippedSlotsAreRefused is the third
// refusal, and the acceptance's own limit: under the SAME cell the green
// half is asserted with — `providers: [openai_compatible]` and all six slots
// pinned to claw + the team's gateway — the field texts the repo actually
// ships are REFUSED. The pins cannot rescue them: the two gpt slots' dial
// carries an EMPTY default, so the walk cannot read it (§4 rule 1), and the
// four claude slots' dial reads its own default `anthropic` hint, which IS
// the route (§5, "refused, never re-routed"); the one spelling that could
// re-point that hint — a `claw` cell carrying `provider: openai_compatible`
// — is what the write surface refuses (the sibling test above).
//
// So a tenant that locks `providers:` gets every Revi run refused today on a
// deployment whose `bot_vars` and pod environment answer none of these
// dials, which is the deployment freezeHostDials fixes and the only one this
// witness speaks for: point the three `ITERION_VIBE_PROVIDER_*` dials at that
// tenant's own gateway and the same cell admits the same texts (the file
// header's second road). The launch names ONE site (the first the walk
// cannot read); the walk's own inventory is asserted beside it, because the
// operator has six fields to fix, not one.
func TestReviIntegrationAcceptance_TheShippedSlotsAreRefused(t *testing.T) {
	freezeHostDials(t, reviShippedBot)
	f := newReviAcceptanceFixture(t)
	pol := integrationRoutingPolicy(t, reviSlots...)
	err := f.launch(t, "run-revi-shipped", reviShippedBot, pol)
	if !errors.Is(err, model.ErrRouteUnreadableUnderAllowlist) {
		t.Fatalf("SubmitLaunch = %v, want the unreadable-route refusal — the gpt slots' `provider:` dial carries no default, and an allowlist makes readability compulsory", err)
	}
	var unread *model.RouteUnreadableError
	if !errors.As(err, &unread) {
		t.Fatalf("error %v does not carry the refusal's site/field", err)
	}
	if unread.Site != "reviewer_gpt" || unread.Field != "provider" {
		t.Errorf("refusal names site=%q field=%q, want reviewer_gpt/provider — the empty-default dial the operator has to resolve first", unread.Site, unread.Field)
	}
	if !strings.Contains(err.Error(), "${ITERION_VIBE_PROVIDER_GPT:-}") {
		t.Errorf("refusal %v does not quote the field text the operator has to edit", err)
	}
	if len(f.published) != 0 {
		t.Errorf("the refused launch published %d messages, want 0", len(f.published))
	}

	// The whole inventory, through the same walk the gate reads: four slots
	// keep the `anthropic` their surviving hint names, and both gpt slots
	// are unreadable. The launch can only name the first.
	wf := compileTestSource(t, reviShippedBot)
	view := model.RouteViewForLaunch(wf, buildModelOverrides(runview.EffectiveModelOverrides(nil, pol)), nil, map[string]string(nil), nil)
	view.AllowedProviders = policyProviders(pol)
	walk := model.EffectiveProviders(view, knownPoolProviders)
	if walk.OnlyEnvFunded() {
		t.Error("the shipped texts classify env-funded under the acceptance's cell — the dials would then be free")
	}
	forbidden := slices.Clone(walk.ForbiddenPrimary["anthropic"])
	slices.Sort(forbidden)
	want := []string{"converge", "converse_agent", "reviewer_claude", "reviewer_claude_glance"}
	if !slices.Equal(forbidden, want) {
		t.Errorf("walk refuses anthropic at %v, want %v — the four claude slots' surviving hint is still their primary route", forbidden, want)
	}
	var unreadable []string
	for _, u := range walk.Unreadable {
		unreadable = append(unreadable, u.Site+"/"+u.Field)
	}
	slices.Sort(unreadable)
	if !slices.Equal(unreadable, []string{"reviewer_gpt/provider", "reviewer_gpt_glance/provider"}) {
		t.Errorf("walk cannot read %v, want both gpt slots' provider field", unreadable)
	}
}

// TestReviIntegrationAcceptance_AnUnpinnedSlotIsRefused: the slot the
// integration forgot. Its own route names nothing the walk can read
// (`claude_code` + a prefix-less model), and under a non-empty allowlist a
// lock that cannot be checked must not pass — refused, naming the node and
// the field the operator has to pin.
func TestReviIntegrationAcceptance_AnUnpinnedSlotIsRefused(t *testing.T) {
	f := newReviAcceptanceFixture(t)
	pol := integrationRoutingPolicy(t, "reviewer_claude", "reviewer_claude_glance", "reviewer_gpt", "reviewer_gpt_glance", "converse_agent")
	err := f.launch(t, "run-revi-gap", reviShapedBot(""), pol)
	if !errors.Is(err, model.ErrRouteUnreadableUnderAllowlist) {
		t.Fatalf("SubmitLaunch = %v, want the unreadable-route refusal for the unpinned slot", err)
	}
	var unread *model.RouteUnreadableError
	if !errors.As(err, &unread) {
		t.Fatalf("error %v does not carry the refusal's site/field", err)
	}
	if unread.Site != "converge" || unread.Field != "model" {
		t.Errorf("refusal names site=%q field=%q, want converge/model", unread.Site, unread.Field)
	}
	if len(f.published) != 0 {
		t.Errorf("the refused launch published %d messages, want 0", len(f.published))
	}
}
