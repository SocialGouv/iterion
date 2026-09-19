package liveledger

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
//   - a SKIPPED test never ran, so it records nothing — a `pass` row
//     would mean "the harness verified this target", which a skip never
//     did;
//   - otherwise pass iff the test did not fail.
//
// When one target ran SEVERAL test functions in the same process (the
// aggregate targets like `test:live`), the rows merge fail-sticky: any
// member's failure fails the row, duration and cost accumulate, and the
// timestamp is the suite's last completion — never the last function's
// private verdict.
func Track(t TB) *Tracker {
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
		return tr
	}
	t.Cleanup(func() { tr.record(t) })
	return tr
}

// record is the cleanup closure: it decides the row's verdict and
// writes it. Extracted from Track so tests can drive it through the
// real Cleanup registration.
func (tr *Tracker) record(t TB) {
	if tr.disabled {
		return
	}
	if t.Skipped() {
		t.Logf("[liveledger] %s skipped — no row written (a skip never ran, so neither pass nor fail applies)", tr.target)
		return
	}
	row := tr.buildRow(t.Failed())
	path := tr.resolvedPath(t)
	row = mergeProcessRow(path, row)
	if err := RecordRow(path, row); err != nil {
		t.Logf("[liveledger] recording row for %s failed: %v", tr.target, err)
	}
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
	}
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
	cmd := exec.Command("git", "rev-parse", "--short=12", "HEAD")
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
// process. fail dominates; duration and cost accumulate; LastRun is the
// suite's last completion.
func mergeRows(a, b Row) Row {
	verdict := VerdictPass
	if a.Verdict == VerdictFail || b.Verdict == VerdictFail {
		verdict = VerdictFail
	}
	last := b.LastRun
	if a.LastRun.After(last) {
		last = a.LastRun
	}
	ref := b.Ref
	if ref == "" {
		ref = a.Ref
	}
	return Row{
		Target:      a.Target,
		LastRun:     last,
		Verdict:     verdict,
		DurationSec: a.DurationSec + b.DurationSec,
		CostUSD:     a.CostUSD + b.CostUSD,
		Ref:         ref,
	}
}

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
	lg.Upsert(row)
	return lg.Write(path)
}
