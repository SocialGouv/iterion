package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	gitlib "github.com/SocialGouv/iterion/pkg/git"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// parkingRunMessage is a run whose engine parks at once: its entry is a human
// gate, so the engine writes paused_waiting_human and returns ErrRunPaused
// without its run ctx ever being cancelled.
func parkingRunMessage(t *testing.T, runID string) *queue.RunMessage {
	t.Helper()
	const src = "schema answer:\n  ok: bool\nhuman gate:\n  output: answer\nworkflow main:\n  entry: gate\n  gate -> done\n"
	pr := parser.Parse("main.bot", src)
	for _, d := range pr.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatalf("parse: %s", d.Error())
		}
	}
	body, err := ast.MarshalFile(pr.File)
	if err != nil {
		t.Fatalf("marshal AST: %v", err)
	}
	return &queue.RunMessage{RunID: runID, WorkflowName: "main", IRCompiled: body}
}

// synctestWorkDir is a runner work dir an engine can run in inside a synctest
// bubble. The executor opens its board store under <workdir>/dispatcher, and
// that store's fsnotify watcher waits on a file descriptor — a wait synctest
// cannot see past, so the bubble's clock would never move. A file where that
// directory goes keeps the store, and its watcher, closed.
func synctestWorkDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "dispatcher"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// stuckUploader is a store whose artifact upload does not return on its own:
// it says when it was entered, then holds until released, whatever its ctx.
type stuckUploader struct {
	store.RunStore
	entered chan struct{}
	release chan struct{}
}

func (u *stuckUploader) UploadRunFiles(context.Context, string) (int, error) {
	close(u.entered)
	<-u.release
	return 0, nil
}

// TestLeaseHold_aParkReleasesItsLeaseWithinTheCeiling: a park never cancels
// the run ctx, so only the engine's return can start the clock on its lease.
// Driven through the path a delivery takes (executeHoldingLease), a real
// engine parks at a human gate and a post-engine step then never returns: the
// lease is held through the post-engine steps, and let go at the ceiling a
// resume's retries are spread over — not for as long as the step hangs.
func TestLeaseHold_aParkReleasesItsLeaseWithinTheCeiling(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st, err := store.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		up := &stuckUploader{RunStore: st, entered: make(chan struct{}), release: make(chan struct{})}
		lease := &countingLease{}
		r := &Runner{cfg: Config{Store: up, WorkDir: synctestWorkDir(t), HeartbeatInterval: 20 * time.Second, Logger: iterlog.Nop()}}
		runCtx, runCancel := context.WithCancelCause(context.Background())
		defer runCancel(nil)
		done := make(chan error, 1)
		go func() {
			done <- r.executeHoldingLease(runCtx, runCancel, parkingRunMessage(t, "run-park"), nil, lease, nopProgress{}, nil)
		}()
		<-up.entered
		time.Sleep(postEngineCeiling - time.Minute)
		synctest.Wait()
		beforeCeiling := lease.count()
		time.Sleep(30 * time.Second)
		synctest.Wait()
		held := lease.count() - beforeCeiling
		time.Sleep(time.Minute)
		synctest.Wait()
		atCeiling := lease.count()
		time.Sleep(10 * time.Minute)
		synctest.Wait()
		past := lease.count() - atCeiling
		cause := context.Cause(runCtx)
		close(up.release)
		execErr := <-done
		if !errors.Is(execErr, runtime.ErrRunPaused) {
			t.Fatalf("executeHoldingLease = %v, want the park (ErrRunPaused)", execErr)
		}
		if held == 0 {
			t.Fatalf("no refresh in the minute before the post-engine ceiling (%s): the park's lease is let go while its post-engine steps may still run", postEngineCeiling)
		}
		if past != 0 {
			t.Fatalf("%d refresh(es) more than %s after a park's engine returned: the lease outlasts the ceiling a resume's retries are spread over", past, postEngineCeiling)
		}
		if !errors.Is(cause, runtime.ErrRunInterrupted) {
			t.Fatalf("at the ceiling the run's cause is %v, want interrupted: what still works under the lease must stop", cause)
		}
	})
}

// deadlineUploader records the time its upload was given.
type deadlineUploader struct {
	store.RunStore
	left time.Duration
}

func (u *deadlineUploader) UploadRunFiles(ctx context.Context, _ string) (int, error) {
	u.left = timeLeft(ctx)
	return 0, nil
}

// timeLeft is how long ctx has before its deadline; zero when it has none.
func timeLeft(ctx context.Context) time.Duration {
	d, ok := ctx.Deadline()
	if !ok {
		return 0
	}
	return time.Until(d)
}

// TestUploadRunFiles_boundsTheUpload: the artifact upload runs after the
// engine returned, with the run's lease still held — an object store that
// never answers must not hold it past the upload's budget.
func TestUploadRunFiles_boundsTheUpload(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	up := &deadlineUploader{RunStore: st}
	r := &Runner{cfg: Config{Store: up, Logger: iterlog.Nop()}}
	r.uploadRunFiles(context.Background(), &queue.RunMessage{RunID: "run-up", TenantID: "team-1", OwnerID: "u1"})
	if up.left <= 0 || up.left > uploadRunFilesBudget {
		t.Fatalf("uploadRunFiles gave its upload %s, want a deadline within %s: an object store that never answers holds the run's lease", up.left, uploadRunFilesBudget)
	}
}

// deadlineGitMetaStore records the time each store call of the git snapshot
// was given — the recorded snapshot's read, the diff offload, the save.
type deadlineGitMetaStore struct {
	store.RunStore
	loadErr                      error
	loadLeft, blobLeft, saveLeft time.Duration
	blobs                        int
	saved                        bool
}

func (f *deadlineGitMetaStore) LoadRunGitMeta(ctx context.Context, _ string) (*store.RunGitMeta, error) {
	f.loadLeft = timeLeft(ctx)
	return nil, f.loadErr
}

func (f *deadlineGitMetaStore) SaveRunGitMeta(ctx context.Context, _ string, _ *store.RunGitMeta) error {
	f.saveLeft = timeLeft(ctx)
	f.saved = true
	return nil
}

func (f *deadlineGitMetaStore) PutRunDiffBlob(ctx context.Context, _, _ string, _ []byte) error {
	f.blobLeft = timeLeft(ctx)
	f.blobs++
	return nil
}

func (f *deadlineGitMetaStore) GetRunDiffBlob(context.Context, string, string) ([]byte, error) {
	return nil, os.ErrNotExist
}

// TestRecordRunGitMeta_boundsItsStoreWork: the git snapshot is recorded after
// the engine returned, with the run's lease still held — each of its store
// calls runs on a deadline, and an empty snapshot whose predecessor cannot be
// read (that deadline spent) is not written over it.
func TestRecordRunGitMeta_boundsItsStoreWork(t *testing.T) {
	msg := &queue.RunMessage{RunID: "run-meta", TenantID: "team-1"}
	repo := func(t *testing.T, r *Runner) (dir, base string) {
		t.Helper()
		dir = t.TempDir()
		if err := r.runGit(context.Background(), dir, "", "init", "-q"); err != nil {
			t.Fatalf("git init: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitCommitAll(t, r, dir, "c1")
		base, err := gitlib.RevParseHead(dir)
		if err != nil {
			t.Fatal(err)
		}
		return dir, base
	}
	t.Run("a snapshot with commits: the diff offload and the save", func(t *testing.T) {
		fake := &deadlineGitMetaStore{}
		r := &Runner{cfg: Config{Store: fake, Logger: iterlog.Nop()}}
		dir, base := repo(t, r)
		// Past the inline cap, so its diff is offloaded to the blob sink.
		if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(strings.Repeat("a line of the run's output\n", 8<<10)), 0o644); err != nil {
			t.Fatal(err)
		}
		gitCommitAll(t, r, dir, "c2")
		r.recordRunGitMeta(context.Background(), msg, dir, base, runtime.WorkspaceIntegrity{})
		if !fake.saved || fake.blobs == 0 {
			t.Fatalf("saved=%v blobs=%d: the fixture does not reach the diff offload and the save", fake.saved, fake.blobs)
		}
		if fake.blobLeft <= 0 || fake.blobLeft > gitMetaDiffBudget {
			t.Errorf("the diff offload got %s, want a deadline within %s", fake.blobLeft, gitMetaDiffBudget)
		}
		if fake.saveLeft <= 0 || fake.saveLeft > gitMetaOpTimeout {
			t.Errorf("the save got %s, want a deadline within %s", fake.saveLeft, gitMetaOpTimeout)
		}
	})
	t.Run("an empty snapshot: the recorded one's read", func(t *testing.T) {
		fake := &deadlineGitMetaStore{}
		r := &Runner{cfg: Config{Store: fake, Logger: iterlog.Nop()}}
		dir, base := repo(t, r)
		r.recordRunGitMeta(context.Background(), msg, dir, base, runtime.WorkspaceIntegrity{})
		if fake.loadLeft <= 0 || fake.loadLeft > gitMetaOpTimeout {
			t.Errorf("the recorded snapshot's read got %s, want a deadline within %s", fake.loadLeft, gitMetaOpTimeout)
		}
		if !fake.saved {
			t.Error("the first snapshot of a run with no commits was not recorded")
		}
	})
	t.Run("an empty snapshot whose predecessor cannot be read is not written", func(t *testing.T) {
		fake := &deadlineGitMetaStore{loadErr: context.DeadlineExceeded}
		r := &Runner{cfg: Config{Store: fake, Logger: iterlog.Nop()}}
		dir, base := repo(t, r)
		r.recordRunGitMeta(context.Background(), msg, dir, base, runtime.WorkspaceIntegrity{})
		if fake.saved {
			t.Fatal("an empty snapshot was saved though the recorded one could not be read — it may replace an earlier attempt's commits")
		}
	})
}

// silentRunLogStore is a run log store that never answers an append on its
// own: each append holds until its ctx ends, or the test releases it.
type silentRunLogStore struct {
	store.RunLogStore
	release chan struct{}
}

func (s *silentRunLogStore) AppendRunLog(ctx context.Context, _ string, _ int64, _ []byte) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.release:
		return errors.New("released by the test")
	}
}

// TestRunLogWriter_closeIsBoundedWhenTheStoreNeverAnswers: the run log's
// close is one of executeRun's defers, run with the lease still held. A store
// that never answers costs each batch its attempts, then the batch is dropped:
// Close returns within its budget — the flush in flight when it was called,
// then the tail's — instead of holding the run's end on the store.
func TestRunLogWriter_closeIsBoundedWhenTheStoreNeverAnswers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ls := &silentRunLogStore{release: make(chan struct{})}
		defer close(ls.release)
		w := newRunLogWriter(context.Background(), ls, "run-log", 0, iterlog.Nop())
		_, _ = w.Write([]byte("a line flushed by the ticker\n"))
		time.Sleep(runLogFlushInterval + runLogFlushInterval/5)
		_, _ = w.Write([]byte("the tail\n"))
		start := time.Now()
		closed := make(chan struct{})
		go func() {
			_ = w.Close()
			close(closed)
		}()
		select {
		case <-closed:
		case <-time.After(runLogCloseBudget + time.Second):
			t.Fatalf("Close still waits on the store %s after it was called, past its %s budget", time.Since(start), runLogCloseBudget)
		}
		if took := time.Since(start); took > runLogCloseBudget {
			t.Fatalf("Close took %s, past its %s budget", took, runLogCloseBudget)
		}
	})
}

// TestLeaseUnwindCeiling_coversTheEngineAndEveryPostEngineStep: the ceiling a
// held lease's resume spreads its retries over covers the whole hold — the
// engine's teardown, then every step executeRun takes once its engine
// returned, each at its own budget — so the spread and the sweeper's cutoff
// cannot come short of it.
func TestLeaseUnwindCeiling_coversTheEngineAndEveryPostEngineStep(t *testing.T) {
	if gitOpTimeout != gitOpTimeoutDefault || attemptBankBudget != attemptBankBudgetDefault {
		t.Skipf("the git op bound (%s) or the park budget (%s) is overridden in this process", gitOpTimeout, attemptBankBudget)
	}
	if LeaseUnwindCeiling != engineUnwindCeiling+postEngineCeiling {
		t.Fatalf("LeaseUnwindCeiling = %s, want the engine's unwind (%s) then the post-engine steps (%s)", LeaseUnwindCeiling, engineUnwindCeiling, postEngineCeiling)
	}
	if engineUnwindCeiling < runtime.SandboxTeardownBudget {
		t.Fatalf("engineUnwindCeiling = %s, under the sandbox's teardown budget %s", engineUnwindCeiling, runtime.SandboxTeardownBudget)
	}
	bank := max(
		liveBankBudget()+2*bankDocOpTimeout,  // the storage bank of a live run
		bankBudget+2*bankDocOpTimeout,        // the storage bank past the run's deadline
		attemptBankBudget+parkStoreOpTimeout, // the attempt ref
	)
	steps := []struct {
		name   string
		budget time.Duration
	}{
		{"the retry circuit's reset", retryCircuitResetTimeout},
		{"the git snapshot", gitMetaOpTimeout + gitMetaDiffBudget + gitMetaOpTimeout},
		{"the bank", bank},
		{"the artifact upload", uploadRunFilesBudget},
		{"the sealed credentials' deletion", runSecretsDeleteTimeout},
		{"the spend records", 3 * spendWriteTimeout},
		{"the run log's close", runLogCloseBudget},
	}
	var sum time.Duration
	for _, s := range steps {
		if s.budget <= 0 {
			t.Fatalf("%s has no budget", s.name)
		}
		sum += s.budget
	}
	if postEngineBudget < sum {
		t.Fatalf("postEngineBudget = %s, under the %s the post-engine steps may take", postEngineBudget, sum)
	}
	if postEngineCeiling < postEngineBudget {
		t.Fatalf("postEngineCeiling = %s, under postEngineBudget %s", postEngineCeiling, postEngineBudget)
	}
}
