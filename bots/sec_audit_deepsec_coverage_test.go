package bots

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Establishing how much of the tree a deep scan reached is the whole point of
// this node: a pass that was capped, or cut short, exports the findings it did
// produce and leaves finding_count looking healthy — on a busy tree a
// truncated pass outnumbers a complete pass on a clean one. A report that
// cannot tell them apart gets transmitted to a product team as a full audit.
//
// Two properties carry that, and each was learned by being defeated:
//
//  1. The oracle is the run metadata the scanner writes about itself, never
//     its log — the audited repository writes into that log (the scanner
//     echoes the paths it reads), so a tree holding a directory named
//     "Batch 4" used to author its own coverage verdict.
//  2. Completion is CONSERVATIVE, and compares nothing across units. A phase
//     is not a completion signal — the scanner records "done" after a quota
//     wall too. Neither are its counters commensurable: --limit counts FILES
//     (its own help text says so), candidatesFound sums CANDIDATES, and the
//     deep pass draws from a persistent backlog that neither measures. A
//     ratio built from them called a healthy unlimited pass incomplete as
//     soon as one file carried two candidates, which is nearly always.
//
// These exercise the REAL reader extracted from the bot against fixture
// metadata. The property is what it produces, never how it is spelled.
func TestDeepsecCoverageReadsTheRunMetaNotTheLog(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	script := deepsecCoverageReader(t)

	// meta writes one run-meta file under <dsw>/data/<project>/runs/.
	meta := func(t *testing.T, dsw, project, name string, doc map[string]any) {
		t.Helper()
		dir := filepath.Join(dsw, "data", project, "runs")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		body, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name+".json"), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// run executes the reader. startEpoch "0" accepts every fixture; a real
	// epoch exercises the run-window pin.
	run := func(t *testing.T, dsw, limit, startEpoch, procFailed string) map[string]any {
		t.Helper()
		f := filepath.Join(t.TempDir(), "coverage.py")
		if err := os.WriteFile(f, []byte(script), 0o644); err != nil {
			t.Fatal(err)
		}
		logDir := filepath.Join(dsw, "logs")
		if err := os.MkdirAll(logDir, 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("python3", f)
		cmd.Env = append(os.Environ(),
			"DSW="+dsw, "PROC_LIMIT="+limit, "START_EPOCH="+startEpoch,
			"PROC_FAILED="+procFailed, "LOG_DIR="+logDir)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("coverage reader failed: %v (out %q)", err, out)
		}
		var got map[string]any
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("reader output is not JSON: %v (%q)", err, out)
		}
		return got
	}

	stamp := func(offset time.Duration) string {
		return time.Now().UTC().Add(offset).Format("2006-01-02T15:04:05.000Z")
	}

	t.Run("a complete unlimited pass is complete", func(t *testing.T) {
		dsw := t.TempDir()
		meta(t, dsw, "p", "s1", map[string]any{"type": "scan", "phase": "done", "createdAt": stamp(-time.Hour),
			"stats": map[string]any{"filesScanned": 1194, "candidatesFound": 2000}})
		meta(t, dsw, "p", "r1", map[string]any{"type": "process", "phase": "done", "createdAt": stamp(-time.Minute),
			"stats": map[string]any{"filesProcessed": 2000}})

		got := run(t, dsw, "0", "0", "0")
		if got["source"] != "run_meta" || got["process_complete"] != true {
			t.Errorf("a whole-tree pass is not reported complete: %v", got)
		}
		if got["limit_truncated"] != false {
			t.Errorf("limit_truncated = %v on an unlimited pass", got["limit_truncated"])
		}
	})

	t.Run("a healthy pass with several candidates per file is complete", func(t *testing.T) {
		// The banner that cries wolf is how an operator learns to ignore the
		// one that matters. With no cap, candidates outnumber files on any
		// real tree (candidatesFound sums candidates, filesScanned counts
		// files), so any predicate comparing the two condemns the normal case.
		dsw := t.TempDir()
		meta(t, dsw, "p", "s1", map[string]any{"type": "scan", "phase": "done", "createdAt": stamp(-time.Hour),
			"stats": map[string]any{"filesScanned": 300, "candidatesFound": 900}})
		meta(t, dsw, "p", "r1", map[string]any{"type": "process", "phase": "done", "createdAt": stamp(-time.Minute),
			"stats": map[string]any{"filesProcessed": 300}})

		if got := run(t, dsw, "0", "0", "0"); got["process_complete"] != true {
			t.Errorf("300 files of 900 candidates, all analysed, reported incomplete: %v", got)
		}
	})

	t.Run("a quota wall is caught although the phase says done", func(t *testing.T) {
		// completeRun(..., "done", ...) runs unconditionally after the workers
		// settle, and the very next statement branches on quotaExhausted — the
		// scanner knows it was walled and records "done" regardless. The
		// truthful term is the one the shell owns: the step failed.
		dsw := t.TempDir()
		meta(t, dsw, "p", "s1", map[string]any{"type": "scan", "phase": "done", "createdAt": stamp(-time.Hour),
			"stats": map[string]any{"candidatesFound": 75}})
		meta(t, dsw, "p", "r1", map[string]any{"type": "process", "phase": "done", "createdAt": stamp(-time.Minute),
			"stats": map[string]any{"filesProcessed": 12}})

		got := run(t, dsw, "0", "0", "1")
		if got["process_complete"] != false {
			t.Errorf("a failed process step reported complete because the phase said done: %v", got)
		}
		if got["process_failed"] != true {
			t.Errorf("process_failed = %v: the step verdict the shell already owns did not travel", got["process_failed"])
		}
	})

	t.Run("nothing processed at all is not a completed pass", func(t *testing.T) {
		// A documented scanner outcome: "Nothing to claim — another run owned
		// every candidate file" also records phase done with filesProcessed 0.
		dsw := t.TempDir()
		meta(t, dsw, "p", "s1", map[string]any{"type": "scan", "phase": "done", "createdAt": stamp(-time.Hour),
			"stats": map[string]any{"candidatesFound": 2000}})
		meta(t, dsw, "p", "r1", map[string]any{"type": "process", "phase": "done", "createdAt": stamp(-time.Minute),
			"stats": map[string]any{"filesProcessed": 0}})

		if got := run(t, dsw, "0", "0", "0"); got["process_complete"] != false {
			t.Errorf("zero files deep-scanned reported as complete: %v", got)
		}
	})

	t.Run("the shipped default cap makes coverage unprovable", func(t *testing.T) {
		// --limit cuts a backlog before batching, so everything downstream of
		// the cap completes normally and looks healthy. Its size is recorded
		// nowhere, so the honest verdict is not "truncated" and not
		// "complete": it is that coverage cannot be shown.
		dsw := t.TempDir()
		meta(t, dsw, "p", "s1", map[string]any{"type": "scan", "phase": "done", "createdAt": stamp(-time.Hour),
			"stats": map[string]any{"candidatesFound": 2000}})
		meta(t, dsw, "p", "r1", map[string]any{"type": "process", "phase": "done", "createdAt": stamp(-time.Minute),
			"stats": map[string]any{"filesProcessed": 50}})

		got := run(t, dsw, "50", "0", "0")
		if got["limit_truncated"] != nil {
			t.Errorf("limit_truncated = %v under a cap: whether the cap BIT cannot be established, "+
				"because --limit cuts a backlog whose size nothing records", got["limit_truncated"])
		}
		// The cap is an INDEPENDENT fact with its own banner. Folding it into
		// the predicate made every run in shipped configuration incomplete for
		// a reason no field carried, which forces the report to invent one.
		if got["process_complete"] != true {
			t.Errorf("a healthy capped pass is condemned by the predicate rather than bannered "+
				"by the cap rule: %v", got)
		}
	})

	t.Run("a scan that died leaves no denominator and no cap claim", func(t *testing.T) {
		// candidatesFound is written ONLY by completeRun(..., "done", ...),
		// and the scanner package has no error call site at all — so a killed
		// scan leaves stats empty. A boolean limit_truncated would then read
		// false whatever the limit, silently disarming the cap banner.
		dsw := t.TempDir()
		meta(t, dsw, "p", "s1", map[string]any{"type": "scan", "phase": "running", "createdAt": stamp(-time.Hour),
			"stats": map[string]any{}})
		meta(t, dsw, "p", "r1", map[string]any{"type": "process", "phase": "done", "createdAt": stamp(-time.Minute),
			"stats": map[string]any{"filesProcessed": 9}})

		got := run(t, dsw, "50", "0", "0")
		if got["candidates_found"] != nil {
			t.Errorf("candidates_found invented: %v", got["candidates_found"])
		}
		if got["limit_truncated"] != nil {
			t.Errorf("limit_truncated = %v under a cap, want null", got["limit_truncated"])
		}
		if got["scan_phase"] != "running" {
			t.Errorf("scan_phase = %v, want running — the report cannot see the scan died", got["scan_phase"])
		}
		// The dead scan is caught by the scan_phase gate, not by this predicate:
		// the PROCESS did run to an end here, and conflating the two would make
		// the reason 3b reports unrecoverable from the fields.
		if got["scan_phase"] == "done" {
			t.Error("a killed scan must not read as done")
		}
	})

	t.Run("metadata from a previous run in the shared scratch is ignored", func(t *testing.T) {
		// PROJECT_SCRATCH_DIR is per-workspace and deliberately shared between
		// runs, so "the newest meta on disk" is not "this run's meta". Without
		// the window, a pass where the scanner did nothing reports the
		// previous pass — the most destructive run yielding the most
		// reassuring statement.
		dsw := t.TempDir()
		meta(t, dsw, "p", "old-s", map[string]any{"type": "scan", "phase": "done", "createdAt": stamp(-2 * time.Hour),
			"stats": map[string]any{"candidatesFound": 2000}})
		meta(t, dsw, "p", "old-r", map[string]any{"type": "process", "phase": "done", "createdAt": stamp(-2 * time.Hour),
			"stats": map[string]any{"filesProcessed": 2000}})

		start := strconv.FormatInt(time.Now().UTC().Add(-time.Minute).Unix(), 10)
		got := run(t, dsw, "0", start, "0")
		if got["source"] != "unavailable" {
			t.Errorf("a stale run was adopted as this run: %v", got)
		}
		if got["process_complete"] != false || got["candidates_found"] != nil {
			t.Errorf("stale numbers leaked into the verdict: %v", got)
		}
	})

	t.Run("a meta with no timestamp is skipped, not ranked first", func(t *testing.T) {
		// String-ranking a missing timestamp puts "None" above every ISO date
		// ('N' > '2'), so an undated stale meta would outrank a live one.
		dsw := t.TempDir()
		meta(t, dsw, "p", "s1", map[string]any{"type": "scan", "phase": "done", "createdAt": stamp(-time.Hour),
			"stats": map[string]any{"candidatesFound": 30}})
		meta(t, dsw, "p", "undated", map[string]any{"type": "process", "phase": "done",
			"stats": map[string]any{"filesProcessed": 999}})
		meta(t, dsw, "p", "live", map[string]any{"type": "process", "phase": "done", "createdAt": stamp(-time.Minute),
			"stats": map[string]any{"filesProcessed": 3}})

		got := run(t, dsw, "0", "0", "0")
		if got["files_processed"] != float64(3) {
			t.Errorf("an undated meta outranked the live one: files_processed = %v", got["files_processed"])
		}
		if got["files_processed"] == float64(999) {
			t.Error("the undated meta numbers reached the verdict")
		}
	})

	t.Run("no run metadata is unavailable, never complete", func(t *testing.T) {
		got := run(t, t.TempDir(), "0", "0", "0")
		if got["source"] != "unavailable" || got["process_complete"] != false {
			t.Errorf("an empty data dir produced %v", got)
		}
	})
}

// The coverage must reach the report WITHOUT dragging the envelope with it,
// on the same shape on every path. A counter computed and not emitted does
// not exist for anyone reading the run afterwards.
func TestDeepsecCoverageTravelsWithoutTheLogTail(t *testing.T) {
	body := deepsecCommand(t)
	bot := readSecAuditBot(t)

	if !strings.Contains(body, `\"coverage\":%s`) {
		t.Error("the success envelope does not emit coverage — it never leaves the pod")
	}

	// Same keys on every path. A consumer that reads process_complete must not
	// get nothing on the path where the scanner was absent: an absent field
	// and a false one are different facts, and only one of them is safe.
	for _, key := range []string{"source", "candidates_found", "files_scanned", "files_processed",
		"scan_phase", "process_phase", "process_limit", "limit_truncated", "process_complete"} {
		if !strings.Contains(body, `\"`+key+`\":`) {
			t.Errorf("the envelope never mentions %q", key)
		}
		if strings.Count(body, `\"`+key+`\":`) < 2 {
			t.Errorf("%q appears on only one envelope path: the degraded and the failed-reader "+
				"shapes must carry the same keys as the success shape", key)
		}
	}

	// report_card holds bash, write_file and board.create. The envelope's
	// errors[] carries up to 3 KB of log tail, and the scanner echoes
	// target-controlled text into its logs — so the node that files findings
	// receives the scalars, never the envelope.
	if strings.Contains(bot, `deepsec_coverage:   "{{outputs.scan_join.deepsec_scan}}"`) {
		t.Error("report_card is fed the whole deepsec envelope: its errors[] carries a log tail " +
			"the audited repository can write into, and report_card holds bash and board.create")
	}
	if !strings.Contains(bot, `deepsec_coverage:   "{{outputs.scan_join.deepsec_coverage}}"`) {
		t.Error("report_card is not fed the projected coverage scalars")
	}

	// ERRS drives the export-unusable guard, which DELETES the export when the
	// count is zero. Folding coverage into it would drop a harvest worth
	// keeping — the regression that cost the pilot a whole run.
	for _, forbidden := range []string{`ERRS="$ERRS partial_coverage"`, `ERRS="$ERRS limit_truncated"`} {
		if strings.Contains(body, forbidden) {
			t.Errorf("coverage is folded into ERRS (%s): it would trip the export-unusable guard", forbidden)
		}
	}

	// The window is pinned BEFORE the scan, not derived at verdict time. The
	// scratch is shared between runs, so a reader that asks "the newest meta"
	// at the end certifies whatever the name points at by then.
	scanAt := strings.Index(body, "deepsec scan )")
	startAt := strings.Index(body, "START_EPOCH=$(date")
	if startAt < 0 {
		t.Fatal("no START_EPOCH pin: the reader would adopt a previous run metadata from the shared scratch")
	}
	if scanAt >= 0 && startAt > scanAt {
		t.Error("START_EPOCH is captured AFTER the scan starts: metadata this run wrote can fall " +
			"outside its own window, and a stale run can fall inside it")
	}

	// The reader must not open a log. An anchored regex is not a repair: the
	// adversary is arbitrary text, and each anchor invites one more spelling.
	reader := deepsecCoverageReader(t)
	for _, forbidden := range []string{"process.log", "scan.log", "export.log", "re.search", "re.match"} {
		if strings.Contains(reader, forbidden) {
			t.Errorf("the coverage reader references %q: reading a log the scanner fills with "+
				"target-controlled text hands the audited tree its own verdict", forbidden)
		}
	}
}

// deepsecCoverageReader extracts the coverage reader from the deepsec node
// body, mirroring deepsecErrReporter: the test exercises what ships, so a
// reshape of the script is caught here rather than in production.
func deepsecCoverageReader(t *testing.T) string {
	t.Helper()
	body := deepsecCommand(t)
	const marker = `python3 -c "`
	i := strings.Index(body, `COVERAGE=$(`)
	if i < 0 {
		t.Fatal("no COVERAGE assignment in the deepsec command: the node no longer establishes how " +
			"much of the tree the deep scan reached, so a capped or quota-walled pass is reported as a full one")
	}
	j := strings.Index(body[i:], marker)
	if j < 0 {
		t.Fatal("the COVERAGE assignment embeds no python3 -c body")
	}
	start := i + j + len(marker)
	end := strings.Index(body[start:], "\n\" 2>>")
	if end < 0 {
		t.Fatal("the coverage reader is not the multi-line python this test can exercise; " +
			"if it was merely reshaped, re-point this extraction at the new form")
	}
	return strings.ReplaceAll(body[start:start+end], `\"`, `"`)
}

func readSecAuditBot(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("sec-audit-source", "main.bot"))
	if err != nil {
		t.Fatalf("read sec-audit-source/main.bot: %v", err)
	}
	return string(body)
}

// A retry that RESUMES a run the scanner already closed returns without
// investigating one batch and exits zero (processor/src/index.ts:255), so the
// `|| ERRS=...` never fires and a first attempt that failed on errored batches
// is laundered into a clean pass. Every conservative term then reads healthy
// over whatever fraction the first attempt reached.
//
// This exercises the REAL node body under sh against a stub reproducing both
// exits, because the flag that catches it is a SHELL variable — the python
// reader alone cannot see it.
func TestDeepsecResumedRetryIsNotACleanPass(t *testing.T) {
	body := secToolCommand(t, "run_deepsec_scanner")

	dir := t.TempDir()
	scanDir, ws, stubs := filepath.Join(dir, "scan"), filepath.Join(dir, "ws"), filepath.Join(dir, "bin")
	out := filepath.Join(scanDir, "deepsec.json")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	stubBin(t, stubs, "node", `echo v22.0.0`)
	// process: first call prints the run id then fails on errored batches;
	// the --run-id retry short-circuits on the already-done run and exits 0.
	stubBin(t, stubs, "deepsec", `
case "$1" in
  process)
    for a in "$@"; do case "$a" in --run-id) exit 0;; esac; done
    echo "Processing complete. Run: RID123"
    echo "40 batch(es) errored — exiting 1 (agent failure, not a clean review)."
    exit 1 ;;
  export)
    prev=""; for a in "$@"; do case "$prev" in --out) echo '[{"id":1}]' > "$a";; esac; prev="$a"; done
    exit 0 ;;
  *) exit 0 ;;
esac`)

	rendered := body
	for ref, val := range map[string]string{
		"{{vars.scan_dir}}":              scanDir,
		"{{vars.workspace_dir}}":         ws,
		"{{vars.deepsec_out}}":           out,
		"{{vars.deepsec_concurrency}}":   "1",
		"{{vars.deepsec_process_limit}}": "0",
		"{{vars.deepsec_root}}":          filepath.Join(dir, "absent"),
	} {
		rendered = strings.ReplaceAll(rendered, ref, val)
	}
	if strings.Contains(rendered, "{{") {
		t.Fatalf("unsubstituted ref left in the command near %q", rendered[strings.Index(rendered, "{{"):])
	}

	raw := runShell(t, rendered, stubs)
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	var env struct {
		Coverage map[string]any `json:"coverage"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &env); err != nil {
		t.Fatalf("envelope is not JSON: %v (%q)", err, lines[len(lines)-1])
	}

	if env.Coverage["process_failed"] != true {
		t.Errorf("a resumed retry laundered a failed process step: process_failed = %v",
			env.Coverage["process_failed"])
	}
	if env.Coverage["process_complete"] != false {
		t.Errorf("coverage reads COMPLETE after a process step that errored 40 batches and a "+
			"retry that investigated nothing: %v", env.Coverage)
	}
}
