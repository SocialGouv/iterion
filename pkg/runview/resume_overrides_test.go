package runview

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sessionboard"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/supervise"
)

// An in-process resume replays the run document's folded override rows to
// the executor, and its supervisors and session board read the SAME rows:
// spawnRun hands them the overrides the executor dispatches with, never
// the launch-only persistence parameter (nil on a resume). Each test
// resumes the same paused run twice — once with no rows (the control: the
// seam captures the authored reading), once with the rows the launch fold
// stamps — so a seam that captured nothing, or a reading that ignored the
// rows, cannot pass.

const resumeSupPinnedBot = `
prompt p:
  Watch the run.

agent work:
  backend: "claw"
  model: "anthropic/claude-opus-5"
  system: p
  user: p

supervisor pacer:
  model: "anthropic/claude-opus-5"
  system: p

workflow sup_resume:
  entry: work
  work -> done
`

const resumeSupUnpinnedBot = `
prompt p:
  Watch the run.

agent work:
  backend: "claw"
  model: "anthropic/claude-opus-5"
  system: p
  user: p

supervisor pacer:
  system: p

workflow sup_resume:
  entry: work
  work -> done
`

// resumeGatewayRouteCell routes the watched node onto the team gateway.
var resumeGatewayRouteCell = store.RunLLMRouteOverride{
	Selector: "work", Backend: "claw", Provider: "openai_compatible",
	Model: "openai_compatible/scaleway/deepseek-v4.1-flash",
}

// resumeFixture writes the bot and returns a service rooted beside it, the
// bot's path, its compiled workflow and hash.
func resumeFixture(t *testing.T, src string) (*Service, string, *ir.Workflow, string) {
	t.Helper()
	dir := t.TempDir()
	botPath := filepath.Join(dir, "sup_resume.bot")
	if err := os.WriteFile(botPath, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	svc, err := NewService(dir, WithLogger(iterlog.Nop()), WithWorkDir(t.TempDir()))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	wf, hash, err := CompileWorkflowWithHash(botPath, "", store.RunTrustDefault)
	if err != nil {
		t.Fatalf("compile fixture: %v", err)
	}
	return svc, botPath, wf, hash
}

// launchFoldRows is what the launch stamps on the run document for this
// policy: the launch fold itself, never rows written by hand.
func launchFoldRows(t *testing.T, wf *ir.Workflow, botID string, cells ...store.RunLLMRouteOverride) []store.RunModelOverride {
	t.Helper()
	spec := &LaunchSpec{BotID: botID, LLMRoutePolicy: &store.RunLLMRoutePolicy{Overrides: cells}}
	if _, err := foldAndScreen(wf, spec, ""); err != nil {
		t.Fatalf("launch fold: %v", err)
	}
	rows := RunModelOverrides(spec.ModelOverrides)
	if len(rows) == 0 {
		t.Fatalf("precondition: the launch fold of %+v kept no row for bot %q", cells, botID)
	}
	return rows
}

// resumePausedRun seeds a run paused past its last node, its document
// carrying rows, and resumes it in-process.
func resumePausedRun(t *testing.T, svc *Service, botPath, hash, runID string, rows []store.RunModelOverride) {
	t.Helper()
	ctx := context.Background()
	if _, err := svc.store.CreateRun(ctx, runID, "sup_resume", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	r, err := svc.store.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	r.WorkflowHash = hash
	r.ModelOverrides = rows
	if err := svc.store.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	if err := svc.store.SaveCheckpoint(ctx, runID, &store.Checkpoint{NodeID: "done"}); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}
	if err := svc.store.UpdateRunStatus(ctx, runID, store.RunStatusPausedOperator, "paused by operator"); err != nil {
		t.Fatalf("UpdateRunStatus: %v", err)
	}
	if _, err := svc.Resume(ctx, ResumeSpec{RunID: runID, FilePath: botPath}); err != nil {
		t.Fatalf("resume %s: %v", runID, err)
	}
}

func resumeTestEnv(t *testing.T, supervisors, board string) {
	t.Helper()
	t.Setenv("ITERION_RUNS_DETACHED", "0")
	t.Setenv("ITERION_SANDBOX_DEFAULT", "none")
	t.Setenv("ITERION_SUPERVISORS", supervisors)
	t.Setenv("ITERION_SESSION_BOARD", board)
	t.Setenv("ITERION_DEFAULT_SUPERVISOR_MODEL", "")
	t.Setenv("ITERION_DEFAULT_SESSIONBOARD_MODEL", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
}

// captureSupervisorSpecs swaps the supervisor start seam for one that hands
// the specs that survive the funding guard to the test.
func captureSupervisorSpecs(t *testing.T) <-chan []supervise.Spec {
	t.Helper()
	ch := make(chan []supervise.Spec, 4)
	restore := swapStartDeclared(func(_ context.Context, _ supervise.Observer, _ supervise.Injector, _ string, specs []supervise.Spec, _ *iterlog.Logger) func() {
		ch <- specs
		return func() {}
	})
	t.Cleanup(restore)
	return ch
}

func onlySupervisorSpec(t *testing.T, ch <-chan []supervise.Spec, runID string) supervise.Spec {
	t.Helper()
	select {
	case specs := <-ch:
		if len(specs) != 1 || specs[0].Name != "pacer" {
			t.Fatalf("%s: supervisor specs = %+v, want the one declared supervisor pacer", runID, specs)
		}
		return specs[0]
	case <-time.After(20 * time.Second):
		t.Fatalf("%s: no supervisor spawn observed on resume", runID)
	}
	return supervise.Spec{}
}

// `supervisor:<name>` pins a supervisor's model on every lane, the
// in-process resume included.
func TestResume_SupervisorsReadTheRunsFoldedRows(t *testing.T) {
	resumeTestEnv(t, "on", "off")
	// Placeholder keys: the funding guard keeps a pinned supervisor only
	// when its vendor resolves a credential.
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-placeholder")
	t.Setenv("OPENAI_API_KEY", "sk-placeholder")
	ch := captureSupervisorSpecs(t)
	svc, botPath, wf, hash := resumeFixture(t, resumeSupPinnedBot)
	rows := launchFoldRows(t, wf, "sup_resume", store.RunLLMRouteOverride{Selector: "sup_resume/supervisor:pacer", Model: "openai/gpt-6-sol"})

	resumePausedRun(t, svc, botPath, hash, "run-resume-sup-control", nil)
	if got := onlySupervisorSpec(t, ch, "control").Model; got != "anthropic/claude-opus-5" {
		t.Fatalf("control: a run with no rows resumes its supervisor on %q, want the authored anthropic/claude-opus-5", got)
	}

	resumePausedRun(t, svc, botPath, hash, "run-resume-sup-pinned", rows)
	if got := onlySupervisorSpec(t, ch, "pinned").Model; got != "openai/gpt-6-sol" {
		t.Fatalf("resumed supervisor pacer runs on %q, the run document's folded rows %+v pin openai/gpt-6-sol", got, rows)
	}
}

// An unpinned supervisor watching a gateway-routed node knows it on resume:
// supervise.Spec.GatewayWatched refuses the vendor default for it.
func TestResume_UnpinnedSupervisorWatchesTheGatewayRoute(t *testing.T) {
	resumeTestEnv(t, "on", "off")
	ch := captureSupervisorSpecs(t)
	svc, botPath, wf, hash := resumeFixture(t, resumeSupUnpinnedBot)
	rows := launchFoldRows(t, wf, "sup_resume", resumeGatewayRouteCell)

	resumePausedRun(t, svc, botPath, hash, "run-resume-supgw-control", nil)
	if onlySupervisorSpec(t, ch, "control").GatewayWatched {
		t.Fatal("control: a run with no rows reads GatewayWatched=true, its node runs on anthropic")
	}

	resumePausedRun(t, svc, botPath, hash, "run-resume-supgw-routed", rows)
	if !onlySupervisorSpec(t, ch, "routed").GatewayWatched {
		t.Fatalf("resumed unpinned supervisor reads GatewayWatched=false, the run document's rows %+v route its node to the gateway", rows)
	}
}

// The session board's curation learns the run is gateway-served on resume,
// so sessionboard.ErrGatewayBoardNeedsPin refuses its vendor default.
func TestResume_SessionBoardWatchesTheGatewayRoute(t *testing.T) {
	resumeTestEnv(t, "off", "on")
	svc, botPath, wf, hash := resumeFixture(t, resumeSupUnpinnedBot)
	svc.sbStore = guardStubSBStore{}
	got := make(chan bool, 4)
	restore := swapSessionBoardCoordinator(func(_ sessionboard.Observer, _ sessionboard.Emitter, _ string, cfg sessionboard.Config, _ sessionboard.Evaluator, _ *iterlog.Logger) *sessionboard.Coordinator {
		got <- cfg.GatewayWatched
		return nil
	})
	t.Cleanup(restore)
	rows := launchFoldRows(t, wf, "sup_resume", resumeGatewayRouteCell)
	watched := func(label string) bool {
		t.Helper()
		select {
		case gw := <-got:
			return gw
		case <-time.After(20 * time.Second):
			t.Fatalf("%s: no session board constructed on resume", label)
		}
		return false
	}

	resumePausedRun(t, svc, botPath, hash, "run-resume-sb-control", nil)
	if watched("control") {
		t.Fatal("control: a run with no rows reads the board GatewayWatched=true, its node runs on anthropic")
	}

	resumePausedRun(t, svc, botPath, hash, "run-resume-sb-routed", rows)
	if !watched("routed") {
		t.Fatalf("resumed session board reads GatewayWatched=false, the run document's rows %+v route node work to the gateway", rows)
	}
}
