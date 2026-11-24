package runner

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The runner's own lines ride the per-run logger it tees into the
// RunLogStore (loop.go's runLogger), and that handle is the one
// buildExecutor installs this run's secret matcher on: a line the RUNNER
// logs there — not the engine, not the executor — reaches run.log
// scrubbed (iterion#2822).
//
// The carrier here is the launch-input refusal, which quotes the input
// NAMES the wire message carried; in production the carrier on this same
// handle is a supervisor's steer (pkg/supervise) and the subbot
// bookkeeping. The pod runs many runs through one r.cfg.Logger, so the
// per-run handle must be a fork: that is what keeps this run's matcher
// off the next run's lines.
func TestTheRunnersOwnLineOnTheRunLogCrossesTheRunsMatcher(t *testing.T) {
	t.Setenv("ITERION_SANDBOX_DEFAULT", "none")
	const value = "hunter2-9f8e7d6c5b4a"
	ctx := store.WithIdentity(context.Background(), "team-1", "u1")
	root := t.TempDir()
	st, err := store.New(root)
	if err != nil {
		t.Fatal(err)
	}
	const runID = "run-redactor-surface"
	if _, err := st.CreateRun(ctx, runID, "main", nil); err != nil {
		t.Fatal(err)
	}
	src := `vars:
  planted: string = ""

secrets:
  db: "{{vars.planted}}"

workflow main:
  entry: done
`
	pr := parsertest.Parse("main.bot", src)
	body, err := ast.MarshalFile(pr.File)
	if err != nil {
		t.Fatal(err)
	}
	// A real level: the pod's process logger, whose per-run fork is what
	// writes run.log. A Nop logger would drop the very line under test.
	r := &Runner{cfg: Config{Store: st, WorkDir: t.TempDir(), Logger: iterlog.New(iterlog.LevelInfo, io.Discard)}}
	msg := &queue.RunMessage{
		RunID: runID, TenantID: "team-1", OwnerID: "u1", WorkflowName: "main", IRCompiled: body,
		// The declared secret's value, and — as an input name nothing
		// declares — the same value as the line's carrier.
		Vars:               map[string]any{"planted": value, value: "x"},
		AllowUnknownInputs: true,
	}
	if err := r.executeRun(ctx, msg, nil, nil); err != nil {
		t.Fatalf("executeRun: %v", err)
	}

	b, err := os.ReadFile(filepath.Join(root, "runs", runID, "run.log"))
	if err != nil {
		t.Fatalf("read run.log: %v", err)
	}
	got := string(b)
	if !strings.Contains(got, "no var of the workflow") {
		t.Fatalf("the surface's own line never reached run.log, so this test proves nothing about it: %q", got)
	}
	if strings.Contains(got, value) {
		t.Errorf("the runner's own line carries the declared value raw in run.log: %q", got)
	}
	if !strings.Contains(got, secretguard.PlaceholderForName("db")) {
		t.Errorf("run.log shows no placeholder where the value was: %q", got)
	}
}

// storeWithoutRunLogs is a RunStore that keeps no run log: the shape the
// pod meets when store.AsRunLogStore answers nil (a store implementation
// without the optional capability). Shadowing AppendRunLog with another
// signature is what takes the store out of the RunLogStore interface
// while every other method stays the filesystem store's.
type storeWithoutRunLogs struct {
	store.RunStore
}

func (storeWithoutRunLogs) AppendRunLog() {}

// Even with no RunLogStore to tee into, the pod's per-run logger is a
// FORK: the run's secret matcher must not land on r.cfg.Logger, which
// every run of this pod shares — the next run would replace it mid-flight
// and scrub this run's lines with another run's values (iterion#2822).
func TestTheRunnersPodLoggerNeverCarriesARunsMatcher(t *testing.T) {
	t.Setenv("ITERION_SANDBOX_DEFAULT", "none")
	const value = "hunter2-9f8e7d6c5b4a"
	ctx := store.WithIdentity(context.Background(), "team-1", "u1")
	fs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st := storeWithoutRunLogs{RunStore: fs}
	if ls := store.AsRunLogStore(st); ls != nil {
		t.Fatal("scenario broken: this store must keep no run log, so the pod takes its fallback")
	}
	const runID = "run-pod-logger"
	if _, err := fs.CreateRun(ctx, runID, "main", nil); err != nil {
		t.Fatal(err)
	}
	src := `vars:
  planted: string = ""

secrets:
  db: "{{vars.planted}}"

workflow main:
  entry: done
`
	body, err := ast.MarshalFile(parsertest.Parse("main.bot", src).File)
	if err != nil {
		t.Fatal(err)
	}
	var podBuf bytes.Buffer
	pod := iterlog.New(iterlog.LevelInfo, &podBuf)
	r := &Runner{cfg: Config{Store: st, WorkDir: t.TempDir(), Logger: pod}}
	msg := &queue.RunMessage{
		RunID: runID, TenantID: "team-1", OwnerID: "u1", WorkflowName: "main", IRCompiled: body,
		Vars: map[string]any{"planted": value},
	}
	if err := r.executeRun(ctx, msg, nil, nil); err != nil {
		t.Fatalf("executeRun: %v", err)
	}

	podBuf.Reset()
	pod.Warn("pod line: %s", value)
	if got := podBuf.String(); !strings.Contains(got, value) {
		t.Errorf("the run installed its matcher on the pod's own logger, which every run shares: %q", got)
	}
}
