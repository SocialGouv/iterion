package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPiRPCArgs(t *testing.T) {
	// pisdk emits `--mode rpc` itself; leaking print mode's `--mode json`
	// would put pi in the wrong mode and hang the handshake.
	t.Run("print mode selection is stripped", func(t *testing.T) {
		args := piRPCArgs(Task{Model: "openai/gpt-5.5"}, "")
		if slices.Contains(args, "json") {
			t.Errorf("argv still carries print mode's output selection: %v", args)
		}
		if i := slices.Index(args, "--mode"); i >= 0 {
			t.Errorf("argv sets --mode itself (%v) — pisdk owns that flag", args[i:])
		}
	})

	// Everything else must be shared with print mode, or the two transports
	// would diverge observably and could not share a backend name.
	t.Run("shares the per-task flags with print mode", func(t *testing.T) {
		task := Task{Model: "anthropic/glm-5.2", ProviderHint: "zai", ReasoningEffort: "high", Readonly: true}
		args := piRPCArgs(task, "/tmp/sys.md")
		for _, want := range [][2]string{
			{"--provider", "zai"},
			{"--model", "glm-5.2"},
			{"--thinking", "high"},
			{"--append-system-prompt", "/tmp/sys.md"},
		} {
			i := slices.Index(args, want[0])
			if i < 0 || args[i+1] != want[1] {
				t.Errorf("argv missing %s %s: %v", want[0], want[1], args)
			}
		}
		if !slices.Contains(args, "--no-approve") {
			t.Errorf("argv must keep refusing the target repo's .pi/: %v", args)
		}
		if !slices.Contains(args, "--tools") {
			t.Errorf("readonly must still pin the tool set: %v", args)
		}
	})

	t.Run("no system prompt flag when there is no prompt", func(t *testing.T) {
		if slices.Contains(piRPCArgs(Task{}, ""), "--append-system-prompt") {
			t.Error("emitted --append-system-prompt with no value")
		}
	})
}

// TestPiRPCLiveEquivalence is the safety net for sharing one backend name
// across two transports: the same task, run both ways, must produce an
// equivalent Result. Anything else means a workflow's behaviour would change
// under the operator when the default flips.
func TestPiRPCLiveEquivalence(t *testing.T) {
	bin := requirePiBinary(t)
	ext := mockProviderPath(t)

	schema := json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}}}`)
	newTask := func(t *testing.T) Task {
		dir := t.TempDir()
		return Task{
			NodeID:           "equiv",
			WorkDir:          dir,
			BaseDir:          dir,
			StoreDir:         dir,
			SystemPrompt:     "You are under test.",
			SystemPromptMode: SystemPromptAppendToNative,
			UserPrompt:       "reply as scripted",
			Model:            "mock/scripted",
			OutputSchema:     schema,
		}
	}

	t.Setenv("ITERION_PI_MOCK_TEXT", `{"answer":"42"}`)
	t.Setenv("ITERION_PI_MOCK_IN", "100")
	t.Setenv("ITERION_PI_MOCK_OUT", "20")
	t.Setenv("ITERION_PI_MOCK_COST", "0.125")
	t.Setenv("ITERION_PI_NO_CONTEXT_FILES", "1")

	printRes, err := newPiSmokeBackend(t, bin, ext).Execute(context.Background(), newTask(t))
	if err != nil {
		t.Fatalf("print transport: %v (stderr: %s)", err, printRes.Stderr)
	}

	// The mock provider loads through the same ExtraArgs seam the iterion
	// extension will use in T2.
	rpc := &PiRPCBackend{Command: bin, Logger: testLogger(), ExtraArgs: []string{"-e", ext}}
	rpcRes, err := rpc.Execute(context.Background(), newTask(t))
	if err != nil {
		t.Fatalf("rpc transport: %v (stderr: %s)", err, rpcRes.Stderr)
	}

	if printRes.Output["answer"] != rpcRes.Output["answer"] {
		t.Errorf("answer differs: print=%v rpc=%v", printRes.Output["answer"], rpcRes.Output["answer"])
	}
	if rpcRes.Output["answer"] != "42" {
		t.Errorf("rpc answer = %v, want 42", rpcRes.Output["answer"])
	}
	if printRes.BackendName != rpcRes.BackendName {
		t.Errorf("BackendName differs: %q vs %q", printRes.BackendName, rpcRes.BackendName)
	}
	if rpcRes.SessionID == "" {
		t.Error("rpc SessionID empty — the handshake should fill it before any token is spent")
	}
	if rpcRes.EffectiveModel != "scripted" {
		t.Errorf("rpc EffectiveModel = %q, want scripted (resolved pre-flight)", rpcRes.EffectiveModel)
	}
	if rpcRes.Tokens <= 0 {
		t.Errorf("rpc Tokens = %d, want the session-stats total", rpcRes.Tokens)
	}
	if got := rpcRes.Output["_cost_usd"]; got == nil {
		t.Error("rpc lost the provider-computed cost")
	}

	// The token count MUST match across transports. Both report
	// input+output excluding cache, the convention claude_code sets — an
	// early RPC version folded cache reads/writes into input, which made
	// `max_tokens` mean something different depending on the transport.
	// This assertion is what gates flipping the default.
	if printRes.Tokens != rpcRes.Tokens {
		t.Errorf("Tokens differ across transports: print=%d rpc=%d — a workflow's "+
			"max_tokens budget would shift under the operator on a transport switch",
			printRes.Tokens, rpcRes.Tokens)
	}
	if printRes.Output["_cost_usd"] != rpcRes.Output["_cost_usd"] {
		t.Errorf("_cost_usd differs: print=%v rpc=%v",
			printRes.Output["_cost_usd"], rpcRes.Output["_cost_usd"])
	}
}

// TestPiRPCLiveHooks covers what only this transport delivers: tool events and
// assistant narration reaching iterion's hooks. Without them the studio's
// timeline and "Produced elements" panel stay empty for every pi node.
func TestPiRPCLiveHooks(t *testing.T) {
	bin := requirePiBinary(t)
	ext := mockProviderPath(t)

	t.Setenv("ITERION_PI_MOCK_TEXT", "narrating")
	t.Setenv("ITERION_PI_NO_CONTEXT_FILES", "1")

	var (
		mu       sync.Mutex
		texts    []string
		turnInfo *TurnFinishedInfo
	)
	dir := t.TempDir()
	task := Task{
		NodeID: "hooks", WorkDir: dir, BaseDir: dir, StoreDir: dir,
		UserPrompt: "go", Model: "mock/scripted",
		Hooks: TaskHooks{
			OnAssistantText: func(text string) {
				mu.Lock()
				texts = append(texts, text)
				mu.Unlock()
			},
			OnTurnFinished: func(info TurnFinishedInfo) {
				mu.Lock()
				turnInfo = &info
				mu.Unlock()
			},
		},
	}

	rpc := &PiRPCBackend{Command: bin, Logger: testLogger(), ExtraArgs: []string{"-e", ext}}
	if _, err := rpc.Execute(context.Background(), task); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !slices.Contains(texts, "narrating") {
		t.Errorf("OnAssistantText never saw the reply; got %v", texts)
	}
	if turnInfo == nil {
		t.Fatal("OnTurnFinished never fired — the runtime cannot checkpoint the turn")
	}
	if turnInfo.SessionID == "" {
		t.Error("TurnFinishedInfo.SessionID empty — fork-from-here would be unavailable")
	}
	if turnInfo.Text != "narrating" {
		t.Errorf("TurnFinishedInfo.Text = %q, want the final assistant text", turnInfo.Text)
	}
}

// TestPiRPCLiveFailureTyping pins that a failed turn is re-typed the same way
// on both transports, so the executor's retry policy does not depend on which
// one is active.
func TestPiRPCLiveFailureTyping(t *testing.T) {
	bin := requirePiBinary(t)
	ext := mockProviderPath(t)

	t.Setenv("ITERION_PI_MOCK_STOP", "error")
	t.Setenv("ITERION_PI_MOCK_ERROR", "rate_limit_error: 429 too many requests")
	t.Setenv("ITERION_PI_MOCK_STATUS", "429")
	t.Setenv("ITERION_PI_NO_CONTEXT_FILES", "1")

	dir := t.TempDir()
	task := Task{NodeID: "fail", WorkDir: dir, BaseDir: dir, StoreDir: dir,
		UserPrompt: "go", Model: "mock/scripted"}

	rpc := &PiRPCBackend{Command: bin, Logger: testLogger(), ExtraArgs: []string{"-e", ext}}
	_, err := rpc.Execute(context.Background(), task)
	var rl *ErrRateLimited
	if !errors.As(err, &rl) {
		t.Fatalf("err = %v (%T), want *ErrRateLimited from the 429 diagnostic", err, err)
	}
}

// TestPiRPCLiveContextCancel guards that cancellation aborts the turn rather
// than leaking a process that lives forever by design.
func TestPiRPCLiveContextCancel(t *testing.T) {
	bin := requirePiBinary(t)
	ext := mockProviderPath(t)

	t.Setenv("ITERION_PI_NO_CONTEXT_FILES", "1")

	dir := t.TempDir()
	task := Task{NodeID: "cancel", WorkDir: dir, BaseDir: dir, StoreDir: dir,
		UserPrompt: "go", Model: "mock/scripted"}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled: Execute must give up promptly, not hang

	rpc := &PiRPCBackend{Command: bin, Logger: testLogger(), ExtraArgs: []string{"-e", ext}}
	start := time.Now()
	_, err := rpc.Execute(ctx, task)
	if err == nil {
		t.Fatal("expected an error on a cancelled context")
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Errorf("took %s — cancellation must not wait out a stream guard", elapsed)
	}
}

func TestPiWriteSystemPrompt(t *testing.T) {
	dir := t.TempDir()
	task := Task{NodeID: "node/with:odd chars", Iteration: 2, WorkDir: dir}

	path, cleanup, err := piWriteSystemPrompt(task, "be terse")
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path) // #nosec G304 — test-controlled path
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "be terse" {
		t.Errorf("prompt file = %q, want the composed prompt", body)
	}
	// Workspace-relative so a sandboxed run can see it through the bind mount.
	rel, err := filepath.Rel(dir, path)
	if err != nil || rel == "" || strings.HasPrefix(rel, "..") {
		t.Errorf("prompt file %q escapes the workspace — a container could not read it (rel=%q)", path, rel)
	}
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("prompt file survived cleanup and would show in the run's diff")
	}

	t.Run("empty prompt writes nothing", func(t *testing.T) {
		p, c, err := piWriteSystemPrompt(Task{WorkDir: dir}, "")
		if err != nil || p != "" {
			t.Fatalf("got (%q, %v), want no file", p, err)
		}
		c()
	})
}
