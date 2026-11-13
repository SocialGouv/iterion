package e2e

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/internal/liveenv"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/detect"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	clawtools "github.com/SocialGouv/iterion/pkg/claw/pkg/api/tools"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/liveledger"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ---------------------------------------------------------------------------
// The launch-knob chokepoint of the live harness
// ---------------------------------------------------------------------------
//
// Every live target obtains its run here, and every launch knob the harness
// reads (internal/liveenv: ITERION_LIVE_MODEL, _BACKEND, _MAX_COST_USD,
// _FUNDED_BY) is applied here and nowhere else:
//
//   - liveOverride is the one reader of the knobs;
//   - newLiveClawExecutor is the one constructor of a live executor, and
//     armLiveRun — which it calls — the one function that applies a knob to
//     a run: the re-target as the executor's "*" override rule, the ceiling
//     on the workflow the engine runs, and the refusal of a re-target this
//     machine cannot fund;
//   - newLiveEngine is the one constructor of a live engine, and refuses an
//     executor newLiveClawExecutor did not arm for that same workflow;
//   - a target launched through a path with no seam for the run knobs (a CLI
//     command, a delegate backend driven directly) calls
//     refuseRetargetWithoutSeam instead, and a target whose subject IS its
//     fixture's own budget calls refuseCapOnBudgetSubject;
//   - recordLiveProvenance is the one place a ledger tracker is told what
//     served the run and who funded it, so the funder knob reaches every row.
//
// The file carries no build tag on purpose: it compiles with the free suite
// as well as with the live harness, so
// TestEveryLaunchKnobReachesEveryLiveTargetShape drives the very functions a
// paid run goes through, for $0. TestEveryLiveRunIsArmedAtTheKnobChokepoint
// (pkg/liveledger) refuses a live test that builds an executor or an engine,
// applies a budget, reads the knobs or feeds provenance anywhere else.

// liveDotEnvPath is the file Task's root `dotenv:` plants into every
// `task test:live:*` process: the repo root's .env, seen from e2e/. A
// variable so the free witness can point it at a scratch file.
var liveDotEnvPath = filepath.Join("..", ".env")

// liveAvailability is the engine's own report of what this machine can
// serve (detect.Detect). Asked only when a re-target is set. A variable so
// the free witness can stand in a report for a machine it describes.
var liveAvailability = func() detect.Report { return detect.Detect(context.Background()) }

// liveOverride reads the harness's launch knobs (ITERION_LIVE_MODEL,
// _BACKEND, _FUNDED_BY, _MAX_COST_USD). A malformed knob is FATAL, never a
// silent default: a cap or a re-target the harness dropped is one the
// operator believes is in force. A knob the repo-root .env assigns refuses
// the run with the reason (see liveenv.DotEnvKnobs): under `task`, that file
// is planted before the harness starts, so the value in the environment
// cannot say whether the operator exported it or a project file did.
func liveOverride(t *testing.T) liveenv.Override {
	t.Helper()
	if planted := dotEnvLaunchKnobs(t); len(planted) > 0 {
		skipLive(t, "the repo-root .env assigns %s: Task's root `dotenv:` plants that file into every `task test:live:*` run before the harness starts, so a project file would re-target the run, move its ceiling or name the funder of a committed receipt — remove the line from .env and export the knob in the shell", strings.Join(planted, ", "))
	}
	ov, err := liveenv.Read(os.Getenv)
	if err != nil {
		t.Fatalf("live harness knobs: %v", err)
	}
	return ov
}

// dotEnvLaunchKnobs names the launch knobs the repo-root .env assigns. Names
// only: the file carries credentials, and no value of it is kept or printed.
func dotEnvLaunchKnobs(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(liveDotEnvPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("read %s to check that it assigns no launch knob: %v", liveDotEnvPath, err)
	}
	return liveenv.DotEnvKnobs(string(b))
}

// skipLive records a `skipped` ledger row naming the prerequisite, then
// skips. THE chokepoint every requireX guard goes through: a target this
// machine cannot run says so in the committed ledger, so `never` stops
// meaning both "nobody has paid for it" and "nobody could run it here".
func skipLive(t *testing.T, format string, args ...any) {
	t.Helper()
	reason := fmt.Sprintf(format, args...)
	// RecordSkip says where the row went, and — only when that is the
	// COMMITTED ledger — which generated twins must be re-derived before
	// `task check` is green again.
	liveledger.RecordSkip(t, reason)
	t.Skip(reason)
}

// liveRetargetUnfunded is THE decision whether this machine can fund the
// run's re-target, asked by the guard helper and by the arming chokepoint
// alike: "" when there is no re-target, when its route is served here, or
// when its route is not decidable (a bare model id).
func liveRetargetUnfunded(ov liveenv.Override) string {
	if !ov.Retargets() {
		return ""
	}
	return ov.UnfundedRetarget(liveAvailability())
}

// overrideCoversProvider reports whether the run's model override routes
// every LLM node away from provider, so a guard protecting that provider's
// credential must not skip: the credential is no longer on the run's path.
// ONE helper for the whole class — requireEnv, requireOpenAI and
// requireOpus48 ask here rather than each re-deciding, so the third site
// cannot drift from the first two.
//
// The re-target is what serves the run once the guard is waved, so ITS
// funding becomes the prerequisite: a re-target onto a route this machine
// cannot fund skips with that reason, where waving the guard would let the
// run reach a provider it cannot pay and record a `fail` — with a ref and a
// served_model — for a run that never had a chance.
func overrideCoversProvider(t *testing.T, provider string) bool {
	t.Helper()
	ov := liveOverride(t)
	if !ov.SupersedesProvider(provider) {
		return false
	}
	if why := liveRetargetUnfunded(ov); why != "" {
		skipLive(t, "no %s credential, and the re-target that would replace it cannot be funded here: %s", provider, why)
	}
	t.Logf("[live] no %s credential, but %s routes every LLM node to %q — the run does not reach %s; the ledger row records what served",
		provider, liveenv.EnvModel, ov.Route(), provider)
	return true
}

// overrideCoversCredential is overrideCoversProvider keyed on the
// credential env var a guard checks, through the one table that maps a
// credential to the route it funds (liveenv).
func overrideCoversCredential(t *testing.T, name string) bool {
	t.Helper()
	return overrideCoversProvider(t, liveenv.ProviderOfCredentialEnv(name))
}

// refuseRetargetWithoutSeam is what a target calls when it launches its run
// through a path the run knobs cannot reach — a CLI command that builds its
// own engine, a delegate backend driven directly. While a re-target or a
// ceiling is set it skips, naming the knobs and the path, rather than spend
// on the fixture's pinned model, unbounded by the operator's ceiling, while
// the operator believes otherwise. The funder knob is not refused: it names
// who pays, and recordLiveProvenance writes it on every path.
func refuseRetargetWithoutSeam(t *testing.T, path string) {
	t.Helper()
	if knobs := liveOverride(t).RunKnobs(); len(knobs) > 0 {
		skipLive(t, "%s cannot reach a run launched through %s (no seam hands it the re-target or the ceiling) — unset it to run this fixture on its pinned model", strings.Join(knobs, ", "), path)
	}
}

// refuseCapOnBudgetSubject is what a target whose SUBJECT is its fixture's
// own budget calls before it runs. ITERION_LIVE_MAX_COST_USD replaces the
// workflow's budget at the chokepoint, so under the knob the cap the test
// asserts on is not the cap that runs, and a healthy target would record a
// `fail`. A re-target stays legal there: re-proving a budget firing on
// another route proves the engine.
func refuseCapOnBudgetSubject(t *testing.T) {
	t.Helper()
	if liveOverride(t).MaxCostUSD > 0 {
		skipLive(t, "%s replaces the fixture's own budget, which is what this target proves — unset it to run this target", liveenv.EnvMaxCostUSD)
	}
}

// liveArmed maps each executor newLiveClawExecutor built to the workflow it
// armed, so newLiveEngine can refuse to run any other pair.
var liveArmed sync.Map

// armLiveRun is THE function that applies the harness's launch knobs to a
// run, called by newLiveClawExecutor before the executor exists:
//
//   - a re-target this machine cannot fund is refused, for a workflow that
//     has an LLM node to re-target — whether or not a vendor guard was
//     consulted on the way (with the vendor's key present, none is);
//   - the ceiling replaces the workflow's own budget through
//     ir.ApplyBudgetOverrides, per-field, non-zero wins, in BOTH directions —
//     the precedence `iterion run --max-cost-usd` has — on the workflow the
//     engine then runs (the engine builds its run budget from it at Run);
//   - the re-target becomes the executor's "*" override rule, which outranks
//     every node's DSL backend:/model: (pkg/backend/model/model_override.go).
func armLiveRun(t *testing.T, wf *ir.Workflow) []model.ClawExecutorOption {
	t.Helper()
	ov := liveOverride(t)
	if ov.Retargets() && wf.UsesLLM() {
		if why := liveRetargetUnfunded(ov); why != "" {
			skipLive(t, "%s", why)
		}
	}
	if bo := ov.BudgetOver(ir.BudgetOverrides{}); !bo.IsZero() {
		if err := bo.Validate(); err != nil {
			t.Fatalf("live budget override: %v", err)
		}
		ir.ApplyBudgetOverrides(wf, bo)
		t.Logf("[live] budget override applied: max_cost_usd=%v (%s)", wf.Budget.MaxCostUSD, liveenv.EnvMaxCostUSD)
	}
	var opts []model.ClawExecutorOption
	if mo := ov.ModelOverrides(); !mo.Empty() {
		t.Logf("[live] model overrides active: %+v", mo.Rows())
		opts = append(opts, model.WithModelOverrides(mo))
	}
	return opts
}

// newLiveClawExecutor is THE place a live test builds a ClawExecutor — the
// tests that wire their own registries, tool sets and MCP managers included
// — and it arms the run first (armLiveRun). The credential guards are waved
// for the whole tree at once (overrideCoversProvider), so an executor built
// beside this function would run the fixture's pinned vendor under a
// re-target the guard has just let through without that vendor's key.
func newLiveClawExecutor(t *testing.T, reg *model.Registry, wf *ir.Workflow, opts ...model.ClawExecutorOption) *model.ClawExecutor {
	t.Helper()
	opts = append(opts, armLiveRun(t, wf)...)
	ex := model.NewClawExecutor(reg, wf, opts...)
	liveArmed.Store(ex, wf)
	t.Cleanup(func() { liveArmed.Delete(ex) })
	return ex
}

// newLiveEngine is THE place a live test builds the engine that runs a
// workflow. It refuses an executor newLiveClawExecutor did not arm for this
// same workflow: the ceiling lives on the workflow the engine runs and the
// re-target on the executor, so a pair assembled from anywhere else would
// run without one of them.
func newLiveEngine(t *testing.T, wf *ir.Workflow, s store.RunStore, ex *model.ClawExecutor, opts ...runtime.EngineOption) *runtime.Engine {
	t.Helper()
	if err := liveArmedFor(ex, wf); err != nil {
		t.Fatalf("newLiveEngine: %v", err)
	}
	return runtime.New(wf, s, ex, opts...)
}

// liveArmedFor reports whether newLiveClawExecutor armed ex for wf.
func liveArmedFor(ex *model.ClawExecutor, wf *ir.Workflow) error {
	armed, ok := liveArmed.Load(ex)
	if !ok {
		return fmt.Errorf("the executor was not built by newLiveClawExecutor, so the launch knobs (%s) never reached it", strings.Join(liveenv.LaunchKnobs(), ", "))
	}
	if armed != wf {
		return fmt.Errorf("the executor was armed for another workflow than the one this engine runs, so the ceiling (%s) is not on this one", liveenv.EnvMaxCostUSD)
	}
	return nil
}

// liveSubagentDefault is the default model a live test hands a subagent
// runner: the fixture's pinned cheap default, unless the run is
// re-targeted — the "*" rule re-points every LLM node of the bot, and a
// child left on the fixture's vendor would reach it without the key the
// waved guard no longer demands.
func liveSubagentDefault(t *testing.T, pinned string) string {
	t.Helper()
	if ov := liveOverride(t); ov.Model != "" {
		return ov.Model
	}
	return pinned
}

// liveExecutorOption configures newLiveExecutor for callers that need
// to override defaults (currently: the logger, used by `_Real` tests
// that tee to <storeDir>/runs/<runID>/run.log so the desktop app's
// cross-store log pane shows live test output).
type liveExecutorOption func(*liveExecutorConfig)

type liveExecutorConfig struct {
	logger *iterlog.Logger
}

// withLiveLogger overrides the default stderr-only logger. Pass a
// tee'd logger (e.g. from prepareLiveRunLog) so the executor's
// hooks + backend output land in run.log alongside stderr.
func withLiveLogger(l *iterlog.Logger) liveExecutorOption {
	return func(c *liveExecutorConfig) { c.logger = l }
}

// newLiveExecutor creates a ClawExecutor with all standard backends registered
// AND the standard claw built-in tool set (bash, read_file, write_file,
// file_edit, glob, grep, web_fetch, …) wired into a tool registry rooted at
// workDir. Without the tool registration step, any workflow node that
// declares `backend: claw` plus `tools: [bash, ...]` errors out at runtime
// with "unknown tool 'bash'" — observed in whole_improve_loop run3.
func newLiveExecutor(t *testing.T, wf *ir.Workflow, s store.RunStore, runID, workDir string, opts ...liveExecutorOption) *model.ClawExecutor {
	t.Helper()
	cfg := &liveExecutorConfig{}
	for _, opt := range opts {
		opt(cfg)
	}
	reg := model.NewRegistry()
	logger := cfg.logger
	if logger == nil {
		logger = iterlog.New(iterlog.LevelDebug, os.Stderr)
	}
	hooks := model.NewStoreEventHooks(context.Background(), s, runID, logger, nil, nil)

	backendReg := delegate.DefaultRegistry(logger)
	backendReg.Register(delegate.BackendClaw, model.NewClawBackend(reg, hooks, model.RetryPolicy{}))

	toolReg := tool.NewRegistry()
	if err := tool.RegisterClawBuiltins(toolReg, workDir); err != nil {
		t.Fatalf("RegisterClawBuiltins: %v", err)
	}
	// `todo_write` is not optional here even though no fixture asks for it:
	// the executor APPENDS it to every claw node that restricts `tools:`
	// (executor_build_task.go — it keeps the run's Tasks tab populated), so
	// a registry without it fails to resolve the node's own tool list and
	// the run dies before its first turn. Production registers the full set
	// via RegisterClawAll; this harness registered only the builtins, and
	// that gap alone was enough to make a live test look like a broken
	// feature.
	if err := tool.RegisterClawTodo(toolReg); err != nil {
		t.Fatalf("RegisterClawTodo: %v", err)
	}
	// Register ask_user with an auto-answer so claw-backed nodes that
	// have interaction:human don't block the test waiting for a real
	// human prompt. The default RegisterAskUser handler raises
	// delegate.ErrAskUser → runtime pause; in a headless test that
	// hangs indefinitely. The claude_code backend ships its own
	// ask_user via the CLI; only claw needs explicit wiring here.
	if err := tool.RegisterAskUser(toolReg, func(_ context.Context, q clawtools.Question) (clawtools.Answer, error) {
		t.Logf("ask_user auto-answered: %s", q.Prompt)
		return clawtools.Answer{
			FreeText: "Auto-answer (live e2e test, no human available). Use your best judgment based on the available context, prioritise correctness over speed, and continue.",
		}, nil
	}); err != nil {
		t.Fatalf("RegisterAskUser: %v", err)
	}

	// The `command` tool (model-invoked workspace slash commands, #1716):
	// production registers it inside RegisterClawAll; this harness's
	// builtins+todo+ask_user subset never did, so a node declaring
	// `tools: [command]` could only die on "unknown tool" here
	// (measured: TestLive_Feat_WorkspaceCommandNarrowing's first run).
	// Same kill switch as the host: off drops the registration and the
	// node fails loudly at dispatch, never half-lives.
	if !model.SlashCommandsDisabled() {
		if err := tool.RegisterClawCommandWithProvider(toolReg, func() string { return workDir }, model.SlashCommandMaxBytesForRegistry()); err != nil {
			t.Fatalf("RegisterClawCommandWithProvider: %v", err)
		}
	}

	return newLiveClawExecutor(t, reg, wf,
		model.WithBackendRegistry(backendReg),
		model.WithToolRegistry(toolReg),
		model.WithWorkDir(workDir),
		model.WithEventHooks(hooks),
	)
}

// recordLiveProvenance is THE place a live ledger tracker is told what served
// the run and which credential route funded it. Every recording path ends
// here — the event-fed runs through feedLedgerProvenance, a target with no
// event stream (claude-bg) directly — so ITERION_LIVE_FUNDED_BY reaches every
// row the harness writes.
func recordLiveProvenance(tr *liveledger.Tracker, ov liveenv.Override, servedModel string, served []string) {
	tr.SetProvenance(servedModel, ov.FundedByFor(served))
}

// liveRunEvents reads a run's persisted events for the ledger. A read that
// fails leaves the row saying the run went UNOBSERVED before the caller
// fails on the error: "" is the column's word for a run with no LLM node,
// which a run whose events could not be read is not known to be.
func liveRunEvents(tr *liveledger.Tracker, ov liveenv.Override, s store.RunStore, runID string) ([]*store.Event, error) {
	events, err := s.LoadEvents(context.Background(), runID)
	if err != nil {
		recordLiveProvenance(tr, ov, liveledger.UnobservedServedModel(ov.Model), nil)
		return nil, fmt.Errorf("LoadEvents %s: %w", runID, err)
	}
	return events, nil
}

// feedLedgerProvenance records what ACTUALLY served the run and which
// credential route funded it. Read from the persisted llm_request events,
// never from the spec the harness asked for: an override that failed to
// reach the executor would otherwise be recorded as though it had, which
// is the one thing the column exists to make impossible.
func feedLedgerProvenance(tr *liveledger.Tracker, override liveenv.Override, events []*store.Event) {
	served := servedModelSpecs(events)
	// What the events did not name is said as such: only the claw path
	// emits llm_request, so a claude_code / codex / pi delegation reports
	// no model, and "" would make an overridden LLM run read as a tool-only
	// one — indistinguishable from a pass on the fixture's pinned model,
	// the exact false receipt this column exists to prevent. The count of
	// LLM nodes that started tells the two apart.
	recordLiveProvenance(tr, override, liveledger.ServedModelFor(served, llmNodesStarted(events), override.Model), served)
}

// llmNodesStarted counts the run's node_started events whose node could
// have served a model: an agent or a judge, and a router in `llm` mode —
// the kinds the "*" override rule re-targets. Every emitter of node_started
// stamps `kind` except the terminal done/fail pair, which is not an LLM
// node; a router's emitter stamps its `mode` too. A human node answered by
// the LLM in auto mode is not counted: it shares its kind with a human node
// that paused, and the events cannot tell the two apart.
func llmNodesStarted(events []*store.Event) int {
	n := 0
	for _, e := range events {
		if e.Type != store.EventNodeStarted {
			continue
		}
		switch kind, _ := e.Data["kind"].(string); kind {
		case ir.NodeAgent.String(), ir.NodeJudge.String():
			n++
		case ir.NodeRouter.String():
			if mode, _ := e.Data["mode"].(string); mode == ir.RouterLLM.String() {
				n++
			}
		}
	}
	return n
}

// servedModelSpecs lists the model specs the run's llm_request events
// carry. Unsorted and with duplicates — liveledger.FoldServedModels owns
// the canonical form, so the ledger column cannot depend on the order the
// steps finished in.
func servedModelSpecs(events []*store.Event) []string {
	var out []string
	for _, e := range events {
		if e.Type != store.EventLLMRequest {
			continue
		}
		if spec, _ := e.Data["model"].(string); spec != "" {
			out = append(out, spec)
		}
	}
	return out
}
