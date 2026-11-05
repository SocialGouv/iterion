package mongo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

const serviceChildBot = `schema out:
  ok: bool

tool step:
  command: ` + "`printf '{\"ok\":true}'`" + `
  output: out

workflow child:
  entry: step
  step -> done
`

const serviceSubbotParentBot = `schema out:
  ok: bool

subbot delegate:
  source: "child.bot"
  output: out

workflow parent:
  entry: delegate
  delegate -> done
`

const serviceGateBot = `schema gate_out:
  approve: bool

prompt gate_prompt:
  Approve?

human gate:
  instructions: gate_prompt
  output: gate_out
  interaction: human

workflow gate:
  worktree: none
  entry: gate
  gate -> done when approve
  gate -> fail when not approve
`

// serviceOnTheMongoStore is a run service executing in process over a fresh
// Mongo database — no cloud publisher, so a launch and a resume run their
// engine here — with bots written into its directory. Skips without
// ITERION_TEST_MONGO_URI, like the conformance suite. The returned context
// bounds the test.
func serviceOnTheMongoStore(t *testing.T, dbPrefix string, bots map[string]string) (*Store, *runview.Service, string, context.Context) {
	t.Helper()
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping the Mongo run service rows")
	}
	t.Setenv("ITERION_SANDBOX_DEFAULT", "none")
	ctx, cancel := mongotest.Ctx(t)
	t.Cleanup(cancel)
	s, err := New(ctx, Config{URI: uri, Database: dbPrefix + bsonNonce(t), Blob: newInMemoryBlob()})
	if err != nil {
		t.Fatalf("mongo New: %v", err)
	}
	t.Cleanup(func() {
		drop, dropCancel := mongotest.TeardownCtx()
		defer dropCancel()
		_ = s.db.Drop(drop)
		_ = s.Close(drop)
	})
	dir := t.TempDir()
	for name, body := range bots {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	svc, err := runview.NewService(dir, runview.WithStore(s), runview.WithLogger(iterlog.Nop()), runview.WithWorkDir(gittest.SourceRepo(t)))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return s, svc, dir, ctx
}

// TestServiceLaunch_ChildRunsOnTheMongoStore drives the run service's
// in-process launch over the cloud store, the two ways a child run is born
// there: a launch that names its parent (parent_run_id — spawnRun's own
// precreate) and a `subbot:` node (the child engine's create). Both create the
// child through CreateChildRun, which READS the parent to inherit its
// workspace provenance — a tenant-scoped read, which the store's filter
// refuses to answer without a tenant by panicking. The launch's identity must
// therefore reach every store call; a call made under context.Background()
// crashes the launch here rather than reading another tenant's parent.
// Gated on ITERION_TEST_MONGO_URI like the conformance suite.
func TestServiceLaunch_ChildRunsOnTheMongoStore(t *testing.T) {
	s, svc, dir, ctx := serviceOnTheMongoStore(t, "iterion_service_child_", map[string]string{"child.bot": serviceChildBot, "parent.bot": serviceSubbotParentBot})
	launchCtx := store.WithIdentity(ctx, "tenant-svc", "owner-svc")
	finished := func(t *testing.T, res *runview.LaunchResult) {
		t.Helper()
		select {
		case <-res.Done:
		case <-ctx.Done():
			t.Fatalf("run %s did not finish: %v", res.RunID, ctx.Err())
		}
		r, err := s.LoadRun(launchCtx, res.RunID)
		if err != nil {
			t.Fatalf("LoadRun(%s): %v", res.RunID, err)
		}
		if r.Status != store.RunStatusFinished {
			t.Fatalf("run %s ended %s (%s), want finished", r.ID, r.Status, r.Error)
		}
	}
	child := func(t *testing.T, parentID string) *store.Run {
		t.Helper()
		ids, err := s.ListChildRuns(launchCtx, parentID)
		if err != nil {
			t.Fatalf("ListChildRuns(%s): %v", parentID, err)
		}
		if len(ids) != 1 {
			t.Fatalf("ListChildRuns(%s) = %v, want exactly one child", parentID, ids)
		}
		r, err := s.LoadRun(launchCtx, ids[0])
		if err != nil {
			t.Fatalf("LoadRun(child %s): %v", ids[0], err)
		}
		if r.TenantID != "tenant-svc" {
			t.Fatalf("child %s TenantID = %q, want the launch's tenant", r.ID, r.TenantID)
		}
		return r
	}

	t.Run("a launch that names its parent", func(t *testing.T) {
		if _, err := s.CreateRun(launchCtx, "run-svc-parent", "parent", nil); err != nil {
			t.Fatalf("CreateRun(parent): %v", err)
		}
		res, err := svc.Launch(launchCtx, runview.LaunchSpec{RunID: "run-svc-rest-child", FilePath: filepath.Join(dir, "child.bot"), ParentRunID: "run-svc-parent"})
		if err != nil {
			t.Fatalf("Launch: %v", err)
		}
		finished(t, res)
		if got := child(t, "run-svc-parent"); got.ID != "run-svc-rest-child" {
			t.Fatalf("the parent's child is %s, want run-svc-rest-child", got.ID)
		}
	})

	t.Run("a subbot node", func(t *testing.T) {
		res, err := svc.Launch(launchCtx, runview.LaunchSpec{RunID: "run-svc-subbot", FilePath: filepath.Join(dir, "parent.bot")})
		if err != nil {
			t.Fatalf("Launch: %v", err)
		}
		finished(t, res)
		if got := child(t, "run-svc-subbot"); got.Status != store.RunStatusFinished {
			t.Fatalf("the subbot child ended %s (%s), want finished", got.Status, got.Error)
		}
	})

	// The trusted path's one change (ADR-162): a launch naming a parent its
	// tenant cannot read is refused, typed, before anything is created.
	t.Run("a launch naming a parent another tenant owns is refused", func(t *testing.T) {
		other := store.WithIdentity(ctx, "tenant-other", "owner-other")
		if _, err := s.CreateRun(other, "run-svc-foreign-parent", "parent", nil); err != nil {
			t.Fatalf("CreateRun(foreign parent): %v", err)
		}
		_, err := svc.Launch(launchCtx, runview.LaunchSpec{RunID: "run-svc-foreign-child", FilePath: filepath.Join(dir, "child.bot"), ParentRunID: "run-svc-foreign-parent"})
		if !errors.Is(err, store.ErrChildParentUnreadable) {
			t.Fatalf("Launch = %v, want %v", err, store.ErrChildParentUnreadable)
		}
		if _, lerr := s.LoadRun(store.TeamBlind(ctx), "run-svc-foreign-child"); lerr == nil {
			t.Fatal("the refused launch created its run")
		}
	})
}

// TestServiceResume_InProcessOnTheMongoStore drives an in-process resume over
// the cloud store: a run parked on a human gate, answered. The resume's run
// goroutine re-reads the run under its lock before the engine claims it — a
// tenant-scoped read, which the store's filter refuses to answer without a
// tenant by panicking, and nothing recovers in the run goroutine: a read made
// there under context.Background() kills the process, not the resume.
func TestServiceResume_InProcessOnTheMongoStore(t *testing.T) {
	s, svc, dir, ctx := serviceOnTheMongoStore(t, "iterion_service_resume_", map[string]string{"gate.bot": serviceGateBot})
	launchCtx := store.WithIdentity(ctx, "tenant-svc", "owner-svc")
	botPath := filepath.Join(dir, "gate.bot")
	ended := func(t *testing.T, res *runview.LaunchResult, want store.RunStatus) {
		t.Helper()
		select {
		case <-res.Done:
		case <-ctx.Done():
			t.Fatalf("run %s did not end: %v", res.RunID, ctx.Err())
		}
		r, err := s.LoadRun(launchCtx, res.RunID)
		if err != nil {
			t.Fatalf("LoadRun(%s): %v", res.RunID, err)
		}
		if r.Status != want {
			t.Fatalf("run %s is %s (%s), want %s", r.ID, r.Status, r.Error, want)
		}
	}

	res, err := svc.Launch(launchCtx, runview.LaunchSpec{RunID: "run-svc-gate", FilePath: botPath})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	ended(t, res, store.RunStatusPausedWaitingHuman)
	rres, err := svc.Resume(launchCtx, runview.ResumeSpec{RunID: res.RunID, FilePath: botPath, Answers: map[string]any{"approve": true}})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	ended(t, rres, store.RunStatusFinished)
}
