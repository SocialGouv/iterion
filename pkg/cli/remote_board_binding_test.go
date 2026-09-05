package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// The `--status-map` flag is the operator's escape hatch from the shipped
// five-column vocabulary, so its parser has to be strict where it matters
// (a silently-dropped pair would leave a column unmapped and inert) and
// forgiving where it does not (spaces around a column name that contains one).

// A LOST column keeps its cached option id — that is what makes the degradation
// re-derivable on the next pass — so the id is no longer the test for "the
// board does not carry this". `missing_statuses`, which every reconciliation
// recomputes, is; without it `board show` renders a broken column exactly like
// a working one, while the studio (which keys off `missing_statuses`) marks it.
func TestPrintBoardBindingMarksALostColumnThatKeptItsID(t *testing.T) {
	var out bytes.Buffer
	p := NewPrinter(OutputHuman)
	p.W = &out

	printBoardBinding(p, forge.BoardBinding{
		TenantID: "team-a", Provider: forge.ProviderGitHub,
		Owner: "SocialGouv", OwnerKind: forge.ProjectOwnerOrg, Number: 203,
		ConnectionID: "conn-1", ProjectID: "PVT_p", StatusFieldID: "PVTSSF_status",
		StatusMapping: []forge.StatusMapping{
			{Status: "Planned", State: "ready"},
			{Status: "In progress", State: "in_progress"},
		},
		// The lost column KEEPS its id; only `missing_statuses` says it is gone.
		StatusOptions:   map[string]string{"ready": "o_planned", "in_progress": "o_prog"},
		MissingStatuses: []string{"In progress"},
		DegradedReason:  `the Status field no longer carries "In progress" (in_progress)`,
	})

	var row string
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.Contains(line, "→ in_progress") {
			row = line
		}
	}
	if row == "" {
		t.Fatal("the status map did not render the in_progress row at all")
	}
	if !strings.HasPrefix(row, "! ") {
		t.Errorf("the lost column's row must be marked, got %q", row)
	}
	// ...and the healthy one must NOT be, or the marker says nothing.
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.Contains(line, "→ ready") && strings.HasPrefix(line, "! ") {
			t.Errorf("a column the board still carries must render unmarked, got %q", line)
		}
	}
}

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
