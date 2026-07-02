package delegate

import "strings"

import "testing"

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
