package liveledger

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/git"
)

// EnvTarget names the env var each Taskfile target sets to identify the
// row the harness must update. The env var is what carries the mapping
// from `task test:live:bot:review-pr` (the operator-facing name) to the
// ledger row for the run, without the test function having to know its
// own target name.
const EnvTarget = "ITERION_LIVE_LEDGER_TARGET"

// EnvOffline disables the ledger writer for tests that drive the
// recording hook in isolation (unit tests, offline probes). When set to
// any non-empty value the Track call is a no-op.
const EnvOffline = "ITERION_LIVE_LEDGER_OFFLINE"

// LedgerPathEnv lets a test point the writer at a scratch file. The
// production default resolves to the checkout's DefaultRelPath.
const LedgerPathEnv = "ITERION_LIVE_LEDGER_PATH"

// Metrics carries the numbers a Track call captures. The harness fills
// them in as it discovers them; a field left zero is written as its
// omitempty default.
type Metrics struct {
	// CostUSD is the aggregate spend of the run.
	CostUSD float64
	// StartedAt is the wall-clock time the test's real work started.
	// Track uses it to compute DurationSec at t.Cleanup time.
	StartedAt time.Time
	// ServedModel names what actually answered the run (see
	// Row.ServedModel). The harness fills it from the run's persisted
	// llm_request events, so it reports the model that served rather than
	// the one the fixture asked for.
	ServedModel string
	// FundedBy names the credential route that paid (see Row.FundedBy).
	FundedBy string
	// SkipReason names the prerequisite a skip lacked. Set by the harness
	// just before it skips, so the row can say WHY instead of leaving the
	// target indistinguishable from one nobody ever paid for.
	SkipReason string
}

// Tracker is the handle the harness holds while a live test runs. The
// test updates its fields as it collects data (cost, refined start
// time). t.Cleanup then writes the row from the final state.
type Tracker struct {
	mu       sync.Mutex
	target   string
	metrics  Metrics
	path     string
	disabled bool
}

// SetCost records the aggregate cost of the run. Safe to call multiple
// times; the last value wins.
func (tr *Tracker) SetCost(usd float64) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.metrics.CostUSD = usd
}

// SetStartedAt overrides the start time captured at Track time (the
// test may know a more meaningful start than "the moment Track was
// called").
func (tr *Tracker) SetStartedAt(t time.Time) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.metrics.StartedAt = t
}

// SetProvenance records what served the run and which credential route
// funded it. Safe to call multiple times; the last values win.
func (tr *Tracker) SetProvenance(servedModel, fundedBy string) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.metrics.ServedModel = servedModel
	tr.metrics.FundedBy = fundedBy
}

// SetSkipReason names the prerequisite the run lacked, for a test that
// skips AFTER Track registered its cleanup (a credential that turned out
// to be dead mid-run). The cleanup then writes a `skipped` row carrying
// the reason instead of leaving the target reading `never`.
func (tr *Tracker) SetSkipReason(reason string) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.metrics.SkipReason = reason
}

// TB is the part of *testing.T (or *testing.B) that Track needs. A
// narrow local interface, because testing.TB carries an unexported
// method and cannot be implemented — or faked — outside the testing
// package.
type TB interface {
	Cleanup(func())
	Failed() bool
	Skipped() bool
	Helper()
	Logf(format string, args ...any)
	Name() string
}

// Track registers a t.Cleanup that writes a ledger row for this test.
// It is designed to be safe to call from the top of any live test —
// missing env vars just make it a no-op with a t.Logf line so the
// operator can see the ledger was not written and why. The returned
// Tracker lets the harness feed cost + start-time updates during the
// run; if the returned pointer is unused, the row's cost is 0 and the
// duration is measured from the Track call.
//
// Verdict derivation at cleanup time:
//   - a SKIPPED test never ran, so it records neither pass nor fail: it
//     records a VerdictSkipped row naming the prerequisite it lacked
//     (SetSkipReason), which never overwrites a verdict a real run
//     recorded — see foldSkipOver;
//   - otherwise pass iff the test did not fail.
//
// When one target ran SEVERAL test functions in the same process (the
// aggregate targets like `test:live`), the rows merge fail-sticky: any
// member's failure fails the row, duration and cost accumulate, and the
// timestamp is the suite's last completion — never the last function's
// private verdict.
func Track(t TB) *Tracker {
	t.Helper()
	tr := newTracker(t)
	if tr.disabled {
		return tr
	}
	t.Cleanup(func() { tr.record(t) })
	return tr
}

// newTracker reads the env into a tracker and decides whether the writer
// is enabled. Shared by Track and RecordSkip so the offline/no-target
// escape hatches hold identically on both paths.
func newTracker(t TB) *Tracker {
	t.Helper()
	tr := &Tracker{
		target:  os.Getenv(EnvTarget),
		metrics: Metrics{StartedAt: time.Now().UTC()},
		path:    os.Getenv(LedgerPathEnv),
	}
	if strings.EqualFold(os.Getenv(EnvOffline), "true") || strings.EqualFold(os.Getenv(EnvOffline), "1") {
		tr.disabled = true
		return tr
	}
	if tr.target == "" {
		t.Logf("[liveledger] %s is unset — ledger row for %s will NOT be updated; set the env var in the Taskfile target that invokes this test", EnvTarget, t.Name())
		tr.disabled = true
	}
	return tr
}

// RecordSkip writes a `skipped` row for the current target, naming the
// prerequisite the run lacked, WITHOUT a Track registration — the shape a
// skip guard needs, because a guard refuses before the harness ever
// reaches Track. The row never overwrites a verdict a real run recorded
// (foldSkipOver); over a `never` row it is what stops `never` from meaning
// "skipped, and nobody wrote down why".
func RecordSkip(t TB, reason string) {
	t.Helper()
	tr := newTracker(t)
	if tr.disabled {
		return
	}
	tr.metrics.SkipReason = reason
	row := tr.buildSkipRow()
	path := tr.resolvedPath(t)
	row = namedSkipReason(mergeProcessRow(path, row))
	if err := RecordRow(path, row); err != nil {
		t.Logf("[liveledger] recording the skip of %s failed: %v", tr.target, err)
		return
	}
	t.Logf("%s", recordedLine(tr.target, row.Verdict, path))
}

// recordedLine says where a row went, after it was written. Only a row
// written to the COMMITTED ledger carries the regeneration note: that file
// has two generated twins the free gate compares cell-for-cell, so a write
// there leaves the tree dirty and `task check` red until they are
// re-derived — said where the cost is incurred, not discovered as an
// unexplained failure. A scratch ledger (LedgerPathEnv) moves nothing the
// gate compares, and a note there would send the operator to copy a file
// that did not change.
func recordedLine(target string, verdict Verdict, path string) string {
	if !isCommittedLedgerPath(path) {
		return fmt.Sprintf("[liveledger] %s row for %s recorded in %s (a scratch ledger: nothing committed moved)", verdict, target, path)
	}
	return fmt.Sprintf("[liveledger] %s row for %s recorded in the committed ledger %s: copy it over pkg/backend/backendcaps/live_seed.json and re-derive the golden (`go test ./pkg/backend/backendcaps/ -args -update-backendcaps`) before `task check` — see docs/live-e2e-coverage.md", verdict, target, path)
}

// isCommittedLedgerPath reports whether path is the checkout's own ledger —
// DefaultRelPath under the module root — as opposed to a scratch file.
func isCommittedLedgerPath(path string) bool {
	root, ok := findModuleRoot()
	return ok && filepath.Clean(path) == filepath.Join(root, DefaultRelPath)
}

// record is the cleanup closure: it decides the row's verdict and
// writes it. Extracted from Track so tests can drive it through the
// real Cleanup registration.
func (tr *Tracker) record(t TB) {
	if tr.disabled {
		return
	}
	var row Row
	if t.Skipped() {
		t.Logf("[liveledger] %s skipped — recording a `skipped` row (a skip never ran, so neither pass nor fail applies)", tr.target)
		row = tr.buildSkipRow()
	} else {
		row = tr.buildRow(t.Failed())
	}
	path := tr.resolvedPath(t)
	row = namedSkipReason(mergeProcessRow(path, row))
	if err := RecordRow(path, row); err != nil {
		t.Logf("[liveledger] recording row for %s failed: %v", tr.target, err)
		return
	}
	t.Logf("%s", recordedLine(tr.target, row.Verdict, path))
}

// buildRow captures the final row shape from the tracker.
func (tr *Tracker) buildRow(failed bool) Row {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	verdict := VerdictPass
	if failed {
		verdict = VerdictFail
	}
	end := time.Now().UTC()
	dur := int64(0)
	if !tr.metrics.StartedAt.IsZero() {
		if d := end.Sub(tr.metrics.StartedAt); d > 0 {
			dur = int64(d.Round(time.Second).Seconds())
		}
	}
	return Row{
		Target:      tr.target,
		LastRun:     end,
		Verdict:     verdict,
		DurationSec: dur,
		CostUSD:     tr.metrics.CostUSD,
		Ref:         currentRef(),
		ServedModel: tr.metrics.ServedModel,
		FundedBy:    tr.metrics.FundedBy,
	}
}

// buildSkipRow captures the shape of an attempt that could not run: no
// LastRun (nothing ran), no duration, no cost, no ref — a date and the
// prerequisite that was missing, left EMPTY when this tracker was never
// told one. The fallback is stamped after the merge (namedSkipReason), not
// here: a guard's skipLive writes the real reason through RecordSkip and
// the same test's Track cleanup then writes a second skip row, so a
// fallback baked in at build time would be unioned next to the real
// prerequisite and read as a second, contradictory cause.
func (tr *Tracker) buildSkipRow() Row {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return Row{
		Target:     tr.target,
		Verdict:    VerdictSkipped,
		SkippedAt:  time.Now().UTC(),
		SkipReason: tr.metrics.SkipReason,
	}
}

// UnnamedSkipReason is what a `skipped` row says when nothing named the
// prerequisite. Recorded anyway: "skipped" with no reason still beats
// `never` with no trace.
const UnnamedSkipReason = "the harness skipped without naming a prerequisite"

// namedSkipReason stamps the fallback on a skip row that reached the write
// path with no reason at all — after the process-level merge, so a row that
// carries one member's real prerequisite never also carries the fallback.
func namedSkipReason(row Row) Row {
	if row.Verdict == VerdictSkipped && row.SkipReason == "" {
		row.SkipReason = UnnamedSkipReason
	}
	return row
}

// resolvedPath returns the ledger file path, resolving the checkout
// root when LedgerPathEnv is unset. The lookup walks up from the test's
// working directory to find the go.mod boundary, then joins
// DefaultRelPath — so a test in a subdirectory of the checkout still
// writes to the same file.
func (tr *Tracker) resolvedPath(t TB) string {
	if tr.path != "" {
		return tr.path
	}
	if root, ok := findModuleRoot(); ok {
		return filepath.Join(root, DefaultRelPath)
	}
	t.Logf("[liveledger] could not locate module root — ledger row will be written to %s in the current directory", DefaultRelPath)
	return DefaultRelPath
}

// findModuleRoot walks up from the current working directory until it
// finds a go.mod, and returns that directory.
func findModuleRoot() (string, bool) {
	dir, err := os.Getwd()
	if err != nil {
		return "", false
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// currentRef returns the git HEAD sha (short) if reachable, else "".
// The ledger writer does not fail on a missing git — the row is still
// useful without a ref.
func currentRef() string {
	cmd := exec.Command("git", git.NoRunHooks("rev-parse", "--short=12", "HEAD")...)
	cmd.Env = git.SanitizeEnv(os.Environ())
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// processWritten remembers every row this process has already written,
// keyed by ledger path + target. Aggregate targets run many test
// functions under ONE row key inside a single `go test` invocation, and
// `go test` runs them sequentially in one process — without the merge,
// the last function's cleanup would overwrite the suite's verdict with
// its own private one (test 1 fails, test 40 passes → the row reports
// `pass` for a suite that failed).
var (
	processWrittenMu sync.Mutex
	processWritten   = map[string]Row{}
)

// mergeProcessRow folds row into the row this process already wrote for
// the same (path, target), if any, and returns what to persist. The
// merge is commutative where it matters: fail wins regardless of which
// function ran first, and duration/cost sum, so the row does not depend
// on test ordering.
func mergeProcessRow(path string, row Row) Row {
	key := path + "\x00" + row.Target
	processWrittenMu.Lock()
	defer processWrittenMu.Unlock()
	prev, ok := processWritten[key]
	if !ok {
		processWritten[key] = row
		return row
	}
	merged := mergeRows(prev, row)
	processWritten[key] = merged
	return merged
}

// mergeRows folds two rows of the same target written by the same
// process. The fold is commutative in every field — fail dominates (then
// pass, then skipped: verdictStrength), duration and cost accumulate,
// LastRun/SkippedAt are the latest, and the provenance and skip reasons
// fold to sorted sets. Nothing here reads the ORDER the members finished
// in, because `go test` gives no guarantee about it and a row that
// depended on it would be a different receipt per run.
func mergeRows(a, b Row) Row {
	last := b.LastRun
	if a.LastRun.After(last) {
		last = a.LastRun
	}
	skipped := b.SkippedAt
	if a.SkippedAt.After(skipped) {
		skipped = a.SkippedAt
	}
	ref := b.Ref
	if ref == "" {
		ref = a.Ref
	}
	return Row{
		Target:      a.Target,
		LastRun:     last,
		Verdict:     strongerVerdict(a.Verdict, b.Verdict),
		DurationSec: a.DurationSec + b.DurationSec,
		CostUSD:     a.CostUSD + b.CostUSD,
		Ref:         ref,
		ServedModel: foldDistinct(a.ServedModel, b.ServedModel),
		FundedBy:    foldDistinct(a.FundedBy, b.FundedBy),
		SkippedAt:   skipped,
		SkipReason:  foldReasons(a.SkipReason, b.SkipReason),
	}
}

// ListSep separates the items of a list-shaped column (served_model,
// funded_by). A comma is safe there and nowhere else: a model spec and a
// credential env var name never contain one, while a skip reason is prose
// that does — splitting reasons on a comma would cut a sentence in half on
// the next merge.
const ListSep = ","

// ReasonSep separates the skip reasons of an aggregate target's members.
// Reserved in a reason: a guard writes these strings, and the fold splits
// on exactly this token to re-union them.
const ReasonSep = " | "

// foldDistinct unions list-shaped columns into one sorted list.
func foldDistinct(lists ...string) string { return foldOn(ListSep, lists) }

// foldReasons unions prose reasons into one sorted list on ReasonSep.
func foldReasons(reasons ...string) string { return foldOn(ReasonSep, reasons) }

// foldOn splits each input on sep, unions the items and re-joins them
// SORTED. The sort is the stable key: the members of an aggregate target
// finish in whatever order the runner chose, and a folded column that
// changed with it would be a different receipt per run.
func foldOn(sep string, lists []string) string {
	seen := map[string]bool{}
	for _, list := range lists {
		for _, item := range strings.Split(list, sep) {
			if item = strings.TrimSpace(item); item != "" {
				seen[item] = true
			}
		}
	}
	if len(seen) == 0 {
		return ""
	}
	out := make([]string, 0, len(seen))
	for item := range seen {
		out = append(out, item)
	}
	sort.Strings(out)
	return strings.Join(out, sep)
}

// FoldServedModels renders the distinct model specs that served a run as
// the ledger's served_model column: sorted, comma-separated. Exported
// because the harness collects the specs from the run's events and the
// fold must be the one the merge above uses.
func FoldServedModels(specs []string) string { return foldOn(ListSep, specs) }

// resetProcessWritten clears the process-level merge registry. Tests
// use it to isolate cases; production never needs it — the registry
// lives exactly as long as the process.
func resetProcessWritten() {
	processWrittenMu.Lock()
	defer processWrittenMu.Unlock()
	processWritten = map[string]Row{}
}

// RecordRow is the write-side primitive Track uses at t.Cleanup, and
// the direct entry point unit tests use to drive the ledger without a
// full live run. Loads the ledger, upserts the row, writes back
// atomically.
func RecordRow(path string, row Row) error {
	if row.Target == "" {
		return fmt.Errorf("liveledger: row.Target is required")
	}
	lg, err := Load(path)
	if err != nil {
		return err
	}
	if row.Verdict == VerdictSkipped {
		prev, _ := lg.Get(row.Target)
		row = foldSkipOver(prev, row)
	}
	lg.Upsert(row)
	return lg.Write(path)
}
