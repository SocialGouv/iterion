//go:build linux || darwin

package runtime

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

type reservedBirthFailure struct {
	store.RunBirthReservation
	cancel context.CancelFunc
	cause  error
}

func (b reservedBirthFailure) Publish(ctx context.Context, spec store.RunCreateSpec) (*store.Run, error) {
	r, e := b.RunBirthReservation.Publish(ctx, spec)
	if e != nil {
		return r, e
	}
	b.cancel()
	return r, b.cause
}
func TestReservedBirthReceiptAndRetirement(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause error
		want  store.RunStatus
	}{
		{"opaque_schema", opaqueBirthSchema{}, store.RunStatusFailed},
		{"unknown_Is_only", unknownBirthCancellation{}, store.RunStatusFailed},
		{"pure_join", fmt.Errorf("publication: %w", errors.Join(context.Canceled)), store.RunStatusCancelled},
		{"mixed_durability", errors.Join(context.Canceled, errors.New("directory sync failed")), store.RunStatusFailed},
		{"ambiguous", &RuntimeError{Code: ErrCodeAmbiguousEffect, Cause: context.Canceled}, store.RunStatusFailed},
		{"typed_business", &RuntimeError{Code: ErrCodeSchemaValidation, Cause: context.Canceled}, store.RunStatusFailed},
		{"nested_typed", &RuntimeError{Code: ErrCodeExecutionFailed, Cause: errors.Join(context.Canceled, &RuntimeError{Code: ErrCodeAmbiguousEffect, Cause: context.Canceled})}, store.RunStatusFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, e := store.New(t.TempDir())
			if e != nil {
				t.Fatal(e)
			}
			active, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx, e := store.WithRunValueFormat(active, store.RunValueFormat{RunID: "child", Format: store.LegacyValueFormat})
			if e != nil {
				t.Fatal(e)
			}
			reservation, e := s.BeginRunBirth(ctx, "child")
			if e != nil {
				t.Fatal(e)
			}
			defer func() { _ = reservation.Unlock() }()
			prepared, e := store.PrepareRunInvocation(ctx, s, store.ReservedRunInvocation{Spec: store.RunCreateSpec{ID: "child", Format: store.LegacyValueFormat, Status: store.RunStatusRunning}, Reservation: reservedBirthFailure{reservation, cancel, tc.cause}})
			if _, _, owned := prepared.OriginalBirth(); !owned || e == nil {
				t.Fatalf("lost failed-publication receipt: owned=%v err=%v", owned, e)
			}
			if !errors.Is(e, context.Canceled) {
				t.Fatal("original cancellation lost", e)
			}
			// Mutating the exposed response cannot replace the private birth observation.
			originalBirth, ok := prepared.OriginalBirthID()
			if !ok {
				t.Fatal("missing immutable birth identity")
			}
			prepared.Run.BirthID = "foreign"
			if got, ok := prepared.OriginalBirthID(); !ok || got != originalBirth {
				t.Fatal("public response changed original birth identity")
			}
			prepared.Run.ID = "foreign"
			prepared.Run.ExecutionEpoch = "foreign"
			if e = RetirePreparedRun(prepared.Context, s, prepared, e); e == nil {
				t.Fatal("retirement hid preparation failure")
			}
			row, e := s.LoadRun(context.WithoutCancel(ctx), "child")
			if e != nil {
				t.Fatal(e)
			}
			if row.Status != tc.want {
				t.Fatalf("status=%s want=%s", row.Status, tc.want)
			}
			if IsAmbiguousEffect(tc.cause) && row.FailureCode != store.FailureAmbiguousEffect {
				t.Fatalf("ambiguity lost: %s", row.FailureCode)
			}
			var typed *RuntimeError
			if !IsAmbiguousEffect(tc.cause) && errors.As(tc.cause, &typed) && typed.Code != ErrCodeExecutionFailed && row.FailureCode != typed.Code {
				t.Fatalf("typed failure lost: %s want=%s", row.FailureCode, typed.Code)
			}

		})
	}
}

type opaqueBirthSchema struct{}

func (opaqueBirthSchema) Error() string { return "opaque schema" }
func (opaqueBirthSchema) Unwrap() error { return context.Canceled }
func (opaqueBirthSchema) As(target any) bool {
	if p, ok := target.(**RuntimeError); ok {
		*p = &RuntimeError{Code: ErrCodeSchemaValidation, Cause: context.Canceled}
		return true
	}
	return false
}

type unknownBirthCancellation struct{}

func (unknownBirthCancellation) Error() string        { return "independent failure claiming cancellation" }
func (unknownBirthCancellation) Is(target error) bool { return target == context.Canceled }
