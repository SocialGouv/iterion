package forge_test

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// The binding caches names AND ids, and the two go stale independently. These
// pin each way they can disagree with the board, and which of the two the
// repair believes.

func vocabProject(options ...forge.ProjectFieldOption) forge.Project {
	return forge.Project{ID: "PVT_p", Number: 203, Fields: []forge.ProjectField{
		{ID: "PVTSSF_status", Name: "Status", DataType: "SINGLE_SELECT", Options: options},
	}}
}

func vocabBinding() *forge.BoardBinding {
	return &forge.BoardBinding{
		TenantID: "team-a", Provider: forge.ProviderGitHub,
		Owner: "SocialGouv", OwnerKind: forge.ProjectOwnerOrg, Number: 203,
		ConnectionID: "conn-1", ProjectID: "PVT_p", StatusFieldID: "PVTSSF_status",
		StatusMapping: []forge.StatusMapping{
			{Status: "Planned", State: "ready"},
			{Status: "In progress", State: "in_progress"},
		},
		StatusOptions: map[string]string{"ready": "o_planned", "in_progress": "o_prog"},
	}
}

func TestReconcileStatusOptionsAdoptsARenamedColumn(t *testing.T) {
	b := vocabBinding()
	rep := b.ReconcileStatusOptions(vocabProject(
		forge.ProjectFieldOption{ID: "o_planned", Name: "Planned"},
		// Same id, new name: the forge keeps the id across a rename.
		forge.ProjectFieldOption{ID: "o_prog", Name: "Doing"},
	))

	if len(rep.Renamed) != 1 || rep.Renamed[0].From != "In progress" || rep.Renamed[0].To != "Doing" {
		t.Fatalf("Renamed = %+v, want the in_progress column", rep.Renamed)
	}
	if got, ok := forge.StatusForState(b.Mapping(), "in_progress"); !ok || got != "Doing" {
		t.Errorf("mapping = %q (ok=%v), want the board's current name", got, ok)
	}
	if id, _ := b.OptionForState("in_progress"); id != "o_prog" {
		t.Errorf("option id = %q, want the unchanged one — a rename keeps it", id)
	}
	// The board maps the new name back onto the state, so the IMPORT direction
	// works again too. Without that it silently no-ops on every item.
	if st, ok := forge.StateForStatus(b.Mapping(), "Doing"); !ok || st != "in_progress" {
		t.Errorf("StateForStatus(%q) = %q,%v — the import direction must resolve too", "Doing", st, ok)
	}
	if rep.Reason() != "" {
		t.Errorf("a rename is repairable, not a degradation: %q", rep.Reason())
	}
	if got := rep.Renames()["in progress"]; got != "Doing" {
		t.Errorf("Renames() = %v, want the old name folded to the new", rep.Renames())
	}
}

func TestReconcileStatusOptionsRebindsARecreatedColumn(t *testing.T) {
	b := vocabBinding()
	rep := b.ReconcileStatusOptions(vocabProject(
		forge.ProjectFieldOption{ID: "o_planned", Name: "Planned"},
		// Same name, NEW id: deleted and re-added.
		forge.ProjectFieldOption{ID: "o_prog2", Name: "In progress"},
	))

	if len(rep.Rebound) != 1 || rep.Rebound[0] != "in_progress" {
		t.Fatalf("Rebound = %+v, want the in_progress column", rep.Rebound)
	}
	if id, _ := b.OptionForState("in_progress"); id != "o_prog2" {
		t.Errorf("option id = %q, want the board's current one", id)
	}
	if rep.Reason() != "" {
		t.Errorf("a re-created column resolves by name; it is not a degradation: %q", rep.Reason())
	}
}

func TestReconcileStatusOptionsAdoptsAColumnAddedSinceTheBind(t *testing.T) {
	b := vocabBinding()
	delete(b.StatusOptions, "in_progress") // the board had no such column at bind time
	b.MissingStatuses = []string{"In progress"}

	rep := b.ReconcileStatusOptions(vocabProject(
		forge.ProjectFieldOption{ID: "o_planned", Name: "Planned"},
		forge.ProjectFieldOption{ID: "o_prog", Name: "In progress"},
	))
	if len(rep.Adopted) != 1 || rep.Adopted[0] != "in_progress" {
		t.Fatalf("Adopted = %+v, want the column the operator added", rep.Adopted)
	}
	if id, _ := b.OptionForState("in_progress"); id != "o_prog" {
		t.Errorf("option id = %q, want the newly discovered one", id)
	}
	if len(b.MissingStatuses) != 0 {
		t.Errorf("MissingStatuses = %v, want empty — the column exists now", b.MissingStatuses)
	}
}

func TestReconcileStatusOptionsLosesADeletedColumn(t *testing.T) {
	b := vocabBinding()
	rep := b.ReconcileStatusOptions(vocabProject(
		forge.ProjectFieldOption{ID: "o_planned", Name: "Planned"},
	))

	if len(rep.Lost) != 1 || rep.Lost[0].State != "in_progress" || rep.Lost[0].Status != "In progress" {
		t.Fatalf("Lost = %+v, want the deleted column with both its names", rep.Lost)
	}
	// The dead id is KEPT — it is the evidence that makes the loss re-derivable
	// on the next pass. What stops a write that cannot land is LostStates.
	if !rep.LostStates()["in_progress"] {
		t.Error("LostStates must refuse the state, since the dead id is still cached")
	}
	reason := rep.Reason()
	if !strings.Contains(reason, "In progress") || !strings.Contains(reason, "in_progress") {
		t.Errorf("reason = %q, want it to name the COLUMN (the remedy) and the state (the effect)", reason)
	}
	// The covered half keeps working: a degradation is partial, never a stop.
	if id, _ := b.OptionForState("ready"); id != "o_planned" {
		t.Errorf("the surviving column must still resolve, got %q", id)
	}
}

// TestReconcileStatusOptionsKeepsReportingALostColumn: the loss must be
// RE-DERIVABLE, not a one-shot observation.
//
// A repair that dropped the cached id destroyed the only evidence the column
// had ever resolved, so the very next pass reported nothing lost — and the
// caller, reading "nothing lost", cleared the degradation on the first
// unrelated adoption while the column was still gone.
func TestReconcileStatusOptionsKeepsReportingALostColumn(t *testing.T) {
	b := vocabBinding()
	deleted := vocabProject(forge.ProjectFieldOption{ID: "o_planned", Name: "Planned"})

	for pass := 1; pass <= 3; pass++ {
		rep := b.ReconcileStatusOptions(deleted)
		if len(rep.Lost) != 1 || rep.Lost[0].State != "in_progress" {
			t.Fatalf("pass %d: Lost = %+v, want the still-missing column on EVERY pass", pass, rep.Lost)
		}
		if !strings.Contains(rep.Reason(), "In progress") {
			t.Fatalf("pass %d: reason = %q, want it to keep naming the column", pass, rep.Reason())
		}
	}
	// The cached id is what makes it re-derivable, so it is kept — the reflect
	// is stopped by the LostStates set, not by a hole in the binding.
	if b.StatusOptions["in_progress"] != "o_prog" {
		t.Errorf("cached id = %q, want it kept as the evidence of the loss", b.StatusOptions["in_progress"])
	}
	if !b.ReconcileStatusOptions(deleted).LostStates()["in_progress"] {
		t.Error("LostStates must name the state, so the reflect can refuse it instead of writing a dead id")
	}
}

// TestReconcileStatusOptionsDoesNotLoseAnUnresolvedColumn: a mapped column the
// board never carried has no cached id, so nothing was lost — it is the
// ordinary `missing_statuses` shape, which BindBoard accepts on purpose. Only
// a column the binding HAD resolved can be lost.
func TestReconcileStatusOptionsDoesNotLoseAnUnresolvedColumn(t *testing.T) {
	b := vocabBinding()
	delete(b.StatusOptions, "in_progress") // never resolved at bind time
	b.MissingStatuses = []string{"In progress"}

	rep := b.ReconcileStatusOptions(vocabProject(
		forge.ProjectFieldOption{ID: "o_planned", Name: "Planned"},
	))
	if len(rep.Lost) != 0 || rep.Reason() != "" {
		t.Fatalf("partial coverage is not a degradation: %+v / %q", rep.Lost, rep.Reason())
	}
	if len(b.MissingStatuses) != 1 || b.MissingStatuses[0] != "In progress" {
		t.Errorf("...but it must still be reported: %v", b.MissingStatuses)
	}
}

// A state the binding has no mapping for at all cannot be lost, whatever the
// board carries: it is inert by design (§2), not broken.
func TestReconcileStatusOptionsIgnoresAnUnmappedState(t *testing.T) {
	b := vocabBinding()
	b.StatusOptions["review"] = "o_review" // an orphan id with no mapping entry

	rep := b.ReconcileStatusOptions(vocabProject(
		forge.ProjectFieldOption{ID: "o_planned", Name: "Planned"},
		forge.ProjectFieldOption{ID: "o_prog", Name: "In progress"},
	))
	for _, c := range rep.Lost {
		if c.State == "review" {
			t.Fatalf("an unmapped state is inert, never lost: %+v", rep.Lost)
		}
	}
}

func TestReconcileStatusOptionsLosesEverythingWhenTheFieldIsGone(t *testing.T) {
	b := vocabBinding()
	rep := b.ReconcileStatusOptions(forge.Project{ID: "PVT_p", Number: 203})

	if len(rep.Lost) != 2 {
		t.Fatalf("Lost = %+v, want every mapped column — there is nothing left to write into", rep.Lost)
	}
	if lost := rep.LostStates(); !lost["ready"] || !lost["in_progress"] {
		t.Errorf("LostStates = %v, want every state refused", lost)
	}
	// Re-derivable: the second pass says the same thing, because the ids the
	// first one read are still there to read.
	if again := b.ReconcileStatusOptions(forge.Project{ID: "PVT_p", Number: 203}); len(again.Lost) != 2 {
		t.Errorf("second pass Lost = %+v, want the same two", again.Lost)
	}
}

func TestReconcileStatusOptionsRebindsARecreatedField(t *testing.T) {
	b := vocabBinding()
	project := vocabProject(
		forge.ProjectFieldOption{ID: "o_planned", Name: "Planned"},
		forge.ProjectFieldOption{ID: "o_prog", Name: "In progress"},
	)
	project.Fields[0].ID = "PVTSSF_fresh" // the whole field deleted and re-added

	rep := b.ReconcileStatusOptions(project)
	if !rep.Changed() || b.StatusFieldID != "PVTSSF_fresh" {
		t.Fatalf("the field id must be re-resolved by name: %q (%+v)", b.StatusFieldID, rep)
	}
	if rep.Reason() != "" {
		t.Errorf("a re-created field resolves by name; it is not a degradation: %q", rep.Reason())
	}
}

func TestReconcileStatusOptionsIsANoOpOnAnAgreeingBoard(t *testing.T) {
	b := vocabBinding()
	rep := b.ReconcileStatusOptions(vocabProject(
		forge.ProjectFieldOption{ID: "o_planned", Name: "Planned"},
		forge.ProjectFieldOption{ID: "o_prog", Name: "In progress"},
	))
	if rep.Changed() {
		t.Fatalf("nothing drifted, so nothing may be written back: %+v", rep)
	}
	if rep.Renames() != nil {
		t.Errorf("Renames() = %v, want nil", rep.Renames())
	}
}

// A labels-only binding (the board carries no Status field, a shape BindBoard
// accepts) has no status vocabulary at all — it must not be reported as
// degraded for something it never bound.
func TestReconcileStatusOptionsLeavesALabelsOnlyBindingAlone(t *testing.T) {
	b := vocabBinding()
	b.StatusFieldID = ""
	b.StatusOptions = nil

	rep := b.ReconcileStatusOptions(forge.Project{ID: "PVT_p", Number: 203})
	if rep.Changed() || rep.Reason() != "" {
		t.Fatalf("a labels-only binding has nothing to repair: %+v", rep)
	}
}

// Case and whitespace are how a board writes a column, not what it is: the
// repair must not report "In Progress" as a rename of "In progress".
func TestReconcileStatusOptionsIgnoresCaseAndSpacing(t *testing.T) {
	b := vocabBinding()
	rep := b.ReconcileStatusOptions(vocabProject(
		forge.ProjectFieldOption{ID: "o_planned", Name: " planned "},
		forge.ProjectFieldOption{ID: "o_prog", Name: "IN PROGRESS"},
	))
	if rep.Changed() {
		t.Fatalf("a case/spacing difference is the same column: %+v", rep)
	}
}
