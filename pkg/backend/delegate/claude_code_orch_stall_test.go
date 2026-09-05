//go:build !windows

package delegate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate/claudesdk"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// fakeClaudeOrch is a stand-in `claude` CLI speaking the stream-json
// protocol: NDJSON on stdout, NDJSON user/control messages on stdin. It
// replays the orchestration-stall shapes the guard must tell apart.
// Scenario via FAKE_CLAUDE_SCENARIO; every stdin line is appended to
// FAKE_CLAUDE_STDIN_LOG so a test can assert what the runtime sent.
const fakeClaudeOrch = `#!/bin/sh
emit() { printf '%s\n' "$1"; }
log="${FAKE_CLAUDE_STDIN_LOG:-/dev/null}"
init='{"type":"system","subtype":"init","session_id":"s1","model":"fake-model","tools":[],"mcp_servers":[]}'
taskout='{"type":"assistant","message":{"id":"m1","type":"message","role":"assistant","model":"fake-model","content":[{"type":"tool_use","id":"tu1","name":"TaskOutput","input":{"task_id":"nope","block":true}}],"usage":{"input_tokens":1,"output_tokens":1}}}'
agentbg='{"type":"assistant","message":{"id":"m0","type":"message","role":"assistant","model":"fake-model","content":[{"type":"tool_use","id":"tu0","name":"Agent","input":{"prompt":"x","run_in_background":true}}],"usage":{"input_tokens":1,"output_tokens":1}}}'
agentres='{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu0","content":"started task t1","is_error":false}]}}'
interrupted='{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"[Request interrupted by user for tool use]","is_error":true}]}}'
resultint='{"type":"result","subtype":"success","is_error":false,"result":"","num_turns":1,"duration_ms":1,"duration_api_ms":1,"session_id":"s1"}'
taskres='{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"task output","is_error":false}]}}'
final='{"type":"assistant","message":{"id":"m2","type":"message","role":"assistant","model":"fake-model","content":[{"type":"text","text":"continuing without the task; done"}],"usage":{"input_tokens":1,"output_tokens":1}}}'
resultok='{"type":"result","subtype":"success","is_error":false,"result":"done","num_turns":2,"duration_ms":1,"duration_api_ms":1,"session_id":"s1"}'

read -r first
printf '%s\n' "$first" >> "$log"
emit "$init"
case "${FAKE_CLAUDE_SCENARIO:-recover}" in
  legit)
    emit "$agentbg"; emit "$agentres"; emit "$taskout"
    sleep "${FAKE_CLAUDE_WAIT:-1}"
    emit "$taskres"; emit "$final"; emit "$resultok"
    exit 0 ;;
esac
emit "$taskout"
while read -r line; do
  printf '%s\n' "$line" >> "$log"
  case "$line" in
    *'"subtype":"interrupt"'*)
      if [ "${FAKE_CLAUDE_SCENARIO}" = "ignore-interrupt" ]; then continue; fi
      emit "$interrupted"; emit "$resultint" ;;
    *'"type":"user"'*)
      emit "$final"; emit "$resultok"; exit 0 ;;
  esac
done
exit 0
`

func writeFakeClaudeOrch(t *testing.T) (script, stdinLog string) {
	t.Helper()
	dir := t.TempDir()
	script = filepath.Join(dir, "fake-claude")
	if err := os.WriteFile(script, []byte(fakeClaudeOrch), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	return script, filepath.Join(dir, "stdin.log")
}

func orchStallEnv(t *testing.T, stall, recovery string) {
	t.Helper()
	t.Setenv("ITERION_CLAUDE_CODE_ORCH_STALL_TIMEOUT", stall)
	t.Setenv("ITERION_CLAUDE_CODE_ORCH_RECOVERY_TIMEOUT", recovery)
	t.Setenv("ITERION_CLAUDE_CODE_CLOSE_GRACE", "100ms")
	t.Setenv("ITERION_CLAUDE_CODE_CLOSE_TERM", "100ms")
}

func runFakeOrchSession(t *testing.T, scenario, wait string) (rm *claudesdk.ResultMessage, stalls []OrchestrationStall, stdin string, err error) {
	t.Helper()
	script, stdinLog := writeFakeClaudeOrch(t)
	task := Task{NodeID: "node", Iteration: 0}
	task.Hooks.OnOrchestrationStall = func(st OrchestrationStall) { stalls = append(stalls, st) }
	opts := []claudesdk.Option{
		claudesdk.WithCLIPath(script),
		claudesdk.WithEnv("FAKE_CLAUDE_SCENARIO", scenario),
		claudesdk.WithEnv("FAKE_CLAUDE_STDIN_LOG", stdinLog),
		claudesdk.WithEnv("FAKE_CLAUDE_WAIT", wait),
	}
	b := &ClaudeCodeBackend{Logger: iterlog.Nop()}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	rm, _, err = b.runSession(ctx, "do the work", task, opts)
	raw, _ := os.ReadFile(stdinLog)
	return rm, stalls, string(raw), err
}

// TestOrchStall_RecoversInPlace: a TaskOutput wait on a task the session
// never created is interrupted and nudged, and the node completes on the
// same session — no kill, no node restart (issue #691).
func TestOrchStall_RecoversInPlace(t *testing.T) {
	orchStallEnv(t, "300ms", "5s")
	rm, stalls, stdin, err := runFakeOrchSession(t, "recover", "0")
	if err != nil {
		t.Fatalf("runSession: %v", err)
	}
	if rm == nil || rm.Result == nil || *rm.Result != "done" {
		t.Fatalf("result = %+v, want the post-recovery turn's \"done\"", rm)
	}
	if len(stalls) != 1 || !stalls[0].Recovered || stalls[0].Tool != "TaskOutput" || stalls[0].Backend != BackendClaudeCode {
		t.Fatalf("stall hook = %+v, want one recovered TaskOutput stall", stalls)
	}
	interruptAt := strings.Index(stdin, `"subtype":"interrupt"`)
	nudgeAt := strings.Index(stdin, "[iterion] Your TaskOutput call was interrupted")
	if interruptAt < 0 || nudgeAt < 0 || nudgeAt < interruptAt {
		t.Fatalf("stdin must carry the interrupt then the follow-up, got:\n%s", stdin)
	}
}

// TestOrchStall_AbortsWhenTheInterruptIsIgnored: a CLI that does not close
// the turn after the interrupt is aborted on the recovery budget, with the
// retryable error text the executor keys on.
func TestOrchStall_AbortsWhenTheInterruptIsIgnored(t *testing.T) {
	orchStallEnv(t, "300ms", "300ms")
	_, stalls, stdin, err := runFakeOrchSession(t, "ignore-interrupt", "0")
	if err == nil {
		t.Fatal("runSession: want an abort, got nil")
	}
	if !strings.Contains(err.Error(), "session idle for") || !strings.Contains(err.Error(), "did not close the turn") {
		t.Fatalf("error = %v, want the retryable stall error naming the failed recovery", err)
	}
	if len(stalls) != 1 || stalls[0].Recovered {
		t.Fatalf("stall hook = %+v, want one aborted stall", stalls)
	}
	if !strings.Contains(stdin, `"subtype":"interrupt"`) {
		t.Fatalf("the interrupt must have been attempted, stdin:\n%s", stdin)
	}
}

// TestOrchStall_LegitimateWaitIsNotAStall: a TaskOutput that follows an
// Agent spawned in the background is a legitimate wait and keeps the hot
// budget — the guard must not fire on the current CLI's tool name (the
// subagent tool is `Agent`; `Task` is the pre-rename name).
func TestOrchStall_LegitimateWaitIsNotAStall(t *testing.T) {
	orchStallEnv(t, "200ms", "5s")
	rm, stalls, _, err := runFakeOrchSession(t, "legit", "0.7")
	if err != nil {
		t.Fatalf("runSession: %v (a legitimate background wait was classified as a stall)", err)
	}
	if rm == nil || rm.Result == nil || *rm.Result != "done" {
		t.Fatalf("result = %+v, want \"done\"", rm)
	}
	if len(stalls) != 0 {
		t.Fatalf("stall hook fired on a legitimate wait: %+v", stalls)
	}
}

// TestOrchStall_DisabledRecoveryAbortsImmediately: a zero recovery budget
// keeps the pre-recovery behaviour — abort on classification.
func TestOrchStall_DisabledRecoveryAbortsImmediately(t *testing.T) {
	orchStallEnv(t, "300ms", "0")
	_, stalls, stdin, err := runFakeOrchSession(t, "recover", "0")
	if err == nil || !strings.Contains(err.Error(), "session idle for") || !strings.Contains(err.Error(), "recovery disabled") {
		t.Fatalf("error = %v, want the immediate abort", err)
	}
	if len(stalls) != 1 || stalls[0].Recovered {
		t.Fatalf("stall hook = %+v, want one aborted stall", stalls)
	}
	if strings.Contains(stdin, `"subtype":"interrupt"`) {
		t.Fatalf("no interrupt may be sent when recovery is disabled, stdin:\n%s", stdin)
	}
}

func TestSpawnsBackgroundWork(t *testing.T) {
	cases := []struct {
		name  string
		block claudesdk.ToolUseBlock
		want  bool
	}{
		{"agent", claudesdk.ToolUseBlock{Name: "Agent", Input: map[string]any{"prompt": "x"}}, true},
		{"legacy task", claudesdk.ToolUseBlock{Name: "Task", Input: map[string]any{}}, true},
		{"background bash", claudesdk.ToolUseBlock{Name: "Bash", Input: map[string]any{"command": "go test ./...", "run_in_background": true}}, true},
		{"foreground bash", claudesdk.ToolUseBlock{Name: "Bash", Input: map[string]any{"command": "ls"}}, false},
		{"task output", claudesdk.ToolUseBlock{Name: "TaskOutput", Input: map[string]any{"task_id": "t", "block": true}}, false},
		{"monitor", claudesdk.ToolUseBlock{Name: "Monitor", Input: map[string]any{}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := spawnsBackgroundWork(&tc.block); got != tc.want {
				t.Fatalf("spawnsBackgroundWork(%s) = %v, want %v", tc.block.Name, got, tc.want)
			}
		})
	}
}

// The opt-in withholding knob: off by default (full native toolset), on it
// removes the orchestration surface from non-ultracode nodes only.
func TestBuildTransportOptions_OrchestrationToolsKnob(t *testing.T) {
	b := &ClaudeCodeBackend{Logger: iterlog.Nop()}
	argsFor := func(task Task) []string {
		opts, cleanup := b.buildTransportOptions(task)
		if cleanup != nil {
			cleanup()
		}
		_, args := claudesdk.ResolveSpawn(opts...)
		return args
	}
	hasDisallow := func(args []string) (string, bool) {
		for i, a := range args {
			if a == "--disallowedTools" && i+1 < len(args) {
				return args[i+1], true
			}
		}
		return "", false
	}

	t.Setenv("ITERION_CLAUDE_CODE_DISALLOW_ORCHESTRATION_TOOLS", "")
	if v, ok := hasDisallow(argsFor(Task{NodeID: "n"})); ok {
		t.Fatalf("default must keep the full toolset, got --disallowedTools %s", v)
	}

	t.Setenv("ITERION_CLAUDE_CODE_DISALLOW_ORCHESTRATION_TOOLS", "1")
	v, ok := hasDisallow(argsFor(Task{NodeID: "n"}))
	if !ok || v != "Agent,Task,TaskOutput,Monitor" {
		t.Fatalf("opt-in must withhold the whole orchestration surface, got %q (%v)", v, ok)
	}
	if v, ok := hasDisallow(argsFor(Task{NodeID: "n", Ultracode: true})); ok {
		t.Fatalf("an ultracode node keeps its orchestration surface, got --disallowedTools %s", v)
	}
}

// The stall error must stay on the executor's retryable path: an aborted
// session re-executes the node instead of failing the run outright.
func TestOrchStallError_IsIdleShaped(t *testing.T) {
	err := orchStallError(4*time.Minute, "TaskOutput", "aborting for auto-retry (in-place recovery disabled)")
	if !strings.HasPrefix(err.Error(), "claude session idle for 4m0s") {
		t.Fatalf("error = %v, want the idle prefix", err)
	}
}
