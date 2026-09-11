package bots

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The deep scanner drives its own LLM calls, outside iterion's network retry,
// so the node retries the pass once itself. That retry used to open a FRESH
// deepsec run: every batch the first attempt had already investigated was
// paid for again, under the same bound that had just expired — and when the
// first attempt died on a provider quota, the retry spent the remaining
// wall-clock proving the wall was still there.
//
// deepsec answers both: `--run-id` resumes a run, and it names a quota stop
// itself ("Stopped: <source> exhausted"). These tests run the REAL retry
// block from the shipped .bot against a stub CLI that records its argv,
// because the property is which invocations the node actually makes.

// deepsecRetryBlock returns the shipped text from the _dsproc definition
// through the end of the retry decision, verbatim. Reading it out of the .bot
// rather than restating it here is the point: a test that carries its own
// copy of the logic certifies the copy.
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
		const endMarker = "_dsproc \"$_RID\" >>\"$LOG_DIR/process.log\" 2>&1 || ERRS=\"$ERRS process\""
		end := strings.Index(blk, endMarker)
		if end < 0 {
			t.Fatal("the deepsec command body defines _dsproc but never calls it with a resumed run id: the retry would restart the pass from scratch")
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

	harness := "set -u\n" +
		"LOG_DIR=" + logDir + "\n" +
		"DSW=" + dsw + "\n" +
		"CLAUDE_BIN=/bin/true\nCONC=4\nPROC_ARGS=\nAGENT_ARGS=\nERRS=\n" +
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

// The case the change exists for: one batch errored, deepsec exited 1, and the
// run id it printed BEFORE exiting is handed back so the retry resumes instead
// of re-investigating every finished batch.
func TestDeepsecRetry_ResumesTheRunItAlreadyStarted(t *testing.T) {
	stdout := "\\033[32mProcessing complete.\\033[0m Run: \\033[1mrun_abc123\\033[0m\\n" +
		"  Analyses: 120\\n  Findings: 87\\n" +
		"\\033[31m3 batch(es) errored — exiting 1 (agent failure, not a clean review).\\033[0m\\n"
	calls, _ := runRetryBlock(t, stdout, 1)
	if len(calls) != 2 {
		t.Fatalf("a transient failure must be retried exactly once, got %d invocation(s): %v", len(calls), calls)
	}
	if !strings.Contains(calls[1], "--run-id run_abc123") {
		t.Fatalf("the retry must RESUME run_abc123, or every finished batch is investigated and paid for twice; got: %s", calls[1])
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

// When the id is unreadable the node says so and starts fresh, rather than
// passing an empty --run-id that deepsec would reject.
func TestDeepsecRetry_StartsFreshWhenNoRunIdWasPrinted(t *testing.T) {
	calls, _ := runRetryBlock(t, "deepsec: command failed before it started\\n", 1)
	if len(calls) != 2 {
		t.Fatalf("a failure with no readable run id is still retried once, got %d: %v", len(calls), calls)
	}
	if strings.Contains(calls[1], "--run-id") {
		t.Fatalf("with no id to resume the retry must start fresh, not pass an empty one: %s", calls[1])
	}
}
