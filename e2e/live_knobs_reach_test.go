package e2e

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/liveenv"
	"github.com/SocialGouv/iterion/pkg/backend/detect"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/liveledger"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The free witnesses of the launch-knob chokepoint (live_knobs_test.go). They
// drive the very functions a paid live run obtains its run through, at $0:
// a tool-only fixture, a scratch ledger, no credential, no network. Not
// parallel: they set the process environment the knobs are read from.

const (
	probeModel   = "openai_compatible/knob-probe-model"
	probeBackend = "claw"
	probeCap     = 7.0
	probeFunder  = "knob-probe-route"
)

// knobProbeEnv states the whole environment a probe judges: the four launch
// knobs (unset unless named), a scratch ledger the rows go to, and a repo
// .env that does not exist — so an operator's own .env or exported knob can
// neither move a verdict here nor be moved by one. Returns the ledger path.
func knobProbeEnv(t *testing.T, knobs map[string]string) string {
	t.Helper()
	for _, k := range liveenv.LaunchKnobs() {
		t.Setenv(k, knobs[k])
	}
	ledger := filepath.Join(t.TempDir(), "ledger.json")
	t.Setenv(liveledger.LedgerPathEnv, ledger)
	t.Setenv(liveledger.EnvOffline, "")
	t.Setenv(liveledger.EnvTarget, "")
	prev := liveDotEnvPath
	liveDotEnvPath = filepath.Join(t.TempDir(), "absent.env")
	t.Cleanup(func() { liveDotEnvPath = prev })
	return ledger
}

// probeCell runs body as a subtest recording under target, and reports
// whether it skipped — read in a deferred call, which runs while t.Skip's
// Goexit unwinds the subtest.
func probeCell(t *testing.T, name, target string, body func(t *testing.T)) (skipped bool) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		t.Setenv(liveledger.EnvTarget, target)
		defer func() { skipped = t.Skipped() }()
		body(t)
	})
	return skipped
}

// probeRow reads target's row from the scratch ledger: the artifact a paid
// run commits, read back rather than the tracker's state.
func probeRow(t *testing.T, ledger, target string) liveledger.Row {
	t.Helper()
	lg, err := liveledger.Load(ledger)
	if err != nil {
		t.Fatalf("load the scratch ledger: %v", err)
	}
	row, ok := lg.Get(target)
	if !ok {
		t.Fatalf("no row for %s in the scratch ledger — the cell recorded nothing", target)
	}
	return row
}

// engineShape is one way a live target obtains its executor and engine.
type engineShape struct {
	name  string
	build func(t *testing.T, wf *ir.Workflow, s store.RunStore, runID string) (*model.ClawExecutor, *runtime.Engine)
}

func engineShapes() []engineShape {
	quiet := iterlog.New(iterlog.LevelError, io.Discard)
	harness := func(t *testing.T, wf *ir.Workflow, s store.RunStore, runID string) (*model.ClawExecutor, *runtime.Engine) {
		ex := newLiveExecutor(t, wf, s, runID, t.TempDir(), withLiveLogger(quiet))
		return ex, newLiveEngine(t, wf, s, ex, runtime.WithLogger(quiet), runtime.WithWorkDir(scratchRepo(t)))
	}
	return []engineShape{
		{"the standard executor: runBotLive, the _Real tests, secured-renovacy, the direct dual-model and claw tests", harness},
		{"an executor wiring its own registries: vision, claw-tool-coverage, workspace-agents, the claw tests with their own tool sets",
			func(t *testing.T, wf *ir.Workflow, s store.RunStore, runID string) (*model.ClawExecutor, *runtime.Engine) {
				ex := newLiveClawExecutor(t, model.NewRegistry(), wf, model.WithWorkDir(t.TempDir()))
				return ex, newLiveEngine(t, wf, s, ex, runtime.WithLogger(quiet), runtime.WithWorkDir(scratchRepo(t)))
			}},
		{"a second executor and engine on an armed workflow: feat:budget-resume's phase 2",
			func(t *testing.T, wf *ir.Workflow, s store.RunStore, runID string) (*model.ClawExecutor, *runtime.Engine) {
				first, eng := harness(t, wf, s, runID+"-phase1")
				if err := eng.Run(context.Background(), runID+"-phase1", nil); err != nil {
					t.Fatalf("phase 1 run: %v", err)
				}
				_ = first.Close()
				return harness(t, wf, s, runID)
			}},
	}
}

// TestEveryLaunchKnobReachesEveryLiveTargetShape is the table knob × target
// shape behind ITERION_LIVE_MODEL, _BACKEND, _MAX_COST_USD and _FUNDED_BY.
// TestEveryLiveRunIsArmedAtTheKnobChokepoint (pkg/liveledger) proves every
// live target obtains its run through the chokepoint; this proves the
// chokepoint applies each knob to each shape a target obtains its run in —
// on the persisted run and the written row, not on the harness's own state —
// or refuses it, by name, where no seam can carry it.
func TestEveryLaunchKnobReachesEveryLiveTargetShape(t *testing.T) {
	ledger := knobProbeEnv(t, map[string]string{
		liveenv.EnvModel:      probeModel,
		liveenv.EnvBackend:    probeBackend,
		liveenv.EnvMaxCostUSD: fmt.Sprint(probeCap),
		liveenv.EnvFundedBy:   probeFunder,
	})
	for i, shape := range engineShapes() {
		target := fmt.Sprintf("test:live:knob-probe-engine-%d", i)
		skipped := probeCell(t, shape.name, target, func(t *testing.T) {
			tracker := liveledger.Track(t)
			wf := compileFixture(t, "live_knob_probe.bot")
			if wf.Budget == nil || wf.Budget.MaxCostUSD != 1 {
				t.Fatalf("fixture premise: live_knob_probe.bot ships max_cost_usd 1, got %+v", wf.Budget)
			}
			s := tmpStore(t)
			const runID = "knob-probe"
			ex, eng := shape.build(t, wf, s, runID)
			defer ex.Close()

			// Re-target: the executor the run uses carries the "*" rule, so
			// every LLM node kind resolves to the knob's model and backend.
			for _, kind := range []ir.NodeKind{ir.NodeAgent, ir.NodeJudge, ir.NodeRouter} {
				got := ex.ModelOverrides().ForNode("any_llm_node", kind)
				if got.Model != probeModel || got.Backend != probeBackend {
					t.Errorf("%s node resolves model %q backend %q, want the knobs' %q / %q — the re-target never reached this executor", kind, got.Model, got.Backend, probeModel, probeBackend)
				}
			}

			// Ceiling: the RUN the engine persisted carries it, not the
			// fixture's own shipped cap.
			if err := eng.Run(context.Background(), runID, nil); err != nil {
				t.Fatalf("run the tool-only probe: %v", err)
			}
			r, err := s.LoadRun(context.Background(), runID)
			if err != nil {
				t.Fatalf("load the run: %v", err)
			}
			if r.Budget == nil || r.Budget.MaxCostUSD != probeCap {
				t.Errorf("the run's effective budget is %+v, want max_cost_usd %v from %s — the fixture's own cap governed the run", r.Budget, probeCap, liveenv.EnvMaxCostUSD)
			}

			// Funder: what the run records names it.
			ov := liveOverride(t)
			events, err := liveRunEvents(tracker, ov, s, runID)
			if err != nil {
				t.Fatalf("%v", err)
			}
			feedLedgerProvenance(tracker, ov, events)
		})
		if skipped {
			t.Fatalf("%s skipped: a tool-only run has nothing to re-target, and no knob refuses it", shape.name)
		}
		row := probeRow(t, ledger, target)
		if row.Verdict != liveledger.VerdictPass || row.FundedBy != probeFunder || row.ServedModel != "" {
			t.Errorf("%s recorded verdict %q funded_by %q served_model %q, want pass / %q / \"\" (a tool-only run)", shape.name, row.Verdict, row.FundedBy, row.ServedModel, probeFunder)
		}
	}

	// The paths with no seam for the run knobs: each knob that acts on the
	// run is refused by name, the funder is not — it reaches the row there
	// too.
	runKnobs := []struct{ name, value string }{
		{liveenv.EnvModel, probeModel},
		{liveenv.EnvBackend, probeBackend},
		{liveenv.EnvMaxCostUSD, fmt.Sprint(probeCap)},
	}
	for i, path := range []string{"cli.RunScheduleRun", "delegate.ClaudeCodeBackend.Execute"} {
		for j, knob := range runKnobs {
			ledger := knobProbeEnv(t, map[string]string{knob.name: knob.value})
			target := fmt.Sprintf("test:live:knob-probe-seamless-%d-%d", i, j)
			if !probeCell(t, path+" refuses "+knob.name, target, func(t *testing.T) { refuseRetargetWithoutSeam(t, path) }) {
				t.Errorf("%s with %s set did not skip — the run would spend without what the operator set", path, knob.name)
				continue
			}
			if row := probeRow(t, ledger, target); row.Verdict != liveledger.VerdictSkipped || !strings.Contains(row.SkipReason, knob.name) || !strings.Contains(row.SkipReason, path) {
				t.Errorf("%s with %s set recorded %q (%q), want a skipped row naming the knob and the path", path, knob.name, row.Verdict, row.SkipReason)
			}
		}
		ledger = knobProbeEnv(t, map[string]string{liveenv.EnvFundedBy: probeFunder})
		target := fmt.Sprintf("test:live:knob-probe-seamless-%d-funder", i)
		if probeCell(t, path+" lets the funder through", target, func(t *testing.T) {
			tracker := liveledger.Track(t)
			refuseRetargetWithoutSeam(t, path)
			recordLiveProvenance(tracker, liveOverride(t), "", nil)
		}) {
			t.Errorf("%s skipped on %s alone — the funder names who pays, it acts on no run", path, liveenv.EnvFundedBy)
		} else if row := probeRow(t, ledger, target); row.FundedBy != probeFunder {
			t.Errorf("%s recorded funded_by %q, want %q", path, row.FundedBy, probeFunder)
		}
	}

	// A target whose subject is its fixture's own budget refuses the
	// ceiling, and only the ceiling.
	ledger = knobProbeEnv(t, map[string]string{liveenv.EnvMaxCostUSD: fmt.Sprint(probeCap)})
	if !probeCell(t, "a budget subject refuses the ceiling", "test:live:knob-probe-budget-cap", refuseCapOnBudgetSubject) {
		t.Error("a budget-subject target ran under the ceiling, which replaces the very budget it asserts on")
	} else if row := probeRow(t, ledger, "test:live:knob-probe-budget-cap"); !strings.Contains(row.SkipReason, liveenv.EnvMaxCostUSD) {
		t.Errorf("budget-subject skip reason %q does not name %s", row.SkipReason, liveenv.EnvMaxCostUSD)
	}
	knobProbeEnv(t, map[string]string{liveenv.EnvModel: probeModel, liveenv.EnvBackend: probeBackend})
	if probeCell(t, "a budget subject keeps the re-target", "test:live:knob-probe-budget-retarget", refuseCapOnBudgetSubject) {
		t.Error("a budget-subject target refused a re-target — re-proving a budget firing on another route proves the engine")
	}

	// A subagent runner's default follows the re-target.
	knobProbeEnv(t, map[string]string{liveenv.EnvModel: probeModel})
	probeCell(t, "a subagent default follows the re-target", "test:live:knob-probe-subagent", func(t *testing.T) {
		if got := liveSubagentDefault(t, "anthropic/pinned"); got != probeModel {
			t.Errorf("liveSubagentDefault = %q under %s=%s, want the re-target", got, liveenv.EnvModel, probeModel)
		}
	})
	knobProbeEnv(t, nil)
	probeCell(t, "a subagent default stays pinned without one", "test:live:knob-probe-subagent-pinned", func(t *testing.T) {
		if got := liveSubagentDefault(t, "anthropic/pinned"); got != "anthropic/pinned" {
			t.Errorf("liveSubagentDefault = %q with no re-target, want the fixture's pinned default", got)
		}
	})
}

// fundingMachine stands in the engine's availability report for a machine
// that can fund openai and neither anthropic nor the gateway.
func fundingMachine(t *testing.T) {
	t.Helper()
	prev := liveAvailability
	liveAvailability = func() detect.Report {
		return detect.Report{Providers: []detect.ProviderStatus{
			{Name: "anthropic", Available: false},
			{Name: "openai", Available: true, Source: "OPENAI_API_KEY"},
			{Name: "openai_compatible", Available: false},
		}}
	}
	t.Cleanup(func() { liveAvailability = prev })
}

// TestARetargetThisMachineCannotFundSkipsAtTheChokepoint: the re-target is
// what serves the run once a vendor guard is waved, so a re-target onto a
// route this machine cannot fund is a `skipped` row naming the route —
// decided where the guards ask (overrideCoversProvider) AND where the run is
// armed (armLiveRun), because with the fixture's vendor key present no guard
// is consulted at all. Waved, the run reaches a provider it cannot pay and
// records a `fail`, with a ref and a served_model, for a run that never had
// a chance.
func TestARetargetThisMachineCannotFundSkipsAtTheChokepoint(t *testing.T) {
	fundingMachine(t)
	cases := []struct {
		name, model, wantRoute string
		body                   func(t *testing.T)
	}{
		{"the vision OR-guard: onto anthropic, with no anthropic credential", "anthropic/claude-haiku-4-5-20251001", `"anthropic"`,
			func(t *testing.T) { overrideCoversCredential(t, "OPENAI_API_KEY") }},
		{"the vendor key present, no guard consulted: onto an unconfigured gateway", "openai_compatible/gpt-oss-120b", `"openai_compatible"`,
			func(t *testing.T) { newLiveClawExecutor(t, model.NewRegistry(), compileFixture(t, "feat_sched.bot")) }},
	}
	for i, c := range cases {
		ledger := knobProbeEnv(t, map[string]string{liveenv.EnvModel: c.model})
		target := fmt.Sprintf("test:live:knob-probe-unfunded-%d", i)
		if !probeCell(t, c.name, target, c.body) {
			t.Errorf("%s: the run went ahead on a route this machine cannot fund", c.name)
			continue
		}
		if row := probeRow(t, ledger, target); row.Verdict != liveledger.VerdictSkipped || !strings.Contains(row.SkipReason, c.wantRoute) {
			t.Errorf("%s recorded %q (%q), want a skipped row naming the route %s", c.name, row.Verdict, row.SkipReason, c.wantRoute)
		}
	}

	// A re-target this machine CAN fund waves the guard and arms the run;
	// a workflow with no LLM node has nothing to re-target, fundable or not.
	knobProbeEnv(t, map[string]string{liveenv.EnvModel: "openai/gpt-5.5"})
	if probeCell(t, "onto a route this machine funds", "test:live:knob-probe-funded", func(t *testing.T) {
		if !overrideCoversCredential(t, "ANTHROPIC_API_KEY") {
			t.Error("a fundable re-target away from anthropic did not wave the anthropic guard")
		}
		_ = newLiveClawExecutor(t, model.NewRegistry(), compileFixture(t, "feat_sched.bot")).Close()
	}) {
		t.Error("a re-target this machine can fund was refused")
	}
	knobProbeEnv(t, map[string]string{liveenv.EnvModel: "openai_compatible/gpt-oss-120b"})
	if probeCell(t, "a tool-only workflow under an unfundable re-target", "test:live:knob-probe-tool-only", func(t *testing.T) {
		_ = newLiveClawExecutor(t, model.NewRegistry(), compileFixture(t, "live_knob_probe.bot")).Close()
	}) {
		t.Error("a tool-only workflow was refused for a re-target it has no LLM node to apply to")
	}
}

// TestALaunchKnobInTheRepoDotEnvRefusesTheRun: Task's root `dotenv:` plants
// the repo-root .env before the harness starts, past its own loader's
// refusal, so the harness refuses to run while that file assigns a knob —
// naming the knob, never a value of the file.
func TestALaunchKnobInTheRepoDotEnvRefusesTheRun(t *testing.T) {
	ledger := knobProbeEnv(t, nil)
	dotenv := filepath.Join(t.TempDir(), ".env")
	liveDotEnvPath = dotenv // knobProbeEnv restores it
	const otherValue = "value-of-a-line-that-must-never-be-printed"
	if err := os.WriteFile(dotenv, []byte("OTHER_SETTING="+otherValue+"\nexport ITERION_LIVE_MAX_COST_USD=999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !probeCell(t, "a planted ceiling", "test:live:knob-probe-dotenv", func(t *testing.T) { liveOverride(t) }) {
		t.Fatal("the harness ran while the repo .env assigns ITERION_LIVE_MAX_COST_USD")
	}
	row := probeRow(t, ledger, "test:live:knob-probe-dotenv")
	if !strings.Contains(row.SkipReason, liveenv.EnvMaxCostUSD) {
		t.Errorf("skip reason %q does not name the planted knob", row.SkipReason)
	}
	if strings.Contains(row.SkipReason, "999") || strings.Contains(row.SkipReason, otherValue) {
		t.Errorf("skip reason %q carries a value of the .env file — names only, the file holds credentials", row.SkipReason)
	}
	if err := os.WriteFile(dotenv, []byte("OTHER_SETTING="+otherValue+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if probeCell(t, "a .env with no knob", "test:live:knob-probe-dotenv-clean", func(t *testing.T) { liveOverride(t) }) {
		t.Error("a .env that assigns no launch knob refused the run")
	}
}

// unreadableEvents is a run store whose event log cannot be read.
type unreadableEvents struct{ store.RunStore }

func (unreadableEvents) LoadEvents(context.Context, string) ([]*store.Event, error) {
	return nil, errors.New("event log unreadable")
}

// TestAnUnreadableEventStreamIsRecordedUnobserved: a run whose events cannot
// be read is not known to have had no LLM node — "" is that column's word —
// so the row says unobserved before the harness fails on the read.
func TestAnUnreadableEventStreamIsRecordedUnobserved(t *testing.T) {
	ledger := knobProbeEnv(t, map[string]string{liveenv.EnvModel: probeModel})
	probeCell(t, "LoadEvents fails", "test:live:knob-probe-unreadable", func(t *testing.T) {
		tracker := liveledger.Track(t)
		if _, err := liveRunEvents(tracker, liveOverride(t), unreadableEvents{}, "run-x"); err == nil {
			t.Error("liveRunEvents returned no error for an unreadable event log — the caller would go on with nothing")
		}
	})
	if got, want := probeRow(t, ledger, "test:live:knob-probe-unreadable").ServedModel, liveledger.UnobservedServedModel(probeModel); got != want {
		t.Errorf("served_model = %q for a run whose events could not be read, want %q", got, want)
	}
}

// TestAnLLMRouterIsAnLLMNodeOfTheServedModelColumn: a router in `llm` mode
// asks a model where to go, and the "*" override re-targets it, so a run
// whose only LLM node is such a router — on a backend that names no model —
// records unobserved, not "" (a run with no LLM node). The deterministic
// modes ask nothing.
func TestAnLLMRouterIsAnLLMNodeOfTheServedModelColumn(t *testing.T) {
	ledger := knobProbeEnv(t, map[string]string{liveenv.EnvModel: probeModel})
	routerStarted := func(mode string) []*store.Event {
		return []*store.Event{{Type: store.EventNodeStarted, NodeID: "pick", Data: map[string]any{"kind": ir.NodeRouter.String(), "mode": mode}}}
	}
	cases := []struct {
		mode string
		want string
	}{
		{ir.RouterLLM.String(), liveledger.UnobservedServedModel(probeModel)},
		{ir.RouterCondition.String(), ""},
		{ir.RouterFanOutAll.String(), ""},
		{ir.RouterRoundRobin.String(), ""},
		{ir.RouterFanOutEach.String(), ""},
	}
	for _, c := range cases {
		target := "test:live:knob-probe-router-" + c.mode
		probeCell(t, "router mode "+c.mode, target, func(t *testing.T) {
			feedLedgerProvenance(liveledger.Track(t), liveOverride(t), routerStarted(c.mode))
		})
		if got := probeRow(t, ledger, target).ServedModel; got != c.want {
			t.Errorf("a run whose only node is a %s router recorded served_model %q, want %q", c.mode, got, c.want)
		}
	}
}

// TestAnEngineRunsOnlyAnArmedPair: newLiveEngine refuses an executor
// newLiveClawExecutor did not build, or built for another workflow — the
// ceiling lives on the workflow the engine runs, the re-target on the
// executor, and a pair assembled elsewhere runs without one of them.
func TestAnEngineRunsOnlyAnArmedPair(t *testing.T) {
	knobProbeEnv(t, map[string]string{liveenv.EnvMaxCostUSD: fmt.Sprint(probeCap)})
	wf := compileFixture(t, "live_knob_probe.bot")
	other := compileFixture(t, "live_knob_probe.bot")
	probeCell(t, "pairs", "test:live:knob-probe-pairs", func(t *testing.T) {
		bare := model.NewClawExecutor(model.NewRegistry(), wf)
		defer bare.Close()
		if err := liveArmedFor(bare, wf); err == nil {
			t.Error("an executor built beside newLiveClawExecutor was accepted — the launch knobs never reached it")
		}
		armed := newLiveClawExecutor(t, model.NewRegistry(), wf)
		defer armed.Close()
		if err := liveArmedFor(armed, other); err == nil {
			t.Error("an executor armed for one workflow was accepted for another — the ceiling is not on the workflow the engine would run")
		}
		if err := liveArmedFor(armed, wf); err != nil {
			t.Errorf("the armed pair was refused: %v", err)
		}
	})
}
