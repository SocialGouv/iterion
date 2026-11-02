package delegate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/backendcaps"
)

// cliAbsentProtocols are the CLI-agent backends whose image-presence cell
// reads "unwired (no image)": no image iterion publishes ships their CLI, so
// a host dispatch of one is the dispatch plane of #1643. The derivation guard
// (TestImagePresenceIsDerivedFromTheImageManifests) flips a cell the day an
// image bakes the CLI — and these witnesses then fail, which is the point:
// the refusal must lift with the measurement, not outlive it.
var cliAbsentProtocols = []CLIAgentProtocol{kimiProtocol, grokProtocol, opencodeProtocol}

// TestRefuseAbsentImageCLINamesTheBackend: on a host whose PATH cannot
// resolve the CLI, the dispatch of each unwired backend is refused BEFORE any
// spawn, with a typed error that names the backend and the executable — the
// failure the ticket measured as `exec: not found` with no cause and no cure.
func TestRefuseAbsentImageCLINamesTheBackend(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // an empty PATH: no CLI resolves here
	for _, proto := range cliAbsentProtocols {
		t.Run(proto.Name, func(t *testing.T) {
			if !backendcaps.UnservedByPublishedImages(proto.Name) {
				t.Fatalf("registry: %s is no longer unwired in every published image — lift this refusal with the measurement", proto.Name)
			}
			b := &CLIAgentBackend{Protocol: proto, Logger: testLogger()}
			res, err := b.Execute(context.Background(), Task{NodeID: "n1", UserPrompt: "hi"})
			var absent *ErrBackendCLIAbsent
			if !errors.As(err, &absent) {
				t.Fatalf("error = %T %v, want *ErrBackendCLIAbsent", err, err)
			}
			if absent.Backend != proto.Name || absent.Binary != proto.DefaultBinary || absent.NodeID != "n1" {
				t.Errorf("refusal = %+v, want backend %q, binary %q, node n1", absent, proto.Name, proto.DefaultBinary)
			}
			for _, needle := range []string{proto.Name, `"` + proto.DefaultBinary + `"`, "PATH"} {
				if !strings.Contains(err.Error(), needle) {
					t.Errorf("refusal message %q does not name %s", err.Error(), needle)
				}
			}
			if res.BackendName != proto.Name || res.ExitCode != -1 {
				t.Errorf("result = %+v, want BackendName %q and ExitCode -1", res, proto.Name)
			}
		})
	}
}

// TestRefuseAbsentImageCLISilentWhenCLIResolves: the same backend with its
// CLI on PATH is NOT refused — the gate answers the absence, never the name.
func TestRefuseAbsentImageCLISilentWhenCLIResolves(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake CLI is POSIX-only")
	}
	dir := t.TempDir()
	fake := filepath.Join(dir, "kimi")
	script := "#!/bin/sh\nprintf '%s\n' '{\"role\":\"assistant\",\"content\":\"ok\"}'\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil { // #nosec G306 — test fixture must be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	proto := CLIAgentProtocol{Name: backendcaps.Kimi, DefaultBinary: "kimi", PromptFlag: "-p", ParseOutput: parseStreamJSONText}
	b := &CLIAgentBackend{Protocol: proto, Logger: testLogger()}
	_, err := b.Execute(context.Background(), Task{NodeID: "n1", UserPrompt: "hi"})
	var absent *ErrBackendCLIAbsent
	if errors.As(err, &absent) {
		t.Fatalf("a CLI on PATH was refused: %v", err)
	}
}

// TestRefuseAbsentImageCLIKeepsAnExplicitPinUntyped: an operator-pinned
// `command:` that resolves to nothing is THEIR pin defect, not the image gap
// this refusal names — it keeps the legacy spawn error.
func TestRefuseAbsentImageCLIKeepsAnExplicitPinUntyped(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "kimi-pinned")
	b := &CLIAgentBackend{Protocol: kimiProtocol, Command: missing, Logger: testLogger()}
	_, err := b.Execute(context.Background(), Task{NodeID: "n1", UserPrompt: "hi"})
	if err == nil {
		t.Fatal("expected the missing pinned binary to fail")
	}
	var absent *ErrBackendCLIAbsent
	if errors.As(err, &absent) {
		t.Fatalf("an explicit command: pin was retyped as an image gap: %v", err)
	}
}

// TestRefuseAbsentImageCLIMeasuredAbsenceOnly: a backend the published images
// DO serve (pi) whose binary is absent from this host keeps the legacy error
// — the refusal reads the measured cell, never the bare fact of a miss.
func TestRefuseAbsentImageCLIMeasuredAbsenceOnly(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	proto := CLIAgentProtocol{Name: backendcaps.Pi, DefaultBinary: "pi-no-such-binary", PromptFlag: "-p"}
	b := &CLIAgentBackend{Protocol: proto, Logger: testLogger()}
	_, err := b.Execute(context.Background(), Task{NodeID: "n1", UserPrompt: "hi"})
	if err == nil {
		t.Fatal("expected the missing binary to fail")
	}
	var absent *ErrBackendCLIAbsent
	if errors.As(err, &absent) {
		t.Fatalf("a served backend was refused as an image gap: %v", err)
	}
}

// TestRefuseAbsentImageCLISandboxPlaneIsTheRunStartRefusals: a dispatch into
// a real sandbox driver is not judged here — the run-start admission knows
// the image, this gate cannot read the container's PATH.
func TestRefuseAbsentImageCLISandboxPlaneIsTheRunStartRefusals(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake CLI is POSIX-only")
	}
	t.Setenv("PATH", t.TempDir())
	run := &recordingRun{script: "exit 1"}
	b := &CLIAgentBackend{Protocol: opencodeProtocol, Logger: testLogger()}
	_, err := b.Execute(context.Background(), Task{NodeID: "n1", UserPrompt: "hi", Sandbox: run})
	if err == nil {
		t.Fatal("expected the in-container failure to surface")
	}
	var absent *ErrBackendCLIAbsent
	if errors.As(err, &absent) {
		t.Fatalf("a sandboxed dispatch was judged by the host gate: %v", err)
	}
	if len(run.allArgv) == 0 {
		t.Fatal("the sandboxed dispatch never reached the driver")
	}
}

// TestRefuseAbsentImageCLINoopDriverIsTheHost: the noop driver is a
// passthrough — the dispatch DOES land on this host, so the gate applies even
// though task.Sandbox is non-nil (that is what Task.Hostless encodes).
func TestRefuseAbsentImageCLINoopDriverIsTheHost(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	run := &recordingRun{driver: "noop"}
	b := &CLIAgentBackend{Protocol: grokProtocol, Logger: testLogger()}
	_, err := b.Execute(context.Background(), Task{NodeID: "n1", UserPrompt: "hi", Sandbox: run})
	var absent *ErrBackendCLIAbsent
	if !errors.As(err, &absent) {
		t.Fatalf("error = %T %v, want *ErrBackendCLIAbsent — the noop driver runs on the host", err, err)
	}
	if len(run.allArgv) != 0 {
		t.Fatal("the refusal must precede the spawn")
	}
}
