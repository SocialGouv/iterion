package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Round 4 of the adversarial loop on #2086, the runtime side.

// blockingOpenStore is a store with no filesystem root whose attachment
// bytes never arrive before the run's context ends: the fetch hangs until
// a cancel, a drain or the deadline cuts it.
type blockingOpenStore struct {
	store.RunStore
	entered chan struct{}
}

func (blockingOpenStore) Root() string { return "" }

func (b blockingOpenStore) OpenAttachment(ctx context.Context, _, _ string) (io.ReadCloser, store.AttachmentRecord, error) {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil, store.AttachmentRecord{}, fmt.Errorf("blob get: %w", ctx.Err())
}

// The call site, not the function: an operator's cancel landing while
// Engine.Run fetches the attachments leaves the run cancelled and returns
// the cancel — no typed code a runner would re-offer the run under.
func TestEngineRun_ACancelDuringTheFetchReturnsTheCancel(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	fs := tmpStore(t)
	st := blockingOpenStore{RunStore: fs, entered: make(chan struct{}, 1)}
	eng := New(devboxTestWorkflow(), st, newStubExecutor(),
		WithWorkDir(t.TempDir()),
		WithSandboxOverride("none"),
		WithLogger(iterlog.Nop()),
		WithAttachmentPromote(func(ctx context.Context, runID string) error {
			return fs.WriteAttachment(ctx, runID, store.AttachmentRecord{Name: "spec", OriginalFilename: "spec.md", MIME: "text/markdown"}, strings.NewReader("# spec"))
		}),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-st.entered:
			cancel()
		case <-time.After(time.Minute):
			cancel()
		}
	}()
	const runID = "run-cancel-during-fetch"
	err := eng.Run(ctx, runID, nil)
	if !errors.Is(err, ErrRunCancelled) {
		t.Fatalf("err = %v, want ErrRunCancelled", err)
	}
	var rt *RuntimeError
	if errors.As(err, &rt) {
		t.Fatalf("a typed failure %s rides the cancel: a runner keyed on it would re-offer a cancelled run", rt.Code)
	}
	run, lerr := fs.LoadRun(context.Background(), runID)
	if lerr != nil {
		t.Fatal(lerr)
	}
	if run.Status != store.RunStatusCancelled {
		t.Fatalf("status = %s, want cancelled", run.Status)
	}
}

// The run's own deadline cutting the fetch is no store failure either: no
// ATTACHMENTS_UNFETCHABLE, which the runner would re-offer after two
// minutes for a run the deadline already ended.
func TestADeadlineCuttingTheFetchCarriesNoStoreCode(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	fs := tmpStore(t)
	attachedRun(t, fs, "run-deadline-fetch")
	e := engineOn(t, blockingOpenStore{RunStore: fs, entered: make(chan struct{}, 1)})
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	_, err := e.materializeAttachments(ctx, "run-deadline-fetch")
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the deadline in the chain", err)
	}
	var rt *RuntimeError
	if errors.As(err, &rt) {
		t.Fatalf("the deadline's failure carries the store code %s", rt.Code)
	}
}

// An operator's cancel that reached the run ctx wins over a setup
// sentinel observed in the same instant — in the recorded status and in
// the returned error alike — so the redelivery never resumes the run the
// operator cancelled. Under a live ctx the sentinel keeps its own reading.
func TestTheCancelWinsOverTheSetupSentinels(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, sentinel := range []error{sandbox.ErrPhaseTimeout, sandbox.ErrCapacity} {
		cause := fmt.Errorf("runtime: sandbox: %w", errors.Join(sentinel, errors.New("in-pod tar extract: signal: killed")))
		st, _, code := setupFailureStatus(cancelled, "sandbox start", cause)
		if st != store.RunStatusCancelled || code != store.FailureCancelled {
			t.Fatalf("%v under a cancel: status %s / %s, want cancelled", sentinel, st, code)
		}
		if err := setupErrFrom(st, code, cause); !errors.Is(err, ErrRunCancelled) || errors.Is(err, sentinel) {
			t.Fatalf("%v under a cancel: err = %v, want the cancel alone", sentinel, err)
		}
		st, _, code = setupFailureStatus(context.Background(), "sandbox start", cause)
		if st != store.RunStatusFailedResumable {
			t.Fatalf("%v under a live ctx: status %s, want failed_resumable", sentinel, st)
		}
		if err := setupErrFrom(st, code, cause); !errors.Is(err, sentinel) || errors.Is(err, ErrRunCancelled) {
			t.Fatalf("%v under a live ctx: err = %v, want the sentinel kept", sentinel, err)
		}
		drained, drain := context.WithCancelCause(context.Background())
		drain(ErrRunInterrupted)
		st, _, code = setupFailureStatus(drained, "sandbox start", cause)
		if st != store.RunStatusFailedResumable {
			t.Fatalf("%v under a drain: status %s, want failed_resumable", sentinel, st)
		}
		// One decision for the record and the return (#2862): the sentinel
		// arm reads the cause before the ctx, so the drain's own marker does
		// not replace it — the runner re-offers under the sentinel, on its
		// delay and under the DLQ park, exactly as the record says.
		if err := setupErrFrom(st, code, cause); !errors.Is(err, sentinel) || errors.Is(err, ErrRunInterrupted) {
			t.Fatalf("%v under a drain: err = %v, want the sentinel alone", sentinel, err)
		}
	}
}
