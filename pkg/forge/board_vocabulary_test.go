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
	if _, ok := b.OptionForState("in_progress"); ok {
		t.Error("the dead option id must be dropped, so nothing retries a write that cannot land")
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

func TestReconcileStatusOptionsLosesEverythingWhenTheFieldIsGone(t *testing.T) {
	b := vocabBinding()
	rep := b.ReconcileStatusOptions(forge.Project{ID: "PVT_p", Number: 203})

	if len(rep.Lost) != 2 {
		t.Fatalf("Lost = %+v, want every mapped column — there is nothing left to write into", rep.Lost)
	}
	if len(b.StatusOptions) != 0 {
		t.Errorf("StatusOptions = %v, want empty", b.StatusOptions)
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
