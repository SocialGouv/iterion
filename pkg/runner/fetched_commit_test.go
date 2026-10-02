package runner

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
)

// forkHeadFixture is prepareRepoWorkspace's own clone shape for a pull
// request served through the BASE repository's own head ref: one remote
// (`origin` = the base), and the PR's commit reached as refs/pull/<n>/head.
//
// The force-push is performed on the REMOTE, by updating that ref — which is
// exactly what a forge does when a fork contributor force-pushes. Nothing
// here is stubbed: the second fetch is a real fetch, and FETCH_HEAD is git's
// own answer.
type forkHeadFixture struct {
	r      *Runner
	clone  string
	origin string
	prRef  string
	first  string // the commit the launch would have admitted
	second string // what the author replaced it with
}

func newForkHeadFixture(t *testing.T) *forkHeadFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin.git")
	gitOut(t, tmp, "init", "--quiet", "--bare", "--initial-branch=main", origin)

	seed := filepath.Join(tmp, "seed")
	gitOut(t, tmp, "clone", "--quiet", origin, seed)
	gitOut(t, seed, "config", "user.email", "t@test.invalid")
	gitOut(t, seed, "config", "user.name", "t")
	gitOut(t, seed, "commit", "--quiet", "--allow-empty", "-m", "baseline")
	gitOut(t, seed, "push", "--quiet", "origin", "HEAD:main")

	// The contribution, as the base repo serves it.
	gitOut(t, seed, "checkout", "--quiet", "-b", "contribution")
	gitOut(t, seed, "commit", "--quiet", "--allow-empty", "-m", "the reviewed contribution")
	first := gitOut(t, seed, "rev-parse", "HEAD")
	const prRef = "refs/pull/7/head"
	gitOut(t, seed, "push", "--quiet", "origin", "HEAD:"+prRef)

	clone := filepath.Join(tmp, "run")
	gitOut(t, tmp, "clone", "--no-tags", "--quiet", origin, clone)

	// And the replacement the author can publish at any moment after the
	// launch was admitted. Pushed with +, i.e. a force-push.
	gitOut(t, seed, "commit", "--quiet", "--allow-empty", "-m", "what the author swapped in")
	second := gitOut(t, seed, "rev-parse", "HEAD")

	return &forkHeadFixture{
		r:      &Runner{cfg: Config{Logger: iterlog.Nop()}},
		clone:  clone,
		origin: origin,
		prRef:  prRef,
		first:  first,
		second: second,
	}
}

func (f *forkHeadFixture) fetch(t *testing.T) {
	t.Helper()
	gitOut(t, f.clone, "fetch", "--no-tags", "--quiet", "origin", f.prRef)
}

func (f *forkHeadFixture) forcePush(t *testing.T, seedDir string) {
	t.Helper()
	gitOut(t, seedDir, "push", "--quiet", "--force", "origin", f.second+":"+f.prRef)
}

// The admitted commit is pinned at the launch; the fetch happens minutes to
// hours later (the sync debounce alone parks for 3 minutes by default and
// re-arms for ~45). A check on the ref NAME certifies what the name meant
// when it was checked, not what runs.
func TestVerifyFetchedCommit_RefusesACommitSwappedAfterTheAdmission(t *testing.T) {
	t.Run("the admitted commit passes", func(t *testing.T) {
		f := newForkHeadFixture(t)
		f.fetch(t)
		msg := &queue.RunMessage{RunID: "run-a", RepoSHA: f.prRef, RepoSHAExpected: f.first}
		if err := f.r.verifyFetchedCommit(context.Background(), f.clone, msg); err != nil {
			t.Fatalf("verifyFetchedCommit = %v, want nil — the unmodified case must not be refused", err)
		}
	})

	t.Run("a commit swapped under the same ref is refused", func(t *testing.T) {
		f := newForkHeadFixture(t)
		seed := filepath.Join(filepath.Dir(f.clone), "seed")
		f.forcePush(t, seed)
		f.fetch(t) // a REAL fetch, after a REAL force-push
		msg := &queue.RunMessage{RunID: "run-b", RepoSHA: f.prRef, RepoSHAExpected: f.first}
		err := f.r.verifyFetchedCommit(context.Background(), f.clone, msg)
		if err == nil {
			t.Fatal("verifyFetchedCommit = nil after the ref was moved — the run would execute a tree nobody approved")
		}
		// The refusal has to name BOTH commits or an operator cannot tell a
		// force-push from a bug in the pin.
		if !strings.Contains(err.Error(), f.first) || !strings.Contains(err.Error(), f.second) {
			t.Fatalf("refusal = %q, want it to name the admitted commit %s and the one that arrived %s", err, f.first, f.second)
		}
	})

	// Every lane that exists today sets no pin, and must be untouched.
	t.Run("no pin, no comparison", func(t *testing.T) {
		f := newForkHeadFixture(t)
		seed := filepath.Join(filepath.Dir(f.clone), "seed")
		f.forcePush(t, seed)
		f.fetch(t)
		msg := &queue.RunMessage{RunID: "run-c", RepoSHA: f.prRef}
		if err := f.r.verifyFetchedCommit(context.Background(), f.clone, msg); err != nil {
			t.Fatalf("verifyFetchedCommit = %v with no RepoSHAExpected, want nil — existing lanes must keep their exact behaviour", err)
		}
	})

	// If the runner cannot say WHICH commit arrived, it cannot say the right
	// one did. A fresh clone with no fetch has no FETCH_HEAD at all.
	t.Run("an unreadable FETCH_HEAD fails closed", func(t *testing.T) {
		f := newForkHeadFixture(t)
		msg := &queue.RunMessage{RunID: "run-d", RepoSHA: f.prRef, RepoSHAExpected: f.first}
		if err := f.r.verifyFetchedCommit(context.Background(), f.clone, msg); err == nil {
			t.Fatal("verifyFetchedCommit = nil with no FETCH_HEAD to read — an unreadable answer must refuse, not admit")
		}
	})
}

// The pin must not be advertised where it cannot be enforced.
// verifyFetchedCommit is reached only from inside the `RepoSHA != ""` block,
// so a message carrying an admitted commit but NO ref to fetch would run the
// clone's default branch with the guard silently skipped — a run document
// claiming a pin nobody checked. The two fields are copied from independent
// sources at every carrier, so their pairing is an assumption, not an
// invariant; this refuses instead of assuming.
func TestPrepareRepoWorkspace_RefusesAPinItCannotEnforce(t *testing.T) {
	r := &Runner{cfg: Config{Logger: iterlog.Nop()}}
	msg := &queue.RunMessage{
		RunID:           "run-pin-no-ref",
		RepoURL:         "https://github.com/o/r.git",
		RepoSHA:         "", // nothing to fetch
		RepoSHAExpected: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
	}
	_, _, err := r.prepareRepoWorkspace(context.Background(), msg)
	if err == nil {
		t.Fatal("prepareRepoWorkspace = nil for a pin with no ref — the run would execute the clone's default branch while its document advertises an admitted commit")
	}
	if !strings.Contains(err.Error(), "no ref to fetch") {
		t.Fatalf("err = %v, want it to name the missing ref", err)
	}
	// And the refusal must come from THIS guard, before any network: the
	// message names a host this test must never reach.
	if strings.Contains(err.Error(), "not a public address") || strings.Contains(err.Error(), "reject repo url") {
		t.Fatalf("err = %v — the guard has to sit ahead of the transport/SSRF checks so it is decidable offline", err)
	}
}
