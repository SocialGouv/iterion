package store

import (
	"context"
	"errors"
	"testing"
)

func TestValueFormatAuthority(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		raw  int
		want ValueFormat
	}{{0, LegacyValueFormat}, {1, LegacyValueFormat}, {2, ExactValueFormat}} {
		got, e := NormalizeValueFormat(tc.raw)
		if e != nil || got != tc.want {
			t.Fatal(tc, got, e)
		}
	}
	for _, v := range []int{-1, 3, 99} {
		if _, e := NormalizeValueFormat(v); !errors.Is(e, ErrUnsupportedValueFormat) {
			t.Fatal(v, e)
		}
	}
}
func TestValueFormatBindingRetainsOriginal(t *testing.T) {
	t.Parallel()
	r := &Run{ID: "r", FormatVersion: 2}
	b, e := CaptureRunValueFormat(r)
	if e != nil {
		t.Fatal(e)
	}
	ctx, e := WithRunValueFormat(t.Context(), b)
	if e != nil {
		t.Fatal(e)
	}
	r.FormatVersion = 1
	b.RunID = "other"
	b.Format = 1
	if f, e := CheckRunValueFormat(ctx, "r", 2); e != nil || f != ExactValueFormat {
		t.Fatal(f, e)
	}
	for _, tc := range []struct {
		id     string
		format int
		err    error
	}{{"r", 1, ErrValueFormatChanged}, {"other", 2, ErrValueFormatChanged}, {"r", 99, ErrUnsupportedValueFormat}} {
		if _, e = CheckRunValueFormat(ctx, tc.id, tc.format); !errors.Is(e, tc.err) {
			t.Fatal(tc, e)
		}
	}
	if f, e := ValueFormatForWrite(t.Context(), "r"); e != nil || f != LegacyValueFormat {
		t.Fatal(f, e)
	}
	if _, e = CheckRunValueFormat(t.Context(), "r", 2); !errors.Is(e, ErrValueFormatChanged) {
		t.Fatal(e)
	}
}
func TestValueFormatReplacementIsImmutableAndMetadataSafe(t *testing.T) {
	t.Parallel()
	for _, format := range []int{0, 1, 2} {
		before := &Run{ID: "r", FormatVersion: format, Name: "old"}
		next := *before
		next.Name = "new"
		if f, e := CheckRunFormatReplacement(t.Context(), before, &next); e != nil || f != format {
			t.Fatal(f, e)
		}
		other := 1
		if format < 2 {
			other = 2
		}
		next.FormatVersion = other
		if _, e := CheckRunFormatReplacement(t.Context(), before, &next); !errors.Is(e, ErrValueFormatChanged) {
			t.Fatal(e)
		}
		if before.Name != "old" || next.Name != "new" || next.FormatVersion != other {
			t.Fatal("check mutated snapshots")
		}
	}
	if f, e := CheckRunFormatReplacement(t.Context(), &Run{ID: "r", FormatVersion: 0}, &Run{ID: "r", FormatVersion: 1}); e != nil || f != 0 {
		t.Fatal(f, e)
	}
	if _, e := CheckRunFormatReplacement(t.Context(), nil, &Run{ID: "r", FormatVersion: 2}); !errors.Is(e, ErrValueFormatCapability) {
		t.Fatal(e)
	}
	if _, e := CheckRunFormatReplacement(t.Context(), nil, &Run{ID: "r", FormatVersion: 1}); e != nil {
		t.Fatal(e)
	}
	ctx, e := WithRunValueFormat(t.Context(), RunValueFormat{"r", LegacyValueFormat})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = CheckRunFormatReplacement(ctx, &Run{ID: "r", FormatVersion: 2}, &Run{ID: "r", FormatVersion: 2}); !errors.Is(e, ErrValueFormatChanged) {
		t.Fatal(e)
	}
}
func TestValueFormatRefusesInvalidAndCancelled(t *testing.T) {
	t.Parallel()
	for _, b := range []RunValueFormat{{"", 1}, {"r", 99}} {
		if _, e := WithRunValueFormat(t.Context(), b); e == nil {
			t.Fatal(b)
		}
	}
	if _, e := CaptureRunValueFormat(nil); !errors.Is(e, ErrInvalidValueFormat) {
		t.Fatal(e)
	}
	c, cancel := context.WithCancel(t.Context())
	cancel()
	if _, e := CheckRunValueFormat(c, "r", 1); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	if _, e := CheckRunFormatReplacement(c, nil, &Run{ID: "r", FormatVersion: 1}); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
}
