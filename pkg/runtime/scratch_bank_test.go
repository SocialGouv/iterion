package runtime

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

// podRun stands in for a sandbox whose ${PROJECT_SCRATCH_DIR} lives and dies
// with it, the kubernetes shape: commands run on the host, the in-sandbox
// scratch path mapped onto this pod's own directory.
type podRun struct {
	sandbox.Run // methods the scratch bank never reaches
	scratch     string
}

func (r *podRun) Driver() string { return "docker" }

func (r *podRun) Command(ctx context.Context, argv []string, _ sandbox.ExecOpts) *exec.Cmd {
	mapped := make([]string, len(argv))
	for i, a := range argv {
		mapped[i] = strings.ReplaceAll(a, sandboxScratchContainerPath, r.scratch)
	}
	return exec.CommandContext(ctx, mapped[0], mapped[1:]...)
}

func (r *podRun) Exec(ctx context.Context, argv []string, opts sandbox.ExecOpts) (sandbox.ExecResult, error) {
	c := r.Command(ctx, argv, opts)
	c.Stdin = opts.Stdin
	return sandbox.ExecCmd(c, opts)
}

func (r *podRun) Cleanup(context.Context) error { return nil }

// podDriver starts a NEW pod — an empty scratch — on every Start, the way a
// resume on kubernetes always does.
type podDriver struct {
	root string
	mu   sync.Mutex
	pods []string
}

func (d *podDriver) Name() string { return "docker" }

func (d *podDriver) Capabilities() sandbox.Capabilities {
	return sandbox.Capabilities{SupportsImage: true, SupportsMounts: true}
}

func (d *podDriver) Prepare(_ context.Context, spec sandbox.Spec) (sandbox.PreparedSpec, error) {
	return stallingPrepared{spec: spec}, nil
}

func (d *podDriver) Start(context.Context, sandbox.PreparedSpec, sandbox.RunInfo) (sandbox.Run, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	pod := filepath.Join(d.root, fmt.Sprintf("pod-%d", len(d.pods)))
	if err := os.MkdirAll(pod, 0o755); err != nil {
		return nil, err
	}
	d.pods = append(d.pods, pod)
	return &podRun{scratch: filepath.Join(pod, "scratch")}, nil
}

// scratch is the live pod's scratch directory.
func (d *podDriver) scratch() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return filepath.Join(d.pods[len(d.pods)-1], "scratch")
}

func scratchWorkflow() *ir.Workflow {
	return &ir.Workflow{
		Name:  "scratch_bank",
		Entry: "measure",
		Nodes: map[string]ir.Node{
			"measure": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "measure"}},
			"gate": &ir.HumanNode{
				BaseNode:          ir.BaseNode{ID: "gate"},
				InteractionFields: ir.InteractionFields{Interaction: ir.InteractionHuman},
			},
			"report": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "report"}},
			"done":   &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{{From: "measure", To: "gate"}, {From: "gate", To: "report"}, {From: "report", To: "done"}},
		// A container-local scratch: the driver has no host bind mounts.
		Sandbox: &ir.SandboxSpec{Mode: "inline", Image: "example.invalid/iterion-sandbox:test", HostState: "none"},
	}
}

func scratchEngine(s store.RunStore, exec *stubExecutor, d *podDriver) *Engine {
	return New(scratchWorkflow(), s, exec,
		WithLogger(iterlog.Nop()),
		WithSandboxDrivers(map[string]sandbox.DriverConstructor{
			"docker": func() (sandbox.Driver, error) { return d, nil },
		}),
	)
}

func eventsOf(t *testing.T, s store.RunStore, runID string, typ store.EventType) []*store.Event {
	t.Helper()
	evs, err := s.LoadEvents(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	var out []*store.Event
	for _, ev := range evs {
		if ev.Type == typ {
			out = append(out, ev)
		}
	}
	return out
}

// TestResume_restoresTheScratchItsParkBanked: a node writes the scratch, the
// run parks, and it resumes in a NEW sandbox whose scratch starts empty.
// The next node reads what the first one wrote: the teardown banked it, the
// resume restored it before the first node ran. The run then finishes, and
// its bank goes with it.
func TestResume_restoresTheScratchItsParkBanked(t *testing.T) {
	t.Setenv("ITERION_MODE", "local") // pin the factory's preference order to docker,podman,noop
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-scratch-bank"
	d := &podDriver{root: t.TempDir()}
	x := newStubExecutor()
	x.on("measure", func(map[string]any) (map[string]any, error) {
		dir := filepath.Join(d.scratch(), "assessment")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		return map[string]any{}, os.WriteFile(filepath.Join(dir, "floor.json"), []byte(`{"files": 31}`), 0o644)
	})
	var read string
	x.on("report", func(map[string]any) (map[string]any, error) {
		b, err := os.ReadFile(filepath.Join(d.scratch(), "assessment", "floor.json"))
		if err != nil {
			return nil, fmt.Errorf("MEASUREMENT_REFUSED: %w", err)
		}
		read = string(b)
		return map[string]any{}, nil
	})

	if err := scratchEngine(s, x, d).Run(ctx, runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("Run: want ErrRunPaused, got %v", err)
	}
	banked := eventsOf(t, s, runID, store.EventSandboxScratchBanked)
	if len(banked) != 1 || banked[0].Data["banked"] != true {
		t.Fatalf("the park did not bank the scratch: %+v", banked)
	}

	if err := scratchEngine(s, x, d).Resume(ctx, runID, map[string]any{"ok": true}); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if len(d.pods) != 2 {
		t.Fatalf("pods started = %d, want 2 — the resume must run in a new sandbox, or this proves nothing", len(d.pods))
	}
	if read != `{"files": 31}` {
		t.Fatalf("the resumed node read %q from the new sandbox's scratch", read)
	}
	if got := eventsOf(t, s, runID, store.EventSandboxScratchRestored); len(got) != 1 {
		t.Fatalf("restorations recorded = %d, want 1", len(got))
	}
	if _, err := store.AsScratchBankStore(s).OpenScratchBank(ctx, runID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a finished run kept its scratch bank: %v", err)
	}
}

// TestResume_refusesARunWhoseScratchWasNotBanked: a teardown that could not
// bank a scratch that held something makes the resume refuse, by name,
// BEFORE it claims the run; --force resumes it without the scratch.
func TestResume_refusesARunWhoseScratchWasNotBanked(t *testing.T) {
	t.Setenv("ITERION_MODE", "local")
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-scratch-unbanked"
	d := &podDriver{root: t.TempDir()}
	x := newStubExecutor()
	x.on("measure", func(map[string]any) (map[string]any, error) { return map[string]any{}, nil })
	x.on("report", func(map[string]any) (map[string]any, error) { return map[string]any{}, nil })
	if err := scratchEngine(s, x, d).Run(ctx, runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("Run: want ErrRunPaused, got %v", err)
	}
	if _, err := s.AppendEvent(ctx, runID, store.Event{Type: store.EventSandboxScratchBanked, Data: map[string]any{
		"banked": false, "empty": false, "reason": "the scratch compresses past the 256 MiB cap",
	}}); err != nil {
		t.Fatal(err)
	}

	err := scratchEngine(s, x, d).Resume(ctx, runID, map[string]any{"ok": true})
	var rt *RuntimeError
	if !errors.As(err, &rt) || rt.Code != ErrCodeScratchNotPortable || !strings.Contains(rt.Message, "past the 256 MiB cap") {
		t.Fatalf("Resume: want SCRATCH_NOT_PORTABLE naming the cause, got %v", err)
	}
	if r, _ := s.LoadRun(ctx, runID); r.Status != store.RunStatusPausedWaitingHuman {
		t.Fatalf("the refused resume claimed the run: status %s", r.Status)
	}

	forced := scratchEngine(s, x, d)
	forced.forceResume = true
	if err := forced.Resume(ctx, runID, map[string]any{"ok": true}); err != nil {
		t.Fatalf("Resume --force: %v", err)
	}
}

// localRun runs a sandbox's commands on the host, its scratch path mapped
// onto dir.
func localRun(dir string) sandbox.Run { return &podRun{scratch: dir} }

// TestBankScratch_emptyDropsAPreviousBank: an empty scratch banks nothing
// and drops the bank of an earlier park, which would otherwise restore a
// state the run has moved past.
func TestBankScratch_emptyDropsAPreviousBank(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-scratch-empty"
	if _, err := s.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatal(err)
	}
	bs := store.AsScratchBankStore(s)
	full := t.TempDir()
	if err := os.WriteFile(filepath.Join(full, "verify.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := bankScratch(ctx, localRun(full), sandboxScratchContainerPath, bs, runID, scratchBankMaxBytes); !got.banked {
		t.Fatalf("a scratch holding a file was not banked: %+v", got)
	}
	got := bankScratch(ctx, localRun(t.TempDir()), sandboxScratchContainerPath, bs, runID, scratchBankMaxBytes)
	if !got.empty || got.banked {
		t.Fatalf("an empty scratch: %+v, want empty and not banked", got)
	}
	if _, err := bs.OpenScratchBank(ctx, runID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the earlier bank survived an empty scratch: %v", err)
	}
}

// TestBankScratch_overTheCapIsNamedNotStored: past the cap nothing is
// uploaded and the reason says so. The payload is random: a compressible
// one would not prove the bound.
func TestBankScratch_overTheCapIsNamedNotStored(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-scratch-over-cap"
	if _, err := s.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	noise := make([]byte, 256<<10)
	if _, err := rand.Read(noise); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "clone.bin"), noise, 0o644); err != nil {
		t.Fatal(err)
	}
	bs := store.AsScratchBankStore(s)
	got := bankScratch(ctx, localRun(dir), sandboxScratchContainerPath, bs, runID, 64<<10)
	if got.banked || !strings.Contains(got.reason, "cap") {
		t.Fatalf("a scratch past the cap: %+v, want not banked, the cap named", got)
	}
	if _, err := bs.OpenScratchBank(ctx, runID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a bank past the cap was stored: %v", err)
	}
}

// TestBankScratch_roundTripsTheTree: nested directories, modes and a link
// come back as they were banked.
func TestBankScratch_roundTripsTheTree(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-scratch-tree"
	if _, err := s.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "sources", "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sources", "app", "Main.java"), []byte("class Main {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "verify.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("sources/app", filepath.Join(src, "app")); err != nil {
		t.Fatal(err)
	}
	bs := store.AsScratchBankStore(s)
	if got := bankScratch(ctx, localRun(src), sandboxScratchContainerPath, bs, runID, scratchBankMaxBytes); !got.banked {
		t.Fatalf("not banked: %+v", got)
	}
	body, err := bs.OpenScratchBank(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	dst := filepath.Join(t.TempDir(), "scratch")
	if err := restoreScratch(ctx, localRun(dst), sandboxScratchContainerPath, body); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "app", "Main.java")); err != nil || string(b) != "class Main {}\n" {
		t.Fatalf("the tree did not come back through its link: %q %v", b, err)
	}
	if fi, err := os.Stat(filepath.Join(dst, "verify.sh")); err != nil || fi.Mode().Perm()&0o100 == 0 {
		t.Fatalf("verify.sh lost its execute bit: %v %v", fi, err)
	}
}

// TestResumeFromFailure_restoresTheScratch: the failure path starts its own
// sandbox, so it restores too — a run that failed after writing its scratch
// resumes in a new sandbox where the failed node reads it back.
func TestResumeFromFailure_restoresTheScratch(t *testing.T) {
	t.Setenv("ITERION_MODE", "local")
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-scratch-failure"
	d := &podDriver{root: t.TempDir()}
	wf := scratchWorkflow()
	delete(wf.Nodes, "gate")
	wf.Edges = []*ir.Edge{{From: "measure", To: "report"}, {From: "report", To: "done"}}
	x := newStubExecutor()
	x.on("measure", func(map[string]any) (map[string]any, error) {
		if err := os.MkdirAll(d.scratch(), 0o755); err != nil {
			return nil, err
		}
		return map[string]any{}, os.WriteFile(filepath.Join(d.scratch(), "verify.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	})
	failOnce := true
	x.on("report", func(map[string]any) (map[string]any, error) {
		if failOnce {
			failOnce = false
			return nil, errors.New("the provider went away")
		}
		if _, err := os.Stat(filepath.Join(d.scratch(), "verify.sh")); err != nil {
			return nil, fmt.Errorf("NO VERIFY SCRIPT: %w", err)
		}
		return map[string]any{}, nil
	})
	eng := func() *Engine {
		return New(wf, s, x, WithLogger(iterlog.Nop()), WithSandboxDrivers(map[string]sandbox.DriverConstructor{
			"docker": func() (sandbox.Driver, error) { return d, nil },
		}))
	}
	if err := eng().Run(ctx, runID, nil); err == nil {
		t.Fatal("the run did not fail on the report node")
	}
	if r, _ := s.LoadRun(ctx, runID); !r.Status.CanOperatorResume() {
		t.Fatalf("status %s is not resumable — this proves nothing", r.Status)
	}
	if err := eng().Resume(ctx, runID, nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if len(d.pods) != 2 {
		t.Fatalf("pods started = %d, want 2", len(d.pods))
	}
}

// TestBankScratchOnCleanup_leavesAHostBackedScratch: a scratch bound to a
// host directory survives its sandbox, so the teardown banks nothing.
func TestBankScratchOnCleanup_leavesAHostBackedScratch(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-scratch-host"
	if _, err := s.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "floor.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := New(scratchWorkflow(), s, newStubExecutor(), WithLogger(iterlog.Nop()))
	var events []store.EventType
	emit := func(t store.EventType, _ map[string]any) error { events = append(events, t); return nil }
	e.bankScratchOnCleanup(runID, &activeSandbox{run: localRun(dir), scratchHostDir: dir}, emit)
	if len(events) != 0 {
		t.Fatalf("a host-backed scratch was banked: %v", events)
	}
	if _, err := store.AsScratchBankStore(s).OpenScratchBank(ctx, runID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a host-backed scratch left a bank: %v", err)
	}
}

// TestResume_aRestoreThatFailsKeepsTheBank: a bank that cannot be extracted
// fails the resume by name, and the failed sandbox's teardown does not
// replace the bank with its own scratch — a later resume retries it.
func TestResume_aRestoreThatFailsKeepsTheBank(t *testing.T) {
	t.Setenv("ITERION_MODE", "local")
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-scratch-bad-bank"
	d := &podDriver{root: t.TempDir()}
	x := newStubExecutor()
	x.on("measure", func(map[string]any) (map[string]any, error) {
		if err := os.MkdirAll(d.scratch(), 0o755); err != nil {
			return nil, err
		}
		return map[string]any{}, os.WriteFile(filepath.Join(d.scratch(), "facts.json"), []byte("{}"), 0o644)
	})
	x.on("report", func(map[string]any) (map[string]any, error) { return map[string]any{}, nil })
	if err := scratchEngine(s, x, d).Run(ctx, runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("Run: want ErrRunPaused, got %v", err)
	}
	bs := store.AsScratchBankStore(s)
	garbage := []byte("not a gzip'd tar")
	if err := bs.PutScratchBank(ctx, runID, strings.NewReader(string(garbage)), int64(len(garbage))); err != nil {
		t.Fatal(err)
	}
	before := len(eventsOf(t, s, runID, store.EventSandboxScratchBanked))

	err := scratchEngine(s, x, d).Resume(ctx, runID, map[string]any{"ok": true})
	var rt *RuntimeError
	if !errors.As(err, &rt) || rt.Code != ErrCodeScratchNotPortable {
		t.Fatalf("Resume on a corrupt bank: want SCRATCH_NOT_PORTABLE, got %v", err)
	}
	if after := len(eventsOf(t, s, runID, store.EventSandboxScratchBanked)); after != before {
		t.Fatalf("the failed restore's teardown banked again (%d → %d events)", before, after)
	}
	body, err := bs.OpenScratchBank(ctx, runID)
	if err != nil {
		t.Fatalf("the bank is gone after a failed restore: %v", err)
	}
	defer body.Close()
	kept := make([]byte, len(garbage)+1)
	if n, _ := body.Read(kept); string(kept[:n]) != string(garbage) {
		t.Fatalf("the bank was replaced after a failed restore: %q", kept[:n])
	}
}

func TestOnlyFileChangedWarnings(t *testing.T) {
	if !onlyFileChangedWarnings("tar: ./a.log: file changed as we read it\ntar: ./b: file changed as we read it\n") {
		t.Error("tar's own changed-file notices were not recognised")
	}
	for _, s := range []string{"", "tar: ./x: Cannot open: Permission denied", "tar: ./a: file changed as we read it\ntar: ./b: Cannot open: Permission denied"} {
		if onlyFileChangedWarnings(s) {
			t.Errorf("%q read as changed-file notices only", s)
		}
	}
}
