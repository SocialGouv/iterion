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
	"strings"
	"time"
)

// SchemaVersion is the on-disk format version. Increment on breaking
// changes; readers must refuse a higher version rather than silently
// dropping fields.
//
// 2 — a row names what served it (ServedModel) and which credential route
// funded it (FundedBy), and a target that could not run records a
// VerdictSkipped row with the prerequisite it lacked. A reader that drops
// those fields reads a run served by an overridden model as if the
// fixture's pinned model had answered: a false receipt, which is exactly
// what refusing a newer schema protects against.
const SchemaVersion = 2

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
	// VerdictSkipped — the last attempt could not run: a prerequisite the
	// target needs was absent (a credential, a CLI, a docker image) and
	// Row.SkipReason names it. Distinct from VerdictNever on purpose:
	// `never` means nobody has paid for this target yet, while `skipped`
	// means nobody could run it HERE — two different asks of the operator,
	// which one word could not carry.
	VerdictSkipped Verdict = "skipped"
)

// verdictStrength ranks the verdicts for the merges: a real failure
// dominates everything, a real pass dominates a skip (something ran and
// held), a skip dominates `never` (an attempt was made and named its
// missing prerequisite). The merges read THIS table, so the two of them
// cannot drift apart.
var verdictStrength = map[Verdict]int{
	VerdictNever:   0,
	VerdictSkipped: 1,
	VerdictPass:    2,
	VerdictFail:    3,
}

// strongerVerdict folds two verdicts commutatively — the result does not
// depend on which member of an aggregate target finished first.
func strongerVerdict(a, b Verdict) Verdict {
	if verdictStrength[b] > verdictStrength[a] {
		return b
	}
	return a
}

// ServedModelUnobserved is the served_model of a run in which an LLM node
// ran and the backend named no model: `llm_request` is emitted by the claw
// path only, so a claude_code, codex or pi delegation reports nothing. It
// keeps that run apart from "" — a run with no LLM node, where nothing
// could have served.
const ServedModelUnobserved = "unobserved"

// ServedModelUnobservedPrefix opens the same sentinel when the harness
// ASKED for a spec: "unobserved:<spec>" keeps the two facts a committed
// receipt needs apart — the row did NOT see the pinned model answer, and it
// did not see the override answer either.
const ServedModelUnobservedPrefix = ServedModelUnobserved + ":"

// UnobservedServedModel renders the served_model of a run whose backend
// named nothing: the bare sentinel, or the sentinel carrying the spec that
// was asked for.
func UnobservedServedModel(asked string) string {
	asked = strings.TrimSpace(asked)
	if asked == "" {
		return ServedModelUnobserved
	}
	return ServedModelUnobservedPrefix + asked
}

// ServedModelFor decides the served_model column from what the run's
// events let the harness observe: the specs its llm_request events named,
// how many LLM nodes (agent, judge) started, and the spec an override
// asked for. What was observed wins. Otherwise the column says WHY it is
// not a spec: "" when no LLM node ran (nothing could have served — a
// tool-only run), the unobserved sentinel when one did and the backend
// named nothing. A re-targeted run therefore never reads as a pass on the
// fixture's pinned model, and a tool-only run never reads as an LLM run
// the harness failed to observe.
func ServedModelFor(served []string, llmNodes int, asked string) string {
	if spec := FoldServedModels(served); spec != "" {
		return spec
	}
	if llmNodes <= 0 {
		return ""
	}
	return UnobservedServedModel(asked)
}

// IsUnobservedServedModel reports whether item is the unobserved sentinel,
// bare or carrying the asked-for spec (returned; "" when bare).
func IsUnobservedServedModel(item string) (asked string, ok bool) {
	if item == ServedModelUnobserved {
		return "", true
	}
	if strings.HasPrefix(item, ServedModelUnobservedPrefix) {
		return strings.TrimPrefix(item, ServedModelUnobservedPrefix), true
	}
	return "", false
}

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
	// ServedModel names the model spec(s) that ACTUALLY answered the
	// run, read from the persisted llm_request events — not the spec the
	// harness asked for. Several distinct specs fold to a sorted,
	// comma-separated list. Without it, a `pass` obtained on an
	// overridden route is indistinguishable from a `pass` on the
	// fixture's pinned model, which makes the row a false receipt.
	//
	// Honest scope: `llm_request` is emitted by the claw path only — the
	// in-process loop and the relay of a claw running inside a sandbox. A
	// claude_code, codex or pi delegation names no model, so the harness
	// has nothing to read. The column is therefore a POSITIVE receipt and
	// never a negative one, and it says which case a reader is in
	// (ServedModelFor): empty means NO LLM NODE RAN (a tool-only run, or a
	// row older than schema 2); ServedModelUnobserved means an agent or
	// judge node ran and the backend named no model; the same under a
	// re-target that asked for a spec is ServedModelUnobservedPrefix + that
	// spec — so an overridden pass can never read as a pass on the pinned
	// model. That reading holds because every recording path feeds the
	// column: the harness's runBotLive before it takes a verdict, and each
	// test that tracks its own row (TestEveryDirectTrackerFeedsProvenance).
	ServedModel string `json:"served_model,omitempty"`
	// FundedBy names the credential ROUTE that paid — a route name or a
	// credential-source env var NAME, never a key value. Empty when the
	// harness cannot attribute the spend without guessing; the operator
	// names it through ITERION_LIVE_FUNDED_BY.
	FundedBy string `json:"funded_by,omitempty"`
	// SkippedAt is when the last attempt was skipped (UTC), zero when the
	// target has never been skipped. It is NOT LastRun: a skip never ran,
	// so it must not move the date a verdict was taken on.
	//
	// omitzero, not omitempty: a zero time.Time is a struct, which
	// omitempty never drops, so omitempty would write
	// "0001-01-01T00:00:00Z" into all 64 rows of a committed file whose
	// value is being diffable.
	SkippedAt time.Time `json:"skipped_at,omitzero"`
	// SkipReason names the prerequisite the last skipped attempt lacked.
	// It survives next to a recorded pass/fail — "it failed in October and
	// today it could not even start, here is why" is two facts, and the
	// row keeps both. An aggregate target's members fold their reasons on
	// ReasonSep, which is therefore reserved inside one reason.
	SkipReason string `json:"skip_reason,omitempty"`
}

// foldSkipOver folds a skip observation over whatever the committed ledger
// already holds for the target. A skip did not run, so it never erases
// what a run proved: a recorded pass/fail keeps its verdict, date,
// duration, cost, ref and provenance, and the skip lands on
// SkippedAt/SkipReason alone. Over a `never` or an earlier `skipped` row
// the verdict BECOMES skipped — that is the point: `never` (nobody has
// paid) stops meaning `skipped` (nobody could run it here).
func foldSkipOver(prev, skip Row) Row {
	if prev.Verdict != VerdictPass && prev.Verdict != VerdictFail {
		return skip
	}
	prev.SkippedAt = skip.SkippedAt
	prev.SkipReason = skip.SkipReason
	return prev
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
