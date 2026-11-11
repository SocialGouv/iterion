package runview

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A launch's run-level backend (LaunchSpec.Backend) replaces the
// workflow's default_backend at dispatch on the in-process lane
// (model.WithDefaultBackend): the screens that lane runs — the launch's
// composed checks and BuildExecutor's — read the node's backend the way
// that dispatch does. The cloud runner never reads it, so the cloud lane's
// screens read the .bot's default.

// runLevelBot is a node that names no backend, under a default_backend
// the cases write.
func runLevelBot(defaultBackend string, extra string) string {
	return `
agent work:
  model: "anthropic/claude-opus-5"
  system: p
  tools: [read_file]
` + extra + `
prompt p:
  Do.

workflow w:
  default_backend: "` + defaultBackend + `"
  entry: work
  work -> done
`
}

const runLevelRoute = "  fallbacks:\n    cheap:\n      model: \"anthropic/claude-sonnet-5\"\n"

// launchRunLevel drives Service.Launch with a run-level backend: on the
// in-process lane (no publisher) or the cloud lane. An in-process launch
// that is admitted runs in a scratch repository and is waited for.
func launchRunLevel(t *testing.T, cloud bool, src, runBackend string, rows []ModelOverrideEntry) (*stubLaunchPublisher, error) {
	t.Helper()
	// An admitted in-process run dispatches: it reads no credential of
	// the host's.
	for _, k := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "OPENAI_API_KEY", "ITERION_DEFAULT_BACKEND"} {
		t.Setenv(k, "")
	}
	t.Setenv("HOME", t.TempDir())
	pub := &stubLaunchPublisher{}
	opts := []ServiceOption{WithLogger(iterlog.Nop()), WithWorkDir(gittest.SourceRepo(t))}
	if cloud {
		opts = append(opts, WithLaunchPublisher(pub))
	}
	svc, err := NewService(t.TempDir(), opts...)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	res, err := svc.Launch(context.Background(), LaunchSpec{FilePath: "w.bot", Source: src, BotID: "w", Backend: runBackend, ModelOverrides: rows})
	if err == nil && !cloud && res != nil {
		select {
		case <-res.Done:
		case <-time.After(2 * time.Minute):
			t.Fatalf("the admitted in-process run %s did not settle", res.RunID)
		}
	}
	return pub, err
}

// requireInProcessRefusal asserts an in-process launch was refused.
func requireInProcessRefusal(t *testing.T, err error, want ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("the in-process launch was admitted, want a refusal naming %q", want)
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error = %q, want it to name %q", err, w)
		}
	}
}

func TestInProcessLaunchScreensReadTheRunLevelBackend(t *testing.T) {
	gwRow := []ModelOverrideEntry{{Selector: "work", Model: gatewayTestModel}}
	t.Run("a run-level claude_code under a claw default is refused a gateway cell", func(t *testing.T) {
		_, err := launchRunLevel(t, false, runLevelBot("claw", ""), "claude_code", gwRow)
		requireInProcessRefusal(t, err, `backend "claude_code" from the launch's run-level backend`, "cannot serve an OpenAI-compatible gateway route")
	})
	t.Run("the node's own backend outranks the run-level backend", func(t *testing.T) {
		src := strings.Replace(runLevelBot("claude_code", ""), "  system: p\n", "  backend: \"claude_code\"\n  system: p\n", 1)
		_, err := launchRunLevel(t, false, src, "claw", gwRow)
		requireInProcessRefusal(t, err, `backend "claude_code" from the node's own backend field`)
	})
}

// The in-process screen admits what dispatch under the run-level backend
// serves: the launch's composed checks (foldAndScreen, the body
// startInProcess runs) and BuildExecutor, whose executor dispatches the
// node and the route on the backends the screens read.
func TestInProcessScreensAdmitWhatTheRunLevelBackendServes(t *testing.T) {
	t.Run("a gateway cell under a run-level claw", func(t *testing.T) {
		src := runLevelBot("claude_code", "")
		wf, path, err := compileRouteCrossingBot(t, src)
		if err != nil {
			t.Fatal(err)
		}
		spec := &LaunchSpec{FilePath: path, Source: src, BotID: "w", Backend: "claw", ModelOverrides: []ModelOverrideEntry{{Selector: "work", Model: gatewayTestModel}}}
		if _, err := foldAndScreen(wf, spec, "claw"); err != nil {
			t.Fatalf("launch screen under run-level claw: %v", err)
		}
		if _, err := foldAndScreen(wf, &LaunchSpec{FilePath: path, Source: src, BotID: "w", ModelOverrides: []ModelOverrideEntry{{Selector: "work", Model: gatewayTestModel}}}, ""); err == nil {
			t.Fatal("control: with no run-level backend the claude_code default must refuse the gateway cell")
		}
	})
	t.Run("the launch's route screen under a run-level claw", func(t *testing.T) {
		src := strings.Replace(runLevelBot("claude_code", runLevelRoute), "  tools: [read_file]\n", "  tools: [read_file]\n  session: persist\n", 1)
		wf, path, err := compileRouteCrossingBot(t, src)
		if err != nil {
			t.Fatal(err)
		}
		moved := &LaunchSpec{FilePath: path, Source: src, BotID: "w", Backend: "claw", ModelOverrides: []ModelOverrideEntry{{Selector: "work#cheap", Backend: "claw"}}}
		if _, err := foldAndScreen(wf, moved, "claw"); err != nil {
			t.Fatalf("a route moved with the run-level claw: %v", err)
		}
		crossed := &LaunchSpec{FilePath: path, Source: src, BotID: "w", Backend: "claw", ModelOverrides: []ModelOverrideEntry{{Selector: "work#cheap", Backend: "claude_code", Model: "claude-sonnet-5"}}}
		if _, err := foldAndScreen(wf, crossed, "claw"); err == nil || !strings.Contains(err.Error(), "routes a claw node to a CLI backend") {
			t.Fatalf("a CLI route under the run-level claw = %v, want the tools inversion refused", err)
		}
	})
	t.Run("a route cell moved with the run-level backend", func(t *testing.T) {
		src := strings.Replace(runLevelBot("claude_code", runLevelRoute), "  tools: [read_file]\n", "  tools: [read_file]\n  session: persist\n", 1)
		rows := []ModelOverrideEntry{{Selector: "work#cheap", Backend: "claw"}}
		exec, wf, err := buildRunLevelExecutor(t, src, "claw", rows, nil)
		if err != nil {
			t.Fatalf("BuildExecutor under run-level claw: %v", err)
		}
		// EffectiveFallbackBackends lists the routes on a backend other
		// than the node's: none, the route runs on the node's claw.
		if node, routes := exec.EffectiveBackendName(wf.Nodes["work"]), exec.EffectiveFallbackBackends(wf.Nodes["work"]); node != "claw" || len(routes) != 0 {
			t.Fatalf("dispatch node=%q other-backend routes=%v, want node and route on claw", node, routes)
		}
		if _, _, err := buildRunLevelExecutor(t, src, "", rows, nil); err == nil || !strings.Contains(err.Error(), "session continuity") {
			t.Fatalf("control: BuildExecutor with no run-level backend = %v, want the session crossing refused", err)
		}
	})
	t.Run("BuildExecutor refuses a route crossing only the run-level backend makes", func(t *testing.T) {
		_, _, err := buildRunLevelExecutor(t, runLevelBot("claude_code", runLevelRoute), "claw",
			[]ModelOverrideEntry{{Selector: "work#cheap", Backend: "claude_code", Model: "claude-sonnet-5"}}, nil)
		if err == nil || !strings.Contains(err.Error(), "routes a claw node to a CLI backend") {
			t.Fatalf("BuildExecutor = %v, want the tools inversion refused", err)
		}
	})
}

// buildRunLevelExecutor builds src's executor under a run-level backend.
func buildRunLevelExecutor(t *testing.T, src, runBackend string, rows []ModelOverrideEntry, tweak func(*ExecutorSpec)) (execOut interface {
	EffectiveBackendName(ir.Node) string
	EffectiveFallbackBackends(ir.Node) []string
}, wf *ir.Workflow, err error) {
	t.Helper()
	wf, _, cerr := compileRouteCrossingBot(t, src)
	if cerr != nil {
		t.Fatal(cerr)
	}
	st, serr := store.New(t.TempDir())
	if serr != nil {
		t.Fatal(serr)
	}
	spec := ExecutorSpec{Ctx: context.Background(), Store: st, RunID: "run-level", Workflow: wf, WorkDir: t.TempDir(),
		Backend: runBackend, ModelOverrides: toModelOverrides(rows), Logger: iterlog.Nop()}
	if tweak != nil {
		tweak(&spec)
	}
	ex, err := BuildExecutor(spec)
	if err != nil {
		return nil, wf, err
	}
	t.Cleanup(func() { _ = ex.Close() })
	return ex, wf, nil
}

// The launch-time fallback and the policy ladder are screened against the
// node's backend under the run-level backend: a CLI stage on a node the
// run puts on claw crosses the tools inversion and is not applied.
func TestBuildExecutorScreensLaunchStagesOnTheRunLevelBackend(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tweak func(*ExecutorSpec)
	}{
		{"run-level fallback", func(s *ExecutorSpec) {
			s.RunFallback = []ir.Fallback{{Backend: "claude_code", Model: "claude-sonnet-5"}}
		}},
		{"policy ladder", func(s *ExecutorSpec) {
			s.PolicyLadder = []ir.PolicyLadderStage{{Harness: "claude_code", Credential: "anthropic_key", On: []string{"usage_window"}}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, row := range []struct {
				runBackend string
				lands      bool
			}{{"claw", false}, {"", true}} {
				_, wf, err := buildRunLevelExecutor(t, runLevelBot("claude_code", ""), row.runBackend, nil, tc.tweak)
				if err != nil {
					t.Fatalf("BuildExecutor: %v", err)
				}
				agent := wf.Nodes["work"].(*ir.AgentNode)
				if landed := len(agent.Fallbacks) == 1; landed != row.lands {
					t.Fatalf("run-level backend %q: stage landed = %v (%+v), want %v", row.runBackend, landed, agent.Fallbacks, row.lands)
				}
			}
		})
	}
}

// The cloud runner never reads the run-level backend: the cloud lane's
// screens read the .bot's default_backend, whatever the launch carries.
func TestCloudLaunchScreensIgnoreTheRunLevelBackend(t *testing.T) {
	gwRow := []ModelOverrideEntry{{Selector: "work", Model: gatewayTestModel}}
	pub, err := launchRunLevel(t, true, runLevelBot("claw", ""), "claude_code", gwRow)
	requireAdmitted(t, pub, err)
	pub, err = launchRunLevel(t, true, runLevelBot("claude_code", ""), "claw", gwRow)
	requireRefusal(t, pub, err, `backend "claude_code" from the workflow's default_backend field`)
	pub, err = launchRunLevel(t, true, runLevelBot("claw", runLevelRoute), "claude_code",
		[]ModelOverrideEntry{{Selector: "work#cheap", Backend: "claude_code", Model: "claude-sonnet-5"}})
	requireRefusal(t, pub, err, "routes a claw node to a CLI backend")
}
