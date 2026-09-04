package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A re-executing repo-targeted run used to start on a bare clone of the
// target branch: the commits its earlier attempt banked (or parked) on the
// forge stayed reachable, but never in the tree the resumed nodes ran on —
// docs-refresh 01a055f9 would have resumed its fourth pass on top of nothing
// after banking three. restoreBankedChain puts the chain back, two-step, so
// the reflog still reads "started from the run's own base".

type restoreFixture struct {
	r      *Runner
	msg    *queue.RunMessage
	ctx    context.Context
	tmp    string
	origin string
	base0  string
	clones int
}

func newRestoreFixture(t *testing.T) *restoreFixture {
	t.Helper()
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin.git")
	gitOut(t, tmp, "init", "--quiet", "--bare", "--initial-branch=main", origin)
	seed := filepath.Join(tmp, "seed")
	gitOut(t, tmp, "clone", "--quiet", origin, seed)
	gitOut(t, seed, "config", "user.email", "t@test.invalid")
	gitOut(t, seed, "config", "user.name", "t")
	gitOut(t, seed, "commit", "--quiet", "--allow-empty", "-m", "baseline")
	gitOut(t, seed, "push", "--quiet", "origin", "HEAD:main")
	base0 := gitOut(t, seed, "rev-parse", "HEAD")

	st, err := store.New(filepath.Join(tmp, "store"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	msg := &queue.RunMessage{RunID: "run-restore-1", TenantID: "team-a", RepoURL: origin, RepoSHA: "main"}
	ctx := store.WithIdentity(context.Background(), "team-a", "")
	if err := st.SaveRun(ctx, &store.Run{ID: msg.RunID, TenantID: msg.TenantID}); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return &restoreFixture{
		r:      &Runner{cfg: Config{Logger: iterlog.Nop(), Store: st}},
		msg:    msg,
		ctx:    ctx,
		tmp:    tmp,
		origin: origin,
		base0:  base0,
	}
}

func (f *restoreFixture) scratchClone(t *testing.T) string {
	t.Helper()
	f.clones++
	dir := filepath.Join(f.tmp, fmt.Sprintf("scratch-%d", f.clones))
	gitOut(t, f.tmp, "clone", "--quiet", f.origin, dir)
	gitOut(t, dir, "config", "user.email", "t@test.invalid")
	gitOut(t, dir, "config", "user.name", "t")
	return dir
}

// pushChain lands n commits on top of origin's main as <ref> — an earlier
// attempt's banked (or parked) work — and returns its tip.
func (f *restoreFixture) pushChain(t *testing.T, ref string, n int) string {
	t.Helper()
	dir := f.scratchClone(t)
	for i := 1; i <= n; i++ {
		gitOut(t, dir, "commit", "--quiet", "--allow-empty", "-m", fmt.Sprintf("docs(pass): commit %d\n\nBot: docs-refresh", i))
	}
	tip := gitOut(t, dir, "rev-parse", "HEAD")
	gitOut(t, dir, "push", "--quiet", "origin", "HEAD:refs/heads/"+ref)
	return tip
}

// advanceMain moves origin's main under the run and returns its new tip.
func (f *restoreFixture) advanceMain(t *testing.T) string {
	t.Helper()
	dir := f.scratchClone(t)
	gitOut(t, dir, "commit", "--quiet", "--allow-empty", "-m", "main moved meanwhile")
	gitOut(t, dir, "push", "--quiet", "origin", "HEAD:main")
	return gitOut(t, dir, "rev-parse", "HEAD")
}

// runnerClone is prepareRepoWorkspace's shape: a fresh clone, then the run's
// ref fetched and checked out as a local branch of the same name.
func (f *restoreFixture) runnerClone(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(f.tmp, "run")
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	gitOut(t, f.tmp, "clone", "--no-tags", "--quiet", f.origin, dir)
	gitOut(t, dir, "fetch", "--no-tags", "--quiet", "origin", "main")
	gitOut(t, dir, "checkout", "--quiet", "-B", "main", "FETCH_HEAD")
	return dir
}

func (f *restoreFixture) restoreEvents(t *testing.T) []*store.Event {
	t.Helper()
	all, err := f.r.cfg.Store.LoadEvents(f.ctx, f.msg.RunID)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	var out []*store.Event
	for _, e := range all {
		if e.Type == store.EventRunWorkspaceBankRestored {
			out = append(out, e)
		}
	}
	return out
}

func (f *restoreFixture) bank(t *testing.T, ref, head string) {
	t.Helper()
	run, err := f.r.cfg.Store.LoadRun(f.ctx, f.msg.RunID)
	if err != nil || run == nil {
		t.Fatalf("load run: %v", err)
	}
	run.FinalBranch, run.FinalCommit = ref, head
	if err := f.r.cfg.Store.SaveRun(f.ctx, run); err != nil {
		t.Fatalf("save run: %v", err)
	}
}

// The target branch moved under the run: the chain is restored on ITS base,
// the run's own commits are what sits ahead of origin/main, and the reflog
// names the base right under the restored head.
func TestRestoreBankedChainOnMovedBase(t *testing.T) {
	f := newRestoreFixture(t)
	ref := "iterion/run-" + f.msg.RunID
	tip := f.pushChain(t, ref, 2)
	f.bank(t, ref, tip)
	moved := f.advanceMain(t)
	dir := f.runnerClone(t)
	if got := gitOut(t, dir, "rev-parse", "HEAD"); got != moved {
		t.Fatalf("fixture: the fresh clone must sit on the moved main (%s), got %s", moved, got)
	}

	base := f.r.restoreBankedChain(f.ctx, f.msg, dir, "", nil)

	if base != f.base0 {
		t.Errorf("restore must report the chain's own base %s as the baseline, got %q", f.base0, base)
	}
	if got := gitOut(t, dir, "rev-parse", "HEAD"); got != tip {
		t.Errorf("HEAD must be the banked head %s, got %s", tip, got)
	}
	if got := gitOut(t, dir, "rev-parse", "--abbrev-ref", "HEAD"); got != "main" {
		t.Errorf("the run's branch name must survive the restore, got %q", got)
	}
	ahead := strings.Fields(gitOut(t, dir, "rev-list", "origin/main..HEAD"))
	if len(ahead) != 2 {
		t.Errorf("exactly the chain's 2 commits must sit ahead of origin/main, got %d: %v", len(ahead), ahead)
	}
	// Two-step checkout: newest reflog entry = the restored head, the one
	// below it = the chain's base — never the moved target branch, which a
	// scope gate reading "the newest entry that is not my own commit" would
	// otherwise take for the run's starting point.
	reflog := strings.Fields(gitOut(t, dir, "reflog", "show", "--format=%H", "HEAD"))
	if len(reflog) < 2 || reflog[0] != tip || reflog[1] != f.base0 {
		t.Errorf("reflog must read [head, base, …] = [%s, %s, …], got %v", tip, f.base0, reflog)
	}
	evs := f.restoreEvents(t)
	if len(evs) != 1 {
		t.Fatalf("want one %s event, got %d", store.EventRunWorkspaceBankRestored, len(evs))
	}
	d := evs[0].Data
	if d["restored"] != true || d["source"] != "bank" || d["ref"] != ref || d["head"] != tip || d["base"] != f.base0 || d["base_moved"] != true || d["from"] != moved {
		t.Errorf("event data: %v", d)
	}
}

// A bank branch that moved past the recorded head is not this run's work
// any more: refuse, keep the fresh clone, say why.
func TestRestoreBankedChainRefusesMovedRef(t *testing.T) {
	f := newRestoreFixture(t)
	ref := "iterion/run-" + f.msg.RunID
	tip := f.pushChain(t, ref, 1)
	f.bank(t, ref, tip)
	// Someone pushed one more commit on the bank branch after the doc was
	// written.
	dir := f.scratchClone(t)
	gitOut(t, dir, "fetch", "--quiet", "origin", "refs/heads/"+ref)
	gitOut(t, dir, "checkout", "--quiet", "-B", "bank", "FETCH_HEAD")
	gitOut(t, dir, "commit", "--quiet", "--allow-empty", "-m", "foreign push")
	gitOut(t, dir, "push", "--quiet", "origin", "HEAD:refs/heads/"+ref)
	run := f.runnerClone(t)
	before := gitOut(t, run, "rev-parse", "HEAD")

	base := f.r.restoreBankedChain(f.ctx, f.msg, run, "", nil)

	if base != "" {
		t.Errorf("a moved ref must not be restored, got baseline %q", base)
	}
	if got := gitOut(t, run, "rev-parse", "HEAD"); got != before {
		t.Errorf("the fresh clone must be left untouched (%s), got %s", before, got)
	}
	evs := f.restoreEvents(t)
	if len(evs) != 1 || evs[0].Data["restored"] != false || evs[0].Data["reason"] != "ref_moved" {
		t.Errorf("want one refusal event with reason ref_moved, got %v", evs)
	}
}

// No terminal bank: the newest parked attempt ref on the timeline is the
// chain to restore (a paused run, an interrupted delivery).
func TestRestoreBankedChainFromParkedRef(t *testing.T) {
	f := newRestoreFixture(t)
	older := f.pushChain(t, "iterion/run-"+f.msg.RunID+"-parked-aaaaaaaaaaaa", 1)
	newer := f.pushChain(t, "iterion/run-"+f.msg.RunID+"-parked-bbbbbbbbbbbb", 3)
	for _, e := range []struct{ ref, head string }{
		{"iterion/run-" + f.msg.RunID + "-parked-aaaaaaaaaaaa", older},
		{"iterion/run-" + f.msg.RunID + "-parked-bbbbbbbbbbbb", newer},
	} {
		if _, err := f.r.cfg.Store.AppendEvent(f.ctx, f.msg.RunID, store.Event{
			Type: store.EventRunBankAttempt,
			Data: map[string]any{"cause": "paused", "ref": e.ref, "head": e.head},
		}); err != nil {
			t.Fatal(err)
		}
	}
	dir := f.runnerClone(t)

	base := f.r.restoreBankedChain(f.ctx, f.msg, dir, "", nil)

	if base != f.base0 {
		t.Errorf("baseline: want %s, got %q", f.base0, base)
	}
	if got := gitOut(t, dir, "rev-parse", "HEAD"); got != newer {
		t.Errorf("the NEWEST parked ref must be restored (%s), got %s", newer, got)
	}
	evs := f.restoreEvents(t)
	if len(evs) != 1 || evs[0].Data["source"] != "parked" || evs[0].Data["base_moved"] != false {
		t.Errorf("event: %v", evs)
	}
}

// A run that never banked anything re-executes exactly as before: no
// restore, no event, fresh clone.
func TestRestoreBankedChainNothingToRestore(t *testing.T) {
	f := newRestoreFixture(t)
	dir := f.runnerClone(t)
	before := gitOut(t, dir, "rev-parse", "HEAD")

	if base := f.r.restoreBankedChain(f.ctx, f.msg, dir, "", nil); base != "" {
		t.Errorf("nothing banked must yield no baseline, got %q", base)
	}
	if got := gitOut(t, dir, "rev-parse", "HEAD"); got != before {
		t.Errorf("HEAD moved from %s to %s with nothing to restore", before, got)
	}
	if evs := f.restoreEvents(t); len(evs) != 0 {
		t.Errorf("no event expected when there is nothing to restore, got %v", evs)
	}
}
