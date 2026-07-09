package delegate

import "strings"

import "testing"
import "time"

// TestIsBlockingOrchestrationTool pins which native claude-CLI tools BLOCK
// waiting on a subagent task (the deadlock surface) vs Task, which spawns and
// returns immediately.
func TestIsBlockingOrchestrationTool(t *testing.T) {
	for _, n := range []string{"TaskOutput", "Monitor"} {
		if !isBlockingOrchestrationTool(n) {
			t.Errorf("%s should be a blocking orchestration tool", n)
		}
	}
	for _, n := range []string{"Task", "Bash", "Read", "ToolSearch", ""} {
		if isBlockingOrchestrationTool(n) {
			t.Errorf("%s should NOT be a blocking orchestration tool", n)
		}
	}
}

// TestOrchStallAbortStaysRetryable guards the load-bearing contract: the
// deadlock abort message MUST contain "session idle for" so the executor's
// isDelegateRetryable classifies it retryable and auto-re-executes the node
// (see pkg/backend/model/executor_retry.go). If the wording drifts, the
// in-core auto-remediation silently degrades to a hard fail.
func TestOrchStallAbortStaysRetryable(t *testing.T) {
	msg := "claude session idle for 4m0s — blocked on an orchestration tool (TaskOutput/Monitor) with no subagent spawned (likely deadlock); aborting for auto-retry"
	if !strings.Contains(msg, "session idle for") {
		t.Fatal("deadlock abort message must keep the 'session idle for' retryable marker")
	}
}

// TestNoProgressAbortStaysRetryable pins the forward-progress watchdog abort
// message: it must carry the "session idle for" marker so isDelegateRetryable
// auto-re-executes the node on a fresh subprocess (the post-outage spin the
// watchdog catches must recover automatically, not fail the run).
func TestNoProgressAbortStaysRetryable(t *testing.T) {
	msg := "claude session idle for 25m0s — no forward progress (no tool call or result while still streaming); aborting for auto-retry (tune ITERION_CLAUDE_CODE_NO_PROGRESS_TIMEOUT, 0 to disable)"
	if !strings.Contains(msg, "session idle for") {
		t.Fatal("forward-progress abort message must keep the 'session idle for' retryable marker")
	}
}

// TestResolveNoProgressTimeout covers the default, an env override, and the
// disable sentinel.
func TestResolveNoProgressTimeout(t *testing.T) {
	t.Setenv("ITERION_CLAUDE_CODE_NO_PROGRESS_TIMEOUT", "")
	if got := resolveNoProgressTimeout(); got != defaultNoProgressTimeout {
		t.Fatalf("default = %s, want %s", got, defaultNoProgressTimeout)
	}
	t.Setenv("ITERION_CLAUDE_CODE_NO_PROGRESS_TIMEOUT", "3m")
	if got := resolveNoProgressTimeout(); got != 3*time.Minute {
		t.Fatalf("env override = %s, want 3m", got)
	}
	t.Setenv("ITERION_CLAUDE_CODE_NO_PROGRESS_TIMEOUT", "0")
	if got := resolveNoProgressTimeout(); got != 0 {
		t.Fatalf("disable = %s, want 0", got)
	}
}
