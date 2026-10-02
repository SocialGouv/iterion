// Package liveledger keeps a committed, single-file record of the LAST
// run of every `task test:live:*` target. It answers "do the live e2e
// still pass?" without spending a cent — see docs/live-e2e-coverage.md
// and #1422.
//
// The ledger is one JSON file, sorted by target, one row per target.
// Written by the harness ITSELF on every live run (pass or fail, panel
// or no panel), so an absent row means the target never ran; a `never`
// verdict is EXPLICIT, never elided. The judge panel keeps its own
// snapshot store (pkg/benchmark/quality), which the ledger does not
// replace — a ledger row is written whether or not
// ITERION_LIVE_QUALITY runs.
package liveledger

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// SchemaVersion is the on-disk format version. Increment on breaking
// changes; readers must refuse a higher version rather than silently
// dropping fields.
const SchemaVersion = 1

// DefaultRelPath is the ledger's canonical location relative to the
// repository root. Kept in code so tests and the CLI resolve it the
// same way; a caller with an offline path passes it explicitly.
const DefaultRelPath = "e2e/testdata/live/ledger.json"

// Verdict is the last-run outcome for a target.
type Verdict string

const (
	// VerdictPass — the last run of this target completed and its test
	// asserted no failure (reliability invariants held; panel gates are
	// out of scope of the ledger).
	VerdictPass Verdict = "pass"
	// VerdictFail — the last run completed but failed at least one
	// assertion, OR the harness recorded a shape it could not classify
	// as pass.
	VerdictFail Verdict = "fail"
	// VerdictNever — this target has never been recorded. A blank
	// verdict reads as "fine" to every future reader, which is the one
	// thing it never means; `never` is EXPLICIT.
	VerdictNever Verdict = "never"
)

// Row is one target's last recorded run.
type Row struct {
	// Target is the Taskfile target name, e.g. "test:live:bot:review-pr".
	// This is the ledger's unique key; each target has AT MOST ONE row.
	Target string `json:"target"`
	// LastRun is the wall-clock time the run completed (UTC). Zero for
	// VerdictNever; always non-zero otherwise.
	LastRun time.Time `json:"last_run,omitempty"`
	// Verdict is the pass/fail/never enum.
	Verdict Verdict `json:"verdict"`
	// DurationSec is the run's wall time in whole seconds. Zero for
	// VerdictNever.
	DurationSec int64 `json:"duration_sec,omitempty"`
	// CostUSD is the aggregate spend of the run, if measurable. Zero
	// for VerdictNever and for runs where the harness could not compute
	// it (e.g. a backend that reports no per-call cost).
	CostUSD float64 `json:"cost_usd,omitempty"`
	// Ref is the git ref/commit the harness recorded at write time
	// (typically the HEAD SHA), so a stale row can be traced back to
	// the code it exercised.
	Ref string `json:"ref,omitempty"`
}

// Ledger is the on-disk shape.
type Ledger struct {
	// Schema is SchemaVersion at write time.
	Schema int `json:"schema"`
	// Rows are sorted by Target on every Write.
	Rows []Row `json:"rows"`
}

// Load reads the ledger from path. A missing file yields an empty
// ledger, not an error — that is the "fresh checkout, nothing has ever
// run" shape.
func Load(path string) (*Ledger, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Ledger{Schema: SchemaVersion}, nil
		}
		return nil, fmt.Errorf("liveledger: read %s: %w", path, err)
	}
	var lg Ledger
	if err := json.Unmarshal(b, &lg); err != nil {
		return nil, fmt.Errorf("liveledger: parse %s: %w", path, err)
	}
	if lg.Schema > SchemaVersion {
		return nil, fmt.Errorf("liveledger: %s carries schema %d, this binary understands %d — refuse rather than silently dropping fields", path, lg.Schema, SchemaVersion)
	}
	return &lg, nil
}

// Get returns the row for target, or (Row{}, false) when absent.
func (l *Ledger) Get(target string) (Row, bool) {
	for _, r := range l.Rows {
		if r.Target == target {
			return r, true
		}
	}
	return Row{}, false
}

// Upsert inserts or replaces the row for r.Target and keeps Rows sorted
// by target. The caller sets r.LastRun to time.Now().UTC() for a real
// run; for a `never` seed row, r.Verdict is VerdictNever and LastRun is
// left zero.
func (l *Ledger) Upsert(r Row) {
	for i := range l.Rows {
		if l.Rows[i].Target == r.Target {
			l.Rows[i] = r
			return
		}
	}
	l.Rows = append(l.Rows, r)
	sort.Slice(l.Rows, func(i, j int) bool { return l.Rows[i].Target < l.Rows[j].Target })
}

// EnsureNeverRows adds a VerdictNever row for every target in targets
// that is not already present in the ledger. A row already carrying a
// real verdict is NOT overwritten — a `never` seed cannot cover up a
// recorded run. Returns the number of rows added.
func (l *Ledger) EnsureNeverRows(targets []string) int {
	added := 0
	for _, t := range targets {
		if _, ok := l.Get(t); ok {
			continue
		}
		l.Upsert(Row{Target: t, Verdict: VerdictNever})
		added++
	}
	return added
}

// Write serialises the ledger to path atomically (write-tmp + rename)
// so a concurrent reader never sees a half-written file. Rows are
// sorted by Target; Schema is set to SchemaVersion.
func (l *Ledger) Write(path string) error {
	l.Schema = SchemaVersion
	sort.Slice(l.Rows, func(i, j int) bool { return l.Rows[i].Target < l.Rows[j].Target })
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return fmt.Errorf("liveledger: marshal: %w", err)
	}
	b = append(b, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("liveledger: mkdir %s: %w", filepath.Dir(path), err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("liveledger: tempfile: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("liveledger: write tmp %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("liveledger: close tmp %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("liveledger: rename %s -> %s: %w", tmpPath, path, err)
	}
	return nil
}

// SortByStaleness returns a copy of the rows ordered oldest-first (a
// `never` row is oldest of all — it has never run) so
// `task test:live:status` prints what the operator most needs to see
// at the top. Ties break by Target ascending for determinism.
func (l *Ledger) SortByStaleness() []Row {
	out := append([]Row(nil), l.Rows...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Verdict == VerdictNever && out[j].Verdict != VerdictNever {
			return true
		}
		if out[i].Verdict != VerdictNever && out[j].Verdict == VerdictNever {
			return false
		}
		if !out[i].LastRun.Equal(out[j].LastRun) {
			return out[i].LastRun.Before(out[j].LastRun)
		}
		return out[i].Target < out[j].Target
	})
	return out
}
