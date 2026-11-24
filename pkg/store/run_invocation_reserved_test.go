//go:build linux || darwin

package store

import (
	"context"
	"errors"
	"testing"
)

type reservedReceiptMutation struct {
	RunBirthReservation
	change func(*Run)
}

func (b reservedReceiptMutation) Publish(ctx context.Context, spec RunCreateSpec) (*Run, error) {
	r, e := b.RunBirthReservation.Publish(ctx, spec)
	if e != nil {
		return r, e
	}
	b.change(r)
	return r, nil
}
func TestReservedBirthRefusesInvalidReceipt(t *testing.T) {
	changes := map[string]func(*Run){
		"id":      func(r *Run) { r.ID = "foreign" },
		"parent":  func(r *Run) { r.ParentRunID = "foreign" },
		"birth":   func(r *Run) { r.BirthID = "" },
		"epoch":   func(r *Run) { r.ExecutionEpoch = "" },
		"version": func(r *Run) { r.CASVersion = 0 },
		"status":  func(r *Run) { r.Status = RunStatusFinished },
		"format":  func(r *Run) { r.FormatVersion = int(ExactValueFormat) },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			s := birthTestStore(t)
			ctx := birthTestContext(t, "child")
			b, e := s.BeginRunBirth(ctx, "child")
			if e != nil {
				t.Fatal(e)
			}
			defer func() { _ = b.Unlock() }()
			p, e := PrepareRunInvocation(ctx, s, ReservedRunInvocation{Spec: RunCreateSpec{ID: "child", Format: LegacyValueFormat, Status: RunStatusRunning}, Reservation: reservedReceiptMutation{b, change}})
			if _, _, owned := p.OriginalBirth(); owned || !errors.Is(e, ErrValueFormatChanged) {
				t.Fatalf("invalid receipt became authority: owned=%v err=%v", owned, e)
			}
			row, e := s.LoadRun(ctx, "child")
			if e != nil || row.Status != RunStatusRunning {
				t.Fatal("invalid response mutated durable birth", e)
			}
		})
	}
}

type publicationCycle struct{}

func (*publicationCycle) Error() string   { return "cycle" }
func (e *publicationCycle) Unwrap() error { return e }

type publicationSlice []error

func (publicationSlice) Error() string     { return "slice" }
func (e publicationSlice) Unwrap() []error { return e }
func TestBirthCancellationClassificationIsBounded(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		pure bool
	}{
		{"cancel", context.Canceled, true},
		{"joined", errors.Join(context.Canceled, context.Canceled), true},
		{"mixed", errors.Join(context.Canceled, errors.New("disk")), false},
		{"cycle", &publicationCycle{}, false},
		{"noncomparable", publicationSlice{context.Canceled}, true},
		{"empty", publicationSlice{}, false},
		{"nil_child", publicationSlice{context.Canceled, nil}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := pureBirthCancellation(tc.err, context.Canceled) != nil; got != tc.pure {
				t.Fatalf("pure=%v want=%v", got, tc.pure)
			}
		})
	}
	wrapped := &runBirthPublicationFailure{cause: errors.Join(context.Canceled), cancellation: context.Canceled}
	if !IsRunBirthPublicationCancellation(wrapped, context.Canceled) {
		t.Fatal("lost publication classification")
	}
	if IsRunBirthPublicationCancellation(errors.Join(wrapped, errors.New("disk")), context.Canceled) {
		t.Fatal("outside independent error erased")
	}
}
