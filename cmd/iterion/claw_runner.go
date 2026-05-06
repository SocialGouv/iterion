package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/spf13/cobra"
)

// clawRunnerCmd is the hidden sub-command that runs the claw backend
// inside an iterion sandbox container.
//
// The command reads exactly one [delegate.IOTask] line from stdin,
// runs it through a minimally-configured ClawBackend (no MCP, no
// ask_user routing — see the V1 limitations in docs/sandbox.md), and
// writes one [delegate.IOResult] line to stdout. Any execution error
// is encoded into the IOResult.Error field AND surfaced via a
// non-zero exit code, so the launcher can detect protocol-level
// failures distinctly from typed-result failures.
//
// The "__" prefix marks this as an internal subcommand — hidden from
// `iterion --help`, not user-facing. The same convention is used by
// __mcp-ask-user.
//
// Phase 4 V1 scope: standard claw tool set (Bash, file edits)
// inside the container; no MCP servers; no mid-tool-loop ask_user
// resume. These limitations are documented in docs/sandbox.md and
// fall out of the "ToolDefs aren't serializable" constraint
// described on [delegate.IOTask].
var clawRunnerCmd = &cobra.Command{
	Use:    "__claw-runner",
	Short:  "Internal: run the claw backend inside an iterion sandbox container",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runClawRunner(cmd.Context(), os.Stdin, os.Stdout, os.Stderr)
	},
}

func init() {
	rootCmd.AddCommand(clawRunnerCmd)
}

// runClawRunner is the testable entry point — separated from the
// Cobra glue so tests can pipe synthetic stdin/stdout pairs without
// invoking the binary.
func runClawRunner(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) error {
	// Read exactly one IOTask from stdin. The launcher writes a
	// single line of JSON; we don't want to keep the runner alive
	// for a multi-task session because each iterion node spawn is
	// already isolated.
	var ioTask delegate.IOTask
	dec := json.NewDecoder(stdin)
	if err := dec.Decode(&ioTask); err != nil {
		writeRunnerError(stdout, fmt.Errorf("decode IOTask: %w", err))
		return err
	}

	task := delegate.FromIOTask(ioTask)
	// Sandbox is intentionally nil — we ARE the sandbox now.
	task.Sandbox = nil

	// Build a minimal ClawBackend. The registry resolves the API
	// client from the standard ITERION_*_KEY env vars, which the
	// docker driver inherits from the host (subject to the env
	// scrubbing the engine applies before container start).
	registry := model.NewRegistry()
	backend := model.NewClawBackend(registry, model.EventHooks{}, model.RetryPolicy{})

	start := time.Now()
	result, err := backend.Execute(ctx, task)
	duration := time.Since(start)
	if duration > 0 && result.Duration == 0 {
		result.Duration = duration
	}

	if err != nil {
		ior := delegate.ToIOResult(result)
		ior.Error = err.Error()
		writeRunnerOutput(stdout, ior)
		fmt.Fprintf(stderr, "iterion-claw-runner: %v\n", err)
		return err
	}

	writeRunnerOutput(stdout, delegate.ToIOResult(result))
	return nil
}

// writeRunnerOutput emits a single-line JSON-encoded IOResult on
// stdout. Failures here are silent (we already lost the channel) but
// we Flush via os.Stdout's buffered Encoder anyway.
func writeRunnerOutput(w io.Writer, r delegate.IOResult) {
	enc := json.NewEncoder(w)
	_ = enc.Encode(r)
}

// writeRunnerError is a convenience for fatal-pre-execute failures
// (decode error, registry init failure). It emits a minimal
// IOResult carrying just the Error field.
func writeRunnerError(w io.Writer, err error) {
	writeRunnerOutput(w, delegate.IOResult{Error: err.Error()})
}
