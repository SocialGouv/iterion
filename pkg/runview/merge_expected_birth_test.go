package runview

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/internal/gittest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// mergeBirthSwapStore deletes the run its caller just loaded and recreates a
// successor under the same ID, right after the first LoadRun that follows
// arming returns — or, armed through beforeLoad, right before it reads —
// then delegates unchanged. It records the birth that load handed its caller
// and what the merge exit door answered; it applies no identity policy of its
// own.
type mergeBirthSwapStore struct {
	store.RunStore
	beforeLoad  func()
	afterLoad   func()
	loadedBirth string
	exits       []error
}

func (s *mergeBirthSwapStore) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	if hook := s.beforeLoad; hook != nil {
		s.beforeLoad = nil
		hook()
	}
	r, err := s.RunStore.LoadRun(ctx, id)
	if hook := s.afterLoad; hook != nil {
		s.afterLoad = nil
		if r != nil {
			s.loadedBirth = r.BirthID
		}
		hook()
	}
	return r, err
}

func (s *mergeBirthSwapStore) UpdateRunMergeIf(ctx context.Context, id string, upd store.RunMergeUpdate, from []store.MergeStatus) (bool, error) {
	changed, err := s.RunStore.UpdateRunMergeIf(ctx, id, upd, from)
	s.exits = append(s.exits, err)
	return changed, err
}

// mergeBirthRepo is a repository whose main carries a base commit and whose
// storage branch adds one commit on top of it.
type mergeBirthRepo struct {
	dir, base, branch, storage string
}

func newMergeBirthRepo(t *testing.T) mergeBirthRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo := mergeBirthRepo{dir: filepath.Join(t.TempDir(), "repo"), branch: "iterion/run/birth"}
	if err := os.MkdirAll(repo.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		return strings.TrimSpace(gittest.Run(t, repo.dir, args...))
	}
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo.dir, "file.txt"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q", "-b", "main")
	git("config", "commit.gpgsign", "false")
	// Repo-local identity: the service's own merge commits run without the
	// helper's env.
	git("config", "user.email", "merge-birth@example.invalid")
	git("config", "user.name", "merge-birth")
	write("alpha\n")
	git("add", "file.txt")
	git("commit", "-qm", "base")
	repo.base = git("rev-parse", "HEAD")
	git("checkout", "-qb", repo.branch)
	write("alpha\nbravo\n")
	git("commit", "-qam", "feat")
	repo.storage = git("rev-parse", "HEAD")
	git("checkout", "-q", "main")
	return repo
}

func (r mergeBirthRepo) main(t *testing.T) string {
	t.Helper()
	return strings.TrimSpace(gittest.Run(t, r.dir, "rev-parse", "main"))
}

// seedMergeBirthRun writes a finished run under id whose merge is pending on
// repo, with finalCommit on finalBranch, and returns it as stored.
func seedMergeBirthRun(t *testing.T, native store.RunStore, ctx context.Context, id string, repo mergeBirthRepo, finalCommit, finalBranch string) *store.Run {
	t.Helper()
	if _, err := native.CreateRun(ctx, id, "wf", nil); err != nil {
		t.Fatal(err)
	}
	r, err := native.LoadRun(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	r.Worktree = true
	r.RepoRoot, r.WorkDir = repo.dir, repo.dir
	r.BaseCommit, r.FinalCommit, r.FinalBranch = repo.base, finalCommit, finalBranch
	r.MergeStatus = store.MergeStatusPending
	r.MergeStrategy = store.MergeStrategySquash
	r.Status = store.RunStatusFinished
	if err = native.SaveRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	if r, err = native.LoadRun(ctx, id); err != nil {
		t.Fatal(err)
	}
	if r.BirthID == "" || r.MergeStatus != store.MergeStatusPending {
		t.Fatalf("fixture: seeded run birth=%q merge_status=%q", r.BirthID, r.MergeStatus)
	}
	return r
}

// replaceMergeBirthRun deletes id and recreates it as a successor with the
// same merge-relevant fields and a fresh birth, which it returns.
func replaceMergeBirthRun(t *testing.T, native snapshotOriginNative, ctx context.Context, id string, repo mergeBirthRepo, finalCommit, finalBranch string) string {
	t.Helper()
	if err := native.DeleteRun(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := native.PruneDeletionMarkers(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	return seedMergeBirthRun(t, native, ctx, id, repo, finalCommit, finalBranch).BirthID
}

// requireSuccessorUntouched reloads id from the native store and requires the
// successor born as birth to hold exactly the pending merge it was born with.
func requireSuccessorUntouched(t *testing.T, native store.RunStore, ctx context.Context, id, birth, original string) {
	t.Helper()
	b, err := native.LoadRun(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if b.BirthID != birth || birth == original {
		t.Fatalf("fixture: stored birth %q, successor %q, original %q", b.BirthID, birth, original)
	}
	if b.MergeStatus != store.MergeStatusPending || b.MergedCommit != "" || b.MergedInto != "" || !b.MergeClaimedAt.IsZero() {
		t.Fatalf("the successor's merge state changed: merge_status=%q merged_commit=%q merged_into=%q merge_claimed_at=%v",
			b.MergeStatus, b.MergedCommit, b.MergedInto, b.MergeClaimedAt)
	}
}

// mergeBirthReconcileScenario: SnapshotCtx loads A, whose pending merge
// already landed out of band; B replaces A under the same ID before the
// reconciliation writes. The write is anchored to A's birth, so B keeps its
// pending merge and the exit door answers ErrRunDataIdentityChanged.
func mergeBirthReconcileScenario(t *testing.T, native snapshotOriginNative, ctx context.Context) {
	repo := newMergeBirthRepo(t)
	a := seedMergeBirthRun(t, native, ctx, "m", repo, repo.base, "main")
	spy := &mergeBirthSwapStore{RunStore: native}
	svc, err := NewService("", WithLogger(iterlog.Nop()), WithStore(spy))
	if err != nil {
		t.Fatal(err)
	}
	// Armed after NewService: its own startup loads must not consume the swap.
	var successor string
	spy.afterLoad = func() { successor = replaceMergeBirthRun(t, native, ctx, "m", repo, repo.base, "main") }
	_, snapErr := svc.SnapshotCtx(ctx, "m")
	if spy.loadedBirth != a.BirthID {
		t.Fatalf("fixture: the swap followed a load of birth %q, want the original %q", spy.loadedBirth, a.BirthID)
	}
	requireSuccessorUntouched(t, native, ctx, "m", successor, a.BirthID)
	if len(spy.exits) != 1 {
		t.Fatalf("fixture: the reconciliation reached the merge writer %d times, want 1", len(spy.exits))
	}
	if !errors.Is(spy.exits[0], store.ErrRunDataIdentityChanged) {
		t.Fatalf("merge exit on the successor = %v, want ErrRunDataIdentityChanged", spy.exits[0])
	}
	if snapErr != nil {
		t.Fatalf("snapshot of the successor: %v", snapErr)
	}
}

// mergeBirthPerformScenario: PerformMergeCtx loads A, B replaces A under the
// same ID before the claim. The claim is anchored to A's birth, so B is never
// claimed, no git work runs and the call answers ErrRunDataIdentityChanged.
func mergeBirthPerformScenario(t *testing.T, native snapshotOriginNative, ctx context.Context) {
	repo := newMergeBirthRepo(t)
	a := seedMergeBirthRun(t, native, ctx, "m", repo, repo.storage, repo.branch)
	spy := &mergeBirthSwapStore{RunStore: native}
	svc, err := NewService("", WithLogger(iterlog.Nop()), WithStore(spy))
	if err != nil {
		t.Fatal(err)
	}
	// Armed after NewService: its own startup loads must not consume the swap.
	var successor string
	spy.afterLoad = func() { successor = replaceMergeBirthRun(t, native, ctx, "m", repo, repo.storage, repo.branch) }
	_, mergeErr := svc.PerformMergeCtx(ctx, "m", MergeRequest{})
	if spy.loadedBirth != a.BirthID {
		t.Fatalf("fixture: the swap followed a load of birth %q, want the original %q", spy.loadedBirth, a.BirthID)
	}
	requireSuccessorUntouched(t, native, ctx, "m", successor, a.BirthID)
	if head := repo.main(t); head != repo.base {
		t.Fatalf("git work ran for a successor: main moved %s -> %s", repo.base, head)
	}
	if !errors.Is(mergeErr, store.ErrRunDataIdentityChanged) {
		t.Fatalf("PerformMergeCtx on a successor = %v, want ErrRunDataIdentityChanged", mergeErr)
	}
}

// mergeBirthPerformExpectScenario: the caller decided on A and hands
// PerformMergeCtx A's birth; B replaces A before the service loads the ID. The
// load finds B and the expectation refuses it before any claim: B keeps its
// pending merge, no git work runs and the call answers
// ErrRunDataIdentityChanged.
func mergeBirthPerformExpectScenario(t *testing.T, native snapshotOriginNative, ctx context.Context) {
	repo := newMergeBirthRepo(t)
	a := seedMergeBirthRun(t, native, ctx, "m", repo, repo.storage, repo.branch)
	spy := &mergeBirthSwapStore{RunStore: native}
	svc, err := NewService("", WithLogger(iterlog.Nop()), WithStore(spy))
	if err != nil {
		t.Fatal(err)
	}
	// Armed after NewService: its own startup loads must not consume the swap.
	var successor string
	spy.beforeLoad = func() { successor = replaceMergeBirthRun(t, native, ctx, "m", repo, repo.storage, repo.branch) }
	_, mergeErr := svc.PerformMergeCtx(ctx, "m", MergeRequest{ExpectBirth: store.ExpectBirthOf(a)})
	if successor == "" {
		t.Fatalf("fixture: the swap never ran (err %v)", mergeErr)
	}
	requireSuccessorUntouched(t, native, ctx, "m", successor, a.BirthID)
	if head := repo.main(t); head != repo.base {
		t.Fatalf("A's merge decision merged successor B: main moved %s -> %s", repo.base, head)
	}
	if len(spy.exits) != 0 {
		t.Fatalf("the merge reached the exit door %d times for a successor, want never", len(spy.exits))
	}
	if !errors.Is(mergeErr, store.ErrRunDataIdentityChanged) {
		t.Fatalf("PerformMergeCtx expecting A's birth on successor B = %v, want ErrRunDataIdentityChanged", mergeErr)
	}
}

// seedConflictedMergeBirthRun seeds id as seedMergeBirthRun does, then parks
// its merge in "conflicted" with a pending squash message.
func seedConflictedMergeBirthRun(t *testing.T, native store.RunStore, ctx context.Context, id string, repo mergeBirthRepo) *store.Run {
	t.Helper()
	r := seedMergeBirthRun(t, native, ctx, id, repo, repo.storage, repo.branch)
	r.MergeStatus = store.MergeStatusConflicted
	r.PendingMergeMessage, r.PendingMergeInto = "squash msg", "main"
	if err := native.SaveRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	r, err := native.LoadRun(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if r.MergeStatus != store.MergeStatusConflicted {
		t.Fatalf("fixture: seeded run merge_status=%q", r.MergeStatus)
	}
	return r
}

// replaceConflictedMergeBirthRun deletes id and recreates it as a conflicted
// successor with a fresh birth, which it returns.
func replaceConflictedMergeBirthRun(t *testing.T, native snapshotOriginNative, ctx context.Context, id string, repo mergeBirthRepo) string {
	t.Helper()
	if err := native.DeleteRun(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := native.PruneDeletionMarkers(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	return seedConflictedMergeBirthRun(t, native, ctx, id, repo).BirthID
}

// requireConflictedSuccessorUntouched requires the successor born as birth to
// still hold the conflicted merge it was born with.
func requireConflictedSuccessorUntouched(t *testing.T, native store.RunStore, ctx context.Context, id, birth, original string) {
	t.Helper()
	b, err := native.LoadRun(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if b.BirthID != birth || birth == original {
		t.Fatalf("fixture: stored birth %q, successor %q, original %q", b.BirthID, birth, original)
	}
	if b.MergeStatus != store.MergeStatusConflicted || b.PendingMergeMessage != "squash msg" || b.MergedCommit != "" || b.MergedInto != "" {
		t.Fatalf("A's conflict exit landed on successor B: merge_status=%q pending_msg=%q merged_commit=%q merged_into=%q",
			b.MergeStatus, b.PendingMergeMessage, b.MergedCommit, b.MergedInto)
	}
}

// mergeBirthAbortScenario: AbortMergeConflict loads conflicted A, B (also
// conflicted) replaces A before the abort persists. The exit is anchored to
// A's birth: B keeps its conflict and the call answers
// ErrRunDataIdentityChanged.
func mergeBirthAbortScenario(t *testing.T, native snapshotOriginNative, ctx context.Context) {
	repo := newMergeBirthRepo(t)
	a := seedConflictedMergeBirthRun(t, native, ctx, "m", repo)
	spy := &mergeBirthSwapStore{RunStore: native}
	svc, err := NewService("", WithLogger(iterlog.Nop()), WithStore(spy))
	if err != nil {
		t.Fatal(err)
	}
	var successor string
	spy.afterLoad = func() { successor = replaceConflictedMergeBirthRun(t, native, ctx, "m", repo) }
	abortErr := svc.AbortMergeConflict(ctx, "m")
	if spy.loadedBirth != a.BirthID {
		t.Fatalf("fixture: the swap followed a load of birth %q, want the original %q", spy.loadedBirth, a.BirthID)
	}
	requireConflictedSuccessorUntouched(t, native, ctx, "m", successor, a.BirthID)
	if !errors.Is(abortErr, store.ErrRunDataIdentityChanged) {
		t.Fatalf("AbortMergeConflict on a successor = %v, want ErrRunDataIdentityChanged", abortErr)
	}
}

// mergeBirthFinalizeScenario: the squash of A's branch is staged and
// FinalizeMergeAfterConflict loads conflicted A; B (also conflicted) replaces
// A before the success persists. The git commit lands, the record does not:
// the exit is anchored to A's birth, B keeps its conflict and the call answers
// ErrRunDataIdentityChanged.
func mergeBirthFinalizeScenario(t *testing.T, native snapshotOriginNative, ctx context.Context) {
	repo := newMergeBirthRepo(t)
	gittest.Run(t, repo.dir, "merge", "--squash", repo.branch)
	a := seedConflictedMergeBirthRun(t, native, ctx, "m", repo)
	spy := &mergeBirthSwapStore{RunStore: native}
	svc, err := NewService("", WithLogger(iterlog.Nop()), WithStore(spy))
	if err != nil {
		t.Fatal(err)
	}
	var successor string
	spy.afterLoad = func() { successor = replaceConflictedMergeBirthRun(t, native, ctx, "m", repo) }
	_, finalizeErr := svc.FinalizeMergeAfterConflict(ctx, "m", "")
	if spy.loadedBirth != a.BirthID {
		t.Fatalf("fixture: the swap followed a load of birth %q, want the original %q", spy.loadedBirth, a.BirthID)
	}
	if head := repo.main(t); head == repo.base {
		t.Fatalf("fixture: the staged squash was never committed (err %v)", finalizeErr)
	}
	requireConflictedSuccessorUntouched(t, native, ctx, "m", successor, a.BirthID)
	if !errors.Is(finalizeErr, store.ErrRunDataIdentityChanged) {
		t.Fatalf("FinalizeMergeAfterConflict on a successor = %v, want ErrRunDataIdentityChanged", finalizeErr)
	}
}

// commitFinalizeSwapStore arms on a successful SaveRun and, on the LoadRun
// that follows (CommitAndFinalizeCtx's re-read), replaces the run by a
// successor — before the read when beforeRead, else after it returns. It
// records what the merge exit door answered and applies no identity policy
// of its own.
type commitFinalizeSwapStore struct {
	store.RunStore
	beforeRead bool
	swap       func()
	armed      bool
	exits      []error
}

func (s *commitFinalizeSwapStore) SaveRun(ctx context.Context, r *store.Run) error {
	err := s.RunStore.SaveRun(ctx, r)
	if err == nil && s.swap != nil {
		s.armed = true
	}
	return err
}

func (s *commitFinalizeSwapStore) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	if !s.armed {
		return s.RunStore.LoadRun(ctx, id)
	}
	s.armed = false
	hook := s.swap
	s.swap = nil
	if s.beforeRead {
		hook()
		return s.RunStore.LoadRun(ctx, id)
	}
	r, err := s.RunStore.LoadRun(ctx, id)
	hook()
	return r, err
}

func (s *commitFinalizeSwapStore) UpdateRunMergeIf(ctx context.Context, id string, upd store.RunMergeUpdate, from []store.MergeStatus) (bool, error) {
	changed, err := s.RunStore.UpdateRunMergeIf(ctx, id, upd, from)
	s.exits = append(s.exits, err)
	return changed, err
}

// commitFinalizeScenario: CommitAndFinalizeCtx commits A's dirty worktree and
// finalizes A (skipped), then re-reads the ID; B, finalized branch-only
// (skipped) too, replaces A before that re-read (beforeRead) or right after
// it, before the skipped→pending flip. Either way A's flip never lands on B:
// the re-read refuses a birth other than A's, the flip's exit is anchored to
// A's birth, and the call answers ErrRunDataIdentityChanged.
func commitFinalizeScenario(t *testing.T, native snapshotOriginNative, ctx context.Context, beforeRead bool) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root := t.TempDir()
	repo, wt := filepath.Join(root, "repo"), filepath.Join(root, "wt")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(dir string, args ...string) string {
		t.Helper()
		return strings.TrimSpace(gittest.Run(t, dir, args...))
	}
	git(repo, "init", "-q", "-b", "main")
	git(repo, "config", "commit.gpgsign", "false")
	git(repo, "config", "user.email", "commit-finalize@example.invalid")
	git(repo, "config", "user.name", "commit-finalize")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(repo, "add", "f.txt")
	git(repo, "commit", "-qm", "base")
	base := git(repo, "rev-parse", "HEAD")
	git(repo, "worktree", "add", "-q", "--detach", wt, base)
	if err := os.WriteFile(filepath.Join(wt, "f.txt"), []byte("a\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	const id = "cf"
	seed := func(merge store.MergeStatus, finalBranch, finalCommit string) *store.Run {
		t.Helper()
		if _, err := native.CreateRun(ctx, id, "wf", nil); err != nil {
			t.Fatal(err)
		}
		r, err := native.LoadRun(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		r.Worktree = true
		r.RepoRoot, r.WorkDir, r.BaseCommit = repo, wt, base
		r.FinalBranch, r.FinalCommit = finalBranch, finalCommit
		r.MergeStatus = merge
		r.Status = store.RunStatusFinished
		if err := native.SaveRun(ctx, r); err != nil {
			t.Fatal(err)
		}
		if r, err = native.LoadRun(ctx, id); err != nil {
			t.Fatal(err)
		}
		return r
	}
	spy := &commitFinalizeSwapStore{RunStore: native, beforeRead: beforeRead}
	svc, err := NewService("", WithLogger(iterlog.Nop()), WithStore(spy))
	if err != nil {
		t.Fatal(err)
	}
	// Seeded after NewService: its startup recovery must not finalize A.
	a := seed("", "", "")
	var successor *store.Run
	spy.swap = func() {
		if err := native.DeleteRun(ctx, id); err != nil {
			t.Fatal(err)
		}
		if _, err := native.PruneDeletionMarkers(ctx, time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		successor = seed(store.MergeStatusSkipped, "iterion/run/other", base)
	}
	_, cfErr := svc.CommitAndFinalizeCtx(ctx, id, "commit and finalize")
	if successor == nil {
		t.Fatalf("fixture: the swap never ran (err %v)", cfErr)
	}
	if successor.BirthID == a.BirthID || successor.MergeStatus != store.MergeStatusSkipped {
		t.Fatalf("fixture: successor birth %q (A %q) merge_status %q", successor.BirthID, a.BirthID, successor.MergeStatus)
	}
	if !beforeRead && len(spy.exits) != 1 {
		t.Fatalf("fixture: the flip reached the exit door %d times, want once", len(spy.exits))
	}
	b, err := native.LoadRun(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if b.BirthID != successor.BirthID || b.MergeStatus != store.MergeStatusSkipped {
		t.Fatalf("A's commit-and-finalize flip landed on successor B: birth %q merge_status %q, want B's %q still skipped", b.BirthID, b.MergeStatus, successor.BirthID)
	}
	if !errors.Is(cfErr, store.ErrRunDataIdentityChanged) {
		t.Fatalf("CommitAndFinalizeCtx with B swapped in = %v, want ErrRunDataIdentityChanged", cfErr)
	}
}

// commitFinalizeRereadScenario: B replaces A before the re-read.
func commitFinalizeRereadScenario(t *testing.T, native snapshotOriginNative, ctx context.Context) {
	commitFinalizeScenario(t, native, ctx, true)
}

// commitFinalizeFlipScenario: B replaces A after the re-read, before the flip.
func commitFinalizeFlipScenario(t *testing.T, native snapshotOriginNative, ctx context.Context) {
	commitFinalizeScenario(t, native, ctx, false)
}

// mergeBirthCallerScenarios are the caller witnesses of the merge doors'
// expected birth; TestMergeBirthCallersAnchorToTheDecidedRun runs them on the
// filesystem store and TestMergeExpectedBirthNativeMongo on Mongo.
var mergeBirthCallerScenarios = []struct {
	name     string
	scenario func(*testing.T, snapshotOriginNative, context.Context)
}{
	{"perform-expect", mergeBirthPerformExpectScenario},
	{"abort", mergeBirthAbortScenario},
	{"finalize", mergeBirthFinalizeScenario},
	{"commit-finalize-reread", commitFinalizeRereadScenario},
	{"commit-finalize-flip", commitFinalizeFlipScenario},
}

// TestMergeBirthCallersAnchorToTheDecidedRun is the FS leg of the caller
// witnesses.
func TestMergeBirthCallersAnchorToTheDecidedRun(t *testing.T) {
	for _, tc := range mergeBirthCallerScenarios {
		t.Run(tc.name, func(t *testing.T) {
			s, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			tc.scenario(t, s, t.Context())
		})
	}
}

// TestSnapshotReconcileAnchorsToLoadedBirth is the FS leg of the
// reconciliation witness; TestMergeExpectedBirthNativeMongo runs the Mongo leg.
func TestSnapshotReconcileAnchorsToLoadedBirth(t *testing.T) {
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mergeBirthReconcileScenario(t, s, t.Context())
}

// TestPerformMergeClaimAnchorsToLoadedBirth is the FS leg of the claim
// witness; TestMergeExpectedBirthNativeMongo runs the Mongo leg.
func TestPerformMergeClaimAnchorsToLoadedBirth(t *testing.T) {
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mergeBirthPerformScenario(t, s, t.Context())
}
