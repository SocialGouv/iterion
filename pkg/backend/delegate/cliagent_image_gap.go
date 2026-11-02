package delegate

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/backendcaps"
)

// ErrBackendCLIAbsent refuses the dispatch of a CLI-agent backend whose
// executable is not on this host's PATH while the backend's image-presence
// cell reads "unwired (no image)": no image iterion publishes ships the CLI
// either, so the absence is a fact about the backend's distribution, not
// about this host's setup (#1643). It carries the run's only honest answer
// for the one plane the sandbox-side admission cannot see — a HOST dispatch
// (unsandboxed, or the noop driver): the run-start refusal
// (admitSandboxImageBackends) knows the resolved sandbox image, and says
// nothing about the host a `sandbox: none` run lands on.
//
// It is deterministic — retrying cannot install a CLI — and the run-level
// classifier maps it to runtime.ErrCodeCapabilityUnsupported, the same code
// as the sandbox-side refusal of the same class.
type ErrBackendCLIAbsent struct {
	// NodeID names the node whose dispatch is refused; empty only for a
	// per-run seam with no node to name, like ErrCapabilityUnsupported.
	NodeID, Backend, Binary string
	// Remedy names the ways out, computed where the registry facts live.
	// A node with no fallback shows this text and nothing else.
	Remedy string
}

func (e *ErrBackendCLIAbsent) Error() string {
	remedy := e.Remedy
	if remedy == "" {
		remedy = "install the CLI, or pin an explicit binary with `command:`"
	}
	msg := fmt.Sprintf("backend %q has no executable here: %q is not on PATH, and no image iterion publishes ships it — %s",
		e.Backend, e.Binary, remedy)
	if e.NodeID == "" {
		return msg
	}
	return fmt.Sprintf("node %q: %s", e.NodeID, msg)
}

// refuseAbsentImageCLI is the dispatch-plane half of #1643: before a
// CLI-agent node spawns on the HOST, refuse the backends whose default CLI
// is provably absent and which no published image could serve anyway —
// kimi, grok and opencode today — instead of letting them die at
// `exec: <cli>: not found`, an explicit failure that named neither the
// cause nor the cure.
//
// Deliberately under-refusing, like the sandbox-side admission:
//
//   - an explicit binary (the node's `command:`, the protocol's host env
//     pin) resolves to something OTHER than DefaultBinary, and its absence
//     is the operator's own pin defect, not the image gap this error names.
//   - a backend whose cell is not UnwiredNoImage (claude_code, claw, codex,
//     pi — and every custom registration, whose image is the operator's)
//     keeps the legacy error: a measured absence is a fact, the absence of
//     a measurement is not.
//   - a sandboxed dispatch (task.Sandbox live on a real driver) is the
//     run-start admission's plane: the image is known there, the container's
//     PATH is not readable from here.
//   - LookPath resolving the binary ends the gate: the CLI is here, wherever
//     it came from.
//
// The gate reads the same derived registry cells as the compile warning
// (C321) and the run-start refusal, so the day an image bakes one of these
// CLIs the cell flips, this refusal lifts itself, and its witnesses fail —
// the derivation discipline is what keeps the three planes in agreement.
func refuseAbsentImageCLI(task Task, proto CLIAgentProtocol, backendName, binary string) error {
	if !task.Hostless() || binary == "" || binary != proto.DefaultBinary {
		return nil
	}
	if !backendcaps.UnservedByPublishedImages(backendName) {
		return nil
	}
	if _, err := exec.LookPath(binary); err == nil {
		return nil
	}
	pin := "`command:` on the node"
	if proto.HostBinaryEnv != "" {
		pin += fmt.Sprintf(" or %s", proto.HostBinaryEnv)
	}
	return &ErrBackendCLIAbsent{
		NodeID:  task.NodeID,
		Backend: backendName,
		Binary:  binary,
		Remedy: fmt.Sprintf(
			"install the CLI or pin its binary (%s); sandboxed, serve it from a custom image (`sandbox: { image: … }` or `build:`); or route the node to a backend the published images serve: %s",
			pin, strings.Join(backendcaps.ServedByImage(backendcaps.ImageFamilyRunner), ", ")),
	}
}
