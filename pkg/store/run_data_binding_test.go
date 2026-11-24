package store

import (
	"context"
	"errors"
	"testing"
)

func TestRunDataBindingRetainsOriginalIncarnation(t *testing.T) {
	original := RunDataHeader{RunID: "run", Format: ExactValueFormat, Protocol: IncarnationRunData, BirthID: "birth-a"}
	b, err := CaptureRunDataBinding(original)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := WithRunDataBinding(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	// Neither the source header nor a returned copy changes the retained identity.
	original.BirthID = "birth-b"
	copied := b.Header()
	copied.BirthID = "birth-b"
	if err := CheckRunDataBinding(ctx, original); !errors.Is(err, ErrRunDataIdentityChanged) {
		t.Fatalf("adopted successor: %v", err)
	}
	if err := CheckRunDataBinding(ctx, b.Header()); err != nil {
		t.Fatal(err)
	}
	successor, err := CaptureRunDataBinding(original)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := WithRunDataBinding(ctx, successor); !errors.Is(err, ErrRunDataIdentityChanged) {
		t.Fatalf("rebind adopted successor: %v", err)
	}
	if err := CheckMissingRunDataParent(ctx, "run"); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("incarnation fell back to orphan: %v", err)
	}
	// Epoch is intentionally absent: resume cannot rotate the data identity.
	if _, err := WithRunDataBinding(ctx, b); err != nil {
		t.Fatal(err)
	}
}

func TestRunDataBindingDoesNotLoseIdentityThroughFormatContext(t *testing.T) {
	h := RunDataHeader{RunID: "parent", Format: LegacyValueFormat, Protocol: IncarnationRunData, BirthID: "p"}
	b, err := CaptureRunDataBinding(h)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := WithRunDataBinding(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	same, err := WithRunValueFormat(ctx, RunValueFormat{RunID: "parent", Format: LegacyValueFormat})
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckRunDataBinding(same, h); err != nil {
		t.Fatal(err)
	}
	child, err := WithRunValueFormat(ctx, RunValueFormat{RunID: "child", Format: LegacyValueFormat})
	if err != nil {
		t.Fatal(err)
	}
	childHeader := RunDataHeader{RunID: "child", Format: LegacyValueFormat, Protocol: IncarnationRunData, BirthID: "c"}
	if err := CheckRunDataBinding(child, childHeader); !errors.Is(err, ErrRunDataIdentityChanged) {
		t.Fatalf("inherited parent context adopted child: %v", err)
	}
	if err := CheckRunDataBinding(child, h); !errors.Is(err, ErrRunDataIdentityChanged) {
		t.Fatalf("format-only child context reused parent data: %v", err)
	}
	if retained, ok := RunDataBindingFrom(child); !ok || retained != b {
		t.Fatal("format rebind erased original data identity")
	}
}

func TestRunDataBindingHistoricalAbsenceIsAnObservation(t *testing.T) {
	h := RunDataHeader{RunID: "legacy", Format: LegacyValueFormat}
	if err := CheckRunDataBinding(t.Context(), h); err != nil {
		t.Fatal(err)
	}
	if _, err := WithRunDataBinding(t.Context(), RunDataBinding{}); !errors.Is(err, ErrRunDataBindingRequired) {
		t.Fatalf("zero binding acquired authority: %v", err)
	}
	b, err := CaptureRunDataBinding(h)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := WithRunDataBinding(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckRunDataBinding(ctx, h); err != nil {
		t.Fatal(err)
	}
	h.BirthID = "new"
	if err := CheckRunDataBinding(ctx, h); !errors.Is(err, ErrRunDataIdentityChanged) {
		t.Fatalf("historical absence adopted new birth: %v", err)
	}
	if err := CheckMissingRunDataParent(ctx, "legacy"); err != nil {
		t.Fatalf("historical orphan compatibility changed: %v", err)
	}
}

func TestRunDataBindingRejectsUnboundUnknownAndCancelled(t *testing.T) {
	h := RunDataHeader{RunID: "strict", Format: LegacyValueFormat, Protocol: IncarnationRunData, BirthID: "birth"}
	if err := CheckRunDataBinding(t.Context(), h); !errors.Is(err, ErrRunDataBindingRequired) {
		t.Fatalf("unbound strict access: %v", err)
	}
	formatOnly, err := WithRunValueFormat(t.Context(), RunValueFormat{RunID: h.RunID, Format: h.Format})
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckRunDataBinding(formatOnly, h); !errors.Is(err, ErrRunDataBindingRequired) {
		t.Fatalf("format-only strict access: %v", err)
	}
	for _, protocol := range []RunDataProtocol{-1, 2, 1000} {
		invalid := h
		invalid.Protocol = protocol
		if _, err := CaptureRunDataBinding(invalid); !errors.Is(err, ErrUnsupportedRunDataProtocol) {
			t.Fatalf("protocol %d: %v", protocol, err)
		}
	}
	invalid := h
	invalid.BirthID = ""
	if _, err := CaptureRunDataBinding(invalid); !errors.Is(err, ErrInvalidRunDataIdentity) {
		t.Fatalf("missing strict birth: %v", err)
	}
	b, err := CaptureRunDataBinding(h)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := WithRunDataBinding(ctx, b); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := CheckRunDataBinding(ctx, h); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := CheckMissingRunDataParent(ctx, "strict"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
