package runner

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// errUnreadableInputs stands for the store failure that leaves a resume
// without its launch values.
var errUnreadableInputs = errors.New("mongo down")

// offWireMonoTopology is review-pr's mono topology with BOTH reviewers pinned
// off the Anthropic wire, so that in this file the declared supervisor is the
// run's only route the cap can meter.
//
// That is what makes a park here ATTRIBUTABLE. With the claude reviewer on
// `claude_code` + a claude id, an arm whose launch reaches that node parks on
// the NODE, and the assertion reads green whatever the supervisor walk
// answered: measured, a mutation dropping every watch-scoped supervisor on an
// unreadable resume (`open == nil → false`) left the whole package green.
func offWireMonoTopology() *ir.Workflow {
	wf := monoTopology(monoFamilyVar("claude"))
	wf.Nodes["reviewer_claude"] = agentRoute("reviewer_claude", delegate.BackendClaw, "openai", "openai/gpt-6-sol")
	return wf
}

// A supervisor is a route on every execution only while its WATCHED nodes
// run: a coordinator evaluates nothing unless one of them is active
// (supervise.Coordinator.armed), so one whose every watched node this run's
// launch vars excluded spends nothing, and metering it parks the run for
// spend no execution can make. That is the supervisor half of #1828's prune,
// and what lets a bi-model bot give each family its own pacer (#2467).
//
// Every arm that keeps the walk armed is pinned beside it: the drop may only
// take a supervisor the run provably never arms. The supervisor is the only
// metered route of every fixture here, so each verdict is its own.
func TestUsageCapPreflight_ASupervisorWatchingOnlyExcludedNodesIsNotARoute(t *testing.T) {
	blankAnthropicWireEnv(t)
	t.Setenv("ITERION_DEFAULT_SUPERVISOR_MODEL", "")
	t.Setenv("ITERION_SUPERVISORS", "")
	t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "")
	ctx := forfaitBesidePinnedKeys()
	// A soft five-hour cap: it stops nothing in flight, so a capped route the
	// run MAY take parks it.
	soft := cappedWire(t, usagecap.WindowFiveHour, 0.95, time.Now().UTC().Add(2*time.Hour))

	onTheWire := "anthropic/claude-opus-5-5"
	withSupervisor := func(sup *ir.Supervisor) *ir.Workflow {
		wf := offWireMonoTopology()
		wf.Supervisors = []*ir.Supervisor{sup}
		return wf
	}
	gpt := map[string]any{"mono_family": "gpt"}

	if err := preflightWithVars(t, soft, ctx, withSupervisor(
		&ir.Supervisor{Name: "pacer", Watches: []string{"reviewer_claude"}, Model: onTheWire}), gpt); err != nil {
		t.Errorf("parked a mono-gpt run for a supervisor watching only the claude reviewer the launch excluded: %v — it never arms, so it never spends", err)
	}

	t.Run("a watch set with one reachable node stays a route", func(t *testing.T) {
		if err := preflightWithVars(t, soft, ctx, withSupervisor(
			&ir.Supervisor{Name: "pacer", Watches: []string{"reviewer_claude", "reviewer_gpt"}, Model: onTheWire}), gpt); err == nil {
			t.Error("started a run whose supervisor watches the reviewer it DOES take — one reachable watched node arms it, and nothing stops it in flight")
		}
	})

	t.Run("a whole-run supervisor is always a route", func(t *testing.T) {
		if err := preflightWithVars(t, soft, ctx, withSupervisor(
			&ir.Supervisor{Name: "pacer", Model: onTheWire}), gpt); err == nil {
			t.Error("started a run whose supervisor declares no `watches:` — an empty watch set is the whole run, armed from its first node")
		}
	})

	t.Run("the declared default decides when the launch overrode nothing", func(t *testing.T) {
		// A fresh launch carrying no vars reads as a reading
		// (launchReading returns have=true), so `open` is the
		// declared-default filter — not nil. `mono_family = "claude"` takes
		// the claude branch, and the supervisor watching it arms.
		if err := preflightWithVars(t, soft, ctx, withSupervisor(
			&ir.Supervisor{Name: "pacer", Watches: []string{"reviewer_claude"}, Model: onTheWire}), nil); err == nil {
			t.Error("started a run with no launch value: the declared default takes the claude branch, and the supervisor watching it arms")
		}
	})

	t.Run("an unreadable resume keeps every supervisor judged", func(t *testing.T) {
		// No launch reading at all: launchVarEdges returns no filter, the
		// walk is the whole graph, and the supervisor stays judged.
		wf := withSupervisor(&ir.Supervisor{Name: "pacer", Watches: []string{"reviewer_claude"}, Model: onTheWire})
		r := capRunner(capTestPolicy(), soft, &inputStore{err: errUnreadableInputs})
		if err := r.usageCapPreflight(ctx, wf, &queue.RunMessage{RunID: "run-sup-resume", Resume: &queue.ResumeSpec{}}, iterlog.Nop()); err == nil {
			t.Error("a resume whose inputs cannot be read started: with no reading of its vars the prune narrows nothing, so the supervisor's watched node stays reachable")
		}
	})
}

// The other half of "the walk lists what the run will spawn": on a run stamped
// to a sovereign runner pool, the spawn drops every supervisor whose effective
// model is not the pool's gateway (runview.PoolSurvivingSpecs, D12) — so
// metering one parks the run for spend no execution can make, exactly as a
// supervisor watching only excluded nodes would.
//
// Both arms, because the filter must not be wider than the spawn's: a run with
// no pool stamp keeps its supervisor judged, and a pooled run keeps a
// gateway-pinned one — which the spawn starts.
func TestUsageCapPreflight_APooledRunIsNotParkedForASupervisorTheSpawnRefuses(t *testing.T) {
	blankAnthropicWireEnv(t)
	t.Setenv("ITERION_DEFAULT_SUPERVISOR_MODEL", "")
	t.Setenv("ITERION_SUPERVISORS", "")
	t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "")
	ctx := forfaitBesidePinnedKeys()
	soft := cappedWire(t, usagecap.WindowFiveHour, 0.95, time.Now().UTC().Add(2*time.Hour))

	wholeRunOnTheWire := func() *ir.Workflow {
		wf := offWireMonoTopology()
		wf.Supervisors = []*ir.Supervisor{{Name: "pacer", Model: "anthropic/claude-opus-5-5"}}
		return wf
	}
	preflight := func(t *testing.T, wf *ir.Workflow, pool string) error {
		t.Helper()
		r := capRunner(capTestPolicy(), soft, &capStatusStore{})
		return r.usageCapPreflight(ctx, wf, &queue.RunMessage{
			RunID: "run-pool", Vars: map[string]any{"mono_family": "gpt"}, RunnerPool: pool,
		}, iterlog.Nop())
	}

	if err := preflight(t, wholeRunOnTheWire(), "sovereign-eu"); err != nil {
		t.Errorf("parked a pool-stamped run for a vendor-pinned supervisor its own spawn refuses to start: %v — PoolSurvivingSpecs drops it before supervise.StartDeclared", err)
	}

	t.Run("the same supervisor off a pool stays a route", func(t *testing.T) {
		if err := preflight(t, wholeRunOnTheWire(), ""); err == nil {
			t.Error("started an unpooled run whose whole-run supervisor rides the capped wire — the pool filter may only drop what a pool stamp drops")
		}
	})

	t.Run("a pooled run keeps a gateway-pinned supervisor judged", func(t *testing.T) {
		wf := offWireMonoTopology()
		wf.Supervisors = []*ir.Supervisor{{Name: "pacer", Model: "openai_compatible/pool-gw"}}
		sups := preflightSupervisors(ctx, wf, &queue.RunMessage{
			RunID: "run-pool-gw", Vars: map[string]any{"mono_family": "gpt"}, RunnerPool: "sovereign-eu",
		}, map[string]any{"mono_family": "gpt"}, wf.LaunchVarEdgeFilter(map[string]any{"mono_family": "gpt"}), iterlog.Nop())
		if len(sups) != 1 || sups[0].id != supervisorRouteIDPrefix+"pacer" {
			t.Errorf("the supervisor walk of a pooled run with a gateway-pinned supervisor is %v, want supervisor:pacer — the pool filter keeps exactly what the spawn keeps", sups)
		}
	})
}

// A spec the pool filter refuses WIDENS this walk: the supervisor stops being
// a metered route, so a run that a capped one would have parked starts
// instead. The refusal that decided it is owed to the operator — the spawn
// makes the identical call with the run's logger (loop.go), and a pre-flight
// discarding it left the admission with nothing greppable about a route it
// stopped counting, while the park reason simply omits it.
func TestUsageCapPreflight_ThePoolFilterNamesTheSupervisorItDrops(t *testing.T) {
	blankAnthropicWireEnv(t)
	t.Setenv("ITERION_DEFAULT_SUPERVISOR_MODEL", "")
	t.Setenv("ITERION_SUPERVISORS", "")
	ctx := forfaitBesidePinnedKeys()
	wf := offWireMonoTopology()
	wf.Supervisors = []*ir.Supervisor{{Name: "pacer", Model: "anthropic/claude-opus-5-5"}}
	vars := map[string]any{"mono_family": "gpt"}

	var logged bytes.Buffer
	sups := preflightSupervisors(ctx, wf, &queue.RunMessage{
		RunID: "run-pool-log", Vars: vars, RunnerPool: "sovereign-eu",
	}, vars, wf.LaunchVarEdgeFilter(vars), iterlog.New(iterlog.LevelWarn, &logged))
	if len(sups) != 0 {
		t.Fatalf("the walk of a pooled run with a vendor-pinned supervisor is %v, want none — its own spawn refuses it (D12)", sups)
	}
	if !strings.Contains(logged.String(), `supervisor "pacer"`) {
		t.Errorf("the pre-flight stopped metering `pacer` and logged %q — the drop widens the walk, so it owes the refusal line its own filter writes (runview.PoolSurvivingSpecs)", logged.String())
	}

	// The other half of the same call. A monitor the spec walk cannot parse
	// is dropped there (supervise.ParseMonitorSpecs), and admission is the
	// first place an operator can be told which one.
	t.Run("a monitor the spec walk refuses is named too", func(t *testing.T) {
		wf := offWireMonoTopology()
		wf.Supervisors = []*ir.Supervisor{{Name: "pacer", Model: "openai_compatible/pool-gw", Monitors: []string{"cost_gt=-1"}}}
		var logged bytes.Buffer
		preflightSupervisors(ctx, wf, &queue.RunMessage{
			RunID: "run-monitor", Vars: vars, RunnerPool: "sovereign-eu",
		}, vars, wf.LaunchVarEdgeFilter(vars), iterlog.New(iterlog.LevelWarn, &logged))
		if !strings.Contains(logged.String(), "dropping monitor") {
			t.Errorf("the pre-flight walked a supervisor whose monitor the parser refuses and logged %q — this walk is the spawn's, diagnostics included", logged.String())
		}
	})
}

// Every line the pre-flight writes that does not format the run id ITSELF,
// read where an operator reads them: through usageCapPreflight, the one
// caller that owns their attribution.
//
// Those lines come from code the spawn shares, which names its own subject
// and not the run — a supervisor the pool filter or the spec walk drops
// (runview.PoolSurvivingSpecs, supervise.SpecsFromWorkflow), a credential
// whose reading failed (cappedRoutes). At admission no run-scoped sink exists
// yet: the pre-flight runs before the clone and the container, and loop.go
// registers the run log writer only after it, so these lines never reach the
// `run.log` where the spawn's identical ones are attributed by sink. One fork
// at the top of the pre-flight carries the run id as a FIELD instead — the
// shape loop_nats.go's admission-rejection lines already use, rendered by the
// runner's own format (cmd/iterion/runner.go loads JSON).
//
// Read from a JSON logger for that reason: a human-format writer drops the
// fields map (pkg/log emit), so asserting on the text would prove nothing
// about attribution.
func TestUsageCapPreflight_EveryAdmissionLineNamesTheRun(t *testing.T) {
	blankAnthropicWireEnv(t)
	t.Setenv("ITERION_DEFAULT_SUPERVISOR_MODEL", "")
	t.Setenv("ITERION_SUPERVISORS", "")
	t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "")
	ctx := forfaitBesidePinnedKeys()
	soft := cappedWire(t, usagecap.WindowFiveHour, 0.95, time.Now().UTC().Add(2*time.Hour))
	wf := offWireMonoTopology()
	wf.Supervisors = []*ir.Supervisor{{Name: "pacer", Model: "anthropic/claude-opus-5-5"}}

	const runID = "run-attributed"
	var logged bytes.Buffer
	r := capRunner(capTestPolicy(), soft, &capStatusStore{})
	if err := r.usageCapPreflight(ctx, wf, &queue.RunMessage{
		RunID: runID, Vars: map[string]any{"mono_family": "gpt"}, RunnerPool: "sovereign-eu",
	}, iterlog.NewWithFormat(iterlog.LevelWarn, &logged, iterlog.FormatJSON)); err != nil {
		t.Fatalf("bench: the pooled run parked for the supervisor its spawn refuses: %v", err)
	}
	assertAttributed(t, logged.String(), `supervisor \"pacer\"`, runID,
		"the drop widens the walk — a run a capped supervisor would have parked starts instead")

	// The third line of the same class, from the other call this logger
	// reaches: a credential the cap store cannot read is named by KEY, and
	// the pre-flight then fails open on it.
	t.Run("a credential the store cannot read is attributed too", func(t *testing.T) {
		const runID = "run-unreadable"
		var logged bytes.Buffer
		wf := offWireMonoTopology()
		wf.Nodes["reviewer_claude"] = agentRoute("reviewer_claude", delegate.BackendClaudeCode, "", "claude-opus-5-5")
		r := capRunner(capTestPolicy(), failingCapStore{usagecap.NewMemStore()}, &capStatusStore{})
		if err := r.usageCapPreflight(ctx, wf, &queue.RunMessage{
			RunID: runID, Vars: map[string]any{"mono_family": "claude"},
		}, iterlog.NewWithFormat(iterlog.LevelWarn, &logged, iterlog.FormatJSON)); err != nil {
			t.Fatalf("the pre-flight parked a run whose cap readings are unreadable: %v — it fails open on every uncertainty", err)
		}
		assertAttributed(t, logged.String(), "usage-cap pre-flight read", runID,
			"the pre-flight proceeded on a credential it could not measure")
	})

	// The fork is taken before every guarded use, so it must survive the
	// logger this function's own `if logger != nil` checks expect: a nil fork
	// stays nil, and the walk still decides.
	t.Run("a nil logger still admits", func(t *testing.T) {
		r := capRunner(capTestPolicy(), soft, &capStatusStore{})
		if err := r.usageCapPreflight(ctx, wf, &queue.RunMessage{
			RunID: "run-nil-logger", Vars: map[string]any{"mono_family": "gpt"}, RunnerPool: "sovereign-eu",
		}, nil); err != nil {
			t.Fatalf("the pre-flight on a nil logger: %v — the fork may not change the verdict", err)
		}
	})
}

// assertAttributed finds the JSON line whose message contains want and holds
// it to carrying runID as a field — the attribution a shared filter's own
// text does not provide.
func assertAttributed(t *testing.T, logged, want, runID, why string) {
	t.Helper()
	var named string
	for _, line := range strings.Split(strings.TrimSpace(logged), "\n") {
		if strings.Contains(line, want) {
			named = line
		}
	}
	if named == "" {
		t.Fatalf("no line matching %q in %q — %s, so it owes the line its own filter writes", want, logged, why)
	}
	if !strings.Contains(named, `"run_id":"`+runID+`"`) {
		t.Errorf("the line is %s — it carries no run_id field, so on a runner serving several runs concurrently it cannot be tied to the run it decided (%s)", named, why)
	}
}
