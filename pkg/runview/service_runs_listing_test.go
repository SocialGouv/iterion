package runview

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// A listing does not carry the text of the unit its runs executed.
//
// The listing loads every matching run whole and its own Limit truncates only
// afterwards, so the heaviest field on the document decides what a request
// costs — and one caller (the board projection) asks with no limit at all.
// The recorded workflow source is that field: hundreds of kilobytes for the
// real bots in this catalog, and no consumer of a listing reads it.
func TestListRunRecordsDoesNotCarryTheRecordedSource(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := context.Background()
	// Big enough that a listing holding it would be unmistakable.
	main := "workflow w:\n  entry: a\n  a -> done\n" + strings.Repeat("# padding\n", 20_000)
	fragment := "agent a:\n  model: \"claude-opus-5\"\n"
	for _, id := range []string{"run-a", "run-b", "run-c"} {
		if _, err := st.CreateRun(ctx, id, "w", nil); err != nil {
			t.Fatal(err)
		}
		r, err := st.LoadRun(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		r.WorkflowSource = main
		r.WorkflowSources = []store.WorkflowSourceFile{
			{Path: "main.bot", Text: main},
			{Path: "lib/nodes.bot", Text: fragment},
		}
		if err := st.SaveRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	svc, err := NewService(dir)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	records, err := svc.ListRunRecordsCtx(ctx, ListFilter{})
	if err != nil {
		t.Fatalf("ListRunRecordsCtx: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("listed %d runs, want 3", len(records))
	}
	var carried int
	for _, r := range records {
		carried += len(r.WorkflowSource)
		for _, f := range r.WorkflowSources {
			carried += len(f.Text)
		}
		// The empty source must keep ONE meaning: a reader has to be able to
		// tell "this copy was never asked for it" from "this run recorded
		// none", or it will read a projection as a run that cannot be rewound.
		if !r.SourceOmitted {
			t.Errorf("run %s came back without SourceOmitted — its empty source is ambiguous", r.ID)
		}
	}
	if carried != 0 {
		t.Errorf("the listing carried %d bytes of recorded source across 3 runs; a listing reads none of it", carried)
	}

	// And the whole document still has it: the projection is the listing's,
	// not the store's — rewind --auto loads runs one by one and must still
	// find what they executed.
	whole, err := st.LoadRun(ctx, "run-a")
	if err != nil {
		t.Fatal(err)
	}
	if whole.WorkflowSource != main || len(whole.WorkflowSources) != 2 {
		t.Error("LoadRun lost the recorded source — the projection escaped the listing")
	}
	if whole.SourceOmitted {
		t.Error("a whole load reported SourceOmitted")
	}
}

// A record that came from a LISTING cannot be written back.
//
// Every SaveRun here replaces the whole document, and a projected record's
// source was never read — so saving one drops the text of the unit the run
// executed, permanently, with no error and no trace: the run simply stops
// being auto-rewindable. Nothing in the repo does it today; the trap is one
// substitution away, in loops of exactly this shape (list ids → load each →
// mutate → SaveRun), and the projection's own doc comment invites it.
func TestSavingARecordFromAListingIsRefused(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := context.Background()
	const main = "workflow w:\n  entry: a\n  a -> done\n"
	if _, err := st.CreateRun(ctx, "run-1", "w", nil); err != nil {
		t.Fatal(err)
	}
	seeded, err := st.LoadRun(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	seeded.WorkflowSource = main
	if err := st.SaveRun(ctx, seeded); err != nil {
		t.Fatal(err)
	}

	svc, err := NewService(dir)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	records, err := svc.ListRunRecordsCtx(ctx, ListFilter{})
	if err != nil || len(records) != 1 {
		t.Fatalf("ListRunRecordsCtx: %v (%d records)", err, len(records))
	}
	// A plausible caller: it changes one field and saves, the way a
	// reconciliation loop does.
	listed := records[0]
	listed.Name = "renamed by a caller that listed"
	if err := st.SaveRun(ctx, listed); !errors.Is(err, store.ErrRunProjected) {
		t.Errorf("SaveRun of a listed record returned %v; want ErrRunProjected", err)
	}
	back, err := st.LoadRun(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if back.WorkflowSource != main {
		t.Errorf("a write from a listing destroyed the recorded source: %d bytes left of %d", len(back.WorkflowSource), len(main))
	}
	if back.SourceOmitted {
		t.Error("SourceOmitted survived a round trip — a listing's shortcut became a property of the run")
	}
}
