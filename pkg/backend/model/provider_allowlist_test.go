// The walk's half of the tenant's provider allowlist (ADR-129 §5): a
// non-empty RouteView.AllowedProviders prunes rescue routes resolving
// outside the list, refuses-for-the-gate a primary with no allowed route
// left (ForbiddenPrimary), and records every route it cannot read
// (Unreadable) — the launch that cannot be checked must not pass.
// Falsified by mutation: pruning disabled → Providers keeps the rescue
// provider → W1 reds on its Providers assertion; the primary refusal
// disabled → ForbiddenPrimary stays empty → W1's refusal half reds;
// readability disabled → Unreadable stays empty → W2 reds on its own
// assertion.

package model

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// gatewayPrimaryZaiRescue is witness 1's workflow: a gateway (openai_compatible)
// primary and a `provider: zai` rescue — the shape the tenancy statement is
// about. The rescue is PRUNED, the launch is NOT refused for it, and no
// credential is sealed for z.ai (witness 3).
var gatewayPrimaryZaiRescue = &ir.Workflow{Nodes: map[string]ir.Node{
	"draft": &ir.AgentNode{
		BaseNode: ir.BaseNode{ID: "draft"},
		LLMFields: ir.LLMFields{
			Backend: "claw",
			Model:   "openai_compatible/gw/m",
		},
		Fallbacks: []ir.Fallback{{Name: "zai", Provider: "zai"}},
	},
}}

func TestEffectiveProviders_AllowlistPrunesARescue(t *testing.T) {
	res := EffectiveProviders(RouteView{Workflow: gatewayPrimaryZaiRescue, AllowedProviders: []string{"openai_compatible"}}, knownForTest)
	if slices.Contains(res.Providers, "zai") {
		t.Errorf("Providers = %v, want zai pruned — a rescue outside the allowlist is not this tenant's spend", res.Providers)
	}
	if !slices.Contains(res.Forbidden["zai"], "draft#zai") {
		t.Errorf("Forbidden = %v, want zai at draft#zai — the refusal surface names the site", res.Forbidden)
	}
	if len(res.ForbiddenPrimary) != 0 {
		t.Errorf("ForbiddenPrimary = %v, want empty — the gateway primary served the node, nothing refuses", res.ForbiddenPrimary)
	}
	if !res.NarrowSafe {
		t.Errorf("NarrowSafe = false, want true — a decided (pruned) route widens nothing")
	}
	if !res.OnlyEnvFunded() {
		t.Errorf("OnlyEnvFunded = false, want true — with the rescue pruned the run rides the gateway alone; the fill must seal nothing")
	}
	if !slices.Contains(res.EnvFunded, "openai_compatible") {
		t.Errorf("EnvFunded = %v, want openai_compatible", res.EnvFunded)
	}
	// The class twin: without an allowlist the same program records the
	// rescue provider as spendable — the prune is the allowlist's doing,
	// not the walk's default posture.
	noList := EffectiveProviders(RouteView{Workflow: gatewayPrimaryZaiRescue}, knownForTest)
	if !slices.Contains(noList.Providers, "zai") {
		t.Errorf("no-allowlist Providers = %v, want zai — the prune is the allowlist's doing, not the walk's default", noList.Providers)
	}
}

// Witness 1's refusal half: a primary pinned OUTSIDE the list with no
// allowed route left is ForbiddenPrimary — the publisher's launch refusal
// (refused, never re-routed). A multi-hint chain that still holds an
// allowed step is not: the node stays servable inside the list, and only
// the outside steps are pruned.
func TestEffectiveProviders_AllowlistRefusesAnUnservablePrimary(t *testing.T) {
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"draft": agentWith("draft", "anthropic", "anthropic/claude-opus-5"),
	}}
	res := EffectiveProviders(RouteView{Workflow: wf, AllowedProviders: []string{"openai"}}, knownForTest)
	if !slices.Contains(res.ForbiddenPrimary["anthropic"], "draft") {
		t.Errorf("ForbiddenPrimary = %v, want anthropic at draft — no allowed route serves the node", res.ForbiddenPrimary)
	}
	if slices.Contains(res.Providers, "anthropic") {
		t.Errorf("Providers = %v, want anthropic out — a refused route is not spendable either", res.Providers)
	}

	// A mixed chain keeps the node servable: the allowed step records, the
	// outside step is pruned and named, nothing refuses.
	wf2 := &ir.Workflow{Nodes: map[string]ir.Node{
		"mixed": agentWith("mixed", "anthropic,zai", ""),
	}}
	res2 := EffectiveProviders(RouteView{Workflow: wf2, AllowedProviders: []string{"anthropic"}}, knownForTest)
	if !slices.Contains(res2.Providers, "anthropic") || slices.Contains(res2.Providers, "zai") {
		t.Errorf("Providers = %v, want exactly the chain's allowed step (anthropic)", res2.Providers)
	}
	if !slices.Contains(res2.Forbidden["zai"], "mixed") {
		t.Errorf("Forbidden = %v, want the chain's zai step named at mixed", res2.Forbidden)
	}
	if _, ok := res2.ForbiddenPrimary["zai"]; ok {
		t.Errorf("ForbiddenPrimary = %v, want zai absent — the chain's anthropic step serves the node", res2.ForbiddenPrimary)
	}
}

// The gateway twin of the mixed-chain contract above: a primary chain
// holding an ALLOWED openai_compatible step is servable inside the list —
// the outside steps prune, nothing refuses — whichever order the hints
// come in. Mutant: the allowed gateway step stops marking the route
// servable → ForbiddenPrimary returns → this reds.
func TestEffectiveProviders_GatewayAllowedStepKeepsAServablePrimary(t *testing.T) {
	for _, chain := range []string{"openai_compatible,zai", "zai,openai_compatible"} {
		wf := &ir.Workflow{Nodes: map[string]ir.Node{
			"draft": agentWith("draft", chain, "openai_compatible/gw/m"),
		}}
		res := EffectiveProviders(RouteView{Workflow: wf, AllowedProviders: []string{"openai_compatible"}}, knownForTest)
		if len(res.ForbiddenPrimary) != 0 {
			t.Errorf("chain %q: ForbiddenPrimary = %v — want empty: the chain's openai_compatible step serves the node inside the list", chain, res.ForbiddenPrimary)
		}
		if len(res.Unreadable) != 0 {
			t.Errorf("chain %q: Unreadable = %v — want empty", chain, res.Unreadable)
		}
		if site, prov, ok := FirstForbiddenPrimary(res); ok {
			t.Errorf("chain %q: a refusal would fire at %s for %s — the node is servable, the gate must not refuse", chain, site, prov)
		}
	}

	// The dispatch half reads the same answer: the chain keeps its allowed
	// step, and only it.
	e := &ClawExecutor{logger: iterlog.Nop(), staticProgramOnly: true, allowedProviders: AllowedProviderSet([]string{"openai_compatible"})}
	node := agentWith("draft", "openai_compatible,zai", "openai_compatible/gw/m")
	chain := e.resolveChain(node)
	if len(chain) != 1 || chain[0].Provider != "openai_compatible" {
		t.Errorf("dispatch chain = %+v, want the openai_compatible step alone (servable)", chain)
	}
}

// Witness 2's walk half: a templated route the vars cannot answer is
// recorded Unreadable, naming the site and the field — under an allowlist
// the publisher refuses on it (a lock that cannot be checked must not
// pass). Without an allowlist the same route only widens, as before.
func TestEffectiveProviders_AllowlistFindsUnreadableRoutes(t *testing.T) {
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"draft": agentWith("draft", "anthropic", "anthropic/claude-opus-5"),
	}}
	wf.Nodes["draft"].(*ir.AgentNode).Fallbacks = []ir.Fallback{{Name: "fb", Provider: "{{vars.fb}}"}}
	res := EffectiveProviders(RouteView{Workflow: wf, AllowedProviders: []string{"anthropic"}}, knownForTest)
	found := false
	for _, u := range res.Unreadable {
		if u.Site == "draft#fb" && u.Field == "provider" && u.Hint == "{{vars.fb}}" {
			found = true
		}
	}
	if !found {
		t.Errorf("Unreadable = %+v, want the templated rescue at draft#fb with its field and spelling", res.Unreadable)
	}
	if !slices.Contains(res.Providers, "anthropic") {
		t.Errorf("Providers = %v, want anthropic — the primary is inside and stays", res.Providers)
	}
	noList := EffectiveProviders(RouteView{Workflow: wf}, knownForTest)
	if len(noList.Unreadable) != 0 {
		t.Errorf("no-allowlist Unreadable = %+v, want empty — the obligation is the allowlist's", noList.Unreadable)
	}
}

// The dispatch half (ADR-129 §5): a rescue resolving outside the run's
// allowlist is dropped from the resolved chain — the route burns no retry
// budget — while the primary and the rest of the chain are untouched.
// Mutant: the drop disabled → the zai element returns → this reds.
func TestResolveChain_DropsAForbiddenRescue(t *testing.T) {
	e := &ClawExecutor{logger: iterlog.Nop(), staticProgramOnly: true, allowedProviders: AllowedProviderSet([]string{"openai_compatible"})}
	node := &ir.AgentNode{
		BaseNode: ir.BaseNode{ID: "n"},
		LLMFields: ir.LLMFields{
			Backend:  "claw",
			Model:    "openai_compatible/gw/m",
			Provider: "openai_compatible",
		},
		Fallbacks: []ir.Fallback{{Name: "zai-rescue", Provider: "zai"}},
	}
	chain := e.resolveChain(node)
	if len(chain) != 1 {
		t.Fatalf("chain = %+v, want only the primary — the forbidden rescue must be dropped", chain)
	}
	if chain[0].Provider != "openai_compatible" {
		t.Errorf("primary provider = %q, want openai_compatible — the node's primary is untouched", chain[0].Provider)
	}
	// The class twin: no allowlist, both elements.
	e2 := &ClawExecutor{logger: iterlog.Nop(), staticProgramOnly: true}
	if got := len(e2.resolveChain(node)); got != 2 {
		t.Errorf("no-allowlist chain length = %d, want 2 — the drop is the allowlist's doing", got)
	}
}

// The dispatch refuses a node whose EVERY route resolves outside the
// allowlist — an empty chain is never silently re-filled with a
// defer-to-process element, which would spend a forbidden provider in
// silence. Mutant: the refusal lifted → dispatch walks an auto element →
// this reds.
func TestDispatchRefusesWhenEveryRouteIsForbidden(t *testing.T) {
	e := &ClawExecutor{logger: iterlog.Nop(), staticProgramOnly: true, allowedProviders: AllowedProviderSet([]string{"anthropic"})}
	node := agentWith("n", "zai", "")
	_, err := e.dispatchWithObservability(context.Background(), "n", "claw", "model: node", e.resolveChain(node), "",
		func(context.Context, int, chainElement, string) (string, delegate.Backend, *delegate.Task, error) {
			return "claw", nil, nil, errors.New("stub build: the refusal must fire before any element is built")
		}, &nodeBuildSession{})
	if err == nil {
		t.Fatal("dispatch = nil error, want the empty-chain refusal — an auto element would spend the forbidden provider")
	}
	if !strings.Contains(err.Error(), "allowlist") || !strings.Contains(err.Error(), "refused, never re-routed") {
		t.Errorf("error = %v, want it to name the allowlist and the contract", err)
	}
	// The dispatch half matches the launch half's class: consumers match the
	// tenancy refusal with errors.Is, not by string.
	if !errors.Is(err, ErrProviderOutsideAllowlist) {
		t.Errorf("error = %v, want it to match ErrProviderOutsideAllowlist", err)
	}

	// The choke point beneath the wrappers refuses too: a future direct
	// dispatchChain caller cannot re-open the leak with an empty chain.
	_, err = e.dispatchChain(context.Background(), "n", nil, "",
		func(context.Context, int, chainElement, string) (string, delegate.Backend, *delegate.Task, error) {
			return "claw", nil, nil, errors.New("stub build: the choke point must refuse first")
		})
	if !errors.Is(err, ErrProviderOutsideAllowlist) {
		t.Errorf("dispatchChain on an empty chain = %v, want the tenancy refusal", err)
	}
	// Without the filter an empty chain is still refused — a programming
	// error, never a silent re-fill with a defer-to-process element.
	e2 := &ClawExecutor{logger: iterlog.Nop(), staticProgramOnly: true}
	_, err = e2.dispatchChain(context.Background(), "n", []chainElement{}, "",
		func(context.Context, int, chainElement, string) (string, delegate.Backend, *delegate.Task, error) {
			return "claw", nil, nil, errors.New("stub build: the choke point must refuse first")
		})
	if err == nil || !strings.Contains(err.Error(), "empty dispatch chain") {
		t.Errorf("dispatchChain on an empty chain without an allowlist = %v, want the empty-chain refusal", err)
	}

	// The façade fails CLOSED: resolveProvider returns the refusal, never
	// the defer spelling an empty chain would spell.
	got, err := e.resolveProvider(agentWith("n", "zai", ""))
	if err == nil || !errors.Is(err, ErrProviderOutsideAllowlist) {
		t.Errorf("resolveProvider = (%q, %v), want the tenancy refusal", got, err)
	}
	if got != "" {
		t.Errorf("resolveProvider = %q, want the empty string beside the error", got)
	}
}

// The set the walk and the dispatch compare against is normalized once:
// case-insensitive, trimmed, empty names dropped, and an empty list is no
// allowlist at all.
func TestAllowedProviderSet_Normalizes(t *testing.T) {
	got := AllowedProviderSet([]string{" Anthropic ", "ZAI", "", "  "})
	if !got["anthropic"] || !got["zai"] || len(got) != 2 {
		t.Errorf("set = %v, want {anthropic, zai} — lower-cased, trimmed, empties dropped", got)
	}
	if AllowedProviderSet(nil) != nil || AllowedProviderSet([]string{}) != nil {
		t.Error("an empty list is no allowlist — the walk and the filter stay inert")
	}
}

// A chain element naming no provider ("" — the defer-to-process spelling)
// is not a resolved route: it survives the filter, because dropping it
// would re-shape a chain the gate already certified. What survives is the
// TRUE defer-to-process element: no provider spelling, and a model whose
// own reading (the walk's — providerOfModelSpec) names no provider either.
// An element whose model names an outside provider is a resolved route,
// and the drop takes it (the next test).
func TestResolveChain_KeepsAnUnnamedElement(t *testing.T) {
	e := &ClawExecutor{logger: iterlog.Nop(), staticProgramOnly: true, allowedProviders: AllowedProviderSet([]string{"anthropic"})}
	node := agentWith("n", "anthropic", "anthropic/claude-opus-5")
	node.Fallbacks = []ir.Fallback{{Name: "bare", Model: "claude-opus-5"}}
	chain := e.resolveChain(node)
	if len(chain) != 2 {
		t.Fatalf("chain = %+v, want the primary and the unnamed rescue — a route that names no provider is not a resolved one", chain)
	}
	if chain[1].Provider != "" {
		t.Errorf("second element provider = %q, want \"\" kept", chain[1].Provider)
	}
	// The class twin: no allowlist, the same chain untouched.
	e2 := &ClawExecutor{logger: iterlog.Nop(), staticProgramOnly: true}
	if got := len(e2.resolveChain(node)); got != 2 {
		t.Errorf("no-allowlist chain length = %d, want 2 — the keep is the filter's inert case", got)
	}
}

// The unnamed rescue that a MODEL implies a provider for is a resolved
// route: a GLM id on the anthropic wire spends z.ai's credential whatever
// the provider spelling says (RouteProviderHint mints the same hint at
// build), so the drop takes it — the gate pruned this exact route, and the
// chain must not re-arm it. Mutant: the model-implied reading disabled →
// the rescue stays in the chain and dispatches under hint zai → the
// dispatch half reds on its calls assertion.
func TestResolveChain_DropsAModelImpliedForbiddenRescue(t *testing.T) {
	node := &ir.AgentNode{
		BaseNode: ir.BaseNode{ID: "draft"},
		LLMFields: ir.LLMFields{
			Backend: "claude_code",
			Model:   "anthropic/claude-opus-5",
		},
		Fallbacks: []ir.Fallback{{Name: "rescue", Model: "glm-5.3"}},
	}
	e := &ClawExecutor{logger: iterlog.Nop(), staticProgramOnly: true, allowedProviders: AllowedProviderSet([]string{"anthropic"})}
	chain := e.resolveChain(node)
	if len(chain) != 1 {
		t.Fatalf("chain = %+v, want only the primary — the rescue's GLM model names zai, outside the list", chain)
	}
	if chain[0].Provider != "" {
		t.Errorf("primary provider = %q, want \"\" — the node's own route is untouched by the drop", chain[0].Provider)
	}

	// End to end through the real dispatch site: the primary fails hard and
	// the chain is exhausted — the forbidden rescue never dispatches, and
	// no z.ai hint is ever minted.
	t.Setenv("ZAI_API_KEY", "sk-allowlist-test")
	rec := &fallbackRecorder{}
	fake := &providerScriptedBackend{fail: map[string]error{
		"": &delegate.ErrModelUnavailable{Provider: "claude_code", Model: "anthropic/claude-opus-5", Detail: "primary down"},
	}}
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, fake)
	e.retry = RetryPolicy{MaxAttempts: 2, BackoffBase: time.Millisecond}
	e.backendRegistry = reg
	e.hooks = rec.hook()
	build := e.newElementBuilder("draft", delegate.BackendClaudeCode, fake,
		func(_ context.Context, _ string) (*delegate.Task, error) {
			return &delegate.Task{NodeID: "draft", Model: "anthropic/claude-opus-5"}, nil
		})
	_, err := e.dispatchWithObservability(context.Background(), "draft", delegate.BackendClaudeCode, "model: node",
		e.resolveChain(node), "anthropic/claude-opus-5", build, &nodeBuildSession{})
	if err == nil {
		t.Fatal("dispatch = nil error, want the chain exhausted after the primary — there is no route left to serve")
	}
	if slices.Contains(fake.calls, "zai") {
		t.Errorf("SPEND LEAK: calls = %v — the pruned rescue dispatched under a z.ai hint", fake.calls)
	}

	// The class twin: no allowlist, the same route dispatches — the drop is
	// the allowlist's doing, not a re-shape of the author's chain.
	e2 := &ClawExecutor{logger: iterlog.Nop(), staticProgramOnly: true,
		retry: RetryPolicy{MaxAttempts: 1, BackoffBase: time.Millisecond}, backendRegistry: reg}
	chain2 := e2.resolveChain(node)
	if len(chain2) != 2 {
		t.Fatalf("no-allowlist chain = %+v, want both elements", chain2)
	}
}

// The readability obligation covers the prefix-less model class its own
// contract names (ProviderResolution.Unreadable's doc, ADR-129 §5): a model
// with no prefix and no GLM family names nothing the walk can read, and
// under an allowlist a lock that cannot be checked must not pass. Mutant:
// the widen tail records nothing → Unreadable stays empty → this reds.
func TestEffectiveProviders_AllowlistFindsAPrefixlessModel(t *testing.T) {
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"draft": agentWith("draft", "", "claude-opus-5"),
	}}
	res := EffectiveProviders(RouteView{Workflow: wf, AllowedProviders: []string{"openai"}}, knownForTest)
	found := false
	for _, u := range res.Unreadable {
		if u.Site == "draft" && u.Field == "model" && u.Hint == "claude-opus-5" {
			found = true
		}
	}
	if !found {
		t.Errorf("Unreadable = %+v, want the prefix-less model at draft with its field and spelling", res.Unreadable)
	}
	noList := EffectiveProviders(RouteView{Workflow: wf}, knownForTest)
	if len(noList.Unreadable) != 0 {
		t.Errorf("no-allowlist Unreadable = %+v, want empty — the obligation is the allowlist's", noList.Unreadable)
	}

	// The class boundary: a bare GLM model is READ, not unreadable — the
	// walk names zai and prunes (or refuses a primary) on the reading.
	glm := &ir.Workflow{Nodes: map[string]ir.Node{
		"draft": agentWith("draft", "", "glm-5.3"),
	}}
	res2 := EffectiveProviders(RouteView{Workflow: glm, AllowedProviders: []string{"anthropic"}}, knownForTest)
	if len(res2.Unreadable) != 0 {
		t.Errorf("GLM-model Unreadable = %+v, want empty — the walk reads the GLM family and names zai", res2.Unreadable)
	}
	if !slices.Contains(res2.ForbiddenPrimary["zai"], "draft") {
		t.Errorf("ForbiddenPrimary = %v, want zai at draft — the reading refuses the primary", res2.ForbiddenPrimary)
	}
}

// The fieldless class (a model-calling node with no LLM fields) has no
// slot to pin and no list that admits it, so the refusal must say THAT:
// its remedies are the node and the list, not the two the general message
// names (pin the slot — no field exists; widen — proven never to suffice).
func TestProviders_FieldlessRefusalNamesTheRealRemedy(t *testing.T) {
	msg := (&RouteUnreadableError{Site: "spawn", Field: fieldlessSite}).Error()
	if !strings.Contains(msg, "no field exposes") {
		t.Errorf("message = %q, want it to name the fieldless class", msg)
	}
	if strings.Contains(msg, "pin the node") || strings.Contains(msg, "widen providers") {
		t.Errorf("message = %q, want it free of the two remedies that cannot work for this class", msg)
	}
	if !strings.Contains(msg, "spawn") {
		t.Errorf("message = %q, want it to name the node", msg)
	}
}

// The structured-output recovery's detector pick is the HOST's word — a
// spend no chain element carried. Under an allowlist a pick the list
// cannot check, or that names a provider outside it, is skipped with the
// reason: the node has already failed, the original error surfaces.
// Mutant: the pick check disabled → the recovery resolves the detected
// vendor model → this reds on the missing skip line.
func TestProviders_DetectorRecoverySkipsAnOutsidePick(t *testing.T) {
	t.Setenv("ZAI_API_KEY", "sk-allowlist-test")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	wf := &ir.Workflow{Nodes: map[string]ir.Node{}}
	e := NewClawExecutor(NewRegistry(), wf, WithLogger(iterlog.Nop()), WithWorkDir(t.TempDir()),
		WithAllowedProviders([]string{"openai_compatible"}))
	spec := e.detectorSuggestedModel()
	if spec == "" {
		t.Skip("no detector suggestion in this environment")
	}
	if providerOfModelSpec(spec) != "zai" {
		t.Fatalf("detector suggested %q (provider %q), want a zai-served spec for this env", spec, providerOfModelSpec(spec))
	}
	var buf bytes.Buffer
	e.logger = iterlog.New(iterlog.LevelDebug, &buf)
	task := &delegate.Task{NodeID: "draft", OutputSchema: []byte(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`)}
	primary := delegate.Result{Output: map[string]any{"text": "hello"}}
	_, _, ok := e.extractStructuredViaClaw(context.Background(), "draft", task, primary, delegate.Result{}, nil, "claude_code")
	if ok {
		t.Fatal("recovery = ok, want the skip — the detected spend is outside the list")
	}
	if !strings.Contains(buf.String(), "outside this run's provider allowlist") {
		t.Errorf("log = %s, want the skip naming the allowlist", buf.String())
	}
	if strings.Contains(buf.String(), "via claw") {
		t.Errorf("log = %s, want no recovery attempt — the vendor call the list forbids must not fire", buf.String())
	}

	// The vocabulary boundary: the same GLM-on-anthropic-wire spec reads as
	// zai under ANY list that does not name zai — [anthropic] does not
	// admit it — and passes a list that does.
	if _, err := e.allowlistedDetectorPick("draft", spec); err == nil {
		t.Error("pick under [openai_compatible] = nil error, want the refusal naming zai")
	}
	e2 := NewClawExecutor(NewRegistry(), wf, WithLogger(iterlog.Nop()), WithAllowedProviders([]string{"anthropic"}))
	if _, err := e2.allowlistedDetectorPick("draft", spec); err == nil {
		t.Error("pick under [anthropic] = nil error, want the refusal — the GLM spec spends z.ai's credential, not anthropic's")
	}
	e3 := NewClawExecutor(NewRegistry(), wf, WithLogger(iterlog.Nop()), WithAllowedProviders([]string{"zai"}))
	if got, err := e3.allowlistedDetectorPick("draft", spec); err != nil || got != spec {
		t.Errorf("pick under [zai] = (%q, %v), want the spec kept", got, err)
	}
}

// The no-detector twin: the host detected no claw provider, and the skip
// says THAT — the operator reads "no provider detected", not a registry
// spec error for an empty pick. Mutant: the empty-pick skip lifted → the
// lane falls into the registry's invalid-spec error → this reds on its
// log assertions.
func TestProviders_RecoverySkipNamesTheNoDetectorCase(t *testing.T) {
	t.Setenv("ZAI_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("OPENAI_API_KEY", "")
	wf := &ir.Workflow{Nodes: map[string]ir.Node{}}
	e := NewClawExecutor(NewRegistry(), wf, WithLogger(iterlog.Nop()), WithWorkDir(t.TempDir()))
	if spec := e.detectorSuggestedModel(); spec != "" {
		t.Skipf("detector suggested %q in this environment — this witness needs an empty pick", spec)
	}
	var buf bytes.Buffer
	e.logger = iterlog.New(iterlog.LevelDebug, &buf)
	task := &delegate.Task{NodeID: "draft", OutputSchema: []byte(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`)}
	primary := delegate.Result{Output: map[string]any{"text": "hello"}}
	_, _, ok := e.extractStructuredViaClaw(context.Background(), "draft", task, primary, delegate.Result{}, nil, "claude_code")
	if ok {
		t.Fatal("recovery = ok, want the skip — nothing was detected to recover with")
	}
	if !strings.Contains(buf.String(), "no claw provider detected") {
		t.Errorf("log = %s, want the skip naming the no-detector case", buf.String())
	}
	if strings.Contains(buf.String(), `resolve ""`) {
		t.Errorf("log = %s, want no registry resolve of the empty pick", buf.String())
	}
}

// The claw build's detector fill is the same host pick: a node with no
// model must not take a spec the list cannot check or forbids. Mutant: the
// fill check disabled → the task builds on the detected vendor model →
// this reds on its error assertion.
func TestProviders_DetectorFillRefusedUnderAnAllowlist(t *testing.T) {
	t.Setenv("ZAI_API_KEY", "sk-allowlist-test")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	node := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "n"}, LLMFields: ir.LLMFields{Backend: "claw"}}
	e := NewClawExecutor(NewRegistry(), &ir.Workflow{Nodes: map[string]ir.Node{"n": node}},
		WithLogger(iterlog.Nop()), WithWorkDir(t.TempDir()), WithAllowedProviders([]string{"openai_compatible"}))
	_, err := e.buildTask(context.Background(), node, backendFields{id: "n"}, map[string]any{}, delegate.BackendClaw, nil)
	if err == nil {
		t.Fatal("buildTask = nil error, want the refusal — the detector's pick spends a provider the list forbids")
	}
	if !errors.Is(err, ErrProviderOutsideAllowlist) {
		t.Errorf("error = %v, want it to match ErrProviderOutsideAllowlist", err)
	}
	if !strings.Contains(err.Error(), "zai") {
		t.Errorf("error = %v, want it to name the provider the pick spends", err)
	}
	// The class twin: a list naming zai admits the pick.
	e2 := NewClawExecutor(NewRegistry(), &ir.Workflow{Nodes: map[string]ir.Node{"n": node}},
		WithLogger(iterlog.Nop()), WithWorkDir(t.TempDir()), WithAllowedProviders([]string{"zai"}))
	task, err := e2.buildTask(context.Background(), node, backendFields{id: "n"}, map[string]any{}, delegate.BackendClaw, nil)
	if err != nil {
		t.Fatalf("buildTask under [zai]: %v", err)
	}
	if task.Model != e2.detectorSuggestedModel() {
		t.Errorf("task model = %q, want the detector's pick %q", task.Model, e2.detectorSuggestedModel())
	}
}

// The walk's wire-default answer respects the lock it serves: a staged
// anthropic-wire slot the allowlist excludes is spent by no route of this
// run (the dispatch filter drops its elements, the seal gate seals nothing
// for it), so it commits no read — any consumer funding or sealing on the
// answer hands over no forbidden credential. Mutant: the commit-loop filter
// dropped → the mixed-gateway route commits the forbidden zai/moonshot/
// anthropic reads → this reds on its empty-reads assertion.
func TestEffectiveProviders_WireReadsHoldNoForbiddenSlot(t *testing.T) {
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"draft": agentWith("draft", "openai_compatible,zai", "openai_compatible/gw/m"),
	}}
	res := EffectiveProviders(RouteView{Workflow: wf, AllowedProviders: []string{"openai_compatible"}}, knownForTest)
	if len(res.AnthropicWireDefaultReads) != 0 {
		t.Errorf("AnthropicWireDefaultReads = %v, want no forbidden slot committed (every slot the route stages is outside the list)", res.AnthropicWireDefaultReads)
	}
	if len(res.Forbidden["zai"]) == 0 {
		t.Errorf("Forbidden = %v, want zai at draft — the answer narrows, the prune is still named", res.Forbidden)
	}
	if !res.NarrowSafe {
		t.Error("NarrowSafe = false, want true — the route stays resolved")
	}

	// The no-list twin: no allowlist, the same route stages as before —
	// the filter is the allowlist's, not the walk's new opinion.
	base := EffectiveProviders(RouteView{Workflow: wf}, knownForTest)
	if want := []string{"anthropic", "moonshot", "zai"}; !slices.Equal(base.AnthropicWireDefaultReads, want) {
		t.Errorf("no-list AnthropicWireDefaultReads = %v, want %v — the baseline is untouched", base.AnthropicWireDefaultReads, want)
	}

	// The keep twin: slots inside the list survive the filter — zai stays,
	// the two outside the list drop.
	both := EffectiveProviders(RouteView{Workflow: wf, AllowedProviders: []string{"openai_compatible", "zai"}}, knownForTest)
	if want := []string{"zai"}; !slices.Equal(both.AnthropicWireDefaultReads, want) {
		t.Errorf("both-allowed AnthropicWireDefaultReads = %v, want %v — the allowed slot survives, the outside ones drop", both.AnthropicWireDefaultReads, want)
	}
}

// The unreadable refusal's remedies are the ones that can work: the field
// is made readable (the prefix spelled, the var declared) or the launch
// goes without the list — widening provably cures no unreadable shape (the
// same route is unreadable under the widest possible list), so the message
// must not suggest it. Mutant: the old text back → this reds on the
// no-widening assertion.
func TestProviders_UnreadableRefusalNamesTheRealRemedy(t *testing.T) {
	generic := (&RouteUnreadableError{Site: "draft", Field: "model", Hint: "claude-opus-4-8"}).Error()
	if strings.Contains(generic, "widen") {
		t.Errorf("generic message = %q, want it free of the remedy that cannot work (widening cures no unreadable shape)", generic)
	}
	if !strings.Contains(generic, "resolve the field") || !strings.Contains(generic, "launch without the providers list") {
		t.Errorf("generic message = %q, want the two real remedies named", generic)
	}
	if !strings.Contains(generic, "draft") || !strings.Contains(generic, "model") {
		t.Errorf("generic message = %q, want the site and field named", generic)
	}
	// The fieldless branch names the same escape hatch (its class witness
	// pins its own text): one refusal, one real way out of the lock.
	fieldless := (&RouteUnreadableError{Site: "spawn", Field: fieldlessSite}).Error()
	if !strings.Contains(fieldless, "launch without the providers list") {
		t.Errorf("fieldless message = %q, want the same escape hatch the generic branch names", fieldless)
	}
}
