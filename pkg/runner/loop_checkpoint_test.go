package runner

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

// checkpointRunner is a Runner with a real store, so the timeline event
// every checkpoint writes is exercised rather than stubbed: the ref name
// lives in that event and nowhere else.
func checkpointRunner(t *testing.T, runID string) *Runner {
	t.Helper()
	st, err := store.New(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if serr := st.SaveRun(context.Background(), &store.Run{ID: runID, TenantID: "team-a"}); serr != nil {
		t.Fatalf("seed run: %v", serr)
	}
	return &Runner{cfg: Config{Logger: iterlog.Nop(), Store: st}}
}

// checkpointRun fakes a copy-based driver: it records every exec and
// answers the checkpoint script with a scripted sha.
type checkpointRun struct {
	sandbox.Run
	mu     sync.Mutex
	execs  []string
	shas   []string // answers for successive checkpoint-script execs
	pushRC int      // exit code the push exec returns
	pushEr string
	// remote is what `git ls-remote` reports the checkpoint ref already
	// holds; empty means the ref does not exist yet (first generation).
	remote string
	keepRC int // exit code of the fetch+push that preserves it
	keepEr string
	lsRC   int // exit code of the ls-remote that READS the ref
}

func (f *checkpointRun) Driver() string { return "fake-k8s" }

// ExportWorkspace makes this driver copy-based in the eyes of the observer.
func (f *checkpointRun) ExportWorkspace(context.Context) error { return nil }

func (f *checkpointRun) Exec(_ context.Context, cmd []string, _ sandbox.ExecOpts) (sandbox.ExecResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	script := cmd[len(cmd)-1]
	f.execs = append(f.execs, script)
	if strings.HasPrefix(script, "git push") {
		return sandbox.ExecResult{ExitCode: f.pushRC, Stderr: []byte(f.pushEr)}, nil
	}
	// Dispatched BEFORE the scripted-sha fallback: answering ls-remote out of
	// the `shas` queue would silently shift every later tick's answer, which
	// is how adding one exec to the product broke two tests that had nothing
	// to do with it.
	if strings.HasPrefix(script, "git ls-remote") {
		// The real command answers `<sha>\t<ref>`; lsRC makes the READ fail.
		out, rc, errOut := f.remote+"\trefs/heads/x\n", f.lsRC, "fatal: unable to access origin"
		if f.remote == "" {
			out = ""
		}
		if rc != 0 {
			out = ""
		}
		// A REAL `sh -c` is emulated here for the one property that matters:
		// a POSIX pipeline exits with its LAST stage's status, and `cut` exits
		// 0 on empty input. Without this, a fake that reports the FIRST
		// stage's status makes the piped and unpiped forms indistinguishable —
		// and the test that exists to forbid the pipeline passes over it.
		if strings.Contains(script, "| cut") {
			rc, errOut = 0, ""
			if i := strings.IndexByte(out, '\t'); i >= 0 {
				out = out[:i] + "\n"
			}
		}
		return sandbox.ExecResult{ExitCode: rc, Stdout: []byte(out), Stderr: []byte(errOut)}, nil
	}
	if strings.HasPrefix(script, "git fetch") {
		return sandbox.ExecResult{ExitCode: f.keepRC, Stderr: []byte(f.keepEr)}, nil
	}
	answer := "h0 t0 aaaaaaaa"
	if len(f.shas) > 0 {
		answer = f.shas[0]
		f.shas = f.shas[1:]
	}
	return sandbox.ExecResult{Stdout: []byte(answer + "\n")}, nil
}

func (f *checkpointRun) pushes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, e := range f.execs {
		if strings.HasPrefix(e, "git push") {
			out = append(out, e)
		}
	}
	return out
}

// TestCheckpointOncePushesOnlyWhatMoved pins the cadence's cost: a tick
// that finds the same commit as the last one pushes nothing, so a run that
// sits idle for hours costs one exec per tick and no network.
func TestCheckpointOncePushesOnlyWhatMoved(t *testing.T) {
	r := checkpointRunner(t, "R1")
	run := &checkpointRun{shas: []string{"h1 t1 c0ffee", "h1 t1 c0ffee", "h1 t2 beef01"}}
	o := sandboxObserverOpts{runID: "R1", tenantID: "team-a", checkpoint: true}

	last := r.checkpointWorkspaceOnce(context.Background(), o, run, "")
	if last != "h1 t1" || len(run.pushes()) != 1 {
		t.Fatalf("first checkpoint must push: last=%q pushes=%v", last, run.pushes())
	}
	if !strings.Contains(run.pushes()[0], "c0ffee:refs/heads/iterion/run-R1-checkpoint") {
		t.Fatalf("the checkpoint must land on the run's own ref: %q", run.pushes()[0])
	}
	last = r.checkpointWorkspaceOnce(context.Background(), o, run, last)
	if len(run.pushes()) != 1 {
		t.Fatalf("an unchanged tree must not push again: %v", run.pushes())
	}
	last = r.checkpointWorkspaceOnce(context.Background(), o, run, last)
	if last != "h1 t2" || len(run.pushes()) != 2 {
		t.Fatalf("a moved tree must push: last=%q pushes=%v", last, run.pushes())
	}
}

// TestCheckpointOnceComparesTheWorkNotTheCommit pins what "nothing moved"
// means. A checkpoint commit embeds a timestamp, so a dirty tree that has
// STOPPED changing still yields a fresh sha every tick. Comparing the commit
// force-pushed the target repository every ten minutes for identical content
// — and, worse, emitted an event each time: every event re-arms stall
// detection, so a run stuck with a dirty workspace would have read as alive
// forever, the safety net blinding the alarm it was laid beside.
func TestCheckpointOnceComparesTheWorkNotTheCommit(t *testing.T) {
	r := checkpointRunner(t, "R1")
	// Same HEAD, same tree, a different commit sha every tick — exactly what
	// `git commit-tree` produces over an unchanged dirty workspace.
	run := &checkpointRun{shas: []string{"h1 t1 c0ffee01", "h1 t1 c0ffee02", "h1 t1 c0ffee03"}}
	o := sandboxObserverOpts{runID: "R1", tenantID: "team-a", checkpoint: true}

	last := r.checkpointWorkspaceOnce(context.Background(), o, run, "")
	for i := 0; i < 2; i++ {
		last = r.checkpointWorkspaceOnce(context.Background(), o, run, last)
	}
	if n := len(run.pushes()); n != 1 {
		t.Fatalf("an unchanged workspace pushed %d times — the comparison is on the commit, not the work", n)
	}
	// And a moved tree still pushes: the guard must not silence the net.
	// Then a new HEAD over the SAME tree — the run committed exactly what was
	// checkpointed — must push too: the content is already held, but the
	// HISTORY that carries it is what a resume reads. Threaded through the
	// state the previous tick RETURNED, so a state that dropped HEAD would
	// go quiet here instead of pushing.
	run.shas = []string{"h1 t2 c0ffee04", "h2 t2 c0ffee05"}
	last = r.checkpointWorkspaceOnce(context.Background(), o, run, last)
	if len(run.pushes()) != 2 {
		t.Fatalf("a changed tree must still be preserved: %v", run.pushes())
	}
	if r.checkpointWorkspaceOnce(context.Background(), o, run, last); len(run.pushes()) != 3 {
		t.Fatalf("a new commit over an unchanged tree must be preserved: %v", run.pushes())
	}
}

// TestCheckpointOnceKeepsTheLastGoodOnAFailedPush: a failed push must not
// record the sha as preserved, or the next tick would skip it and the work
// would be lost with the pod anyway — silently, which is the failure mode
// this whole file exists against.
func TestCheckpointOnceKeepsTheLastGoodOnAFailedPush(t *testing.T) {
	r := checkpointRunner(t, "R1")
	run := &checkpointRun{shas: []string{"h1 t1 beef01", "h1 t1 beef01"}, pushRC: 128, pushEr: "fatal: could not read Username"}
	o := sandboxObserverOpts{runID: "R1", tenantID: "team-a", checkpoint: true}

	last := r.checkpointWorkspaceOnce(context.Background(), o, run, "")
	if last != "" {
		t.Fatalf("a failed push must not read as preserved, got %q", last)
	}
	if r.checkpointWorkspaceOnce(context.Background(), o, run, last); len(run.pushes()) != 2 {
		t.Fatalf("the next tick must try again: %v", run.pushes())
	}
}

// TestCheckpointOnceIsQuietWhenThereIsNothingToHold: exit 3 is the script
// saying "no repository, or no commit to parent a checkpoint on" — a state,
// not a failure, and it must not push or warn every tick for a whole run.
func TestCheckpointOnceIsQuietWhenThereIsNothingToHold(t *testing.T) {
	var buf bytes.Buffer
	r := checkpointRunner(t, "R1")
	r.cfg.Logger = iterlog.New(iterlog.LevelWarn, &buf)
	run := &nothingToHoldRun{}
	o := sandboxObserverOpts{runID: "R1", tenantID: "team-a"}
	last := r.checkpointWorkspaceOnce(context.Background(), o, run, "")
	if last != "" || run.pushed {
		t.Fatalf("nothing to hold must be a quiet no-op: last=%q pushed=%v", last, run.pushed)
	}
	if buf.Len() != 0 {
		t.Fatalf("a state is not a failure: warning every tick for a whole run would bury the ticks that matter, got %q", buf.String())
	}
	// A real failure to READ the tree, by contrast, is said: that one is
	// the operator's signal that the net is not being laid.
	run.exitCode = 1
	r.checkpointWorkspaceOnce(context.Background(), o, run, "")
	if !strings.Contains(buf.String(), "cannot read the sandbox tree") {
		t.Fatalf("a failed read must be reported, got %q", buf.String())
	}
}

type nothingToHoldRun struct {
	sandbox.Run
	pushed   bool
	exitCode int // 0 = the script's "nothing to hold" (3)
}

func (f *nothingToHoldRun) Driver() string                        { return "fake-k8s" }
func (f *nothingToHoldRun) ExportWorkspace(context.Context) error { return nil }
func (f *nothingToHoldRun) Exec(_ context.Context, cmd []string, _ sandbox.ExecOpts) (sandbox.ExecResult, error) {
	if strings.HasPrefix(cmd[len(cmd)-1], "git push") {
		f.pushed = true
	}
	if f.exitCode != 0 {
		return sandbox.ExecResult{ExitCode: f.exitCode, Stderr: []byte("fatal: not a git repository")}, nil
	}
	return sandbox.ExecResult{ExitCode: 3, Stderr: []byte("no-commit-yet")}, nil
}

// TestCheckpointScriptPreservesTheTreeAndTouchesNothing is the one that
// matters: the script runs against a REAL repository, and the run's own
// history must come out of it byte-identical. A checkpoint that moved HEAD,
// wrote the index, or staged the agent's work would be a runner committing
// on behalf of the party a gate judges.
func TestCheckpointScriptPreservesTheTreeAndTouchesNothing(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	ws := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		return gittest.Run(t, ws, args...)
	}
	git("init", "-q", "-b", "main")
	git("config", "user.email", "lot@run")
	git("config", "user.name", "the lot")
	if err := os.WriteFile(filepath.Join(ws, "committed.txt"), []byte("landed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-qm", "the lot's own commit")
	head := git("rev-parse", "HEAD")

	// Eight hours of uncommitted work, in one file and one staged change.
	if err := os.WriteFile(filepath.Join(ws, "uncommitted.txt"), []byte("the work that would be lost\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "committed.txt"), []byte("landed, then edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "committed.txt")
	statusBefore := git("status", "--porcelain")
	indexBefore := git("rev-parse", ":committed.txt")

	sh := exec.Command("sh", "-c", checkpointScript)
	sh.Dir = ws
	sh.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := sh.Output()
	if err != nil {
		t.Fatalf("checkpoint script: %v (%s)", err, out)
	}
	state, sha := checkpointState(string(out))
	if sha == head || state == "" {
		t.Fatalf("a dirty tree must produce a checkpoint commit, not HEAD: state=%q sha=%q", state, sha)
	}
	if state != head+" "+git("rev-parse", sha+"^{tree}") {
		t.Fatalf("the state must be (HEAD, tree) — what a second tick compares: %q", state)
	}
	// The property the comparison rests on: over an UNCHANGED dirty tree the
	// state is stable while the commit is not (commit-tree stamps a time).
	sh2 := exec.Command("sh", "-c", checkpointScript)
	sh2.Dir = ws
	// git stamps a commit to the SECOND, so two back-to-back ticks land on
	// the same sha and the property would be invisible. Production ticks are
	// ten minutes apart; here the second one's date is pinned, so the
	// difference is deterministic instead of clock-dependent. It was a
	// t.Skip — which calls runtime.Goexit and took the NINE assertions below
	// with it, on essentially every run (measured: 3/3 skipped), turning the
	// guard this test exists for into a green no-op (review finding).
	sh2.Env = append(append([]string(nil), sh.Env...),
		"GIT_AUTHOR_DATE=2023-11-14T22:23:20+00:00",
		"GIT_COMMITTER_DATE=2023-11-14T22:23:20+00:00")
	out2, err2 := sh2.Output()
	if err2 != nil {
		t.Fatalf("second tick: %v (%s)", err2, out2)
	}
	state2, sha2 := checkpointState(string(out2))
	if state2 != state {
		t.Fatalf("an unchanged workspace changed state: %q -> %q", state, state2)
	}
	if sha2 == sha {
		t.Fatalf("a second tick over an unchanged workspace must still yield a NEW commit sha — the state, not the commit, is what a tick may compare")
	}

	// The run's own state: untouched, in all three places it lives.
	if got := git("rev-parse", "HEAD"); got != head {
		t.Fatalf("the checkpoint moved HEAD: %s -> %s", head, got)
	}
	if got := git("status", "--porcelain"); got != statusBefore {
		t.Fatalf("the checkpoint changed the working tree state:\n%s\n---\n%s", statusBefore, got)
	}
	if got := git("rev-parse", ":committed.txt"); got != indexBefore {
		t.Fatalf("the checkpoint wrote the run's index: %s -> %s", indexBefore, got)
	}
	if got := git("branch", "--contains", sha); got != "" {
		t.Fatalf("the checkpoint commit is reachable from a branch: %q", got)
	}

	// And it holds what would have been lost, under iterion's name.
	if got := git("show", sha+":uncommitted.txt"); got != "the work that would be lost" {
		t.Fatalf("the checkpoint does not carry the uncommitted file: %q", got)
	}
	if got := git("show", sha+":committed.txt"); got != "landed, then edited" {
		t.Fatalf("the checkpoint does not carry the edit: %q", got)
	}
	if got := git("log", "-1", "--format=%ae", sha); got != "checkpoint@iterion.invalid" {
		t.Fatalf("a checkpoint must not wear the run's identity, got %q", got)
	}
	if got := git("rev-parse", sha+"^"); got != head {
		t.Fatalf("the checkpoint must be parented on the run's HEAD, got %s", got)
	}

	// A clean tree has nothing uncommitted to hold: the script prints HEAD,
	// so the commits still leave the pod and no empty commit is created.
	git("add", "-A")
	git("commit", "-qm", "the lot commits its work")
	head2 := git("rev-parse", "HEAD")
	sh3 := exec.Command("sh", "-c", checkpointScript)
	sh3.Dir = ws
	sh3.Env = sh.Env
	out3, err := sh3.Output()
	if err != nil {
		t.Fatalf("checkpoint script on a clean tree: %v (%s)", err, out3)
	}
	if _, got := checkpointState(string(out3)); got != head2 {
		t.Fatalf("a clean tree must checkpoint HEAD itself, got %q want %q", got, head2)
	}
}

// TestCheckpointsWorkspaceOnlyWhereWorkCanBeLost pins both gates. The
// first: a bind-mount driver's work is already on the host disk, which
// outlives the container — a checkpoint there preserves nothing and costs
// a push every ten minutes. The second: a subbot child under a SHARED
// sandbox is handed its parent's handle, so a second loop would push the
// same tree under a second name and tell two stories about one workspace.
func TestCheckpointsWorkspaceOnlyWhereWorkCanBeLost(t *testing.T) {
	copyBased := &checkpointRun{}
	bindMount := fakeSandboxRun{}
	owner := sandboxObserverOpts{runID: "R1", checkpoint: true}
	child := sandboxObserverOpts{runID: "R2"}

	if !checkpointsWorkspace(copyBased, owner) {
		t.Fatal("a copy-based sandbox owned by this run must be checkpointed: its work is unreachable until teardown")
	}
	if checkpointsWorkspace(bindMount, owner) {
		t.Fatal("a bind-mount workspace lives on the host and survives the container — nothing to preserve")
	}
	if checkpointsWorkspace(copyBased, child) {
		t.Fatal("a shared child must not open a second loop on its parent's pod")
	}
}

// A checkpoint ref is force-pushed to ONE name per run, so the first push of a
// new runner generation destroys what the previous one left. That is harmless
// when the resume continued the same tree and irreversible when it did not —
// measured 08/09: a sandbox died mid-gate, the workspace export died with it,
// the resume reset the tree to the run's LAUNCH ref, and ten minutes later the
// checkpoint of that reset tree replaced the pre-crash one. The dead attempt's
// work survived only because it was fetched by hand in the gap, out of an
// object already unreferenced and waiting for GC.
func TestCheckpointPreservesWhatANewGenerationWouldErase(t *testing.T) {
	t.Run("a differing ref is copied aside FIRST, then overwritten", func(t *testing.T) {
		r := checkpointRunner(t, "R1")
		run := &checkpointRun{shas: []string{"h9 t9 newsha0"}, remote: "0123456789abcdef0123"}
		o := sandboxObserverOpts{runID: "R1", tenantID: "team-a", checkpoint: true}

		// last == "" is the generation's first push: the destructive one.
		r.checkpointWorkspaceOnce(context.Background(), o, run, "")

		var keep, force int
		var keepBeforeForce bool
		for _, e := range run.execs {
			switch {
			case strings.HasPrefix(e, "git fetch"):
				keep++
				if force == 0 {
					keepBeforeForce = true
				}
			case strings.HasPrefix(e, "git push --force"):
				force++
			}
		}
		if keep != 1 || force != 1 {
			t.Fatalf("want one preserve and one checkpoint push, got keep=%d force=%d: %v", keep, force, run.execs)
		}
		if !keepBeforeForce {
			t.Fatal("the copy must happen BEFORE the force-push — after it, there is nothing left to copy")
		}
		// The fetch is what makes the copy possible at all: the pod is pushing
		// a commit it does not have, because the reset clone never contained
		// the previous generation's checkpoint.
		var kept string
		for _, e := range run.execs {
			if strings.HasPrefix(e, "git fetch") {
				kept = e
			}
		}
		if !strings.Contains(kept, "git fetch --no-tags origin 0123456789abcdef0123") {
			t.Fatalf("the previous commit must be fetched before it can be pushed: %q", kept)
		}
		if !strings.Contains(kept, ":refs/heads/iterion/run-R1-checkpoint-superseded-0123456789ab") {
			t.Fatalf("the copy must land on its own name, distinguishable by name alone: %q", kept)
		}
	})

	t.Run("nothing is copied when there is nothing to lose", func(t *testing.T) {
		for name, run := range map[string]*checkpointRun{
			"first generation — the ref does not exist":        {shas: []string{"h1 t1 c0ffee"}, remote: ""},
			"the ref already holds what we are about to write": {shas: []string{"h1 t1 c0ffee"}, remote: "c0ffee"},
		} {
			r := checkpointRunner(t, "R2")
			o := sandboxObserverOpts{runID: "R2", tenantID: "team-a", checkpoint: true}
			r.checkpointWorkspaceOnce(context.Background(), o, run, "")
			for _, e := range run.execs {
				if strings.HasPrefix(e, "git fetch") {
					t.Fatalf("%s: preserved a ref that was not at risk: %v", name, run.execs)
				}
			}
		}
	})

	t.Run("only the FIRST push of a generation pays for it", func(t *testing.T) {
		r := checkpointRunner(t, "R3")
		run := &checkpointRun{shas: []string{"h1 t1 aaa111", "h1 t2 bbb222"}, remote: "0123456789abcdef0123"}
		o := sandboxObserverOpts{runID: "R3", tenantID: "team-a", checkpoint: true}

		last := r.checkpointWorkspaceOnce(context.Background(), o, run, "")
		last = r.checkpointWorkspaceOnce(context.Background(), o, run, last)
		_ = last

		keeps := 0
		for _, e := range run.execs {
			if strings.HasPrefix(e, "git fetch") {
				keeps++
			}
		}
		if keeps != 1 {
			t.Fatalf("the copy is a per-generation cost, not a per-tick one: %d fetches in %v", keeps, run.execs)
		}
	})

	t.Run("a failed copy does not stop the checkpoint", func(t *testing.T) {
		r := checkpointRunner(t, "R4")
		run := &checkpointRun{shas: []string{"h1 t1 c0ffee"}, remote: "0123456789abcdef0123",
			keepRC: 1, keepEr: "remote rejected"}
		o := sandboxObserverOpts{runID: "R4", tenantID: "team-a", checkpoint: true}

		// Best-effort by construction: a run's outcome is decided by its
		// nodes, never by whether its safety net could be laid.
		if last := r.checkpointWorkspaceOnce(context.Background(), o, run, ""); last != "h1 t1" {
			t.Fatalf("the checkpoint must still land when the copy fails: last=%q", last)
		}
		if len(run.pushes()) != 1 {
			t.Fatalf("want the checkpoint push despite the failed copy: %v", run.pushes())
		}
	})
}

// An UNREADABLE ref is not an ABSENT one, and conflating them destroys the
// very checkpoint this preservation exists to save.
//
// The first version of this probe ran `git ls-remote … | cut -f1`. A POSIX
// pipeline exits with its LAST command's status and `cut` exits 0 on empty
// input, so a dead ls-remote — a network flake, a credential hiccup, exit 128
// — reported success with no output. That read as "the ref does not exist,
// the force-push destroys nothing", in exactly the flaky conditions where a
// resume happens in the first place.
func TestCheckpointDoesNotReadAFailedProbeAsAnEmptyRef(t *testing.T) {
	var buf bytes.Buffer
	r := checkpointRunner(t, "R5")
	r.cfg.Logger = iterlog.New(iterlog.LevelWarn, &buf)
	run := &checkpointRun{shas: []string{"h1 t1 c0ffee"}, remote: "", lsRC: 128}
	o := sandboxObserverOpts{runID: "R5", tenantID: "team-a", checkpoint: true}

	r.checkpointWorkspaceOnce(context.Background(), o, run, "")

	if !strings.Contains(buf.String(), "cannot read") {
		t.Fatalf("a failed read must SAY so — a silent one is indistinguishable from an absent ref: %q", buf.String())
	}
	// And it must still checkpoint: refusing to preserve the new work because
	// the old could not be read trades one loss for another.
	if len(run.pushes()) != 1 {
		t.Fatalf("the checkpoint must still land: %v", run.pushes())
	}
}
