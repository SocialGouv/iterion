package model

import (
	"errors"
	"strings"
	"testing"
)

// TestGrowText_BoundsAndAppends pins the OOM guard on streamed
// text/thinking accumulation: growText appends under the cap and fails
// loud (never silently truncates) once a single content block would
// exceed maxTextBlockSize.
func TestGrowText_BoundsAndAppends(t *testing.T) {
	bs := &blockState{}
	if err := bs.growText("hello"); err != nil {
		t.Fatalf("growText under cap: unexpected err %v", err)
	}
	if err := bs.growText(" world"); err != nil {
		t.Fatalf("growText under cap: unexpected err %v", err)
	}
	if bs.text != "hello world" {
		t.Fatalf("text = %q, want %q", bs.text, "hello world")
	}

	// A delta that would push the buffer past the cap must fail loud and
	// leave the buffer unchanged (no partial append, no silent truncation).
	big := strings.Repeat("x", maxTextBlockSize)
	before := bs.text
	err := bs.growText(big)
	if err == nil {
		t.Fatal("growText over cap: expected error, got nil")
	}
	if !errors.Is(err, ErrTextBlockTooLarge) {
		t.Fatalf("growText over cap: err = %v, want ErrTextBlockTooLarge", err)
	}
	if bs.text != before {
		t.Fatalf("buffer mutated on over-cap growText: %d bytes, want %d", len(bs.text), len(before))
	}
}
