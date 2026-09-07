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

// TestCheckpointOnceDoesNotPushAnUnreadableAnswer pins the guard that came
// with the pair. The script's answer is now THREE fields, so a last line
// that is not one must never be mistaken for the work: until it was a pair,
// any non-empty last line was taken for a sha, and a ref built out of a
// credential helper's chatter is a push that cannot succeed and a state that
// holds nothing. Two properties, and both must survive whatever the
// unreadable tick is later made to SAY about itself: nothing is pushed, and
// `last` — the state that really was preserved — comes out untouched, so the
// next readable tick still compares against it instead of re-pushing.
func TestCheckpointOnceDoesNotPushAnUnreadableAnswer(t *testing.T) {
	for _, tc := range []struct{ name, answer string }{
		{"noise instead of an answer", "fatal: could not read Username for 'https://forge'"},
		{"truncated — the commit is missing", "h1 t1"},
		{"a hook appended to the line", "h1 t1 c0ffee01 and-then-some"},
		{"nothing at all", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := checkpointRunner(t, "R1")
			run := &checkpointRun{shas: []string{tc.answer}}
			o := sandboxObserverOpts{runID: "R1", tenantID: "team-a", checkpoint: true}

			got := r.checkpointWorkspaceOnce(context.Background(), o, run, "h9 t9")
			if got != "h9 t9" {
				t.Fatalf("an unreadable answer replaced the state that WAS preserved: %q", got)
			}
			if p := run.pushes(); len(p) != 0 {
				t.Fatalf("an unreadable answer was pushed as if it were the run's work: %v", p)
			}
		})
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
		cmd := exec.Command("git", append([]string{"-C", ws}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
		return strings.TrimSpace(string(out))
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

	// git stamps a commit to the SECOND, so two back-to-back ticks land on the
	// same sha and the property under test — an unchanged workspace still
	// yields a new commit — would vanish. Production ticks are ten minutes
	// apart: pin BOTH of them to fixed, distinct dates so that gap is
	// simulated rather than waited for. Pinning both (not just the second) is
	// what makes it hermetic: an ambient GIT_AUTHOR_DATE/GIT_COMMITTER_DATE in
	// the test process then cannot collide with a pin and collapse the shas.
	// exec.Cmd resolves duplicate keys last-wins, so appending is enough — and
	// each tick gets its OWN copy, or one tick's pin would leak into another
	// through a shared backing array.
	const (
		tick1Date = "2023-11-14T22:23:20+00:00"
		tick2Date = "2023-11-14T22:33:20+00:00" // ten minutes later: one checkpoint interval
	)
	baseEnv := append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	gitEnvAt := func(date string) []string {
		return append(append([]string(nil), baseEnv...),
			"GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
	}

	sh := exec.Command("sh", "-c", checkpointScript)
	sh.Dir = ws
	sh.Env = gitEnvAt(tick1Date)
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
	sh2.Env = gitEnvAt(tick2Date)
	out2, err2 := sh2.Output()
	if err2 != nil {
		t.Fatalf("second tick: %v (%s)", err2, out2)
	}
	state2, sha2 := checkpointState(string(out2))
	if state2 != state {
		t.Fatalf("an unchanged workspace changed state: %q -> %q", state, state2)
	}
	if sha2 == sha {
		t.Fatalf("a second tick over an unchanged workspace must still yield a NEW commit — that it does is exactly why the comparison is on (HEAD,tree) and not on the commit: %q", sha2)
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
	sh3.Env = baseEnv // a clean tree never reaches commit-tree: no date to pin
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
