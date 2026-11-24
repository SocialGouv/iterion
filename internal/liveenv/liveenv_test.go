package liveenv

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
	"github.com/SocialGouv/iterion/pkg/backend/detect"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// env builds a getenv over a literal map so a case states exactly the
// environment it describes — no process state, no ordering.
func env(pairs map[string]string) func(string) string {
	return func(k string) string { return pairs[k] }
}

// A knob set re-targets EVERY LLM node, whatever its kind: the "*"
// selector is the whole point — a live re-prove re-points the bot, and
// pkg/backend/model documents "*" as outranking the node's DSL model:.
func TestTheModelKnobRetargetsEveryLLMNodeKind(t *testing.T) {
	o, err := Read(env(map[string]string{
		EnvModel:   "openai_compatible/gpt-oss-120b",
		EnvBackend: "claw",
	}))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	mo := o.ModelOverrides()
	if mo.Empty() {
		t.Fatal("the knobs produced no override — newLiveExecutor would pass an empty set and the run would keep the fixture's pinned model")
	}
	for _, kind := range []ir.NodeKind{ir.NodeAgent, ir.NodeJudge, ir.NodeRouter} {
		got := mo.ForNode("any_node_id", kind)
		if got.Model != "openai_compatible/gpt-oss-120b" {
			t.Errorf("%s node resolved model %q, want the knob's spec", kind, got.Model)
		}
		if got.Backend != "claw" {
			t.Errorf("%s node resolved backend %q, want the knob's backend", kind, got.Backend)
		}
	}
	// The selector is the run-level default, so it must also be the rule a
	// launch layer can stack over: MergeOver keeps the knob on top.
	rows := mo.Rows()
	if len(rows) != 2 {
		t.Fatalf("Rows() = %v, want one model and one backend directive", rows)
	}
	for _, r := range rows {
		if r.Selector != "*" {
			t.Errorf("directive %+v is not written under the \"*\" selector — a narrower selector would leave some LLM nodes on the fixture's pinned model", r)
		}
	}
}

// No knob = no override: the zero value must not re-target anything, or
// every unknobbed live run would silently change route.
func TestNoKnobRetargetsNothing(t *testing.T) {
	o, err := Read(env(nil))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !o.Empty() {
		t.Fatalf("empty environment produced %+v, want the zero override", o)
	}
	if !o.ModelOverrides().Empty() {
		t.Fatal("empty environment produced a non-empty override set")
	}
	if got := o.BudgetOver(ir.BudgetOverrides{MaxCostUSD: 100}); got.MaxCostUSD != 100 {
		t.Fatalf("an unknobbed run changed the target's own cap to %v, want 100", got.MaxCostUSD)
	}
}

// The authorised per-target cap must reach the run's budget and LOWER the
// bot's shipped cap. The raise-only steering path cannot do this, which is
// the trap this witness pins: secured-renovacy ships $100 against an
// authorised $20.
func TestTheAuthorisedCapLowersTheShippedBudget(t *testing.T) {
	o, err := Read(env(map[string]string{EnvMaxCostUSD: "20"}))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	wf := &ir.Workflow{Budget: &ir.Budget{MaxCostUSD: 100}}
	ir.ApplyBudgetOverrides(wf, o.BudgetOver(ir.BudgetOverrides{}))
	if wf.Budget.MaxCostUSD != 20 {
		t.Fatalf("run budget max_cost_usd = %v, want the authorised 20 — the shipped 100 survived the override", wf.Budget.MaxCostUSD)
	}
}

// The env cap is the operator's launch gesture, so it outranks the
// target's own override; without the env cap the target's value stands.
func TestTheEnvCapOutranksTheTargetsOwnOverride(t *testing.T) {
	o, err := Read(env(map[string]string{EnvMaxCostUSD: "20"}))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got := o.BudgetOver(ir.BudgetOverrides{MaxCostUSD: 80}).MaxCostUSD; got != 20 {
		t.Fatalf("composed cap %v, want the operator's 20", got)
	}
	bare, err := Read(env(nil))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got := bare.BudgetOver(ir.BudgetOverrides{MaxCostUSD: 80}).MaxCostUSD; got != 80 {
		t.Fatalf("composed cap %v, want the target's own 80 when no env cap is set", got)
	}
}

// The ceiling moves in BOTH directions. Raising is not a bug to clamp: the
// knob is the operator's launch gesture with the precedence `iterion run
// --max-cost-usd` already has, and clamping 120 back to the bot's shipped 5
// would silently replace an explicit choice. This case exists so a future
// clamp cannot land quietly — it would redden here, with the reason.
func TestTheEnvCeilingRaisesTheShippedBudgetToo(t *testing.T) {
	o, err := Read(env(map[string]string{EnvMaxCostUSD: "120"}))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	wf := &ir.Workflow{Budget: &ir.Budget{MaxCostUSD: 5}}
	ir.ApplyBudgetOverrides(wf, o.BudgetOver(ir.BudgetOverrides{}))
	if wf.Budget.MaxCostUSD != 120 {
		t.Fatalf("run budget max_cost_usd = %v, want the operator's 120 — a clamp to the shipped 5 replaced an explicit launch choice", wf.Budget.MaxCostUSD)
	}
	// The compose path too: the knob raises a target's own SMALLER
	// override, which is the one place inside this package where a clamp
	// could land.
	if got := o.BudgetOver(ir.BudgetOverrides{MaxCostUSD: 80}).MaxCostUSD; got != 120 {
		t.Fatalf("composed ceiling %v, want the operator's 120 over the target's own 80 — a clamp inside BudgetOver replaced an explicit launch choice", got)
	}
}

// A malformed cap is an error, never a silent zero: a cap the harness
// dropped is a cap the operator believes is in force.
func TestAMalformedCapIsAnError(t *testing.T) {
	for _, raw := range []string{"twenty", "20usd", "0", "-5"} {
		if _, err := Read(env(map[string]string{EnvMaxCostUSD: raw})); err == nil {
			t.Errorf("%s=%q was accepted — a dropped cap reads as 'uncapped'", EnvMaxCostUSD, raw)
		}
	}
}

// The ledger is committed, so a key pasted into the funded_by knob must be
// refused — and the refusal must not echo the value.
func TestACredentialShapedFunderIsRefusedWithoutEchoingIt(t *testing.T) {
	secret := "sk-ant-" + strings.Repeat("x", 80)
	_, err := Read(env(map[string]string{EnvFundedBy: secret}))
	if err == nil {
		t.Fatal("a credential-shaped funded_by was accepted — it would land in the committed ledger")
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), strings.Repeat("x", 8)) {
		t.Fatalf("the refusal echoes the value it refused: %v", err)
	}
	if _, err := Read(env(map[string]string{EnvFundedBy: strings.Repeat("a", maxFundedByLen+1)})); err == nil {
		t.Fatalf("a %d-character funder was accepted past the %d-character bound", maxFundedByLen+1, maxFundedByLen)
	}
	if _, err := Read(env(map[string]string{EnvFundedBy: "pliage-team-key"})); err != nil {
		t.Fatalf("a plain route name was refused: %v", err)
	}
	// The shape rule, past the prefix list: a value that is not shaped
	// like a name is refused whatever it starts with.
	for _, bad := range []string{"Bearer abc", "abc def", "-leading-dash", "pad==", "a+b", "tok\nen"} {
		if _, err := Read(env(map[string]string{EnvFundedBy: bad})); err == nil {
			t.Errorf("%s=%q was accepted — not shaped like a route name", EnvFundedBy, bad)
		}
	}
	// Honest scope, pinned so the doc and the code cannot drift apart: a
	// short name-shaped secret PASSES. The guard bounds the accident; what
	// keeps funded_by safe is that it is a name the operator chooses.
	uuid := "2f1c9b84-7a3e-4d52-9c61-0b8e5d4a7f33"
	if _, err := Read(env(map[string]string{EnvFundedBy: uuid})); err != nil {
		t.Fatalf("the shape rule is wider than documented — it refused a bare UUID (%v), so the doc's \"catches the accident, not a determined paste\" is wrong in the other direction", err)
	}
}

// The guard chokepoint: an overridden run must not skip on a vendor key it
// no longer reaches, and must still skip on the key its own route needs.
func TestTheOverrideSupersedesOnlyTheVendorsItRoutesAwayFrom(t *testing.T) {
	cases := []struct {
		name       string
		knob       string
		credential string
		want       bool
	}{
		{"gateway run does not need the anthropic key", "openai_compatible/gpt-oss-120b", "ANTHROPIC_API_KEY", true},
		{"gateway run does not need the openai key", "openai_compatible/gpt-oss-120b", "OPENAI_API_KEY", true},
		{"gateway run still needs the gateway key", "openai_compatible/gpt-oss-120b", "OPENAI_COMPATIBLE_API_KEY", false},
		{"an override onto anthropic still needs the anthropic key", "anthropic/claude-sonnet-4-6", "ANTHROPIC_API_KEY", false},
		{"an override onto anthropic does not need the openai key", "anthropic/claude-sonnet-4-6", "OPENAI_API_KEY", true},
		{"a bare model id supersedes nothing — the route is a guess", "gpt-oss-120b", "ANTHROPIC_API_KEY", false},
		{"a non-route prefix is a model id, not a route", "scaleway/gpt-oss-120b", "ANTHROPIC_API_KEY", false},
		{"a vendor-shaped id prefix is not a route either", "meta-llama/Llama-3.3-70B", "OPENAI_API_KEY", false},
		{"no override supersedes nothing", "", "ANTHROPIC_API_KEY", false},
		{"an unmapped credential supersedes nothing", "openai_compatible/gpt-oss-120b", "SOME_OTHER_KEY", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o, err := Read(env(map[string]string{EnvModel: c.knob}))
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if got := o.SupersedesCredentialEnv(c.credential); got != c.want {
				t.Fatalf("SupersedesCredentialEnv(%q) with %s=%q = %v, want %v", c.credential, EnvModel, c.knob, got, c.want)
			}
		})
	}
}

// funded_by names a ROUTE the harness can attribute, or nothing. It never
// guesses, and the operator's explicit name is never replaced.
func TestFundedByNamesARouteOrNothing(t *testing.T) {
	bare, err := Read(env(nil))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	// The WHOLE environment is empty here: no gateway key is set, so
	// naming one would be a guess written into a committed receipt. A
	// gateway can be reached by network auth or through the DB credential
	// cascade with no env key at all.
	if got := bare.FundedByFor([]string{"openai_compatible/gpt-oss-120b"}); got != "" {
		t.Fatalf("gateway run funded_by = %q with OPENAI_COMPATIBLE_API_KEY unset — a derived funder must name a variable that IS set", got)
	}
	// Set it, and the same run names it: the derivation is a reading of
	// the environment, not a lookup in the route table.
	funded, err := Read(env(map[string]string{"OPENAI_COMPATIBLE_API_KEY": "x"}))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got := funded.FundedByFor([]string{"openai_compatible/gpt-oss-120b"}); got != "OPENAI_COMPATIBLE_API_KEY" {
		t.Fatalf("gateway run funded_by = %q with the key set, want the gateway credential source", got)
	}
	// A vendor route has two possible pockets (a metered key or a CLI
	// forfait), so the harness derives nothing rather than guessing.
	for _, spec := range []string{"anthropic/claude-sonnet-4-6", "openai/gpt-5.5", "gpt-oss-120b", ""} {
		if got := bare.FundedByFor([]string{spec}); got != "" {
			t.Errorf("spec %q derived funded_by %q — the pocket depends on the backend, so this is a guess", spec, got)
		}
	}
	// Two attributable routes fold to a SORTED list, so the column cannot
	// depend on the order the steps finished in.
	named, err := Read(env(map[string]string{EnvFundedBy: "pliage-team-key"}))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got := named.FundedByFor([]string{"anthropic/claude-sonnet-4-6"}); got != "pliage-team-key" {
		t.Fatalf("funded_by = %q, want the operator's explicit name — it is never silently replaced", got)
	}
	if got := named.FundedByFor(nil); got != "pliage-team-key" {
		t.Fatalf("funded_by = %q for a run with no LLM spec, want the operator's explicit name", got)
	}
}

// The folded list is order-independent: both orders of the same set
// produce the same column, so a reader cannot reconstruct a different
// fact from the order concurrent steps happened to finish in.
func TestTheFundedByFoldIsOrderIndependent(t *testing.T) {
	o, err := Read(env(map[string]string{"OPENAI_COMPATIBLE_API_KEY": "x"}))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	a := o.FundedByFor([]string{"openai_compatible/gpt-oss-120b", "anthropic/claude-sonnet-4-6"})
	b := o.FundedByFor([]string{"anthropic/claude-sonnet-4-6", "openai_compatible/gpt-oss-120b"})
	if a != b {
		t.Fatalf("the fold depends on the order: %q vs %q", a, b)
	}
	if a != "OPENAI_COMPATIBLE_API_KEY" {
		t.Fatalf("folded funded_by = %q, want only the attributable route", a)
	}
	// The fold itself, driven past one element: the join is keyed on a
	// SORT, not on map iteration order, so two runs of the same set read
	// the same column.
	set := map[string]bool{"Z_API_KEY": true, "A_API_KEY": true, "M_API_KEY": true}
	if got := joinSorted(set); got != "A_API_KEY,M_API_KEY,Z_API_KEY" {
		t.Fatalf("joinSorted = %q, want the sorted list — map iteration order is not a stable key", got)
	}
	if got := joinSorted(nil); got != "" {
		t.Fatalf("joinSorted(nil) = %q, want the empty column", got)
	}
}

// Every credential env var this package maps must name a route
// modelroute recognises, or the guard chokepoint silently stops
// superseding anything for it.
func TestEveryMappedCredentialNamesAKnownRoute(t *testing.T) {
	if len(credentialEnvRoutes) < 5 {
		t.Fatalf("only %d credential(s) mapped — an empty table would make the guard chokepoint vacuous", len(credentialEnvRoutes))
	}
	for env, route := range credentialEnvRoutes {
		if route == "" {
			t.Errorf("%s maps to an empty route", env)
		}
		if !strings.HasSuffix(env, "_API_KEY") {
			t.Errorf("%s is not a credential env var name", env)
		}
	}
	for route := range soleFundedRoutes {
		if len(credentialEnvsForRoute(route)) == 0 {
			t.Errorf("route %q is declared solely funded but no credential env var maps to it, so funded_by would read empty for every run it serves", route)
		}
	}
}

// The reverse lookup returns EVERY env var mapping to a route, sorted — not
// the first one Go's map range happened to yield. A "first match" verdict
// is a different receipt per run the day a second variable funds one route.
func TestTheReverseCredentialLookupIsAStableKey(t *testing.T) {
	if got := credentialEnvsForRoute("nothing-maps-here"); len(got) != 0 {
		t.Fatalf("credentialEnvsForRoute(unknown) = %v, want none", got)
	}
	got := credentialEnvsForRoute(credentialEnvRoutes["OPENAI_COMPATIBLE_API_KEY"])
	if len(got) != 1 || got[0] != "OPENAI_COMPATIBLE_API_KEY" {
		t.Fatalf("credentialEnvsForRoute = %v, want exactly the gateway credential", got)
	}
	// Two names on one route: the lookup must report BOTH, sorted, so the
	// column cannot depend on the iteration order.
	route := "probe-route"
	credentialEnvRoutes["Z_PROBE_API_KEY"] = route
	credentialEnvRoutes["A_PROBE_API_KEY"] = route
	t.Cleanup(func() {
		delete(credentialEnvRoutes, "Z_PROBE_API_KEY")
		delete(credentialEnvRoutes, "A_PROBE_API_KEY")
	})
	// Asked many times: a lookup that reported the map's own order would
	// yield both orders across these calls, so one pass could not witness
	// the sort.
	for i := 0; i < 256; i++ {
		if got := credentialEnvsForRoute(route); len(got) != 2 || got[0] != "A_PROBE_API_KEY" || got[1] != "Z_PROBE_API_KEY" {
			t.Fatalf("credentialEnvsForRoute(%q) = %v on call %d, want both names sorted on every call", route, got, i+1)
		}
	}
}

// The knobs are the operator's shell gesture: every name this package reads
// must be in envtrust's operator-grade inventory, which the harness's own
// dotenv loader and the engine's loaders refuse from a project file. Pinned
// here so a fifth knob cannot arrive without its classification — and so a
// .env line cannot wave a guard, move a ceiling or name a funder.
func TestEveryLaunchKnobIsOperatorGrade(t *testing.T) {
	knobs := LaunchKnobs()
	if len(knobs) < 4 {
		t.Fatalf("LaunchKnobs() = %v — the list lost a knob, and a shorter list makes this pin vacuous", knobs)
	}
	for _, name := range knobs {
		if !envtrust.OperatorGradeRefused(name) {
			t.Errorf("%s is read by the live harness but a project .env may plant it (envtrust does not refuse it) — a file could wave the credential guards, move the spend ceiling or name the funder of a committed receipt", name)
		}
	}
	// The credential the file is FOR stays plantable: the rule is on the
	// engine's own names, never on a vendor key.
	if envtrust.OperatorGradeRefused("OPENAI_COMPATIBLE_API_KEY") {
		t.Fatal("a vendor credential is refused from .env — the file exists to carry it")
	}
}

// fundingReport is a detect.Report stating one machine: anthropic and the
// gateway cannot be funded here, openai can; claude_code cannot be served,
// codex can. Built by hand so each case states the machine it judges.
func fundingReport() detect.Report {
	return detect.Report{
		Providers: []detect.ProviderStatus{
			{Name: "anthropic", Available: false},
			{Name: "openai", Available: true, Source: "OPENAI_API_KEY"},
			{Name: "openai_compatible", Available: false},
		},
		Backends: []detect.BackendStatus{
			{Name: detect.BackendClaw, Available: true},
			{Name: detect.BackendClaudeCode, Available: false},
			{Name: detect.BackendCodex, Available: true},
		},
	}
}

// A re-target is what serves the run once it waves a vendor guard, so ITS
// funding is the prerequisite: a re-target onto a route this machine cannot
// fund is named (and the harness skips with that reason) instead of letting
// the run reach a provider it cannot pay and recording a `fail`.
func TestARetargetThisMachineCannotFundIsNamed(t *testing.T) {
	cases := []struct {
		name  string
		knobs map[string]string
		// wantIn lists substrings the reason must carry; nil = fundable.
		wantIn []string
	}{
		{"onto a vendor whose key is absent", map[string]string{EnvModel: "anthropic/claude-haiku-4-5-20251001"},
			[]string{EnvModel, `"anthropic"`, "ANTHROPIC_API_KEY"}},
		{"onto the gateway with no gateway configured", map[string]string{EnvModel: "openai_compatible/gpt-oss-120b"},
			[]string{`"openai_compatible"`, "OPENAI_COMPATIBLE_API_KEY", "OPENAI_COMPATIBLE_BASE_URL"}},
		{"onto claw explicitly, still the route's provider decides", map[string]string{EnvModel: "anthropic/claude-haiku-4-5-20251001", EnvBackend: "claw"},
			[]string{`"anthropic"`}},
		{"onto a CLI backend this machine cannot serve", map[string]string{EnvBackend: "claude_code"},
			[]string{EnvBackend, "claude_code"}},
		{"onto a backend detection does not report", map[string]string{EnvBackend: "no_such_backend"},
			[]string{"no_such_backend", "does not report"}},
		{"onto a routed provider detection does not report", map[string]string{EnvModel: "xai/grok-3"},
			[]string{`"xai"`, "does not report"}},
		{"onto a vendor this machine can fund", map[string]string{EnvModel: "openai/gpt-5.5"}, nil},
		{"onto a CLI backend this machine can serve", map[string]string{EnvBackend: "codex"}, nil},
		{"a bare id names no route: nothing is decided, and no guard was waved for it", map[string]string{EnvModel: "gpt-oss-120b"}, nil},
		{"a non-route prefix is a model id", map[string]string{EnvModel: "scaleway/gpt-oss-120b"}, nil},
		{"no re-target, nothing to fund", map[string]string{EnvMaxCostUSD: "3", EnvFundedBy: "pliage-team-key"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o, err := Read(env(c.knobs))
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			got := o.UnfundedRetarget(fundingReport())
			if c.wantIn == nil {
				if got != "" {
					t.Fatalf("UnfundedRetarget = %q, want \"\" — this re-target is fundable here (or undecidable), and refusing it would skip a run that can be paid", got)
				}
				return
			}
			if got == "" {
				t.Fatalf("UnfundedRetarget = \"\" for %v — the re-target cannot be funded on this machine, and waving the guard onto it records a `fail` for a run that never had a chance", c.knobs)
			}
			for _, want := range c.wantIn {
				if !strings.Contains(got, want) {
					t.Errorf("reason %q does not name %q", got, want)
				}
			}
		})
	}
}

// RunKnobs names exactly the knobs a path with no override seam cannot
// honour — the re-target pair and the ceiling — in a stable order, and never
// the funder, which every recording path hands to the row.
func TestRunKnobsNameWhatActsOnTheRun(t *testing.T) {
	cases := []struct {
		knobs map[string]string
		want  string
	}{
		{map[string]string{EnvModel: "openai_compatible/gpt-oss-120b"}, EnvModel},
		{map[string]string{EnvBackend: "claw"}, EnvBackend},
		{map[string]string{EnvMaxCostUSD: "7"}, EnvMaxCostUSD},
		{map[string]string{EnvFundedBy: "pliage-team-key"}, ""},
		{nil, ""},
		{map[string]string{EnvMaxCostUSD: "7", EnvFundedBy: "pliage-team-key", EnvBackend: "claw", EnvModel: "openai/gpt-5.5"},
			EnvModel + "," + EnvBackend + "," + EnvMaxCostUSD},
	}
	for _, c := range cases {
		o, err := Read(env(c.knobs))
		if err != nil {
			t.Fatalf("Read(%v): %v", c.knobs, err)
		}
		if got := strings.Join(o.RunKnobs(), ","); got != c.want {
			t.Errorf("RunKnobs(%v) = %q, want %q", c.knobs, got, c.want)
		}
		if retargets := o.Model != "" || o.Backend != ""; o.Retargets() != retargets {
			t.Errorf("Retargets(%v) = %v, want %v", c.knobs, o.Retargets(), retargets)
		}
	}
}

// A dotenv file that assigns a launch knob is named by the knob, in every
// shape Task's loader plants, whatever the value — and a line that only
// looks like one (a comment, a longer name, a ledger sibling) is not.
func TestDotEnvKnobsNamesEveryAssignedKnob(t *testing.T) {
	content := strings.Join([]string{
		"OPENAI_API_KEY=value-that-is-not-a-knob",
		"ITERION_LIVE_MODEL=openai_compatible/gpt-oss-120b",
		"export ITERION_LIVE_BACKEND=claw",
		"export\tITERION_LIVE_MAX_COST_USD=999",
		"  ITERION_LIVE_FUNDED_BY : pliage-team-key",
		"# ITERION_LIVE_MODEL=a-comment-assigns-nothing",
		"ITERION_LIVE_MODELX=a-longer-name",
		"ITERION_LIVE_LEDGER_TARGET=test:live:a-ledger-sibling",
		"ITERION_LIVE_MODEL=assigned-twice\r",
	}, "\n")
	got := DotEnvKnobs(content)
	want := []string{EnvBackend, EnvFundedBy, EnvMaxCostUSD, EnvModel}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("DotEnvKnobs = %v, want %v (sorted, each once)", got, want)
	}
	for _, name := range got {
		if strings.ContainsAny(name, "=: /") {
			t.Fatalf("DotEnvKnobs returned %q — a name, never a value", name)
		}
	}
	if got := DotEnvKnobs("OPENAI_API_KEY=x\n# ITERION_LIVE_MODEL=y\nITERION_LIVE_LEDGER_PATH=/tmp/l.json\n"); len(got) != 0 {
		t.Fatalf("DotEnvKnobs = %v for a file that assigns no knob", got)
	}
}
