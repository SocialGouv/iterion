package liveledger

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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

// Track registers a t.Cleanup that writes a ledger row for this test.
// It is designed to be safe to call from the top of any live test —
// missing env vars just make it a no-op with a t.Logf line so the
// operator can see the ledger was not written and why. The returned
// Tracker lets the harness feed cost + start-time updates during the
// run; if the returned pointer is unused, the row's cost is 0 and the
// duration is measured from the Track call.
//
// Verdict derivation: at t.Cleanup, `t.Failed()` reports whether ANY
// Errorf/Fatalf ran during the test. Pass iff not failed.
func Track(t *testing.T) *Tracker {
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
	t.Cleanup(func() {
		if tr.disabled {
			return
		}
		row := tr.buildRow(t.Failed())
		path := tr.resolvedPath(t)
		if err := RecordRow(path, row); err != nil {
			t.Logf("[liveledger] recording row for %s failed: %v", tr.target, err)
		}
	})
	return tr
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
func (tr *Tracker) resolvedPath(t *testing.T) string {
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
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
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
