package liveledger

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A round-trip write + read reproduces the same rows in target order
// regardless of the order they were Upserted. The invariant matters
// because the ledger is committed and diffs must be minimal — a
// reorder that only shuffles rows in the file would poison every
// review.
func TestLedger_UpsertSortsAndRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	l := &Ledger{Schema: SchemaVersion}
	// Upsert in reverse order on purpose.
	l.Upsert(Row{Target: "test:live:z", Verdict: VerdictNever})
	l.Upsert(Row{Target: "test:live:a", Verdict: VerdictPass, LastRun: time.Unix(1_700_000_000, 0).UTC(), DurationSec: 42, CostUSD: 1.23, Ref: "deadbeef"})
	l.Upsert(Row{Target: "test:live:m", Verdict: VerdictFail, LastRun: time.Unix(1_700_000_100, 0).UTC()})
	if err := l.Write(path); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if want := []string{"test:live:a", "test:live:m", "test:live:z"}; len(got.Rows) != 3 ||
		got.Rows[0].Target != want[0] || got.Rows[1].Target != want[1] || got.Rows[2].Target != want[2] {
		t.Fatalf("rows not sorted after round-trip: got=%+v want target order %v", targets(got.Rows), want)
	}
	// The pass row must carry its metrics faithfully.
	if got.Rows[0].DurationSec != 42 || got.Rows[0].CostUSD != 1.23 || got.Rows[0].Ref != "deadbeef" {
		t.Fatalf("row a lost its fields on round-trip: %+v", got.Rows[0])
	}
}

// A missing file is not an error — it is the shape of a fresh
// checkout, and Load must return an empty ledger the caller can then
// EnsureNeverRows into. The alternative (error) would force callers to
// distinguish "file missing" from "file unreadable", which is exactly
// where absence-vs-error bugs live.
func TestLedger_LoadMissingFileIsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist.json")
	l, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error on missing file: %v", err)
	}
	if len(l.Rows) != 0 {
		t.Fatalf("Load produced rows for missing file: %+v", l.Rows)
	}
}

// A ledger that carries a HIGHER schema than this binary understands
// must be REFUSED. A silent drop would let a future field disappear on
// a rewrite and stay disappeared, exactly the class of bug the schema
// field exists to catch.
func TestLedger_LoadRefusesHigherSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.json")
	if err := os.WriteFile(path, []byte(`{"schema":99,"rows":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatalf("Load accepted a higher schema (99) — the field would then be dropped on rewrite")
	}
}

// A `never` seed cannot cover up a recorded run. This is the guarantee
// the ledger's "written by the harness, not by the author" rule stands
// on: an operator running EnsureNeverRows AFTER a real run must not
// erase what was written.
func TestLedger_EnsureNeverRowsNeverOverwrites(t *testing.T) {
	l := &Ledger{Schema: SchemaVersion}
	real := Row{Target: "test:live:bot:review-pr", Verdict: VerdictPass, LastRun: time.Now().UTC(), CostUSD: 2.0}
	l.Upsert(real)
	added := l.EnsureNeverRows([]string{"test:live:bot:review-pr", "test:live:bot:bmady"})
	if added != 1 {
		t.Fatalf("EnsureNeverRows added %d, want 1 (only the missing row)", added)
	}
	got, _ := l.Get("test:live:bot:review-pr")
	if got.Verdict != VerdictPass || got.CostUSD != 2.0 {
		t.Fatalf("real row was overwritten by never seed: %+v", got)
	}
	got, _ = l.Get("test:live:bot:bmady")
	if got.Verdict != VerdictNever {
		t.Fatalf("bmady row not seeded as never: %+v", got)
	}
}

// SortByStaleness puts `never` first, then oldest LastRun, then Target
// alphabetically. The operator answering "which target should I re-run
// today?" reads the top of the list.
func TestLedger_SortByStalenessPutsNeverFirst(t *testing.T) {
	old := time.Unix(1_000_000_000, 0).UTC()
	recent := time.Unix(1_700_000_000, 0).UTC()
	l := &Ledger{Schema: SchemaVersion, Rows: []Row{
		{Target: "test:live:m", Verdict: VerdictPass, LastRun: recent},
		{Target: "test:live:z", Verdict: VerdictNever},
		{Target: "test:live:a", Verdict: VerdictFail, LastRun: old},
	}}
	got := l.SortByStaleness()
	if got[0].Target != "test:live:z" {
		t.Fatalf("never row not first: %v", targets(got))
	}
	if got[1].Target != "test:live:a" {
		t.Fatalf("older-real row not second: %v", targets(got))
	}
	if got[2].Target != "test:live:m" {
		t.Fatalf("recent-real row not third: %v", targets(got))
	}
}

// RecordRow is the entry point Track uses at t.Cleanup and the entry
// point unit tests use to drive the ledger without a live run. A
// direct call must round-trip through Load/Upsert/Write.
func TestRecordRow_DrivesLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	row := Row{
		Target:      "test:live:probe",
		LastRun:     time.Unix(1_700_000_500, 0).UTC(),
		Verdict:     VerdictPass,
		DurationSec: 12,
		CostUSD:     0.05,
		Ref:         "abc123",
	}
	if err := RecordRow(path, row); err != nil {
		t.Fatalf("RecordRow: %v", err)
	}
	l, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := l.Get("test:live:probe")
	if !ok {
		t.Fatal("RecordRow did not write the row")
	}
	if got.Verdict != VerdictPass || got.DurationSec != 12 || got.CostUSD != 0.05 || got.Ref != "abc123" {
		t.Fatalf("row round-trip differs: %+v", got)
	}
}

// RecordRow with an empty Target must FAIL — a row without a key is
// the "written but silent" shape the ledger's rules exist to prevent.
func TestRecordRow_RefusesEmptyTarget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	if err := RecordRow(path, Row{Target: "", Verdict: VerdictPass}); err == nil {
		t.Fatal("RecordRow accepted an empty target")
	}
}

// A recorded row overwritten by a later run of the same target retains
// only the LATEST — the ledger is "last known" per target, not an
// audit trail (that role belongs to the quality snapshot store).
func TestRecordRow_OverwritesPrior(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	first := Row{Target: "test:live:probe", Verdict: VerdictFail, LastRun: time.Unix(100, 0).UTC(), CostUSD: 0.5}
	later := Row{Target: "test:live:probe", Verdict: VerdictPass, LastRun: time.Unix(200, 0).UTC(), CostUSD: 0.7}
	if err := RecordRow(path, first); err != nil {
		t.Fatal(err)
	}
	if err := RecordRow(path, later); err != nil {
		t.Fatal(err)
	}
	l, _ := Load(path)
	got, _ := l.Get("test:live:probe")
	if got.Verdict != VerdictPass || got.CostUSD != 0.7 {
		t.Fatalf("later row did not overwrite: %+v", got)
	}
}

func targets(rows []Row) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Target
	}
	return out
}
