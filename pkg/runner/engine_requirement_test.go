package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The production incident this file guards: a bot pushed as a platform
// override used builtins the runner's evaluator did not have. It COMPILED
// (a function call parses generically), then died at its first evaluation,
// and the auto-resume looped on a verdict that could never change.
//
// The runner now reads the bundle's declared engine floor before any node
// executes, and refuses terminally.

// bundleRequiring writes a minimal bundle dir whose manifest carries the
// given `requires:` block (empty = none) and opens it.
func bundleRequiring(t *testing.T, requires string) *bundle.Bundle {
	t.Helper()
	dir := t.TempDir()
	manifest := "name: probe\nversion: 1.6.0\n"
	if requires != "" {
		manifest += "requires:\n  iterion: \"" + requires + "\"\n"
	}
	if err := os.WriteFile(filepath.Join(dir, bundle.MainBotFile), []byte("workflow main:\n  entry: done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, bundle.ManifestFile), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := bundle.OpenDir(dir)
	if err != nil {
		t.Fatalf("open bundle: %v", err)
	}
	return b
}

// pinEngineBuild pins the build the guard compares against. `go test`
// binaries carry appinfo.Version = "dev", which is deliberately unorderable,
// so without this the guard would be permanently inconclusive under test.
func pinEngineBuild(t *testing.T, v string) {
	t.Helper()
	prev := engineBuild
	engineBuild = func() string { return v }
	t.Cleanup(func() { engineBuild = prev })
}

func requirementRun(t *testing.T, id string) store.RunStore {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateRun(context.Background(), id, "main", nil); err != nil {
		t.Fatal(err)
	}
	return st
}

// A bundle whose manifest names an engine floor above this build is refused
// before any node executes, with the typed TERMINAL verdict on the run — not
// a generic execution failure a redelivery would replay.
func TestGuardEngineRequirement_RefusesABundleRequiringANewerEngine(t *testing.T) {
	pinEngineBuild(t, "v3.112.7+abc123def456")
	st := requirementRun(t, "run-req")
	ctx := context.Background()
	r := &Runner{cfg: Config{Store: st, Logger: iterlog.New(iterlog.LevelDebug, nil)}}
	msg := &queue.RunMessage{RunID: "run-req", WorkflowName: "main", BotID: "needy"}

	err := r.guardEngineRequirement(ctx, msg, bundleRequiring(t, ">= 3.112.14"))
	if !errors.Is(err, ErrBotRequiresNewerEngine) {
		t.Fatalf("guard err = %v, want ErrBotRequiresNewerEngine", err)
	}
	if !strings.Contains(err.Error(), ">= 3.112.14") || !strings.Contains(err.Error(), "3.112.7") {
		t.Fatalf("err = %q, want both the floor and the running build named", err)
	}

	run, _ := st.LoadRun(ctx, "run-req")
	if run.Status != store.RunStatusFailed {
		t.Fatalf("status = %q, want failed (NOT failed_resumable: an automatic resume re-queues onto the same fleet and re-reads the same manifest)", run.Status)
	}
	if run.FailureCode != store.FailureBotRequiresNewerEngine {
		t.Fatalf("failure code = %q, want BOT_REQUIRES_NEWER_ENGINE", run.FailureCode)
	}
	if !strings.Contains(run.Error, ">= 3.112.14") {
		t.Fatalf("final error = %q, want the floor it could not meet named", run.Error)
	}
	evs, _ := st.LoadEvents(ctx, "run-req")
	var found bool
	for _, ev := range evs {
		if ev.Type == store.EventRunFailed && ev.Data["code"] == string(store.FailureBotRequiresNewerEngine) {
			found = true
			if ev.Data["runner_version"] == nil || ev.Data["required"] != ">= 3.112.14" {
				t.Fatalf("run_failed must name the build and the requirement: %v", ev.Data)
			}
		}
	}
	if !found {
		t.Fatal("no run_failed {code: BOT_REQUIRES_NEWER_ENGINE}: the refusal is not readable from the run")
	}
}

// The negative case, without which the guard above proves nothing: a bundle
// whose requirement this build MEETS, and one that declares none, both reach
// the engine untouched. A guard that refuses everything is not a guard.
func TestGuardEngineRequirement_AdmitsWhatThisBuildCanRun(t *testing.T) {
	pinEngineBuild(t, "v3.112.14")
	for _, tc := range []struct{ name, requires string }{
		{"requirement met exactly", ">= 3.112.14"},
		{"requirement met with room", ">= 3.100.0"},
		{"no requirement at all", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := requirementRun(t, "run-ok")
			r := &Runner{cfg: Config{Store: st, Logger: iterlog.New(iterlog.LevelDebug, nil)}}
			msg := &queue.RunMessage{RunID: "run-ok", WorkflowName: "main", BotID: "modest"}
			if err := r.guardEngineRequirement(context.Background(), msg, bundleRequiring(t, tc.requires)); err != nil {
				t.Fatalf("guard refused a bundle this build can run: %v", err)
			}
			run, _ := st.LoadRun(context.Background(), "run-ok")
			if run.Status == store.RunStatusFailed {
				t.Fatalf("run was failed although %s", tc.name)
			}
		})
	}
}

// A build with no orderable version (a `dev` build, a fork's naming scheme)
// cannot decide the comparison. It must WARN and proceed — refusing would
// stop every developer build, and passing in silence is the failure mode the
// declaration exists to close.
func TestGuardEngineRequirement_UnorderableBuildWarnsAndProceeds(t *testing.T) {
	pinEngineBuild(t, "dev")
	var logged strings.Builder
	st := requirementRun(t, "run-dev")
	r := &Runner{cfg: Config{Store: st, Logger: iterlog.New(iterlog.LevelWarn, &logged)}}
	msg := &queue.RunMessage{RunID: "run-dev", WorkflowName: "main", BotID: "needy"}

	if err := r.guardEngineRequirement(context.Background(), msg, bundleRequiring(t, ">= 3.112.14")); err != nil {
		t.Fatalf("an inconclusive check refused the run: %v", err)
	}
	if !strings.Contains(logged.String(), "3.112.14") {
		t.Fatalf("the unchecked requirement left no trace in the log: %q", logged.String())
	}
}

// A nil bundle (a loose .bot, an unresolvable bot id) has no manifest and no
// requirement — the guard must not invent one.
func TestGuardEngineRequirement_NoBundleIsNoRequirement(t *testing.T) {
	pinEngineBuild(t, "v3.112.7")
	st := requirementRun(t, "run-none")
	r := &Runner{cfg: Config{Store: st, Logger: iterlog.New(iterlog.LevelDebug, nil)}}
	if err := r.guardEngineRequirement(context.Background(), &queue.RunMessage{RunID: "run-none"}, nil); err != nil {
		t.Fatalf("guard err = %v on a run with no bundle", err)
	}
}

// The WIRING, without which every test above certifies a helper nobody calls:
// executeRun, handed a message whose stored bundle names a newer engine,
// refuses before the engine runs and returns the typed error.
func TestExecuteRun_GuardsTheEngineRequirement(t *testing.T) {
	pinEngineBuild(t, "v3.112.7+abc123def456")
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := st.CreateRun(ctx, "run-wire", "main", nil); err != nil {
		t.Fatal(err)
	}
	mem := botsource.NewMemoryStore()
	created, err := mem.Create(store.WithTenant(ctx, botsource.PlatformTenantID), botsource.BotSource{
		TenantID: botsource.PlatformTenantID,
		Slug:     "needy",
		Files: map[string]string{
			botsource.MainBotFile: "workflow main:\n  entry: done\n",
			"manifest.yaml":       "name: needy\nversion: 1.6.0\nrequires:\n  iterion: \">= 3.112.14\"\n",
		},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	const src = "workflow main:\n  entry: a\n  a -> done\nagent a:\n  backend: \"claw\"\n  model: \"openai/gpt-5.4-mini\"\n  system: sys\nprompt sys:\n  hi\n"
	pr := parser.Parse("main.bot", src)
	body, err := ast.MarshalFile(pr.File)
	if err != nil {
		t.Fatalf("marshal AST: %v", err)
	}
	r := &Runner{cfg: Config{Store: st, BotSources: mem, WorkDir: t.TempDir(), Logger: iterlog.New(iterlog.LevelDebug, nil)}}
	msg := &queue.RunMessage{
		RunID: "run-wire", WorkflowName: "main", BotID: "needy", IRCompiled: body,
		BotBundle: &queue.BotBundleRef{TenantID: botsource.PlatformTenantID, Slug: "needy", Version: created.Version},
	}
	execErr := r.executeRun(ctx, msg, nil)
	if !errors.Is(execErr, ErrBotRequiresNewerEngine) {
		t.Fatalf("executeRun err = %v, want ErrBotRequiresNewerEngine — the guard is not on the path a run takes", execErr)
	}
	run, _ := st.LoadRun(ctx, "run-wire")
	if run.Status != store.RunStatusFailed || run.FailureCode != store.FailureBotRequiresNewerEngine {
		t.Fatalf("run = %s/%s, want failed/BOT_REQUIRES_NEWER_ENGINE written before the return", run.Status, run.FailureCode)
	}
}

// The delivery decision: acked, never naked. A redelivery reaches the same
// image and the same arithmetic — that loop is the incident.
func TestClassifyExecResult_BotRequiresNewerEngineIsAcked(t *testing.T) {
	got := classifyExecResult(errors.New("runner: "+ErrBotRequiresNewerEngine.Error()), "r1")
	if got.action == actionAck && got.finalStatus == "bot_requires_newer_engine" {
		t.Fatal("classifyExecResult matched on the error TEXT — it must match the sentinel, or a reworded message silently re-opens the redelivery loop")
	}
	wrapped := errors.New("wrapped")
	got = classifyExecResult(errors.Join(ErrBotRequiresNewerEngine, wrapped), "r1")
	if got.action != actionAck || got.finalStatus != "bot_requires_newer_engine" {
		t.Fatalf("classified as %s/%v, want bot_requires_newer_engine/ack", got.finalStatus, got.action)
	}
}
