package runner

import (
	"fmt"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/mcp"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runtime/recovery"
)

// TestClassify_TrustFloorRefusalsAreNotRedelivered.
//
// Every typed refusal of the workspace-trust floor is decided by the run's own
// trust and program: a redelivery meets the same answer, after paying a pod and
// a clone of an outsider's tree. Each is acked, wrapped the way it reaches the
// classifier; an ordinary failure is the control that still retries.
func TestClassify_TrustFloorRefusalsAreNotRedelivered(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
	}{
		{"an MCP server the workflow activates is withdrawn", fmt.Errorf("runner: resolve MCP servers for r: %w", fmt.Errorf("%w: plug", mcp.ErrWithdrawnMCPServer))},
		{"a backend with no floor", fmt.Errorf("node a: %w", delegate.ErrUntrustedWorkspaceRefused)},
		{"a host capability the node declares, withdrawn", engineFailure(fmt.Errorf("tool node: %w", tool.ErrHostCapabilityWithdrawn))},
	} {
		out := classifyExecResult(c.err, "run-1")
		if out.action != actionAck || out.finalStatus != "untrusted_workspace_refused" {
			t.Errorf("%s: action=%v status=%q, want an ack as untrusted_workspace_refused", c.name, out.action, out.finalStatus)
		}
		if bankableStatus(out.finalStatus) {
			t.Errorf("%s: banked — an untrusted run's work is not pushed", c.name)
		}
	}
	if out := classifyExecResult(fmt.Errorf("transient network blip"), "run-1"); out.action == actionAck && out.finalStatus == "untrusted_workspace_refused" {
		t.Error("an ordinary failure was classified as a trust refusal — the rows above prove nothing")
	}
}

// TestClassify_AHostCapabilityTheProgramNeverDeclaredIsItsOwnFault.
//
// A tool node naming a host tool its workflow does not grant is refused on
// every trust, every attempt: the program is at fault, not the run's trust.
// It fails as the engine codes it (TOOL_FAILED_PERMANENT, through the one
// recovery classification every host wires) and the runner acks it as the
// deterministic failure it is — banked like one, so a trusted campaign that
// hits it keeps the commits it made on the way.
func TestClassify_AHostCapabilityTheProgramNeverDeclaredIsItsOwnFault(t *testing.T) {
	out := classifyExecResult(engineFailure(fmt.Errorf("tool node: %w", tool.ErrHostCapabilityNotHeld)), "run-1")
	if out.action != actionAck || out.finalStatus != "deterministic_failure" {
		t.Fatalf("action=%v status=%q, want an ack as deterministic_failure", out.action, out.finalStatus)
	}
	if !bankableStatus(out.finalStatus) {
		t.Error("not banked: a trusted run's in-stride commits would die with the pod")
	}
}

// engineFailure is the shape a node failure reaches the runner in: the
// engine's RuntimeError, coded by the recovery classification the runner
// wires, the executor's error as its cause.
func engineFailure(execErr error) error {
	return &runtime.RuntimeError{Code: recovery.Classify(execErr), NodeID: "plant", Message: "node \"plant\" execution failed: " + execErr.Error(), Cause: execErr}
}
