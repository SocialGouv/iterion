package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestBindRunObservationCarriesHeaderAndFormatOnly(t *testing.T) {
	t.Parallel()
	header := RunDataHeader{RunID: "r", Format: ExactValueFormat, BirthID: "birth-a"}
	o, err := CaptureRunObservation(header, "stored-tenant", true)
	if err != nil {
		t.Fatal(err)
	}
	caller := WithIdentity(t.Context(), "requester-tenant", "u")
	bound, err := BindRunObservation(caller, o)
	if err != nil {
		t.Fatal(err)
	}
	if b, ok := RunDataBindingFrom(bound); !ok || b.Header() != header {
		t.Fatalf("binding = %+v %v", b, ok)
	}
	if f, err := ValueFormatForWrite(bound, "r"); err != nil || f != ExactValueFormat {
		t.Fatalf("format = %v %v", f, err)
	}
	if tenant, _ := TenantFromContext(bound); tenant != "requester-tenant" {
		t.Fatalf("transport tenant replaced by %q", tenant)
	}
	if err := CheckRunDataBinding(bound, header); err != nil {
		t.Fatal(err)
	}
	successor := header
	successor.BirthID = "birth-b"
	if err := CheckRunDataBinding(bound, successor); !errors.Is(err, ErrRunDataIdentityChanged) {
		t.Fatalf("successor admitted: %v", err)
	}
	if again, err := BindRunObservation(bound, o); err != nil || again == nil {
		t.Fatalf("same observation refused: %v", err)
	}
}

func TestBindRunObservationComparesWhatContextCarries(t *testing.T) {
	t.Parallel()
	a, err := CaptureRunObservation(RunDataHeader{RunID: "r", Format: LegacyValueFormat, BirthID: "a"}, "", true)
	if err != nil {
		t.Fatal(err)
	}
	b, err := CaptureRunObservation(RunDataHeader{RunID: "r", Format: LegacyValueFormat, BirthID: "b"}, "", true)
	if err != nil {
		t.Fatal(err)
	}
	boundA, err := BindRunObservation(t.Context(), a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := BindRunObservation(boundA, b); !errors.Is(err, ErrRunDataIdentityChanged) {
		t.Fatalf("carried binding replaced: %v", err)
	}
	exact, err := WithRunValueFormat(t.Context(), RunValueFormat{RunID: "r", Format: ExactValueFormat})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := BindRunObservation(exact, a); !errors.Is(err, ErrRunDataIdentityChanged) {
		t.Fatalf("carried format replaced: %v", err)
	}
}

func TestBindRunObservationRefusesWhatItCannotBind(t *testing.T) {
	t.Parallel()
	if _, err := BindRunObservation(t.Context(), RunObservation{}); !errors.Is(err, ErrRunDataBindingRequired) {
		t.Fatalf("unknown observation: %v", err)
	}
	absent, err := CaptureRunObservation(RunDataHeader{RunID: "r", Format: LegacyValueFormat}, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := BindRunObservation(t.Context(), absent); !errors.Is(err, ErrRunDataCapability) {
		t.Fatalf("observed absence bound: %v", err)
	}
	present, err := CaptureRunObservation(RunDataHeader{RunID: "r", Format: LegacyValueFormat, BirthID: "a"}, "", true)
	if err != nil {
		t.Fatal(err)
	}
	//nolint:staticcheck // SA1012: the nil context is the refused input.
	if _, err := BindRunObservation(nil, present); !errors.Is(err, ErrRunDataBindingRequired) {
		t.Fatalf("nil context: %v", err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := BindRunObservation(cancelled, present); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled context: %v", err)
	}
}

// The native filesystem store enforces what the helper carries: a successor
// recreated under the same ID is refused, not loaded.
func TestBindRunObservationNativeLoadRefusesSuccessor(t *testing.T) {
	t.Parallel()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	a, err := s.CreateRunWithValueFormat(ctx, RunCreateSpec{ID: "r", WorkflowName: "A", Format: ExactValueFormat})
	if err != nil {
		t.Fatal(err)
	}
	o, err := CaptureRunObservation(RunDataHeader{RunID: a.ID, Format: ValueFormat(a.FormatVersion), Protocol: a.DataProtocol, BirthID: a.BirthID}, a.TenantID, true)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := BindRunObservation(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.LoadRun(bound, "r"); err != nil || got.WorkflowName != "A" {
		t.Fatalf("original refused: %v", err)
	}
	if err := s.UpdateRunStatus(ctx, "r", RunStatusFinished, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRun(ctx, "r"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PruneDeletionMarkers(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRunWithValueFormat(ctx, RunCreateSpec{ID: "r", WorkflowName: "B", Format: ExactValueFormat}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.LoadRun(bound, "r"); !errors.Is(err, ErrRunDataIdentityChanged) {
		t.Fatalf("successor loaded: %+v %v", got, err)
	}
}
