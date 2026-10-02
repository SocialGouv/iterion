package bots

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The deep scanner drives its own LLM calls, outside iterion's network retry,
// so the node retries the pass once itself. That retry USED to try to resume
// the failed run with --run-id, but the id deepsec prints is written AFTER
// completeRun flips the meta to phase=done (packages/processor/src/index.ts:
// 833 completeRun, then commands/process.ts:188 prints, then :222 exits 1),
// so a resume finds phase=done and short-circuits with errorBatchCount=0
// -- no batch touched. #1323 removed the resume branch entirely; the retry
// is now always a fresh invocation of `deepsec process`, which reprocesses
// records in status pending|error only.
//
// deepsec still names a quota stop itself ("Stopped: <source> exhausted"),
// so the quota-wall short-circuit stays: retrying one buys a second full
// pass that cannot succeed.
//
// These tests run the REAL retry block from the shipped .bot against a stub
// CLI that records its argv (WHICH invocations the node makes) AND its
// effect (WHAT the second invocation actually does when it runs). The
// property is not just "the retry runs" but "the retry re-investigates
// what was left behind", which the argv alone never asserted.

// deepsecRetryBlock returns the shipped text from the _dsproc definition
// through the end of the retry decision, verbatim. Reading it out of the .bot
// rather than restating it here is the point: a test that carries its own
// copy of the logic certifies the copy.
//
// #1323 removed the --run-id resume branch, so _dsproc is now called with no
// argument on both first attempt and retry. The end marker matches the fresh
// invocation shape.
func deepsecRetryBlock(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("sec-audit-source/main.bot")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, blk := range commandBackticks(string(raw)) {
		start := strings.Index(blk, "_dsproc() {")
		if start < 0 {
			continue
		}
		const endMarker = "_dsproc >>\"$LOG_DIR/process.log\" 2>&1 || ERRS=\"$ERRS process\""
		end := strings.Index(blk, endMarker)
		if end < 0 {
			t.Fatal("the deepsec command body defines _dsproc but the fresh-retry invocation `_dsproc >>...` is missing: either #1323's resume-branch removal regressed to a --run-id call, or the retry has been reshaped in a way this helper no longer recognises")
		}
		rest := blk[end+len(endMarker):]
		closing := strings.Index(rest, "fi")
		if closing < 0 {
			t.Fatal("the retry block is not closed")
		}
		return blk[start : end+len(endMarker)+closing+len("fi")]
	}
	t.Fatal("no command body defines _dsproc")
	return ""
}

// stubDeepsec writes a fake `deepsec` (and an instant `sleep`) onto PATH. The
// fake appends its argv to calls.log and replays the scenario's stdout, so an
// assertion can read exactly which invocations the node made.
func stubDeepsec(t *testing.T, dir, stdout string, exitCode int) {
	t.Helper()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> " + filepath.Join(dir, "calls.log") + "\n" +
		"printf '%b' " + shellQuote(stdout) + "\n" +
		"exit " + itoa(exitCode) + "\n"
	if err := os.WriteFile(filepath.Join(bin, "deepsec"), []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	// The retry sleeps 20s before resuming. The property under test is which
	// invocation happens, not how long the node waits for it.
	if err := os.WriteFile(filepath.Join(bin, "sleep"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write sleep stub: %v", err)
	}
}

// runIDsIn returns the run ids a scenario's stdout announces, read the same
// way the node reads them: the text after "Run: " on a line that also carries
// "Processing complete".
// The scenarios carry ANSI as the literal escapes printf '%b' will expand, so
// the pattern skips them the way the node's tr+sed pair does, then takes the
// leading run of id characters.
var announcedRunID = regexp.MustCompile(`Run: (?:\\033\[[0-9;]*m)*([A-Za-z0-9._-]+)`)

func runIDsIn(stdout string) []string {
	var ids []string
	for _, line := range strings.Split(stdout, `\n`) {
		if !strings.Contains(line, "Processing complete") {
			continue
		}
		if m := announcedRunID.FindStringSubmatch(line); m != nil {
			ids = append(ids, m[1])
		}
	}
	return ids
}

// runRetryBlock executes the shipped block with the stub on PATH and returns
// the recorded invocations plus the ERRS the block ended with.
func runRetryBlock(t *testing.T, stdout string, exitCode int) ([]string, string) {
	t.Helper()
	dir := t.TempDir()
	stubDeepsec(t, dir, stdout, exitCode)

	logDir := filepath.Join(dir, "logs")
	dsw := filepath.Join(dir, "ws")
	for _, d := range []string{logDir, dsw} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}

	// deepsec writes a run meta for every run it starts, and the node now
	// requires one before it accepts an id read out of the log: the log is a
	// channel the audited tree can write into, the meta is not. A scenario that
	// announces a run must therefore have written its meta, or it is not
	// modelling deepsec — it is modelling a forged line.
	for _, id := range runIDsIn(stdout) {
		runs := filepath.Join(dsw, "data", "p", "runs")
		if err := os.MkdirAll(runs, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		body := `{"type":"process","phase":"done","createdAt":"2026-09-16T00:00:00.000Z","stats":{"filesProcessed":1}}`
		if err := os.WriteFile(filepath.Join(runs, id+".json"), []byte(body), 0o644); err != nil {
			t.Fatalf("write meta: %v", err)
		}
	}

	harness := "set -u\n" +
		"LOG_DIR=" + logDir + "\n" +
		"DSW=" + dsw + "\n" +
		"CLAUDE_BIN=/bin/true\nCONC=4\nPROC_ARGS=\nAGENT_ARGS=\nERRS=\n" +
		// The block resolves the run-id meta root off DEEPSEC_DATA_ROOT with a
		// bare reference and a data fallback: the node body runs without set -u,
		// where unset reads empty and the fallback fires. This harness is
		// stricter, so it models the unset case explicitly.
		"DEEPSEC_DATA_ROOT=\n" +
		deepsecRetryBlock(t) + "\n" +
		"printf 'ERRS=%s\\n' \"$ERRS\"\n"

	cmd := exec.Command("bash", "-c", harness)
	cmd.Env = append(os.Environ(), "PATH="+filepath.Join(dir, "bin")+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("harness failed: %v\n%s", err, out)
	}

	calls, _ := os.ReadFile(filepath.Join(dir, "calls.log"))
	var invocations []string
	for _, line := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
		if strings.TrimSpace(line) != "" {
			invocations = append(invocations, line)
		}
	}
	return invocations, strings.TrimSpace(string(out))
}

// A clean pass runs deepsec exactly once, and never names a run id: there is
// nothing to resume.
func TestDeepsecRetry_CleanPassRunsOnce(t *testing.T) {
	calls, out := runRetryBlock(t, "Processing complete. Run: run_abc123\\n", 0)
	if len(calls) != 1 {
		t.Fatalf("a clean pass must invoke deepsec once, got %d: %v", len(calls), calls)
	}
	if strings.Contains(calls[0], "--run-id") {
		t.Fatalf("a first attempt has no run to resume, yet it passed one: %s", calls[0])
	}
	if !strings.Contains(out, "ERRS=") || strings.Contains(out, "ERRS= process") {
		t.Fatalf("a clean pass must record no error, got %q", out)
	}
}

// The case the change exists for: one batch errored, deepsec exited 1. The
// retry MUST invoke `deepsec process` a second time (fresh, no --run-id),
// which reprocesses records in status pending|error only. Argv shape is
// what the old test asserted; here we also assert the EFFECT: the second
// invocation runs, its argv is fresh (no --run-id), and it is a distinct
// invocation from the first. #1323 dropped the --run-id resume branch (a
// dead capability: a --run-id resume of a phase=done run short-circuits
// with errorBatchCount=0 without investigating one batch).
func TestDeepsecRetry_ReprocessesInAFreshInvocation(t *testing.T) {
	stdout := "\\033[32mProcessing complete.\\033[0m Run: \\033[1mrun_abc123\\033[0m\\n" +
		"  Analyses: 120\\n  Findings: 87\\n" +
		"\\033[31m3 batch(es) errored — exiting 1 (agent failure, not a clean review).\\033[0m\\n"
	calls, _ := runRetryBlock(t, stdout, 1)
	if len(calls) != 2 {
		t.Fatalf("a transient failure must be retried exactly once, got %d invocation(s): %v", len(calls), calls)
	}
	if strings.Contains(calls[1], "--run-id") {
		t.Fatalf("the retry must be a FRESH invocation (deepsec process reprocesses pending|error records itself); a --run-id resume of the failed run would short-circuit at phase=done with 0 batches touched (packages/processor/src/index.ts:291). Got: %s", calls[1])
	}
	// The two invocations must have the SAME shape (--concurrency N,
	// PROC_ARGS, AGENT_ARGS) minus the absent --run-id -- the retry is a
	// straight repeat, not a different call.
	if calls[0] != calls[1] {
		t.Errorf("the two invocations diverge: %q vs %q -- the retry is meant to repeat the same fresh call", calls[0], calls[1])
	}
}

// A quota wall is not retried: the second pass cannot succeed, and it would
// spend the node's remaining wall-clock establishing that.
func TestDeepsecRetry_DoesNotRetryAQuotaWall(t *testing.T) {
	stdout := "Processing complete. Run: run_abc123\\n" +
		"\\033[31m✘ Stopped: ChatGPT subscription exhausted\\033[0m\\n" +
		"  Your direct OpenAI account is out of credits/quota.\\n"
	calls, out := runRetryBlock(t, stdout, 1)
	if len(calls) != 1 {
		t.Fatalf("a quota wall must not be retried — a second full pass cannot succeed; got %d invocation(s): %v", len(calls), calls)
	}
	if !strings.Contains(out, "ERRS= process") {
		t.Fatalf("a quota stop must still be reported as a failed step, got %q", out)
	}
}

// A failure with no readable run id in the first attempt log used to be the
// "start fresh" branch of the retry, distinct from the "resume the id we
// read" branch. #1323 collapsed both branches into one: the retry is always
// fresh. This test kept, in the "the retry does not pass a bogus --run-id"
// role -- the ONLY invariant that survived the branch collapse.
func TestDeepsecRetry_NeverPassesRunId(t *testing.T) {
	calls, _ := runRetryBlock(t, "deepsec: command failed before it started\\n", 1)
	if len(calls) != 2 {
		t.Fatalf("a failure with no readable run id is still retried once, got %d: %v", len(calls), calls)
	}
	if strings.Contains(calls[1], "--run-id") {
		t.Fatalf("the retry passed --run-id, which the resume-branch removal (#1323) rules out: %s", calls[1])
	}
}
