package server

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/forge"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// The binding's cached status vocabulary — (state → option id) plus the column
// NAMES those ids carried at bind time — against a board whose columns an
// operator keeps editing.
//
// Ids are what the reflect WRITES; names are what both directions COMPARE. The
// two go stale independently, and every one of these tests is a shape where
// they disagree.

// boundBinding resolves a binding the way the bind endpoint does — by READING
// the board — so its cached option ids are the fixture project's real ones.
// testBinding()'s hand-written ids predate the id-based reflect and cannot
// exercise it.
func boundBinding(t *testing.T, project forge.Project) *forge.BoardBinding {
	t.Helper()
	b, err := forge.BindBoard(context.Background(), &fakeBoardClient{project: project}, forge.BindRequest{
		TenantID: "team-a", Provider: forge.ProviderGitHub,
		Ref: testProjectRef, ConnectionID: "conn-1",
	})
	if err != nil {
		t.Fatalf("BindBoard: %v", err)
	}
	return &b
}

// renamedColumn returns the fixture project with one Status option renamed,
// KEEPING its id — which is exactly what the forge does on a rename.
func renamedColumn(t *testing.T, from, to string) forge.Project {
	t.Helper()
	return editColumn(t, from, func(o *forge.ProjectFieldOption) { o.Name = to })
}

// recreatedColumn returns the fixture project with one Status option given a
// NEW id under the same name — a column deleted and re-added.
func recreatedColumn(t *testing.T, name, newID string) forge.Project {
	t.Helper()
	return editColumn(t, name, func(o *forge.ProjectFieldOption) { o.ID = newID })
}

// deletedColumn returns the fixture project with one Status option removed.
func deletedColumn(t *testing.T, name string) forge.Project {
	t.Helper()
	p := testProject()
	for i, f := range p.Fields {
		if !strings.EqualFold(f.Name, forge.ProjectStatusFieldName) {
			continue
		}
		opts := make([]forge.ProjectFieldOption, 0, len(f.Options))
		for _, o := range f.Options {
			if strings.EqualFold(o.Name, name) {
				continue
			}
			opts = append(opts, o)
		}
		if len(opts) == len(f.Options) {
			t.Fatalf("fixture has no %q column", name)
		}
		p.Fields[i].Options = opts
		return p
	}
	t.Fatalf("fixture has no %s field", forge.ProjectStatusFieldName)
	return p
}

func editColumn(t *testing.T, name string, edit func(*forge.ProjectFieldOption)) forge.Project {
	t.Helper()
	p := testProject()
	for i, f := range p.Fields {
		if !strings.EqualFold(f.Name, forge.ProjectStatusFieldName) {
			continue
		}
		opts := append([]forge.ProjectFieldOption(nil), f.Options...)
		for j := range opts {
			if strings.EqualFold(opts[j].Name, name) {
				edit(&opts[j])
				p.Fields[i].Options = opts
				return p
			}
		}
		t.Fatalf("fixture has no %q column", name)
	}
	t.Fatalf("fixture has no %s field", forge.ProjectStatusFieldName)
	return p
}

// statusOption is statusValue plus the option id the forge reports alongside
// the name. The id is the load-bearing half: it survives a rename, so it is
// the only honest answer to "is the board already carrying what we would
// write?".
func statusOption(name, optionID string, at time.Time) forge.ProjectItemField {
	fv := statusValue(name, at)
	fv.OptionID = optionID
	return fv
}

// optionID reads a column's id out of the fixture project.
func optionID(t *testing.T, project forge.Project, name string) string {
	t.Helper()
	f, ok := project.Field(forge.ProjectStatusFieldName)
	if !ok {
		t.Fatalf("fixture has no %s field", forge.ProjectStatusFieldName)
	}
	o, ok := f.Option(name)
	if !ok {
		t.Fatalf("fixture has no %q column", name)
	}
	return o.ID
}

// TestSyncProjectBoardConvergesAfterAColumnIsRenamed is the whole finding.
//
// The reflect decides by NAME and writes by OPTION ID. A renamed column keeps
// its id, so after "In progress" becomes "Doing": the import goes inert
// (StateForStatus misses the new name), and the reflect resolves the SAME
// still-valid option, calls SetSingleSelect — a no-op on the forge's side —
// then records the old name again. The next pass reads the board's new name
// and repeats, identically, forever: one GraphQL mutation per bound card per
// pass, indefinitely, with nothing anywhere saying why.
func TestSyncProjectBoardConvergesAfterAColumnIsRenamed(t *testing.T) {
	board := newTestBoard(t)
	at := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	// The card moved to in_progress natively; the board still says Planned,
	// which is what we recorded — so the reflect has a real push to make.
	seedSynced(t, board, 613, native.StateInProgress, "Planned", at)
	project := testProject()
	inProgressID := optionID(t, project, "In progress")

	bindings := forge.NewMemoryBoardBindingStore()
	binding := boundBinding(t, project)
	if err := bindings.Upsert(context.Background(), *binding); err != nil {
		t.Fatalf("seed binding: %v", err)
	}
	bc := &fakeBoardClient{project: project, pages: [][]forge.ProjectItem{{
		item("PVTI_1", 613, statusOption("Planned", optionID(t, project, "Planned"), at)),
	}}}
	opts := &ProjectImportOptions{Binding: binding, Bindings: bindings}

	res, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, board, opts)
	if err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if res.Reflected != 1 || len(bc.writes) != 1 || bc.writes[0].OptionID != inProgressID {
		t.Fatalf("pass 1 must push the native move: reflected=%d writes=%+v", res.Reflected, bc.writes)
	}

	// The operator renames the column. The option KEEPS its id.
	renamed := renamedColumn(t, "In progress", "Doing")
	bc.project = renamed
	bc.pages = [][]forge.ProjectItem{{
		item("PVTI_1", 613, statusOption("Doing", inProgressID, at.Add(time.Minute))),
	}}

	for pass := 2; pass <= 4; pass++ {
		if _, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, board, opts); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
	}
	if len(bc.writes) != 1 {
		t.Errorf("writes = %+v, want the single pass-1 push — a renamed column must not be rewritten on every pass", bc.writes)
	}

	// And the binding healed itself, on BOTH twins' contract: the column's new
	// name is resolved from the id it kept, so the IMPORT direction works too.
	stored, err := bindings.GetByTenant(context.Background(), "team-a")
	if err != nil {
		t.Fatalf("read binding: %v", err)
	}
	if got, ok := forge.StatusForState(stored.Mapping(), native.StateInProgress); !ok || got != "Doing" {
		t.Errorf("stored column for in_progress = %q (ok=%v), want %q — the repair must be remembered", got, ok, "Doing")
	}
	if stored.StatusOptions[native.StateInProgress] != inProgressID {
		t.Errorf("stored option id = %q, want the unchanged %q", stored.StatusOptions[native.StateInProgress], inProgressID)
	}
	if stored.Degraded() {
		t.Errorf("a rename is repairable, not a degradation: %q", stored.DegradedReason)
	}
}

// TestSyncProjectBoardRebindsARecreatedColumn: a column deleted and re-added
// under the same name gets a NEW id. The cached id is dead, but the NAME still
// resolves — which is exactly what the bind did — so this self-heals too.
func TestSyncProjectBoardRebindsARecreatedColumn(t *testing.T) {
	board := newTestBoard(t)
	at := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	id := seedSynced(t, board, 613, native.StateInProgress, "Planned", at)
	project := testProject()

	bindings := forge.NewMemoryBoardBindingStore()
	binding := boundBinding(t, project)
	if err := bindings.Upsert(context.Background(), *binding); err != nil {
		t.Fatalf("seed binding: %v", err)
	}
	recreated := recreatedColumn(t, "In progress", "PVTSSO_fresh")
	bc := &fakeBoardClient{project: recreated, pages: [][]forge.ProjectItem{{
		item("PVTI_1", 613, statusOption("Planned", optionID(t, project, "Planned"), at)),
	}}}

	res, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, board,
		&ProjectImportOptions{Binding: binding, Bindings: bindings})
	if err != nil {
		t.Fatalf("ImportProjectBoard: %v", err)
	}
	if res.Reflected != 1 || len(bc.writes) != 1 {
		t.Fatalf("the reflect must still push: reflected=%d writes=%+v", res.Reflected, bc.writes)
	}
	if bc.writes[0].OptionID != "PVTSSO_fresh" {
		t.Errorf("wrote option %q, want the board's CURRENT id — the cached one is dead", bc.writes[0].OptionID)
	}
	stored, err := bindings.GetByTenant(context.Background(), "team-a")
	if err != nil {
		t.Fatalf("read binding: %v", err)
	}
	if stored.StatusOptions[native.StateInProgress] != "PVTSSO_fresh" {
		t.Errorf("stored option id = %q, want the re-resolved one", stored.StatusOptions[native.StateInProgress])
	}
	if got := mustGet(t, board, id).State; got != native.StateInProgress {
		t.Errorf("state = %q, want it untouched", got)
	}
}

// TestSyncProjectBoardDegradesOnADeletedColumn: neither the cached id nor the
// mapped name is on the field any more. Nothing can repair that from here, so
// the binding says so ONCE — an explicit state on the record, not a Warn per
// card per pass forever — and stops retrying the cards it cannot serve.
func TestSyncProjectBoardDegradesOnADeletedColumn(t *testing.T) {
	board := newTestBoard(t)
	at := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	seedSynced(t, board, 613, native.StateInProgress, "Planned", at)
	project := testProject()

	bindings := forge.NewMemoryBoardBindingStore()
	binding := boundBinding(t, project)
	if err := bindings.Upsert(context.Background(), *binding); err != nil {
		t.Fatalf("seed binding: %v", err)
	}
	var logs bytes.Buffer
	opts := &ProjectImportOptions{Binding: binding, Bindings: bindings,
		Logger: iterlog.New(iterlog.LevelWarn, &logs)}
	bc := &fakeBoardClient{project: deletedColumn(t, "In progress"), pages: [][]forge.ProjectItem{{
		item("PVTI_1", 613, statusOption("Planned", optionID(t, project, "Planned"), at)),
	}}}

	res, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, board, opts)
	if err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if res.ReflectNoColumn != 1 || len(bc.writes) != 0 || res.ReflectFailed != 0 {
		t.Fatalf("result = %+v, writes=%+v: the card must be counted, not written and not failed", res, bc.writes)
	}
	stored, err := bindings.GetByTenant(context.Background(), "team-a")
	if err != nil {
		t.Fatalf("read binding: %v", err)
	}
	if !stored.Degraded() || !strings.Contains(stored.DegradedReason, "In progress") {
		t.Fatalf("binding must be degraded and NAME the lost column, got %q", stored.DegradedReason)
	}
	if stored.StatusOptions[native.StateInProgress] != "" {
		t.Errorf("the dead option id must be dropped, got %q", stored.StatusOptions[native.StateInProgress])
	}

	// Two more passes: still no writes, and the reason is logged ONCE — an
	// operator reads the state off the binding, not off a repeating Warn.
	before := strings.Count(logs.String(), "no longer")
	for pass := 2; pass <= 3; pass++ {
		if _, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, board, opts); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
	}
	if len(bc.writes) != 0 {
		t.Errorf("writes = %+v, want none — a degraded column must not be retried", bc.writes)
	}
	if got := strings.Count(logs.String(), "no longer"); got != before {
		t.Errorf("the degradation was logged %d more times; it is a STATE, logged on the transition", got-before)
	}
}

// TestSyncProjectBoardClearsDegradedWhenTheColumnComesBack: the flag is a
// readout of the last resolution, so a re-created column clears it without a
// re-bind.
func TestSyncProjectBoardClearsDegradedWhenTheColumnComesBack(t *testing.T) {
	board := newTestBoard(t)
	at := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	seedSynced(t, board, 613, native.StateInProgress, "Planned", at)
	project := testProject()

	bindings := forge.NewMemoryBoardBindingStore()
	binding := boundBinding(t, project)
	if err := bindings.Upsert(context.Background(), *binding); err != nil {
		t.Fatalf("seed binding: %v", err)
	}
	opts := &ProjectImportOptions{Binding: binding, Bindings: bindings}
	bc := &fakeBoardClient{project: deletedColumn(t, "In progress"), pages: [][]forge.ProjectItem{{
		item("PVTI_1", 613, statusOption("Planned", optionID(t, project, "Planned"), at)),
	}}}
	if _, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, board, opts); err != nil {
		t.Fatalf("pass 1: %v", err)
	}

	bc.project = recreatedColumn(t, "In progress", "PVTSSO_back")
	if _, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, board, opts); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	stored, err := bindings.GetByTenant(context.Background(), "team-a")
	if err != nil {
		t.Fatalf("read binding: %v", err)
	}
	if stored.Degraded() {
		t.Errorf("the column is back; the binding must not stay degraded: %q", stored.DegradedReason)
	}
	if stored.StatusOptions[native.StateInProgress] != "PVTSSO_back" {
		t.Errorf("stored option id = %q, want the re-resolved one", stored.StatusOptions[native.StateInProgress])
	}
	if len(bc.writes) != 1 || bc.writes[0].OptionID != "PVTSSO_back" {
		t.Errorf("writes = %+v, want the push the degraded pass could not make", bc.writes)
	}
}

// TestSyncProjectBoardNeverRewritesTheOptionItAlreadyCarries pins the guard
// under the repair: the reflect must compare the OPTION ID it is about to
// write against the one the item carries, not only the names.
//
// The reachable case is a caller-supplied StatusMapping, which overrides the
// binding's own — so `want` comes from the override while the option id comes
// from the binding, and the names can differ while the value cannot. Names are
// a view of a column; the id IS the column.
func TestSyncProjectBoardNeverRewritesTheOptionItAlreadyCarries(t *testing.T) {
	board := newTestBoard(t)
	at := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	project := testProject()
	inProgressID := optionID(t, project, "In progress")
	seedSynced(t, board, 613, native.StateInProgress, "Planned", at)
	bc := &fakeBoardClient{project: project, pages: [][]forge.ProjectItem{{
		item("PVTI_1", 613, statusOption("Doing", inProgressID, at)),
	}}}

	res, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, board,
		&ProjectImportOptions{
			Binding: testBinding(),
			// The caller names the column differently from what the board
			// reports for that same option id.
			StatusMapping: forge.DefaultStatusMapping(),
		})
	if err != nil {
		t.Fatalf("ImportProjectBoard: %v", err)
	}
	if len(bc.writes) != 0 {
		t.Errorf("writes = %+v, want none — the item already carries option %s", bc.writes, inProgressID)
	}
	if res.Reflected != 0 {
		t.Errorf("Reflected = %d, want 0 — nothing was written", res.Reflected)
	}
}

// TestSyncProjectBoardWithoutABindingStoreStillConverges: the local one-shot
// `iterion issue import --project` has no binding store. The repair still
// applies in memory for that pass — it is simply not remembered.
func TestSyncProjectBoardWithoutABindingStoreStillConverges(t *testing.T) {
	board := newTestBoard(t)
	at := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	seedSynced(t, board, 613, native.StateInProgress, "In progress", at)
	project := testProject()
	binding := boundBinding(t, project)

	bc := &fakeBoardClient{project: renamedColumn(t, "In progress", "Doing"), pages: [][]forge.ProjectItem{{
		item("PVTI_1", 613, statusOption("Doing", optionID(t, project, "In progress"), at)),
	}}}
	res, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, board,
		&ProjectImportOptions{Binding: binding})
	if err != nil {
		t.Fatalf("ImportProjectBoard: %v", err)
	}
	if len(bc.writes) != 0 {
		t.Errorf("writes = %+v, want none — the board already carries this option", bc.writes)
	}
	if res.Moved != 0 {
		t.Errorf("Moved = %d, want 0 — the renamed column maps to the state the card is already in", res.Moved)
	}
}
