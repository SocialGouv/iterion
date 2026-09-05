package cli

import (
	"strings"
	"testing"
)

// The `--status-map` flag is the operator's escape hatch from the shipped
// five-column vocabulary, so its parser has to be strict where it matters
// (a silently-dropped pair would leave a column unmapped and inert) and
// forgiving where it does not (spaces around a column name that contains one).

func TestParseStatusMapFlag(t *testing.T) {
	got, err := ParseStatusMapFlag("Todo=ready,In Progress=in_progress,Shipped=done")
	if err != nil {
		t.Fatalf("ParseStatusMapFlag: %v", err)
	}
	want := map[string]string{"Todo": "ready", "In Progress": "in_progress", "Shipped": "done"}
	if len(got) != len(want) {
		t.Fatalf("got %d pairs, want %d: %v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%q → %q, want %q", k, got[k], v)
		}
	}
}

func TestParseStatusMapFlagTrimsAroundTheSeparators(t *testing.T) {
	got, err := ParseStatusMapFlag("  Todo = ready , Shipped=done ")
	if err != nil {
		t.Fatalf("ParseStatusMapFlag: %v", err)
	}
	if got["Todo"] != "ready" || got["Shipped"] != "done" {
		t.Errorf("trimming wrong: %v", got)
	}
}

func TestParseStatusMapFlagEmptyIsNoOverride(t *testing.T) {
	got, err := ParseStatusMapFlag("   ")
	if err != nil {
		t.Fatalf("ParseStatusMapFlag: %v", err)
	}
	if got != nil {
		t.Errorf("an empty flag means 'use the default', got %v", got)
	}
}

func TestParseStatusMapFlagRejectsMalformedPairs(t *testing.T) {
	for _, in := range []string{
		"Todo",         // no '='
		"Todo=",        // no state
		"=ready",       // no column
		"Todo=ready,,", // an empty pair is a typo, not an intention
		"Todo=ready=oops",
	} {
		if _, err := ParseStatusMapFlag(in); err == nil {
			t.Errorf("ParseStatusMapFlag(%q) must fail: a dropped pair leaves a column silently inert", in)
		}
	}
}

func TestParseStatusMapFlagRejectsADuplicateColumn(t *testing.T) {
	_, err := ParseStatusMapFlag("Todo=ready,Todo=done")
	if err == nil {
		t.Fatal("a column named twice is ambiguous")
	}
	if !strings.Contains(err.Error(), "Todo") {
		t.Errorf("the error must name the column, got %q", err)
	}
}
