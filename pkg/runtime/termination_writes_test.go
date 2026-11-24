package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

type terminationContextKey struct{}

func TestTerminationWriteSequenceContract(t *testing.T) {
	for _, refusal := range []error{store.ErrValueFormatChanged, store.ErrUnsupportedValueFormat, errCorrectionOwnershipLost} {
		for _, kind := range []string{"cancellation", "interruption", "business", "ambiguous"} {
			t.Run(refusal.Error()+"/"+kind, func(t *testing.T) {
				base := context.WithValue(t.Context(), terminationContextKey{}, "kept")
				bound, err := store.WithRunValueFormat(base, store.RunValueFormat{RunID: "original", Format: 2})
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(bound)
				cancel()
				original := context.Canceled
				if kind == "interruption" {
					original = ErrRunInterrupted
				}
				if kind == "business" {
					original = &RuntimeError{Code: ErrCodeBudgetExceeded, Message: "established"}
				}
				if kind == "ambiguous" {
					original = &RuntimeError{Code: ErrCodeAmbiguousEffect, Message: "undecided"}
				}
				e := &Engine{}
				WithTerminationWriteTimeout(-1)(e)
				w := e.newTerminationWrites(ctx, original)
				defer w.Close()
				calls := 0
				first := w.Do(func(c context.Context) error {
					calls++
					if c.Err() != nil || c.Value(terminationContextKey{}) != "kept" {
						t.Fatal("context lost")
					}
					if f, err := store.ValueFormatForWrite(c, "original"); err != nil || f != 2 {
						t.Fatalf("binding=%v %v", f, err)
					}
					if _, err := store.ValueFormatForWrite(c, "successor"); !errors.Is(err, store.ErrValueFormatChanged) {
						t.Fatal("identity lost")
					}
					return refusal
				})
				second := w.Do(func(context.Context) error { calls++; return nil })
				result := w.Result(ErrRunCancelled)
				if first != refusal || second != refusal || !errors.Is(result, refusal) || calls != 1 {
					t.Fatalf("REFUSAL_NOT_STICKY: %v %v result=%v calls=%d", first, second, result, calls)
				}
				if errors.Is(result, ErrRunCancelled) || errors.Is(result, ErrRunInterrupted) {
					t.Fatalf("FALSE_PERSISTED_STOP: %v", result)
				}
				if kind == "business" || kind == "ambiguous" {
					var got *RuntimeError
					if !errors.As(result, &got) || got != original {
						t.Fatalf("VERDICT_ERASED: %v", result)
					}
				}
			})
		}
	}
	for _, budget := range []time.Duration{0, -1, time.Hour} {
		t.Run(budget.String(), func(t *testing.T) {
			e := &Engine{}
			WithTerminationWriteTimeout(budget)(e)
			w := e.newTerminationWrites(t.Context(), nil)
			defer w.Close()
			_, bounded := w.ctx.Deadline()
			if bounded != (budget >= 0) {
				t.Fatal("budget contract")
			}
			ordinary := errors.New("disk")
			if w.Do(func(context.Context) error { return ordinary }) != ordinary {
				t.Fatal("ordinary hidden")
			}
			calls := 0
			_ = w.Do(func(context.Context) error { calls++; return nil })
			if calls != 1 || w.Result(ordinary) != ordinary {
				t.Fatal("ordinary policy changed")
			}
		})
	}
	e := &Engine{}
	w := e.newTerminationWrites(t.Context(), store.ErrValueFormatChanged)
	defer w.Close()
	calls := 0
	_ = w.Do(func(context.Context) error { calls++; return nil })
	if calls != 0 {
		t.Fatal("initial refusal writes")
	}
}
