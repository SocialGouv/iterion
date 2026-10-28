// ADR-121's parity property under ADR-129 §7: fill, restore, preview and
// the launch gate must give ONE answer for a run whose routing fields are
// TEMPLATED and whose vars pin them — and the property is owed from an
// INTEGRATION-level cell, because a witness written over launcher rows
// would pass without ever exercising that level.
//
// The answer, in both directions:
//   - the gateway inside the tenant's list: nothing is acquired (the run is
//     env-funded through the vars its launch pinned), the launch gate refuses
//     nothing, the resume replays the same reading, and the preview says so;
//   - the gateway OUTSIDE it: the launch refuses by name, and the preview
//     names the same refusal where the operator is looking.
//
// What is NOT claimed: this is parity between the four READERS given one
// launch shape. End-to-end parity for a run also needs every lane to hand
// each reader the same shape — the preview request type carries no vars at
// all today, so the preview of a manual launch made with `--var` answers for
// the declared defaults (ADR-129 §7, the remaining step).
//
// The program carries one slot per mechanism: `draft`, whose routing fields
// are `{{vars.*}}` spans the LAUNCH's own vars pin, and `review`, whose
// route the integration's `overrides:` cell pins through the ONE fold (§6).
// Each leg reads both, so each mutant below has a leg to red.
//
// Mutants, each red on its own leg:
//   - the restore reading the program with its vars declared LATE
//     (publisher.go's resumeView back to RouteViewForLaunch with no trusted
//     set) → `draft` goes undecided, the frozen list refuses the resume →
//     the restore leg reds;
//   - the restore reading the program WITHOUT the run's vars
//     (resumeView's prior.Inputs → nil) → `draft` falls back to its
//     declared default, which is the anthropic pair the list forbids → the
//     restore leg reds;
//   - the preview's view built without the launch's vars
//     (credential_preview.go's spec.Launch.Vars → nil) → the preview names
//     a refusal the fill did not raise → the preview leg reds;
//   - the policy's override cells dropped at the fold
//     (runview.policyOverrideEntries → nil) → `review` keeps its own
//     anthropic route → the fill leg reds.

package cloudpublisher

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/llmroute"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// templatedRoutingBot is the shape §4 exists for. `draft`'s routing fields
// are `{{vars.*}}` spans whose DECLARED DEFAULTS ride the anthropic pair and
// whose launch values ride the team's gateway: the two readings differ, so a
// reader that drops the run's vars answers a different question and says so.
// `review` carries literal fields on the same anthropic pair, and is the slot
// the integration's cell re-points. The `zai` rescue is the route a tenant
// allowlist prunes.
const templatedRoutingBot = "vars:\n" +
	"  revi_backend: string = \"claw\"\n" +
	"  revi_model: string = \"anthropic/claude-opus-5-5\"\n" +
	"\nagent draft:\n" +
	"  model: \"{{vars.revi_model}}\"\n" +
	"  backend: \"{{vars.revi_backend}}\"\n" +
	"  fallbacks:\n" +
	"    glm:\n" +
	"      model: \"glm-5.3\"\n" +
	"      provider: \"zai\"\n" +
	"      on: [usage_window, auth, unavailable]\n" +
	"\nagent review:\n" +
	"  model: \"anthropic/claude-opus-5-5\"\n" +
	"  backend: \"claw\"\n" +
	"\nworkflow main:\n  draft -> review\n  review -> done\n"

// parityLaunchVars is the operator's own launch values — the CLI's `--var`,
// the studio launch form — pinning `draft` onto the team's gateway.
var parityLaunchVars = map[string]string{"revi_backend": "claw", "revi_model": reviGatewayModel}

// parityFixture is one publisher with the real sealing stack and the shared
// PLATFORM tier holding the two credentials the tenant forbids, so a sealed
// one is a measurable leak rather than an absence.
type parityFixture struct {
	pub       *Publisher
	runs      *store.FilesystemRunStore
	logs      *bytes.Buffer
	published []*queue.RunMessage
}

func newParityFixture(t *testing.T) *parityFixture {
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
	f := &parityFixture{runs: rs, logs: &bytes.Buffer{}}
	f.pub = &Publisher{
		store:                rs,
		apiKeys:              keys,
		oauthForfait:         secrets.NewMemoryOAuthStore(),
		runSecrets:           secrets.NewMemoryRunSecretsStore(),
		genericSecrets:       secrets.NewMemoryGenericSecretStore(),
		usageCaps:            usagecap.NewMemStore(),
		sealer:               sealer,
		requireLLMCredential: true,
		logger:               iterlog.New(iterlog.LevelInfo, f.logs),
		publishRun: func(_ context.Context, m *queue.RunMessage) error {
			cp := *m
			f.published = append(f.published, &cp)
			return nil
		},
	}
	return f
}

func (f *parityFixture) ctx() context.Context {
	return store.WithIdentity(context.Background(), "team1", "webhook:cfg-1")
}

// launchSpecFor is the ONE launch shape all four readers are given, so a
// disagreement can only come from a reader, never from its input. It carries
// the operator's vars AND the names the lane pins for them — the shape
// runs_launch.go builds for `POST /api/runs` — because the property §7 adds
// to ADR-121's is about a run whose VARS pin its routes: with no vars the
// run's Inputs are empty and every reader computes the same
// declared-defaults-only reading, which no mutant can tell apart.
func launchSpecFor(pol *store.RunLLMRoutePolicy) runview.LaunchSpec {
	return runview.LaunchSpec{
		FilePath:        "revi.bot",
		Source:          templatedRoutingBot,
		BotID:           "review-pr",
		LLMRoutePolicy:  pol,
		ModelOverrides:  runview.EffectiveModelOverrides(nil, pol),
		Vars:            parityLaunchVars,
		TrustedVarNames: []string{"revi_backend", "revi_model"},
	}
}

// assertPinnedReviewRow is the integration cell's ONE fold (§6) as each
// reader must carry it: the `review` slot's row, claw + the team's gateway
// model, on the wire the reader produced. Without it the slot keeps its own
// anthropic route and the tenant's list refuses the launch.
func assertPinnedReviewRow(t *testing.T, leg string, rows []queue.ModelOverride) {
	t.Helper()
	var found bool
	for _, row := range rows {
		if row.Selector != "review" {
			continue
		}
		found = true
		if row.Backend != llmroute.HarnessClaw || row.Model != reviGatewayModel {
			t.Errorf("%s: review row = %+v, want claw + %q — the integration's cell is what re-points that slot", leg, row, reviGatewayModel)
		}
	}
	if !found {
		t.Errorf("%s: wire model_overrides = %+v, want the integration's review row — the ONE fold never reached this reader", leg, rows)
	}
}

// TestRoutingParity_TemplatedProgramUnderAnIntegrationCell: the four
// readers agree that nothing is acquired for a program whose every route
// rides the tenant's own gateway — `draft` through the vars the launch
// pinned, `review` through the integration's override cell.
func TestRoutingParity_TemplatedProgramUnderAnIntegrationCell(t *testing.T) {
	f := newParityFixture(t)
	// The cell re-points `review`; `draft` is pinned by the launch's vars.
	// Both halves of an integration cell (§5's allowlist, §6's overrides)
	// are therefore read on every leg.
	pol := integrationRoutingPolicy(t, "review")
	wf := compileTestSource(t, templatedRoutingBot)
	cs := &runview.CompiledSource{Hash: "hash"}
	spec := launchSpecFor(pol)

	// Reader 1 — FILL. The launch gate (requireLLMCredential, armed) refuses
	// nothing, and the message carries the frozen list with no bundle.
	if _, err := f.pub.SubmitLaunch(f.ctx(), "run-parity", spec, wf, cs); err != nil {
		t.Fatalf("fill: SubmitLaunch = %v, want nil — the launch's vars and the integration's cell resolve every route onto a provider the list admits", err)
	}
	if len(f.published) != 1 {
		t.Fatalf("fill published %d messages, want 1", len(f.published))
	}
	fill := f.published[0]
	if fill.SecretsRef != "" || fill.BundleDEK != nil {
		t.Errorf("fill sealed a bundle (secrets_ref=%q) — a gateway-routed run acquires nothing", fill.SecretsRef)
	}
	if !slices.Equal(fill.LLMProviders, []string{"openai_compatible"}) {
		t.Errorf("fill llm_providers = %v, want the frozen [openai_compatible]", fill.LLMProviders)
	}
	assertPinnedReviewRow(t, "fill", fill.ModelOverrides)
	run, err := f.runs.LoadRun(f.ctx(), "run-parity")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if len(run.PinnedProviders) != 0 {
		t.Errorf("fill pinned_providers = %v, want none — the pruned z.ai rescue is not this run's spend", run.PinnedProviders)
	}

	// Reader 2 — RESTORE. The resume reads the SAME program through the run's
	// own inputs and the run doc's frozen policy: same answer, or a paused
	// run can never resume (an undecided field is a refusal under a list).
	if err := f.runs.UpdateRunStatus(f.ctx(), "run-parity", store.RunStatusPausedWaitingHuman, ""); err != nil {
		t.Fatalf("pause: %v", err)
	}
	f.published = nil
	if err := f.pub.SubmitResume(f.ctx(), runview.ResumeSpec{
		RunID: "run-parity", FilePath: "revi.bot", Source: templatedRoutingBot,
	}, wf, cs); err != nil {
		t.Fatalf("restore: SubmitResume = %v, want nil — the restore must read the templated routes exactly as the fill did", err)
	}
	if len(f.published) != 1 {
		t.Fatalf("restore published %d messages, want 1", len(f.published))
	}
	restore := f.published[0]
	if restore.SecretsRef != "" || restore.BundleDEK != nil {
		t.Errorf("restore sealed a bundle (secrets_ref=%q) where the fill sealed none", restore.SecretsRef)
	}
	if !slices.Equal(restore.LLMProviders, fill.LLMProviders) {
		t.Errorf("restore llm_providers = %v, fill = %v — the frozen list must be replayed, never re-resolved", restore.LLMProviders, fill.LLMProviders)
	}
	assertPinnedReviewRow(t, "restore", restore.ModelOverrides)
	if !slices.Equal(restore.ModelOverrides, fill.ModelOverrides) {
		t.Errorf("restore model_overrides = %+v, fill = %+v — both resumes replay the launch's folded rows (§6)", restore.ModelOverrides, fill.ModelOverrides)
	}
	// The credential property itself, re-read after the resume's own
	// re-stamp: a reader that declared the run's inputs late would classify
	// the gateway route undecided and hand the resumed attempt a platform
	// key the launch deliberately did not acquire.
	resumed, err := f.runs.LoadRun(f.ctx(), "run-parity")
	if err != nil {
		t.Fatalf("LoadRun after resume: %v", err)
	}
	if len(resumed.CredFingerprints) != 0 {
		t.Errorf("run cred_fingerprints = %v after the resume, want none — the fill acquired nothing, so the resume must acquire nothing", resumed.CredFingerprints)
	}

	// Reader 3 — PREVIEW. It observes the same answer: the env-funded note,
	// the pruned rescue named, and no refusal claimed where there is none.
	v, err := f.pub.PreviewCredentials(f.ctx(), runview.CredentialPreviewSpec{
		Context:        runview.CredentialPreviewContext{TeamID: "team1", BotID: "review-pr"},
		OwnerID:        "webhook:cfg-1",
		Launch:         spec,
		LLMRoutePolicy: pol,
	}, wf)
	if err != nil {
		t.Fatalf("preview: PreviewCredentials = %v", err)
	}
	warnings := strings.Join(v.Warnings, "\n")
	if !strings.Contains(warnings, "rides the runner's openai_compatible gateway") {
		t.Errorf("preview warnings = %v, want the env-funded note the fill logged", v.Warnings)
	}
	if !strings.Contains(warnings, "prunes the zai route") {
		t.Errorf("preview warnings = %v, want the pruned z.ai rescue named", v.Warnings)
	}
	if strings.Contains(warnings, "The live launch refuses") {
		t.Errorf("preview claims a refusal the fill did not raise: %v", v.Warnings)
	}
	for _, c := range v.Candidates {
		if c.Selected {
			t.Errorf("preview selects candidate %s:%s:%s — the fill acquired nothing", c.Tier, c.Source, c.Provider)
		}
	}
}

// TestRoutingParity_TheRefusalIsTheSameOnEveryReader: the other direction of
// the one answer. The integration's list excludes the gateway the launch's
// vars resolve to, so the fill REFUSES by name — and the preview, which
// never refuses, names the same refusal with the same provider and the same
// level, where the operator is looking.
func TestRoutingParity_TheRefusalIsTheSameOnEveryReader(t *testing.T) {
	f := newParityFixture(t)
	cell := llmroute.Policy{Providers: []string{"anthropic"}}
	if err := llmroute.Validate(cell); err != nil {
		t.Fatalf("the integration's cell is not a policy a write surface would accept: %v", err)
	}
	resolved, sources := llmroute.Resolve(llmroute.Layer{Source: llmroute.SourceWebhook, Policy: cell})
	pol := &store.RunLLMRoutePolicy{PairOrder: resolved.PairOrder, Triggers: resolved.Triggers, Providers: resolved.Providers, Sources: sources}
	wf := compileTestSource(t, templatedRoutingBot)
	spec := launchSpecFor(pol)

	_, err := f.pub.SubmitLaunch(f.ctx(), "run-parity-refused", spec, wf, &runview.CompiledSource{Hash: "hash"})
	if !errors.Is(err, model.ErrProviderOutsideAllowlist) {
		t.Fatalf("fill: SubmitLaunch = %v, want the outside-allowlist refusal — the launch's vars resolve the primary onto a provider the list forbids", err)
	}
	var outside *model.ProviderOutsideError
	if !errors.As(err, &outside) {
		t.Fatalf("fill error %v does not carry the refusal's provider/site/level", err)
	}
	if outside.Site != "draft" || outside.Provider != "openai_compatible" || outside.Level != llmroute.SourceWebhook {
		t.Errorf("fill refusal = site %q provider %q level %q, want draft/openai_compatible/%s", outside.Site, outside.Provider, outside.Level, llmroute.SourceWebhook)
	}

	v, perr := f.pub.PreviewCredentials(f.ctx(), runview.CredentialPreviewSpec{
		Context:        runview.CredentialPreviewContext{TeamID: "team1", BotID: "review-pr"},
		OwnerID:        "webhook:cfg-1",
		Launch:         spec,
		LLMRoutePolicy: pol,
	}, wf)
	if perr != nil {
		t.Fatalf("preview: PreviewCredentials = %v", perr)
	}
	warnings := strings.Join(v.Warnings, "\n")
	if !strings.Contains(warnings, "The live launch refuses") ||
		!strings.Contains(warnings, "node draft pins provider \"openai_compatible\"") ||
		!strings.Contains(warnings, "set by the "+llmroute.SourceWebhook+" level") {
		t.Errorf("preview warnings = %v, want the fill's refusal named with its node, provider and level", v.Warnings)
	}
}

// backendOnlyTemplatedBot carries the catalog's only `{{vars.…}}` routing
// dial (`bots/docs-refresh/main.bot:885`, :925) with the declaration that
// feeds it, verbatim (`llm_backend: string = "claude_code"`, :251): a
// `backend:` a var pins, over a model the allowlist can read. The declared
// default is load-bearing here — it is the value a CARD lane reads, the
// var's name being outside every carry list, so it has to be the catalog's
// own and not the one an operator layer pins.
//
// The model is NOT the catalog's: docs-refresh dials it on
// `${ITERION_DOC_ALIGN_MODEL_CLAUDE:-claude-opus-5-5}`, a bare anthropic id
// a non-empty `providers:` list refuses as unreadable — identically on both
// lanes, since no launch var reaches an `${…}` span (§7). The substitution
// is the point: with the model half readable, the legs below measure the
// `backend:` dial and nothing else.
const backendOnlyTemplatedBot = "vars:\n" +
	"  llm_backend: string = \"claude_code\"\n" +
	"\nagent draft:\n" +
	"  model: \"" + reviGatewayModel + "\"\n" +
	"  backend: \"{{vars.llm_backend}}\"\n" +
	"\nworkflow main:\n  draft -> done\n"

// cardLaneSpec is the launch shape BOTH card lanes build, for the same
// integration cell and the same program the direct lane above launches: the
// card's `bot_args` as the whole var set — of the integration's var layers a
// card keeps only the NAMES its carry list holds, and a routing var is not
// one of them (TestLaunchLanes_CarryNoIntegrationVarLayer measures both
// halves on the lanes themselves) — no name pinned operator-decided
// (TestLaunchLanes_PinNoLaunchVarAsOperatorDecided), and the dispatcher's
// warn-and-proceed input contract.
func cardLaneSpec(pol *store.RunLLMRoutePolicy, source string, vars map[string]string) runview.LaunchSpec {
	spec := launchSpecFor(pol)
	spec.Source = source
	spec.Vars, spec.TrustedVarNames, spec.AllowUnknownInputs = vars, nil, true
	return spec
}

// Since #2665 item 1 (ARB-7) the card lanes hand the run the integration's
// var layers and trust the operator's names (pkg/server,
// TestLaunchLanes_HandTheRunTheIntegrationsVarLayers), so the shape below is
// no longer theirs: the legs keep measuring the PUBLISHER's own reading of a
// var-less, untrusted spec — the shape a hand-typed card and the trigger
// spine still hand it.
//
// TestRoutingParity_TheCardLaneShapeIsItsOwn is §7's second lane-shape
// residual as a measurement: the parity above is four readers given ONE
// launch shape, and the two card lanes hand them a DIFFERENT one. Under the
// same integration cell and the same program, that shape can REFUSE where
// the direct lane and the credential preview launch — once because the card
// carries no routing var of the integration's, its name being outside the
// carry list (the field answers from its declared default, the anthropic
// pair the list forbids), once because a value a client wrote onto the card
// certifies nothing (rule 2 of §4), and the refusal is the allowlist's
// "readability is compulsory" (§5).
//
// Asserted here rather than fixed, with the bound that makes the residual
// accepted: the catalog's only var dial pins `backend:` alone and the
// allowlist reads the model text, so that shape launches from a card like
// anywhere else — and where the catalog's own model text is a `${…}` dial
// no list can read, both lanes refuse it alike, which is agreement too and
// not this divergence. The day the lanes hand the run the integration's var
// layers, the first leg reds and §7's residual goes with it.
//
// Mutant: publisher.go's launch view back to RouteViewForDispatch (every
// declared var trusted, whoever wrote it) → the client-written leg is
// admitted and reds, which is the forbidden alternative — a forge comment
// certifying the route a run's credentials are chosen from.
func TestRoutingParity_TheCardLaneShapeIsItsOwn(t *testing.T) {
	pol := integrationRoutingPolicy(t, "review")
	templated := compileTestSource(t, templatedRoutingBot)
	cs := &runview.CompiledSource{Hash: "hash"}

	// The control: the direct lane's shape, which the parity test above
	// asserts in full. Re-measured here so the divergence is this test's
	// one subject and cannot be read as a property of the program.
	direct := newParityFixture(t)
	if _, err := direct.pub.SubmitLaunch(direct.ctx(), "run-direct", launchSpecFor(pol), templated, cs); err != nil {
		t.Fatalf("direct lane: SubmitLaunch = %v, want nil — the divergence below must be the SHAPE's, not the program's", err)
	}

	// Leg 1 — the card as ensureBoardCard builds it: the payload carry list,
	// none of the integration's vars. `draft` falls back to its declared
	// default, which is the anthropic pair the integration's list forbids.
	noVars := newParityFixture(t)
	_, err := noVars.pub.SubmitLaunch(noVars.ctx(), "run-card-no-vars", cardLaneSpec(pol, templatedRoutingBot,
		map[string]string{"pr_url": "https://forge.example/o/r/pull/7"}), templated, cs)
	var outside *model.ProviderOutsideError
	if !errors.As(err, &outside) {
		t.Fatalf("card lane, no integration var: SubmitLaunch = %v, want the outside-allowlist refusal — the declared default is the anthropic pair the cell forbids", err)
	}
	if outside.Site != "draft" || outside.Provider != "anthropic" || outside.Level != llmroute.SourceWebhook {
		t.Errorf("refusal = site %q provider %q level %q, want draft/anthropic/%s — the operator reads which node, which provider and whose list",
			outside.Site, outside.Provider, outside.Level, llmroute.SourceWebhook)
	}

	// Leg 2 — the same card with the routing var written onto it (a client's
	// board-task write, or a route whose args var happens to be that name).
	// The value is the one the direct lane launches on, and the gate still
	// refuses: it certifies no name this lane did not pin.
	written := newParityFixture(t)
	_, err = written.pub.SubmitLaunch(written.ctx(), "run-card-written", cardLaneSpec(pol, templatedRoutingBot, parityLaunchVars), templated, cs)
	var unread *model.RouteUnreadableError
	if !errors.As(err, &unread) {
		t.Fatalf("card lane, var written onto the card: SubmitLaunch = %v, want the unreadable-route refusal — a card-carried value leaves the field undecided (§4 rule 2)", err)
	}
	if unread.Site != "draft" || unread.Field != "model" || unread.Hint != "{{vars.revi_model}}" {
		t.Errorf("refusal = site %q field %q hint %q, want draft/model/{{vars.revi_model}} — the refusal names the span the operator has to resolve",
			unread.Site, unread.Field, unread.Hint)
	}

	// The bound that makes the residual acceptable, measured as the
	// AGREEMENT it claims rather than as one green launch: the catalog's only
	// var dial is a `backend:`, the allowlist reads the model text, so the
	// THREE readings that dial can get — the declared `claude_code` a card
	// lane falls back to, the undecided a card-written value leaves (rule 2
	// of §4), and the `claw` the direct lane pins operator-decided — admit
	// the same program. Each reading is asserted through the view the gate
	// itself builds, because legs reading the SAME value would measure one
	// lane several times and say nothing about the divergence.
	//
	// This is also §7's "changes the harness a carded run uses, not the
	// vocabulary it may spend" as a measurement: the dial moves
	// `llm_backend` three ways and moves no verdict.
	boundWF := compileTestSource(t, backendOnlyTemplatedBot)
	directDial := launchSpecFor(pol)
	directDial.Source = backendOnlyTemplatedBot
	directDial.Vars, directDial.TrustedVarNames = map[string]string{"llm_backend": "claw"}, []string{"llm_backend"}
	for _, leg := range []struct {
		runID, name string
		read        any
		spec        runview.LaunchSpec
	}{
		{"run-card-backend-dial", "card lane, no routing var — the declared default", "claude_code",
			cardLaneSpec(pol, backendOnlyTemplatedBot, map[string]string{"pr_url": "https://forge.example/o/r/pull/7"})},
		{"run-card-backend-written", "card lane, the var written onto the card — undecided", nil,
			cardLaneSpec(pol, backendOnlyTemplatedBot, map[string]string{"llm_backend": "claw"})},
		{"run-direct-backend-dial", "direct lane, the operator's llm_backend", "claw", directDial},
	} {
		view := model.RouteViewForLaunch(boundWF, buildModelOverrides(leg.spec.ModelOverrides), nil, leg.spec.Vars, leg.spec.TrustedVarNames)
		if got := view.DecidedVars()["llm_backend"]; got != leg.read {
			t.Errorf("%s: the gate's view reads llm_backend = %v, want %v — the legs must read DIFFERENT values or the bound measures one lane twice", leg.name, got, leg.read)
		}
		bound := newParityFixture(t)
		if _, err := bound.pub.SubmitLaunch(bound.ctx(), leg.runID, leg.spec, boundWF, cs); err != nil {
			t.Errorf("%s: SubmitLaunch = %v, want nil — a `backend:` dial costs its slot nothing, which is why §7 names the residual instead of closing it", leg.name, err)
		}
	}
}
