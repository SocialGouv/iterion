package runtime

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/iotest"

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

func mustLoadRun(t *testing.T, s store.RunStore, runID string) *store.Run {
	t.Helper()
	r, err := s.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// dataOf is what a list of events carries, for a failure message.
func dataOf(evs []*store.Event) []map[string]any {
	out := make([]map[string]any, len(evs))
	for i, ev := range evs {
		out[i] = ev.Data
	}
	return out
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
	if got := eventsOf(t, s, runID, store.EventSandboxScratchRestored); len(got) != 1 || got[0].Data["stale"] == true {
		t.Fatalf("restorations recorded: %v, want one, not stale — the answered human node finishes before the sandbox starts", dataOf(got))
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
	e.bankScratchOnCleanup(ctx, runID, &activeSandbox{run: localRun(dir), scratchHostDir: dir})
	if got := eventsOf(t, s, runID, store.EventSandboxScratchBanked); len(got) != 0 {
		t.Fatalf("a host-backed scratch was banked: %v", dataOf(got))
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
	if !onlyFileChangedWarnings("tar: ./tmp/part.json: File removed before we read it\ntar: ./a.log: file changed as we read it\n") {
		t.Error("tar's removed-file notice was not recognised")
	}
	for _, s := range []string{"", "tar: ./x: Cannot open: Permission denied", "tar: ./a: file changed as we read it\ntar: ./b: Cannot open: Permission denied"} {
		if onlyFileChangedWarnings(s) {
			t.Errorf("%q read as changed-file notices only", s)
		}
	}
}

// TestResume_aFailedRunRewoundRestoresItsScratch: a failed run comes back
// through a rewind, so its teardown banks like a park's. The rewound node
// reads what the run wrote before it failed — a file the park's bank never
// held.
func TestResume_aFailedRunRewoundRestoresItsScratch(t *testing.T) {
	t.Setenv("ITERION_MODE", "local")
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-scratch-failed-rewound"
	d := &podDriver{root: t.TempDir()}
	wf := scratchWorkflow()
	delete(wf.Nodes, "done")
	wf.Nodes["check"] = &ir.FailNode{BaseNode: ir.BaseNode{ID: "check"}}
	wf.Edges = []*ir.Edge{{From: "measure", To: "gate"}, {From: "gate", To: "report"}, {From: "report", To: "check"}}
	x := newStubExecutor()
	x.on("measure", func(map[string]any) (map[string]any, error) {
		if err := os.MkdirAll(d.scratch(), 0o755); err != nil {
			return nil, err
		}
		return map[string]any{}, os.WriteFile(filepath.Join(d.scratch(), "floor.json"), []byte("{}"), 0o644)
	})
	var seen []string
	x.on("report", func(map[string]any) (map[string]any, error) {
		entries, err := os.ReadDir(d.scratch())
		if err != nil {
			return nil, err
		}
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		seen = append(seen, strings.Join(names, ","))
		return map[string]any{}, os.WriteFile(filepath.Join(d.scratch(), fmt.Sprintf("report-%d.json", len(seen))), []byte("{}"), 0o644)
	})
	eng := func() *Engine {
		return New(wf, s, x, WithLogger(iterlog.Nop()), WithSandboxDrivers(map[string]sandbox.DriverConstructor{
			"docker": func() (sandbox.Driver, error) { return d, nil },
		}))
	}
	if err := eng().Run(ctx, runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("Run: want ErrRunPaused, got %v", err)
	}
	if err := eng().Resume(ctx, runID, map[string]any{"ok": true}); err == nil {
		t.Fatal("the run did not fail at its fail node")
	}
	r, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != store.RunStatusFailed || r.Checkpoint == nil {
		t.Fatalf("status %s, checkpoint %v: want a failed run an operator can rewind — this proves nothing", r.Status, r.Checkpoint)
	}
	if banked := eventsOf(t, s, runID, store.EventSandboxScratchBanked); len(banked) != 2 || banked[1].Data["banked"] != true {
		t.Fatalf("the failed run's teardown did not bank its scratch: %d banked events", len(banked))
	}

	cp := *r.Checkpoint
	cp.NodeID = "report"
	if err := s.FailRunResumable(ctx, runID, &cp, "operator rewound", ""); err != nil {
		t.Fatal(err)
	}
	if err := eng().Resume(ctx, runID, nil); err == nil {
		t.Fatal("the rewound run did not reach its fail node again")
	}
	if len(d.pods) != 3 {
		t.Fatalf("pods started = %d, want 3 — each resume must run in a new sandbox, or this proves nothing", len(d.pods))
	}
	if len(seen) != 2 || !strings.Contains(seen[1], "report-1.json") {
		t.Fatalf("the rewound node read %q: the failed run's scratch did not come back", seen)
	}
}

// TestResume_forceContinuesPastAMissingBank: a bank the last teardown
// recorded but that is gone fails the resume by name; --force continues
// without it, and says so.
func TestResume_forceContinuesPastAMissingBank(t *testing.T) {
	t.Setenv("ITERION_MODE", "local")
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-scratch-missing-bank"
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
		return map[string]any{}, nil
	})
	eng := func(force bool) *Engine {
		e := New(wf, s, x, WithLogger(iterlog.Nop()), WithSandboxDrivers(map[string]sandbox.DriverConstructor{
			"docker": func() (sandbox.Driver, error) { return d, nil },
		}))
		e.forceResume = force
		return e
	}
	if err := eng(false).Run(ctx, runID, nil); err == nil {
		t.Fatal("the run did not fail on the report node")
	}
	if err := store.AsScratchBankStore(s).DeleteScratchBank(ctx, runID); err != nil {
		t.Fatal(err)
	}

	err := eng(false).Resume(ctx, runID, nil)
	var rt *RuntimeError
	if !errors.As(err, &rt) || rt.Code != ErrCodeScratchNotPortable || !strings.Contains(rt.Hint, "--force") {
		t.Fatalf("Resume on a missing bank: want SCRATCH_NOT_PORTABLE naming --force, got %v", err)
	}
	if err := eng(true).Resume(ctx, runID, nil); err != nil {
		t.Fatalf("Resume --force past a missing bank: %v", err)
	}
	restored := eventsOf(t, s, runID, store.EventSandboxScratchRestored)
	if len(restored) != 1 || restored[0].Data["forced"] != true || restored[0].Data["restored"] != false {
		t.Fatalf("the forced resume did not record that it ran without the scratch: %v", dataOf(restored))
	}
}

// failingListRun is a sandbox whose every command exits 1 — a listing whose
// find fails.
type failingListRun struct{ sandbox.Run }

func (failingListRun) Exec(context.Context, []string, sandbox.ExecOpts) (sandbox.ExecResult, error) {
	return sandbox.ExecResult{ExitCode: 1, Stderr: []byte("find: '/tmp/iterion-scratch': Permission denied")}, nil
}

// TestBankScratch_aListingThatFailsKeepsTheBank: a listing that fails is not
// an empty scratch — the previous bank stays and the reason is named — while
// a scratch directory that does not exist is.
func TestBankScratch_aListingThatFailsKeepsTheBank(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-scratch-listing"
	if _, err := s.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatal(err)
	}
	bs := store.AsScratchBankStore(s)
	full := t.TempDir()
	if err := os.WriteFile(filepath.Join(full, "floor.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := bankScratch(ctx, localRun(full), sandboxScratchContainerPath, bs, runID, scratchBankMaxBytes); !got.banked {
		t.Fatalf("not banked: %+v", got)
	}
	got := bankScratch(ctx, failingListRun{}, sandboxScratchContainerPath, bs, runID, scratchBankMaxBytes)
	if got.banked || got.empty || !strings.Contains(got.reason, "listing the scratch exited 1") {
		t.Fatalf("a failing listing: %+v, want not banked, not empty, the exit named", got)
	}
	if _, err := bs.OpenScratchBank(ctx, runID); err != nil {
		t.Fatalf("a failing listing dropped the previous bank: %v", err)
	}
	if got := bankScratch(ctx, localRun(filepath.Join(t.TempDir(), "absent")), sandboxScratchContainerPath, bs, runID, scratchBankMaxBytes); !got.empty || got.reason != "" {
		t.Fatalf("a scratch directory that does not exist: %+v, want empty", got)
	}
}

// unreadableEventsStore fails every event read.
type unreadableEventsStore struct{ store.RunStore }

func (unreadableEventsStore) LoadEvents(context.Context, string) ([]*store.Event, error) {
	return nil, errors.New("the event store is unreachable")
}

func (unreadableEventsStore) ScanEvents(context.Context, string, func(*store.Event) bool) error {
	return errors.New("the event store is unreachable")
}

// TestScratchBank_anUnreadableEventLogIsNotNothingRecorded: events that
// cannot be read never read as "nothing banked". The restore fails — as a
// read to retry, not a deterministic refusal, --force or not — and holds
// the bank, so this sandbox's teardown does not replace it; the pre-claim
// refusals refuse rather than wave the resume through.
func TestScratchBank_anUnreadableEventLogIsNotNothingRecorded(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-scratch-unreadable-events"
	if _, err := s.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatal(err)
	}
	for _, force := range []bool{false, true} {
		e := New(scratchWorkflow(), unreadableEventsStore{s}, newStubExecutor(), WithLogger(iterlog.Nop()))
		e.forceResume = force
		e.activeShare = &SharedSandbox{Run: localRun(t.TempDir())}
		err := e.restoreBankedScratch(ctx, runID)
		var rt *RuntimeError
		if err == nil || errors.As(err, &rt) {
			t.Fatalf("restore on an unreadable event log (force=%v): want an error to retry, got %v", force, err)
		}
		if !e.scratchBankHeld {
			t.Fatalf("the failed restore (force=%v) did not hold the bank: this sandbox's teardown would replace it", force)
		}
	}
	e := New(scratchWorkflow(), unreadableEventsStore{s}, newStubExecutor(), WithLogger(iterlog.Nop()))
	r, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.refuseResumeLosingScratch(ctx, r); err == nil {
		t.Fatal("the scratch refusal waved a resume through an unreadable event log")
	}
	child := *r
	child.ParentRunID = "run-parent"
	if err := e.refuseResumeOfSharedChild(ctx, &child); err == nil {
		t.Fatal("the shared-child refusal waved a resume through an unreadable event log")
	}
}

// bankLessStore keeps no scratch bank.
type bankLessStore struct{ store.RunStore }

// TestBankScratchOnCleanup_aStoreWithoutBankRecordsWhy: a store that keeps
// no bank does not lose a scratch in silence — the teardown records why it
// was not banked, and the resume refuses by name.
func TestBankScratchOnCleanup_aStoreWithoutBankRecordsWhy(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-scratch-no-bank-store"
	if _, err := s.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "floor.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := New(scratchWorkflow(), bankLessStore{s}, newStubExecutor(), WithLogger(iterlog.Nop()))
	e.bankScratchOnCleanup(ctx, runID, &activeSandbox{run: localRun(dir)})
	got := eventsOf(t, s, runID, store.EventSandboxScratchBanked)
	if len(got) != 1 || got[0].Data["banked"] != false || !strings.Contains(fmt.Sprint(got[0].Data["reason"]), "keeps no scratch bank") {
		t.Fatalf("a store without a bank: %v, want one not-banked event naming why", dataOf(got))
	}
	r, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	var rt *RuntimeError
	if err := e.refuseResumeLosingScratch(ctx, r); !errors.As(err, &rt) || rt.Code != ErrCodeScratchNotPortable {
		t.Fatalf("the resume after it: want SCRATCH_NOT_PORTABLE, got %v", err)
	}
}

// strictStore behaves as the cloud store does: a query without the run's
// tenant panics, and a cancelled context fails.
type strictStore struct{ store.RunStore }

func (s strictStore) Unwrap() store.RunStore { return s.RunStore }

func (s strictStore) check(ctx context.Context) error {
	if tenant, ok := store.TenantFromContext(ctx); !ok || tenant == "" {
		panic("store/mongo: tenant-scoped query without tenant in ctx")
	}
	return ctx.Err()
}

func (s strictStore) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	if err := s.check(ctx); err != nil {
		return nil, err
	}
	return s.RunStore.LoadRun(ctx, id)
}

func (s strictStore) AppendEvent(ctx context.Context, runID string, evt store.Event) (*store.Event, error) {
	if err := s.check(ctx); err != nil {
		return nil, err
	}
	return s.RunStore.AppendEvent(ctx, runID, evt)
}

// TestBankScratchOnCleanup_writesUnderTheRunsTenantPastItsCancel: the
// teardown reaches the store with the run's identity — the cloud store
// panics on a tenant-less query — and after the run's context is cancelled,
// which a drain, a lost lease and an operator's cancel all do before it:
// the bank and its record both land.
func TestBankScratchOnCleanup_writesUnderTheRunsTenantPastItsCancel(t *testing.T) {
	s := tmpStore(t)
	const runID = "run-scratch-tenant"
	idCtx := store.WithIdentity(context.Background(), "tenant-1", "owner-1")
	if _, err := s.CreateRun(idCtx, runID, "wf", nil); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "floor.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(idCtx)
	cancel()
	e := New(scratchWorkflow(), strictStore{s}, newStubExecutor(), WithLogger(iterlog.Nop()))
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("the teardown queried the store without the run's tenant: %v", r)
			}
		}()
		e.bankScratchOnCleanup(runCtx, runID, &activeSandbox{run: localRun(dir)})
	}()
	got := eventsOf(t, s, runID, store.EventSandboxScratchBanked)
	if len(got) != 1 || got[0].Data["banked"] != true {
		t.Fatalf("a cancelled run's teardown: %v, want one banked record", dataOf(got))
	}
	if _, err := store.AsScratchBankStore(s).OpenScratchBank(idCtx, runID); err != nil {
		t.Fatalf("a cancelled run's teardown stored no bank: %v", err)
	}
}

// flakyBankStore fails every read of its scratch bank on the way.
type flakyBankStore struct{ store.RunStore }

func (f flakyBankStore) PutScratchBank(ctx context.Context, runID string, body io.Reader, size int64) error {
	return store.AsScratchBankStore(f.RunStore).PutScratchBank(ctx, runID, body, size)
}

func (f flakyBankStore) OpenScratchBank(context.Context, string) (io.ReadCloser, error) {
	return nil, errors.New("blob: GET sessions/run/scratch.tgz: 503 Slow Down")
}

func (f flakyBankStore) DeleteScratchBank(ctx context.Context, runID string) error {
	return store.AsScratchBankStore(f.RunStore).DeleteScratchBank(ctx, runID)
}

// TestResume_aBankReadThatFailsOnTheWayIsRetried: a bank that could not be
// read on the way is not SCRATCH_NOT_PORTABLE — the deterministic code the
// runner acks without redelivering — and --force does not skip it: the bank
// is there, and held for the retry.
func TestResume_aBankReadThatFailsOnTheWayIsRetried(t *testing.T) {
	t.Setenv("ITERION_MODE", "local")
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-scratch-flaky-bank"
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
	// Twice without --force: the first attempt's answered human node
	// finishes before its sandbox starts, and must not read as a run that
	// moved past its bank.
	for _, force := range []bool{false, false, true} {
		before := len(eventsOf(t, s, runID, store.EventSandboxScratchBanked))
		e := scratchEngine(flakyBankStore{s}, x, d)
		e.forceResume = force
		err := e.Resume(ctx, runID, map[string]any{"ok": true})
		var rt *RuntimeError
		if err == nil || (errors.As(err, &rt) && rt.Code == ErrCodeScratchNotPortable) {
			t.Fatalf("Resume on a bank read failing on the way (force=%v): want an error to retry, not %v", force, err)
		}
		if after := len(eventsOf(t, s, runID, store.EventSandboxScratchBanked)); after != before {
			t.Fatalf("the failed restore's teardown (force=%v) banked over the bank (%d → %d events)", force, before, after)
		}
		if r, _ := s.LoadRun(ctx, runID); r.FailureCode == store.FailureScratchNotPortable {
			t.Fatalf("the run was parked SCRATCH_NOT_PORTABLE on a read to retry (force=%v)", force)
		}
		if err := s.SaveRun(ctx, resumable(t, s, runID)); err != nil {
			t.Fatal(err)
		}
	}
}

// resumable puts runID back in the status its pause left, as an operator's
// next attempt finds it.
func resumable(t *testing.T, s store.RunStore, runID string) *store.Run {
	t.Helper()
	r, err := s.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != store.RunStatusPausedWaitingHuman {
		r.Status, r.Error, r.FailureCode = store.RunStatusFailedResumable, "", ""
	}
	return r
}

// TestResume_refusesABankTheRunHasMovedPast: a node finished, in a sandbox
// started after the last teardown banked the scratch, and no teardown banked
// again — the attempt that ran it lost its sandbox without one. Restored,
// the bank would revert what the node wrote: the resume refuses by name
// before claiming the run, and --force restores it anyway, marked stale.
// The human node a resume records as answered ran nowhere: no such attempt.
// Whether that attempt's sandbox_started landed does not matter.
func TestResume_refusesABankTheRunHasMovedPast(t *testing.T) {
	t.Setenv("ITERION_MODE", "local")
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-scratch-stale-bank"
	d := &podDriver{root: t.TempDir()}
	x := newStubExecutor()
	x.on("measure", func(map[string]any) (map[string]any, error) {
		if err := os.MkdirAll(d.scratch(), 0o755); err != nil {
			return nil, err
		}
		return map[string]any{}, os.WriteFile(filepath.Join(d.scratch(), "ledger.json"), []byte(`{"round": 1}`), 0o644)
	})
	x.on("report", func(map[string]any) (map[string]any, error) { return map[string]any{}, nil })
	if err := scratchEngine(s, x, d).Run(ctx, runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("Run: want ErrRunPaused, got %v", err)
	}
	if _, err := s.AppendEvent(ctx, runID, store.Event{Type: store.EventNodeFinished, NodeID: "gate"}); err != nil {
		t.Fatal(err)
	}
	if err := scratchEngine(s, x, d).refuseResumeLosingScratch(ctx, mustLoadRun(t, s, runID)); err != nil {
		t.Fatalf("an answered human node was read as a run past its bank: %v", err)
	}
	if _, err := s.AppendEvent(ctx, runID, store.Event{Type: store.EventNodeFinished, NodeID: "report"}); err != nil {
		t.Fatal(err)
	}

	err := scratchEngine(s, x, d).Resume(ctx, runID, map[string]any{"ok": true})
	var rt *RuntimeError
	if !errors.As(err, &rt) || rt.Code != ErrCodeScratchNotPortable || !strings.Contains(rt.Message, "revert") {
		t.Fatalf("Resume past a stale bank: want SCRATCH_NOT_PORTABLE naming the revert, got %v", err)
	}
	if r, _ := s.LoadRun(ctx, runID); r.Status != store.RunStatusPausedWaitingHuman {
		t.Fatalf("the refused resume claimed the run: status %s", r.Status)
	}
	forced := scratchEngine(s, x, d)
	forced.forceResume = true
	if err := forced.Resume(ctx, runID, map[string]any{"ok": true}); err != nil {
		t.Fatalf("Resume --force: %v", err)
	}
	restored := eventsOf(t, s, runID, store.EventSandboxScratchRestored)
	if len(restored) != 1 || restored[0].Data["stale"] != true {
		t.Fatalf("the forced restore of a stale bank did not say so: %v", dataOf(restored))
	}
}

// TestResume_twoFailedRestoresThenAHealthyOneIsNotRefused: the answered
// human node every pause resume records — on the pause path and on the gate
// replay alike — is not a node that ran past the bank, whatever sandbox
// started before it. Two resumes whose bank read fails on the way, then a
// healthy one without --force: restored, not refused.
func TestResume_twoFailedRestoresThenAHealthyOneIsNotRefused(t *testing.T) {
	t.Setenv("ITERION_MODE", "local")
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-scratch-two-flaky-reads"
	d := &podDriver{root: t.TempDir()}
	x := newStubExecutor()
	x.on("measure", func(map[string]any) (map[string]any, error) {
		if err := os.MkdirAll(d.scratch(), 0o755); err != nil {
			return nil, err
		}
		return map[string]any{}, os.WriteFile(filepath.Join(d.scratch(), "facts.json"), []byte("{}"), 0o644)
	})
	var read bool
	x.on("report", func(map[string]any) (map[string]any, error) {
		_, err := os.Stat(filepath.Join(d.scratch(), "facts.json"))
		read = err == nil
		return map[string]any{}, err
	})
	if err := scratchEngine(s, x, d).Run(ctx, runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("Run: want ErrRunPaused, got %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := scratchEngine(flakyBankStore{s}, x, d).Resume(ctx, runID, map[string]any{"ok": true}); err == nil {
			t.Fatalf("resume %d on a flaky bank read succeeded — this proves nothing", i+1)
		}
		if err := s.SaveRun(ctx, resumable(t, s, runID)); err != nil {
			t.Fatal(err)
		}
	}
	if err := scratchEngine(s, x, d).Resume(ctx, runID, map[string]any{"ok": true}); err != nil {
		t.Fatalf("the healthy resume after two failed reads: %v", err)
	}
	if !read {
		t.Fatal("the healthy resume did not restore the scratch")
	}
	restored := eventsOf(t, s, runID, store.EventSandboxScratchRestored)
	if len(restored) != 1 || restored[0].Data["stale"] == true {
		t.Fatalf("restorations: %v, want one, not stale", dataOf(restored))
	}
}

// cutOffBankStore delivers half of the bank, then a reset connection.
type cutOffBankStore struct{ store.RunStore }

func (c cutOffBankStore) PutScratchBank(ctx context.Context, runID string, body io.Reader, size int64) error {
	return store.AsScratchBankStore(c.RunStore).PutScratchBank(ctx, runID, body, size)
}

func (c cutOffBankStore) OpenScratchBank(ctx context.Context, runID string) (io.ReadCloser, error) {
	body, err := store.AsScratchBankStore(c.RunStore).OpenScratchBank(ctx, runID)
	if err != nil {
		return nil, err
	}
	all, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil {
		return nil, err
	}
	return io.NopCloser(io.MultiReader(bytes.NewReader(all[:len(all)/2]), iotest.ErrReader(errors.New("read tcp: connection reset by peer")))), nil
}

func (c cutOffBankStore) DeleteScratchBank(ctx context.Context, runID string) error {
	return store.AsScratchBankStore(c.RunStore).DeleteScratchBank(ctx, runID)
}

// TestResume_aBankCutOffMidStreamIsRetried: a bank that stops on the way
// makes tar fail on a truncated archive — which is not a bank that does not
// extract. It is a read to retry, --force or not, and the bank is kept: a
// forced run on the half it received would bank that half over it.
func TestResume_aBankCutOffMidStreamIsRetried(t *testing.T) {
	t.Setenv("ITERION_MODE", "local")
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-scratch-cut-off"
	d := &podDriver{root: t.TempDir()}
	noise := make([]byte, 1<<20)
	if _, err := rand.Read(noise); err != nil {
		t.Fatal(err)
	}
	x := newStubExecutor()
	x.on("measure", func(map[string]any) (map[string]any, error) {
		if err := os.MkdirAll(d.scratch(), 0o755); err != nil {
			return nil, err
		}
		return map[string]any{}, os.WriteFile(filepath.Join(d.scratch(), "clone.bin"), noise, 0o644)
	})
	x.on("report", func(map[string]any) (map[string]any, error) { return map[string]any{}, nil })
	if err := scratchEngine(s, x, d).Run(ctx, runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("Run: want ErrRunPaused, got %v", err)
	}
	banked := eventsOf(t, s, runID, store.EventSandboxScratchBanked)
	for _, force := range []bool{false, true} {
		e := scratchEngine(cutOffBankStore{s}, x, d)
		e.forceResume = force
		err := e.Resume(ctx, runID, map[string]any{"ok": true})
		var rt *RuntimeError
		if err == nil || (errors.As(err, &rt) && rt.Code == ErrCodeScratchNotPortable) {
			t.Fatalf("a bank cut off mid-stream (force=%v): want an error to retry, got %v", force, err)
		}
		if got := eventsOf(t, s, runID, store.EventSandboxScratchBanked); len(got) != len(banked) {
			t.Fatalf("a bank cut off mid-stream (force=%v) was banked over: %v", force, dataOf(got))
		}
		if err := s.SaveRun(ctx, resumable(t, s, runID)); err != nil {
			t.Fatal(err)
		}
	}
}

// TestResume_forcePastABankThatDoesNotExtractStartsEmpty: --force past a
// bank tar refuses runs the nodes without the scratch — not on the files an
// extraction left before it stopped.
func TestResume_forcePastABankThatDoesNotExtractStartsEmpty(t *testing.T) {
	t.Setenv("ITERION_MODE", "local")
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-scratch-half-extracted"
	d := &podDriver{root: t.TempDir()}
	noise := make([]byte, 1<<20)
	if _, err := rand.Read(noise); err != nil {
		t.Fatal(err)
	}
	x := newStubExecutor()
	x.on("measure", func(map[string]any) (map[string]any, error) {
		if err := os.MkdirAll(d.scratch(), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(d.scratch(), "a-small.json"), []byte("{}"), 0o644); err != nil {
			return nil, err
		}
		return map[string]any{}, os.WriteFile(filepath.Join(d.scratch(), "z-clone.bin"), noise, 0o644)
	})
	var seen []string
	x.on("report", func(map[string]any) (map[string]any, error) {
		entries, _ := os.ReadDir(d.scratch())
		for _, e := range entries {
			seen = append(seen, e.Name())
		}
		return map[string]any{}, nil
	})
	if err := scratchEngine(s, x, d).Run(ctx, runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("Run: want ErrRunPaused, got %v", err)
	}
	bs := store.AsScratchBankStore(s)
	body, err := bs.OpenScratchBank(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	full, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil {
		t.Fatal(err)
	}
	truncated := full[:len(full)*3/5]
	if err := bs.PutScratchBank(ctx, runID, bytes.NewReader(truncated), int64(len(truncated))); err != nil {
		t.Fatal(err)
	}
	e := scratchEngine(s, x, d)
	e.forceResume = true
	if err := e.Resume(ctx, runID, map[string]any{"ok": true}); err != nil {
		t.Fatalf("Resume --force past a bank that does not extract: %v", err)
	}
	if len(seen) != 0 {
		t.Fatalf("the forced run's node found %v in its scratch, want none", seen)
	}
}

// TestResume_aBankedRunResumedWithoutASandboxIsRefused: a run that banked
// its scratch in a sandbox, resumed on a path that starts none, has nowhere
// to restore it: refused by name — never run without it in silence — and
// --force continues without it.
func TestResume_aBankedRunResumedWithoutASandboxIsRefused(t *testing.T) {
	t.Setenv("ITERION_MODE", "local")
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-scratch-unsandboxed-resume"
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
	unsandboxed := func(force bool) *Engine {
		e := New(scratchWorkflow(), s, x, WithLogger(iterlog.Nop()), WithSandboxOverride("none"))
		e.forceResume = force
		return e
	}
	err := unsandboxed(false).Resume(ctx, runID, map[string]any{"ok": true})
	var rt *RuntimeError
	if !errors.As(err, &rt) || rt.Code != ErrCodeScratchNotPortable || !errors.Is(err, errResumedWithoutSandbox) {
		t.Fatalf("an unsandboxed resume of a banked run: want SCRATCH_NOT_PORTABLE naming the missing sandbox, got %v", err)
	}
	if err := s.SaveRun(ctx, resumable(t, s, runID)); err != nil {
		t.Fatal(err)
	}
	if err := unsandboxed(true).Resume(ctx, runID, map[string]any{"ok": true}); err != nil {
		t.Fatalf("Resume --force without a sandbox: %v", err)
	}
	if restored := eventsOf(t, s, runID, store.EventSandboxScratchRestored); len(restored) != 1 || restored[0].Data["forced"] != true {
		t.Fatalf("the forced unsandboxed resume did not say it ran without the scratch: %v", dataOf(restored))
	}
}

// TestResume_aScratchTheTeardownCouldNotReadIsNotRefused: a teardown that
// cannot list the scratch — the sandbox is already gone, OOM-killed or
// evicted — does not know whether it held anything. The resume goes on
// without it, as the runner's redelivery always did, and says so; a run that
// never wrote the scratch is not stopped over it.
func TestResume_aScratchTheTeardownCouldNotReadIsNotRefused(t *testing.T) {
	t.Setenv("ITERION_MODE", "local")
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-scratch-unlistable"
	d := &podDriver{root: t.TempDir()}
	x := newStubExecutor()
	x.on("measure", func(map[string]any) (map[string]any, error) { return map[string]any{}, nil })
	x.on("report", func(map[string]any) (map[string]any, error) { return map[string]any{}, nil })
	if err := scratchEngine(s, x, d).Run(ctx, runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("Run: want ErrRunPaused, got %v", err)
	}
	e := scratchEngine(s, x, d)
	e.bankScratchOnCleanup(ctx, runID, &activeSandbox{run: failingListRun{}})
	got := eventsOf(t, s, runID, store.EventSandboxScratchBanked)
	if last := got[len(got)-1]; last.Data["unknown"] != true || last.Data["banked"] != false {
		t.Fatalf("an unlistable scratch was recorded %v, want unknown and not banked", last.Data)
	}
	if err := scratchEngine(s, x, d).Resume(ctx, runID, map[string]any{"ok": true}); err != nil {
		t.Fatalf("Resume after a teardown that could not read the scratch: %v", err)
	}
	restored := eventsOf(t, s, runID, store.EventSandboxScratchRestored)
	if len(restored) != 1 || restored[0].Data["restored"] != false || !strings.Contains(fmt.Sprint(restored[0].Data["reason"]), "could not read") {
		t.Fatalf("the resume did not say it started without the scratch: %v", dataOf(restored))
	}
}

// flakyRecordStore refuses the first write of a banked record.
type flakyRecordStore struct {
	store.RunStore
	mu      sync.Mutex
	refused int
}

func (f *flakyRecordStore) Unwrap() store.RunStore { return f.RunStore }

func (f *flakyRecordStore) AppendEvent(ctx context.Context, runID string, evt store.Event) (*store.Event, error) {
	f.mu.Lock()
	refuse := evt.Type == store.EventSandboxScratchBanked && f.refused == 0
	if refuse {
		f.refused++
	}
	f.mu.Unlock()
	if refuse {
		return nil, errors.New("store: write conflict, retry")
	}
	return f.RunStore.AppendEvent(ctx, runID, evt)
}

// TestBankScratchOnCleanup_retriesItsRecord: the record is what a resume
// decides from — a store blip on its write must not leave an exact bank
// behind an older record.
func TestBankScratchOnCleanup_retriesItsRecord(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-scratch-record-retry"
	if _, err := s.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "floor.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	flaky := &flakyRecordStore{RunStore: s}
	New(scratchWorkflow(), flaky, newStubExecutor(), WithLogger(iterlog.Nop())).bankScratchOnCleanup(ctx, runID, &activeSandbox{run: localRun(dir)})
	if got := eventsOf(t, s, runID, store.EventSandboxScratchBanked); flaky.refused != 1 || len(got) != 1 || got[0].Data["banked"] != true {
		t.Fatalf("after one refused write: refused=%d records=%v, want the record written on retry", flaky.refused, dataOf(got))
	}
}

// brokenStreamDriver starts pods whose tar extraction breaks, the way a
// kubectl exec stream does when its connection drops: an exit code, nothing
// on the Go side to tell it from a bad archive.
type brokenStreamDriver struct{ *podDriver }

func (d brokenStreamDriver) Start(ctx context.Context, p sandbox.PreparedSpec, info sandbox.RunInfo) (sandbox.Run, error) {
	run, err := d.podDriver.Start(ctx, p, info)
	if err != nil {
		return nil, err
	}
	return brokenStreamRun{run.(*podRun)}, nil
}

type brokenStreamRun struct{ *podRun }

func (r brokenStreamRun) Exec(ctx context.Context, argv []string, opts sandbox.ExecOpts) (sandbox.ExecResult, error) {
	if strings.Contains(strings.Join(argv, " "), "-xzf") {
		return sandbox.ExecResult{ExitCode: 1, Stderr: []byte("error: unable to upgrade connection: container not found")}, nil
	}
	return r.podRun.Exec(ctx, argv, opts)
}

// TestResume_aStreamThatBreaksInTheSandboxIsRetried: the bank was checked
// on the host, so an extraction that fails in the sandbox is a transport's
// — retried, the bank held — never SCRATCH_NOT_PORTABLE.
func TestResume_aStreamThatBreaksInTheSandboxIsRetried(t *testing.T) {
	t.Setenv("ITERION_MODE", "local")
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-scratch-broken-stream"
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
	before := len(eventsOf(t, s, runID, store.EventSandboxScratchBanked))
	broken := brokenStreamDriver{d}
	e := New(scratchWorkflow(), s, x, WithLogger(iterlog.Nop()), WithSandboxDrivers(map[string]sandbox.DriverConstructor{
		"docker": func() (sandbox.Driver, error) { return broken, nil },
	}))
	err := e.Resume(ctx, runID, map[string]any{"ok": true})
	var rt *RuntimeError
	if err == nil || (errors.As(err, &rt) && rt.Code == ErrCodeScratchNotPortable) {
		t.Fatalf("a stream that broke in the sandbox: want an error to retry, got %v", err)
	}
	if after := len(eventsOf(t, s, runID, store.EventSandboxScratchBanked)); after != before {
		t.Fatalf("the failed restore's teardown banked over the bank (%d → %d)", before, after)
	}
}
