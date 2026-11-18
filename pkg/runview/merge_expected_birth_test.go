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
// arming returns, then delegates unchanged. It records the birth that load
// handed its caller and what the merge exit door answered; it applies no
// identity policy of its own.
type mergeBirthSwapStore struct {
	store.RunStore
	afterLoad   func()
	loadedBirth string
	exits       []error
}

func (s *mergeBirthSwapStore) LoadRun(ctx context.Context, id string) (*store.Run, error) {
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
