package bots

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Every log the deep scanner writes lives under LOG_DIR inside the sandbox,
// and the pod is destroyed when the run ends. An operator reading
// "step failed: process" afterwards has no way to learn why.
//
// Measured 2026-09-10: four consecutive launches reported a failed step and
// nothing else. The cause — an unauthenticated agent answering "Not logged
// in" — was only found once the log tail travelled in the node's envelope.
//
// So the envelope must carry the LOG, and this exercises the real reporter
// against a fixture directory rather than asserting on its source text: the
// property is what the script produces, not how it is spelled.
func TestDeepsecEnvelopeCarriesTheLogNotJustTheStepName(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	script := deepsecErrReporter(t)

	logDir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(logDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("preflight.log", "PREFLIGHT DETAIL\nNot logged in · Please run /login\n")
	write("env.log", "[deepsec] token exported\n[preflight] ok=no\n")

	run := func(t *testing.T, tokens string) []string {
		t.Helper()
		f := filepath.Join(t.TempDir(), "report.py")
		if err := os.WriteFile(f, []byte(script), 0o644); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("python3", f)
		cmd.Stdin = strings.NewReader(tokens)
		cmd.Env = append(os.Environ(), "LOG_DIR="+logDir)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("reporter failed: %v (out %q)", err, out)
		}
		var lines []string
		if err := json.Unmarshal(out, &lines); err != nil {
			t.Fatalf("reporter output is not a JSON array: %v (%q)", err, out)
		}
		return lines
	}

	joined := func(lines []string) string { return strings.Join(lines, "\n") }

	t.Run("a token more specific than its log still finds it", func(t *testing.T) {
		// The token names the failure; the file is named after the STEP. A
		// reporter that derives one from the other reports "unreadable" on
		// exactly the failure it exists to explain — measured on three runs.
		got := joined(run(t, "preflight_unreachable"))
		if !strings.Contains(got, "Not logged in") {
			t.Errorf("the preflight log did not travel: %s", got)
		}
		if !strings.Contains(got, "preflight.log") {
			t.Errorf("the envelope does not name the file it read: %s", got)
		}
	})

	t.Run("a missing log names what was tried AND what is there", func(t *testing.T) {
		got := joined(run(t, "process"))
		if strings.Contains(got, "unreadable (") && !strings.Contains(got, "holds:") {
			t.Errorf("the reporter said only that it failed: %s", got)
		}
		for _, want := range []string{"process.log", "holds:", "preflight.log"} {
			if !strings.Contains(got, want) {
				t.Errorf("the message does not mention %q, so the reader cannot tell a wrong name "+
					"from an absent log: %s", want, got)
			}
		}
	})

	t.Run("env.log travels whatever failed", func(t *testing.T) {
		// The credential and transport verdicts are written there at startup;
		// they are what tells a degraded pass from a broken one.
		got := joined(run(t, "export"))
		if !strings.Contains(got, "token exported") || !strings.Contains(got, "ok=no") {
			t.Errorf("env.log did not travel: %s", got)
		}
	})
}

// TestDeepsecTimeoutEscalates pins the kill-after. Bare `timeout` sends
// SIGTERM only and the node process under deepsec ignores it: measured
// 2026-09-10, a 240s bound returned after 27 minutes 39. A bound without -k
// is advisory while reading as enforced.
func TestDeepsecTimeoutEscalates(t *testing.T) {
	body := deepsecCommand(t)
	for _, line := range strings.Split(body, "\n") {
		if !strings.Contains(line, "timeout ") || !strings.Contains(line, "deepsec ") {
			continue
		}
		if !strings.Contains(line, "timeout -k ") {
			t.Errorf("a deepsec invocation is bounded by a bare `timeout`, which only sends SIGTERM — "+
				"the process ignores it and the bound is advisory. Use `timeout -k <grace> <limit>`: %s",
				strings.TrimSpace(line))
		}
	}
}

// deepsecCommand returns the shell body of the deep-scanner node.
func deepsecCommand(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("sec-audit-source/main.bot")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, blk := range commandBackticks(string(raw)) {
		if strings.Contains(blk, "deepsec export --format json") {
			return blk
		}
	}
	t.Fatal("no deepsec command block found")
	return ""
}

// deepsecErrReporter extracts the embedded python that builds the errors[]
// array, with the shell-level escaping of its double quotes undone — which is
// exactly what the shell hands to python3 -c.
func deepsecErrReporter(t *testing.T) string {
	t.Helper()
	body := deepsecCommand(t)
	const marker = `python3 -c "`
	i := strings.Index(body, `ERR_JSON=$(`)
	if i < 0 {
		t.Fatal("no ERR_JSON assignment in the deepsec command")
	}
	j := strings.Index(body[i:], marker)
	if j < 0 {
		t.Fatal("the ERR_JSON assignment embeds no python3 -c body")
	}
	start := i + j + len(marker)
	// The terminator is the closing quote of `python3 -c "…"`, which sits at
	// the start of its own line. Searching for a bare `")` cuts the body at
	// the first ESCAPED quote before a paren — and python then dies on a
	// syntax error with empty stdout, which reads as "the reporter is broken"
	// rather than "the test mis-parsed it".
	end := strings.Index(body[start:], "\n\")")
	if end < 0 {
		t.Fatal("the errors[] reporter is not the multi-line python this test can exercise. If it was " +
			"reverted to the one-liner that emits step NAMES only, the log tail no longer travels in the " +
			"envelope and the failure it explains is unreadable once the pod is gone. If it was merely " +
			"reshaped, re-point this extraction at the new form.")
	}
	return strings.ReplaceAll(body[start:start+end], `\"`, `"`)
}
