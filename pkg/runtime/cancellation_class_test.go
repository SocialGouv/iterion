package runtime

import (
	"context"
	"errors"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/store"
	"testing"
	"time"
)

func TestWrappedCancellationUsesLiveOriginalForFinalWrites(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"live", "cancelled", "deadline"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			st := tmpStore(t)
			r, err := st.CreateRun(t.Context(), "run", "wf", nil)
			if err != nil {
				t.Fatal(err)
			}
			ctx, err := store.BindRunValueFormat(t.Context(), r)
			if err != nil {
				t.Fatal(err)
			}
			var cause = context.Canceled
			if phase == "cancelled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if phase == "deadline" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer cancel()
				cause = context.DeadlineExceeded
			}
			calls := 0
			e := admissionSnapshotEngine(t, st, nil, &calls)
			rs := e.newRunState("run", nil)
			rs.ctx = ctx
			err = e.failRunErrWithCheckpoint(rs, "node", fmt.Errorf("wrapped non-model wait: %w", cause))
			if err == nil {
				t.Fatal("lost error")
			}
			fresh, loadErr := st.LoadRun(t.Context(), "run")
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			want := store.RunStatusFailedResumable
			if phase == "cancelled" {
				want = store.RunStatusCancelled
				if !errors.Is(err, ErrRunCancelled) {
					t.Fatalf("cancel cause=%v", err)
				}
			}
			if fresh.Status != want {
				t.Fatalf("phase=%s status=%s want=%s error=%v", phase, fresh.Status, want, err)
			}
		})
	}
}

func TestCancellationClassifierPreservesUnrelatedAndAuthorityErrors(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"unrelated", "format", "ownership"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			st := tmpStore(t)
			r, err := st.CreateRun(t.Context(), "run", "wf", nil)
			if err != nil {
				t.Fatal(err)
			}
			ctx, err := store.BindRunValueFormat(t.Context(), r)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(ctx)
			cancel()
			cause := errors.New("unrelated business error")
			if kind == "format" {
				cause = errors.Join(store.ErrValueFormatChanged, context.Canceled)
			}
			if kind == "ownership" {
				cause = errors.Join(errCorrectionOwnershipLost, context.Canceled)
			}
			calls := 0
			e := admissionSnapshotEngine(t, st, nil, &calls)
			rs := e.newRunState("run", nil)
			rs.ctx = ctx
			got := e.failRunErrWithCheckpoint(rs, "node", cause)
			if errors.Is(got, ErrRunCancelled) {
				t.Fatalf("%s was reclassified as cancellation: %v", kind, got)
			}
			if kind == "format" && !errors.Is(got, store.ErrValueFormatChanged) {
				t.Fatalf("format refusal cause lost: %v", got)
			}
			if kind == "ownership" && got != cause {
				t.Fatalf("ownership error replaced: %v", got)
			}
			fresh, err := st.LoadRun(t.Context(), "run")
			if err != nil {
				t.Fatal(err)
			}
			events, err := st.LoadEvents(t.Context(), "run")
			if err != nil {
				t.Fatal(err)
			}
			if kind == "unrelated" {
				// The checkpoint entry now owns a live write sequence. It records
				// this failure without reclassifying it as the concurrent cancel.
				var verdict *RuntimeError
				if fresh.Status != store.RunStatusFailedResumable || fresh.FailureCode != ErrCodeExecutionFailed || fresh.Error != cause.Error() || fresh.Checkpoint == nil || fresh.Checkpoint.NodeID != "node" || !errors.As(got, &verdict) || verdict.Code != ErrCodeExecutionFailed || verdict.Message != cause.Error() || len(events) != 1 || events[0].Type != store.EventRunFailed {
					t.Fatalf("UNRELATED_FAILURE_NOT_RECORDED status=%s code=%s error=%q checkpoint=%+v events=%v result=%v", fresh.Status, fresh.FailureCode, fresh.Error, fresh.Checkpoint, events, got)
				}
			} else if fresh.Status != store.RunStatusRunning || fresh.CASVersion != r.CASVersion || fresh.Checkpoint != nil || len(events) != 0 {
				t.Fatalf("AUTHORITY_REFUSAL_WROTE status=%s version=%d want=%d events=%v", fresh.Status, fresh.CASVersion, r.CASVersion, events)
			}
		})
	}
}

func TestBothFailureEntrypointsPreserveFormatRefusal(t *testing.T) {
	t.Parallel()
	for _, checkpoint := range []bool{false, true} {
		for _, cancelled := range []bool{false, true} {
			t.Run(fmt.Sprintf("checkpoint=%t/cancelled=%t", checkpoint, cancelled), func(t *testing.T) {
				t.Parallel()
				st := tmpStore(t)
				r, err := st.CreateRun(t.Context(), "run", "wf", nil)
				if err != nil {
					t.Fatal(err)
				}
				ctx, err := store.BindRunValueFormat(t.Context(), r)
				if err != nil {
					t.Fatal(err)
				}
				if cancelled {
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					cancel()
				}
				original := errors.Join(store.ErrValueFormatChanged, context.Canceled)
				calls := 0
				e := admissionSnapshotEngine(t, st, nil, &calls)
				var got error
				if checkpoint {
					rs := e.newRunState("run", nil)
					rs.ctx = ctx
					got = e.failRunErrWithCheckpoint(rs, "node", original)
				} else {
					got = e.failRunErr(ctx, "run", "node", original)
				}
				if got != original {
					t.Fatalf("refusal lost: %v", got)
				}
				fresh, err := st.LoadRun(t.Context(), "run")
				if err != nil || fresh.Status != store.RunStatusRunning {
					t.Fatalf("refusal wrote run: %+v %v", fresh, err)
				}
				events, err := st.LoadEvents(t.Context(), "run")
				if err != nil || len(events) != 0 {
					t.Fatalf("refusal emitted event: %v %v", events, err)
				}
			})
		}
	}
}

func TestFailureEntrypointsKeepUnsupportedFormatOverDeadline(t *testing.T) {
	t.Parallel()
	for _, checkpoint := range []bool{false, true} {
		t.Run(fmt.Sprint(checkpoint), func(t *testing.T) {
			t.Parallel()
			st := tmpStore(t)
			r, err := st.CreateRun(t.Context(), "run", "wf", nil)
			if err != nil {
				t.Fatal(err)
			}
			ctx, err := store.BindRunValueFormat(t.Context(), r)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
			defer cancel()
			original := &RuntimeError{Code: ErrCodeExecutionFailed, Message: "future format", Cause: errors.Join(store.ErrUnsupportedValueFormat, context.DeadlineExceeded)}
			calls := 0
			e := admissionSnapshotEngine(t, st, nil, &calls)
			var got error
			if checkpoint {
				rs := e.newRunState("run", nil)
				rs.ctx = ctx
				got = e.failRunErrWithCheckpoint(rs, "node", original)
			} else {
				got = e.failRunErr(ctx, "run", "node", original)
			}
			var typed *RuntimeError
			if got != original || !errors.Is(got, store.ErrUnsupportedValueFormat) || !errors.As(got, &typed) || typed != original {
				t.Fatalf("future refusal lost: %v", got)
			}
			fresh, err := st.LoadRun(t.Context(), "run")
			if err != nil || fresh.Status != store.RunStatusRunning {
				t.Fatalf("refusal wrote run: %+v %v", fresh, err)
			}
		})
	}
}
