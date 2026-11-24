package runview

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/internal/gittest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestSubmissionRunIDIsStablePerTenantAndKey(t *testing.T) {
	a := SubmissionRunID("team-a", "k1")
	if a != SubmissionRunID("team-a", "k1") {
		t.Fatal("the same key derives two ids")
	}
	if a == SubmissionRunID("team-b", "k1") || a == SubmissionRunID("team-a", "k2") {
		t.Fatal("distinct tenants or keys share an id")
	}
	// The separator is not a key byte a caller can shift across.
	if SubmissionRunID("team-a", "\x00x") == SubmissionRunID("team-a\x00", "x") {
		t.Fatal("tenant and key are not separated")
	}
	if !IsSubmissionRunID(a) || IsSubmissionRunID("0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b") {
		t.Fatalf("the reserved namespace is not the derived one (%q)", a)
	}
}

func TestClassifySubmission(t *testing.T) {
	queuedAt := time.Date(2026, 11, 14, 22, 0, 0, 0, time.UTC)
	later := queuedAt.Add(time.Minute)
	run := func(status store.RunStatus, code store.FailureCode) *store.Run {
		return &store.Run{ID: "idem-x", TenantID: "team-a", Status: status, FailureCode: code, Error: "publish failed"}
	}
	// queuedBy is a run queued by the launch path's own attempt, which a
	// mutation may then move away from it.
	queuedBy := func(mutate func(*store.Run)) *store.Run {
		at := queuedAt
		r := &store.Run{ID: "idem-x", TenantID: "team-a", Status: store.RunStatusQueued, QueuedAt: &at, SubmissionAttemptAt: &queuedAt}
		if mutate != nil {
			mutate(r)
		}
		return r
	}
	for _, c := range []struct {
		name      string
		r         *store.Run
		publishes bool
		want      SubmissionVerdict
		spent     bool
		refused   bool
	}{
		{name: "no run", r: nil, publishes: true, want: SubmissionFresh},
		{name: "queued by its launch, published by a queue", r: queuedBy(nil), publishes: true, want: SubmissionComplete},
		{name: "queued by its launch, in process", r: queuedBy(nil), publishes: false, want: SubmissionAcknowledged},
		// A resume's flip stamps a queued_at of its own: the run executed
		// and was handed on, whatever its status reads now.
		{name: "re-queued by a resume", r: queuedBy(func(r *store.Run) { r.QueuedAt = &later }), publishes: true, want: SubmissionAcknowledged},
		{name: "queued with a checkpoint", r: queuedBy(func(r *store.Run) { r.Checkpoint = &store.Checkpoint{NodeID: "step2"} }), publishes: true, want: SubmissionAcknowledged},
		{name: "queued by no submission attempt", r: queuedBy(func(r *store.Run) { r.SubmissionAttemptAt = nil }), publishes: true, want: SubmissionAcknowledged},
		// A store that stamps no tenant (the filesystem one) leaves the id,
		// derived from this tenant's key, as the only identity.
		{name: "recorded under no tenant", r: queuedBy(func(r *store.Run) { r.TenantID = "" }), publishes: true, want: SubmissionComplete},
		{name: "running", r: run(store.RunStatusRunning, ""), publishes: true, want: SubmissionAcknowledged},
		{name: "parked on a credential slot", r: run(store.RunStatusFailedResumable, store.FailureCredSlotWait), publishes: true, want: SubmissionAcknowledged},
		{name: "failed after running", r: run(store.RunStatusFailed, store.FailureExecutionFailed), publishes: true, want: SubmissionAcknowledged},
		{name: "never left the launch path", r: run(store.RunStatusFailed, store.FailureLaunchFailed), publishes: true, spent: true},
		{name: "never left the launch path, in process", r: run(store.RunStatusFailed, store.FailureLaunchFailed), publishes: false, spent: true},
		{name: "another tenant's", r: &store.Run{ID: "idem-x", TenantID: "team-b", Status: store.RunStatusQueued}, publishes: true, refused: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := ClassifySubmission(c.r, "team-a", c.publishes)
			switch {
			case c.spent:
				var spent *SubmissionSpentError
				if !errors.As(err, &spent) || !errors.Is(err, ErrSubmissionSpent) || !spent.SubmissionSpent() || spent.Cause != "publish failed" {
					t.Fatalf("ClassifySubmission = (%v, %v), want a spent submission carrying its cause", got, err)
				}
			case c.refused:
				var refused *SubmissionRefusedError
				if !errors.As(err, &refused) || !errors.Is(err, ErrRunIDTaken) || refused.RefusalReason() == "" {
					t.Fatalf("ClassifySubmission = (%v, %v), want a refusal", got, err)
				}
			default:
				if err != nil || got != c.want {
					t.Fatalf("ClassifySubmission = (%v, %v), want %v", got, err, c.want)
				}
			}
		})
	}
}

func localSubmissionService(t *testing.T) (*Service, store.RunStore, string) {
	t.Helper()
	dir := t.TempDir()
	botPath := filepath.Join(dir, "demo.bot")
	if err := os.WriteFile(botPath, []byte("workflow demo:\n  entry: done\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fs, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := NewService(dir, WithLogger(iterlog.Nop()), WithStore(fs), WithWorkDir(gittest.SourceRepo(t)))
	if err != nil {
		t.Fatal(err)
	}
	return svc, fs, botPath
}

// A keyed submission repeated in process starts its run once: the second call
// is answered with the run the first started.
func TestLaunch_ARepeatedSubmissionStartsOneRun(t *testing.T) {
	svc, fs, bot := localSubmissionService(t)
	ctx := context.Background()
	first, err := svc.Launch(ctx, LaunchSpec{FilePath: bot, SubmissionKey: "pair-1#0"})
	if err != nil {
		t.Fatalf("first submission: %v", err)
	}
	<-first.Done
	if first.RunID != SubmissionRunID("", "pair-1#0") || first.AlreadySubmitted {
		t.Fatalf("first submission = %+v, want the derived id, freshly started", first)
	}
	again, err := svc.Launch(ctx, LaunchSpec{FilePath: bot, SubmissionKey: "pair-1#0"})
	if err != nil {
		t.Fatalf("repeated submission: %v", err)
	}
	if again.RunID != first.RunID || !again.AlreadySubmitted {
		t.Fatalf("repeated submission = %+v, want the first run, already submitted", again)
	}
	runs, err := fs.ListRuns(ctx)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs = %d (%v), want one", len(runs), err)
	}
}

// The submission's id answers from the run it names BEFORE the request is
// judged again: a re-check that would now refuse must not turn a launch that
// happened into an error the caller would retry.
func TestLaunch_ASubmittedRunIsAnsweredBeforeTheRequestIsJudgedAgain(t *testing.T) {
	svc, _, bot := localSubmissionService(t)
	ctx := context.Background()
	first, err := svc.Launch(ctx, LaunchSpec{FilePath: bot, SubmissionKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	<-first.Done
	again, err := svc.Launch(ctx, LaunchSpec{FilePath: bot, SubmissionKey: "k", BranchName: "not a valid..branch"})
	if err != nil || !again.AlreadySubmitted {
		t.Fatalf("repeat with a request a re-check refuses = (%+v, %v), want the submitted run", again, err)
	}
	if _, err := svc.Launch(ctx, LaunchSpec{FilePath: bot, SubmissionKey: "fresh", BranchName: "not a valid..branch"}); err == nil {
		t.Fatal("a fresh submission skipped the request's checks")
	}
}

func TestLaunch_ACallerCannotTakeASubmissionsRunID(t *testing.T) {
	svc, _, bot := localSubmissionService(t)
	ctx := context.Background()
	reserved := SubmissionRunID("", "someone-elses-key")
	if _, err := svc.Launch(ctx, LaunchSpec{FilePath: bot, RunID: reserved}); !errors.Is(err, ErrRunIDReserved) {
		t.Fatalf("a caller-chosen id in the reserved namespace = %v, want ErrRunIDReserved", err)
	}
	if _, err := svc.Launch(ctx, LaunchSpec{FilePath: bot, RunID: "plain-id", SubmissionKey: "k"}); err == nil || !strings.Contains(err.Error(), "not the one submission key") {
		t.Fatalf("a keyed launch naming another id = %v, want a refusal", err)
	}
}

// What a keyed id already names decides the answer: a run that never left
// its launch path spends the id, a deleted one refuses it, another tenant's
// refuses it, and a store that cannot answer is an error — never "no run".
func TestLaunch_ASubmissionAnswersFromTheRunItsIDNames(t *testing.T) {
	ctx := context.Background()
	seed := func(t *testing.T, fs store.RunStore, key string, mutate func(*store.Run)) string {
		t.Helper()
		id := SubmissionRunID("", key)
		r, err := fs.CreateRun(ctx, id, "demo", nil)
		if err != nil {
			t.Fatal(err)
		}
		if mutate != nil {
			mutate(r)
			if err := fs.SaveRun(ctx, r); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}

	t.Run("spent", func(t *testing.T) {
		svc, fs, bot := localSubmissionService(t)
		id := seed(t, fs, "k", nil)
		if err := store.FailRunAtLaunch(ctx, fs, id, "detached runner would not start"); err != nil {
			t.Fatal(err)
		}
		_, err := svc.Launch(ctx, LaunchSpec{FilePath: bot, SubmissionKey: "k"})
		var spent *SubmissionSpentError
		if !errors.As(err, &spent) || spent.RunID != id || !strings.Contains(spent.Cause, "detached runner") {
			t.Fatalf("Launch = %v, want the id spent with its cause", err)
		}
	})

	t.Run("deleted", func(t *testing.T) {
		svc, fs, bot := localSubmissionService(t)
		id := seed(t, fs, "k", func(r *store.Run) { r.Status = store.RunStatusFinished })
		if err := fs.DeleteRun(ctx, id); err != nil {
			t.Fatal(err)
		}
		_, err := svc.Launch(ctx, LaunchSpec{FilePath: bot, SubmissionKey: "k"})
		var refused *SubmissionRefusedError
		if !errors.As(err, &refused) || !errors.Is(err, ErrRunIDTaken) || !errors.Is(err, store.ErrRunDeleted) {
			t.Fatalf("Launch = %v, want a refusal naming the deletion", err)
		}
	})

	t.Run("another tenant", func(t *testing.T) {
		svc, fs, bot := localSubmissionService(t)
		seed(t, fs, "k", func(r *store.Run) { r.TenantID = "team-b" })
		_, err := svc.Launch(ctx, LaunchSpec{FilePath: bot, SubmissionKey: "k"})
		var refused *SubmissionRefusedError
		if !errors.As(err, &refused) {
			t.Fatalf("Launch = %v, want a refusal", err)
		}
	})

	t.Run("unreadable", func(t *testing.T) {
		svc, fs, bot := localSubmissionService(t)
		svc.store = &failingLoadStore{RunStore: fs}
		_, err := svc.Launch(ctx, LaunchSpec{FilePath: bot, SubmissionKey: "k"})
		if err == nil || !strings.Contains(err.Error(), "read submission run") {
			t.Fatalf("Launch over an unreadable store = %v, want the read error", err)
		}
		if runs, _ := fs.ListRuns(ctx); len(runs) != 0 {
			t.Fatalf("a run was started beside one the store could not show: %d", len(runs))
		}
	})
}

type failingLoadStore struct{ store.RunStore }

func (f *failingLoadStore) LoadRun(context.Context, string) (*store.Run, error) {
	return nil, errors.New("store unreachable")
}

// A local launch that fails after creating its run ends that run as never
// having left its launch path: a row reading running with nothing behind it
// would be taken, by the key's next submission, for a run that was handed on.
func TestLaunch_ALaunchThatFailsAfterCreatingItsRunEndsIt(t *testing.T) {
	svc, fs, bot := localSubmissionService(t)
	svc.store = &stampFailStore{RunStore: fs}
	ctx := context.Background()
	spec := LaunchSpec{FilePath: bot, SubmissionKey: "k", LLMRoutePolicy: &store.RunLLMRoutePolicy{}}
	_, err := svc.Launch(ctx, spec)
	var persisted *RunPersistedError
	if !errors.As(err, &persisted) {
		t.Fatalf("first submission = %v, want the persisted run's error", err)
	}
	id := SubmissionRunID("", "k")
	r, err := fs.LoadRun(ctx, id)
	if err != nil || r.Status != store.RunStatusFailed || r.FailureCode != store.FailureLaunchFailed {
		t.Fatalf("the run left behind = %+v (%v), want it ended LAUNCH_FAILED", r, err)
	}
	var spent *SubmissionSpentError
	if _, err := svc.Launch(ctx, spec); !errors.As(err, &spent) {
		t.Fatalf("the key's next submission = %v, want its id spent", err)
	}
}

// The run is born before the launch builds its executor, so a failure spawnRun
// does not end itself — here the manager refusing the registration as the
// service stops — ends the run the same way, at the launch's own exit.
func TestLaunch_AFailureAfterTheBirthOutsideSpawnRunsWritesEndsTheRun(t *testing.T) {
	svc, fs, bot := localSubmissionService(t)
	ctx := context.Background()
	svc.manager.Stop(ctx)
	spec := LaunchSpec{FilePath: bot, SubmissionKey: "k"}
	_, err := svc.Launch(ctx, spec)
	var persisted *RunPersistedError
	if !errors.As(err, &persisted) || !strings.Contains(err.Error(), "manager is stopped") {
		t.Fatalf("first submission = %v, want the persisted run's registration refusal", err)
	}
	r, err := fs.LoadRun(ctx, SubmissionRunID("", "k"))
	if err != nil || r.Status != store.RunStatusFailed || r.FailureCode != store.FailureLaunchFailed {
		t.Fatalf("the run left behind = %+v (%v), want it ended LAUNCH_FAILED", r, err)
	}
	var spent *SubmissionSpentError
	if _, err := svc.Launch(ctx, spec); !errors.As(err, &spent) {
		t.Fatalf("the key's next submission = %v, want its id spent", err)
	}
}

// stampFailStore fails the first SaveRun that stamps a routing policy — the
// write spawnRun performs right after its exclusive CreateRun.
type stampFailStore struct {
	store.RunStore
	once sync.Once
}

func (s *stampFailStore) SaveRun(ctx context.Context, r *store.Run) error {
	fail := false
	if r.LLMRoutePolicy != nil {
		s.once.Do(func() { fail = true })
	}
	if fail {
		return errors.New("store blip on the routing-policy stamp")
	}
	return s.RunStore.SaveRun(ctx, r)
}

// A keyed launch under a tenant lands in a store that stamps none; its next
// submission is answered by that run, never refused as another tenant's.
func TestLaunch_ASubmissionInATenantlessStoreIsAnsweredByItsRun(t *testing.T) {
	svc, _, bot := localSubmissionService(t)
	ctx := store.WithIdentity(context.Background(), "team-a", "connector:c1")
	first, err := svc.Launch(ctx, LaunchSpec{FilePath: bot, SubmissionKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	<-first.Done
	again, err := svc.Launch(ctx, LaunchSpec{FilePath: bot, SubmissionKey: "k"})
	if err != nil || again.RunID != first.RunID || !again.AlreadySubmitted {
		t.Fatalf("the key's next submission = (%+v, %v), want its run, already submitted", again, err)
	}
}
