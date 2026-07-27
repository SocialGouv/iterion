package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// This file drives the REAL `pi` binary end to end, with no credentials, no
// network and no cost: a test-only pi extension registers a provider whose
// model replays a scripted response (testdata/mock-provider.ts, resolved
// from the pisdk package).
//
// It exists because pisdk/ is a PORT of pi's wire types, and the only thing
// that can tell you a port still matches is a stream pi actually produced.
// A hand-written fixture cannot notice that pi renamed a field — and pi
// ships roughly weekly on a 0.x line. Every assertion here failed to be
// obvious from reading the source: the empty `message_start` object, the
// exit-0-on-failure behaviour, and pi's internal retry loop were all found
// by running this.
//
// Skipped when `pi` is not installed, so it is free for developers and CI
// that do not have it, and automatic drift detection for those that do.

func requirePiBinary(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the smoke harness uses a POSIX shell fixture layout")
	}
	bin := os.Getenv("ITERION_PI_BIN")
	if bin == "" {
		var err error
		bin, err = exec.LookPath("pi")
		if err != nil {
			t.Skip("pi not installed — skipping the real-binary smoke test " +
				"(npm i -g @earendil-works/pi-coding-agent, or set ITERION_PI_BIN)")
		}
	}
	return bin
}

// mockProviderPath resolves the test-only extension. It lives beside the SDK
// it guards, not beside this test, because it is the fixture for the port.
func mockProviderPath(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("pisdk", "testdata", "mock-provider.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("mock provider extension missing at %s: %v", abs, err)
	}
	return abs
}

// piSmokeTask builds a task whose argv exercises the flags the protocol emits.
func piSmokeTask(t *testing.T, ext string) Task {
	t.Helper()
	dir := t.TempDir()
	return Task{
		NodeID:           "smoke",
		WorkDir:          dir,
		BaseDir:          dir,
		StoreDir:         dir,
		SystemPrompt:     "You are under test.",
		SystemPromptMode: SystemPromptAppendToNative,
		UserPrompt:       "- reply exactly as scripted",
		Model:            "mock/scripted",
		ReasoningEffort:  "high",
		ExtraEnv:         []string{"ITERION_PI_EXT=" + ext},
	}
}

// newPiSmokeBackend wires the real binary with the mock-provider extension
// appended to the protocol's argv.
func newPiSmokeBackend(t *testing.T, bin, ext string) *CLIAgentBackend {
	t.Helper()
	proto := piProtocol
	base := proto.ExtraArgsFor
	proto.ExtraArgsFor = func(task Task) []string {
		return append(base(task), "-e", ext)
	}
	return &CLIAgentBackend{Protocol: proto, Command: bin, Logger: testLogger()}
}

// TestPiSmokeHappyPath is the load-bearing assertion of the whole port: a
// stream pi really emitted, decoded by pisdk, surfacing as a Result.
func TestPiSmokeHappyPath(t *testing.T) {
	bin := requirePiBinary(t)
	ext := mockProviderPath(t)

	b := newPiSmokeBackend(t, bin, ext)
	task := piSmokeTask(t, ext)
	task.OutputSchema = json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}}}`)

	t.Setenv("ITERION_PI_MOCK_TEXT", `{"answer":"42"}`)
	t.Setenv("ITERION_PI_MOCK_COST", "0.25")
	t.Setenv("ITERION_PI_MOCK_IN", "120")
	t.Setenv("ITERION_PI_MOCK_OUT", "30")
	t.Setenv("ITERION_PI_MOCK_REASONING", "12")

	res, err := b.Execute(context.Background(), task)
	if err != nil {
		t.Fatalf("Execute against the real pi: %v (stderr: %s)", err, res.Stderr)
	}

	if res.BackendName != BackendPi {
		t.Errorf("BackendName = %q, want %q", res.BackendName, BackendPi)
	}
	// pi mints its own session id when we pass --session-id; the header is
	// the only place it appears, so an empty value means the header shape
	// drifted.
	if res.SessionID == "" {
		t.Error("SessionID empty — pi's session header shape changed")
	}
	if res.Output["answer"] != "42" {
		t.Errorf("Output[answer] = %v, want 42 (schema extraction over a real stream)", res.Output["answer"])
	}
	if res.Tokens != 150 {
		t.Errorf("Tokens = %d, want 150 — pi's usage.input/output shape drifted", res.Tokens)
	}
	if res.ThinkingTokens != 12 {
		t.Errorf("ThinkingTokens = %d, want 12 — usage.reasoning drifted", res.ThinkingTokens)
	}
	if got := res.Output["_cost_usd"]; got != 0.25 {
		t.Errorf("_cost_usd = %v, want 0.25 — usage.cost.total drifted", got)
	}
	if res.EffectiveModel != "scripted" {
		t.Errorf("EffectiveModel = %q, want scripted", res.EffectiveModel)
	}
}

// TestPiSmokeExitZeroOnFailure pins the behaviour that makes stopReason the
// only usable failure signal: pi's json mode exits 0 even on a failed turn.
// If a future pi starts exiting non-zero, this test tells us the parser's
// error path is no longer the only one that matters.
func TestPiSmokeExitZeroOnFailure(t *testing.T) {
	bin := requirePiBinary(t)
	ext := mockProviderPath(t)

	b := newPiSmokeBackend(t, bin, ext)
	t.Setenv("ITERION_PI_MOCK_STOP", "error")
	t.Setenv("ITERION_PI_MOCK_ERROR", "invalid x-api-key")

	res, err := b.Execute(context.Background(), piSmokeTask(t, ext))
	if err == nil {
		t.Fatal("expected an error: pi reported stopReason=error")
	}
	if res.ExitCode != 0 {
		t.Logf("NOTE: pi exited %d on a failed turn — it used to exit 0 in --mode json. "+
			"The stopReason path is still correct, but the exit code is now informative too.", res.ExitCode)
	}
	// An auth failure must stay deterministic so the executor does not burn
	// retries on a misconfiguration.
	var transient *ErrTransient
	var rateLimited *ErrRateLimited
	if errors.As(err, &transient) || errors.As(err, &rateLimited) {
		t.Errorf("err = %v (%T), want a plain error for an auth failure", err, err)
	}
}

// TestPiSmokeInternalRetriesAreSurfaced covers the finding that motivated
// CLIAgentParse.Notices: pi retries upstream failures itself, each attempt is
// billed, and only the last transcript survives — so the reported cost is
// short by the number of discarded attempts. The run must at minimum say so.
func TestPiSmokeInternalRetriesAreSurfaced(t *testing.T) {
	bin := requirePiBinary(t)
	ext := mockProviderPath(t)

	// A retryable-looking failure makes pi exercise its own retry loop
	// (3 attempts, 2s/4s/8s backoff) before giving up.
	t.Setenv("ITERION_PI_MOCK_STOP", "error")
	t.Setenv("ITERION_PI_MOCK_ERROR", "rate_limit_error: 429 too many requests")

	// Drive pi directly and parse its raw stream, rather than going through
	// Execute: the CLIAgentBackend would add its own retry loop on top of
	// pi's, tripling an already slow test.
	//
	// The prompt MUST be delivered — with an empty stdin pi has nothing to
	// do, emits only its session header and exits, so there is no turn to
	// fail and no retry to observe. (That is how this test first passed
	// vacuously in 0.46s instead of pi's ~14s of backoff.)
	// pi's default backoff is 2s/4s/8s, which would put 14 seconds into every
	// `task test`. A pinned agent dir with baseDelayMs=1 keeps all three real
	// attempts while making them instant — and exercises the
	// ITERION_PI_AGENT_DIR escape hatch the docs point operators at.
	agentDir := t.TempDir()
	settings := `{"retry":{"enabled":true,"maxRetries":3,"baseDelayMs":1}}`
	if err := os.WriteFile(filepath.Join(agentDir, "settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "--mode", "json", "--no-approve", "-e", ext, "--model", "mock/scripted") // #nosec G204 — test fixture
	cmd.Dir = t.TempDir()
	cmd.Stdin = strings.NewReader("trigger the scripted failure")
	cmd.Env = append(os.Environ(),
		"PI_CODING_AGENT_DIR="+agentDir,
		"ITERION_PI_MOCK_STOP=error",
		"ITERION_PI_MOCK_ERROR=rate_limit_error: 429 too many requests",
	)
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("pi invocation failed (%v) — not a port regression", err)
	}

	parsed := parsePiOutput(string(out))
	if len(parsed.Notices) == 0 {
		t.Error("no Notices: pi's internal retries would be invisible, " +
			"leaving a slow node with an under-reported cost and no explanation")
	}
	var rateLimited *ErrRateLimited
	if !errors.As(parsed.Err, &rateLimited) {
		t.Errorf("Err = %v (%T), want *ErrRateLimited", parsed.Err, parsed.Err)
	}
}

// TestPiSmokeFlagsAccepted guards the whole argv surface at once. A flag pi
// removes or renames becomes a hard startup failure, which is exactly the
// class of breakage a weekly 0.x release can introduce.
func TestPiSmokeFlagsAccepted(t *testing.T) {
	bin := requirePiBinary(t)

	out, err := exec.Command(bin, "--help").CombinedOutput() // #nosec G204 — resolved binary
	if err != nil {
		t.Fatalf("pi --help: %v", err)
	}
	help := string(out)

	// Every flag piProtocol / piExtraArgsFor can emit.
	for _, flag := range []string{
		"--mode", "--no-approve", "--no-prompt-templates", "--no-themes",
		"--append-system-prompt", "--thinking", "--provider", "--model",
		"--session-id", "--fork", "--session-dir", "--skill", "--tools",
		"--offline", "--approve",
	} {
		if !bytesContainsFlag(help, flag) {
			t.Errorf("pi --help no longer documents %q — the protocol emits a flag pi may have dropped", flag)
		}
	}
}

func bytesContainsFlag(help, flag string) bool {
	for i := 0; i+len(flag) <= len(help); i++ {
		if help[i:i+len(flag)] != flag {
			continue
		}
		// Require a boundary after the flag so "--model" does not match
		// inside "--models".
		if i+len(flag) == len(help) {
			return true
		}
		switch help[i+len(flag)] {
		case ' ', ',', '=', '\n', '\t', '\r', '<', '[':
			return true
		}
	}
	return false
}
