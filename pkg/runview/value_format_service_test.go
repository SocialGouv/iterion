package runview

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/trigger"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

type birthRefusalStore struct {
	store.RunStore
	cause   error
	creates int
}

func (s *birthRefusalStore) CreateRun(context.Context, string, string, map[string]any) (*store.Run, error) {
	s.creates++
	return nil, s.cause
}

const invocationSource = "schema checked:\n  ok: bool\n\ncompute check:\n  output: checked\n  expr:\n    ok: \"true\"\n\nworkflow probe:\n  worktree: none\n  sandbox: none\n  entry: check\n  check -> done\n"

func TestServiceBirthFailurePrecedesLogAndManager(t *testing.T) {
	t.Parallel()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cause := errors.New("birth unavailable")
	spy := &birthRefusalStore{RunStore: st, cause: cause}
	svc, err := NewService("", WithLogger(iterlog.Nop()), WithStore(spy))
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.Launch(t.Context(), LaunchSpec{RunID: "new", Source: invocationSource, WorkDir: t.TempDir()})
	if !errors.Is(err, cause) || got != nil || spy.creates != 1 {
		t.Fatalf("launch=%v error=%v creates=%d", got, err, spy.creates)
	}
	var owned *RunPersistedError
	if errors.As(err, &owned) {
		t.Fatal("failed create was adopted")
	}
	if len(svc.runLogs) != 0 {
		t.Fatal("log allocated before successful birth")
	}
	if _, err = st.LoadRun(t.Context(), "new"); !errors.Is(err, store.ErrRunNotFound) {
		t.Fatal(err)
	}
}
func TestServiceResumeRefusesOriginalBeforeRunLog(t *testing.T) {
	t.Parallel()
	for _, actual := range []store.ValueFormat{1, 2} {
		t.Run(string(rune('0'+actual)), func(t *testing.T) {
			t.Parallel()
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if _, err = store.CreateRunWithValueFormat(t.Context(), st, store.RunCreateSpec{ID: "run", Format: actual}); err != nil {
				t.Fatal(err)
			}
			ctx, err := store.WithRunValueFormat(t.Context(), store.RunValueFormat{RunID: "run", Format: 3 - actual})
			if err != nil {
				t.Fatal(err)
			}
			svc, err := NewService("", WithLogger(iterlog.Nop()), WithStore(st))
			if err != nil {
				t.Fatal(err)
			}
			result, err := svc.Resume(ctx, ResumeSpec{RunID: "run", FilePath: "not-opened.bot"})
			if !errors.Is(err, store.ErrValueFormatChanged) || result != nil {
				t.Fatalf("result=%v error=%v", result, err)
			}
			if len(svc.runLogs) != 0 {
				t.Fatal("refusal allocated logs")
			}
		})
	}
}

// The completion projection is another observer of the run document. It must
// not publish the successor's state after the engine rejects its format.
type invocationOutcomePublisher struct{ events []trigger.Event }

func (p *invocationOutcomePublisher) Publish(_ context.Context, e trigger.Event) error {
	p.events = append(p.events, e)
	return nil
}
func TestServiceFormatRefusalDoesNotPublishSuccessorOutcome(t *testing.T) {
	t.Parallel()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc, err := NewService("", WithLogger(iterlog.Nop()), WithStore(st))
	if err != nil {
		t.Fatal(err)
	}
	pub := &invocationOutcomePublisher{}
	svc.SetEventPublisher(pub)
	var swapErr, outcome error
	swapped := false
	observer := func(ev store.Event) {
		if ev.Type != "run_started" || swapped {
			return
		}
		swapped = true
		if swapErr = st.DeleteRun(context.Background(), "run"); swapErr != nil {
			return
		}
		if _, swapErr = st.PruneDeletionMarkers(context.Background(), time.Now().Add(time.Hour)); swapErr != nil {
			return
		}
		_, swapErr = store.CreateRunWithValueFormat(context.Background(), st, store.RunCreateSpec{ID: "run", Format: store.ExactValueFormat})
	}
	result, err := svc.Launch(t.Context(), LaunchSpec{RunID: "run", Source: invocationSource, WorkDir: t.TempDir(), ExtraObservers: []func(store.Event){observer}, OnOutcome: func(err error) { outcome = err }})
	if err != nil {
		t.Fatal(err)
	}
	<-result.Done
	if swapErr != nil || !swapped {
		t.Fatalf("swap=%v reached=%v", swapErr, swapped)
	}
	if !errors.Is(outcome, store.ErrValueFormatChanged) {
		t.Fatalf("outcome=%v", outcome)
	}
	if len(pub.events) != 0 {
		t.Fatalf("old invocation published successor outcome: %+v", pub.events)
	}
}

func TestServiceNormalCompletionStillPublishesOutcome(t *testing.T) {
	t.Parallel()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc, err := NewService("", WithLogger(iterlog.Nop()), WithStore(st))
	if err != nil {
		t.Fatal(err)
	}
	pub := &invocationOutcomePublisher{}
	svc.SetEventPublisher(pub)
	var outcome error
	result, err := svc.Launch(t.Context(), LaunchSpec{RunID: "run", Source: invocationSource, WorkDir: t.TempDir(), OnOutcome: func(err error) { outcome = err }})
	if err != nil {
		t.Fatal(err)
	}
	<-result.Done
	if outcome != nil {
		t.Fatal(outcome)
	}
	if len(pub.events) != 1 || pub.events[0].Kind != "run.finished" {
		t.Fatalf("normal outcome lost: %+v", pub.events)
	}
}

type birthCancellationStore struct {
	store.RunStore
	cancel context.CancelFunc
}

func (s *birthCancellationStore) CreateRun(ctx context.Context, id, name string, inputs map[string]any) (*store.Run, error) {
	r, err := s.RunStore.CreateRun(ctx, id, name, inputs)
	if err == nil {
		s.cancel()
	}
	return r, err
}
func TestServiceRetainsSuccessfulBirthWhenCancelledBeforeBuilder(t *testing.T) {
	t.Parallel()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	spy := &birthCancellationStore{RunStore: st, cancel: cancel}
	svc, err := NewService("", WithLogger(iterlog.Nop()), WithStore(spy))
	if err != nil {
		t.Fatal(err)
	}
	result, err := svc.Launch(ctx, LaunchSpec{RunID: "new", Source: invocationSource, WorkDir: t.TempDir()})
	var owned *RunPersistedError
	if result != nil || !errors.Is(err, context.Canceled) || !errors.As(err, &owned) || owned.RunID != "new" {
		t.Fatalf("result=%v error=%v owned=%v", result, err, owned)
	}
	if len(svc.runLogs) != 0 {
		t.Fatal("cancel after birth allocated builder log")
	}
	if _, err = st.LoadRun(t.Context(), "new"); err != nil {
		t.Fatal(err)
	}
}
