package main

import (
	"context"
	"encoding/json"
	"errors"
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
// Wire format (V2-1+): bidirectional NDJSON envelopes over stdin /
// stdout (see [delegate.Envelope]). The launcher seeds the runner
// with one [delegate.EnvelopeTask] envelope; the runner emits any
// number of intermediate envelopes (tool_call / ask_user /
// session_capture / event) and finishes with a terminal
// [delegate.EnvelopeResult]. Errors during execution are encoded into
// the result envelope's [delegate.IOResult].Error field AND surfaced
// via a non-zero exit code so the launcher can detect protocol-level
// failures distinctly from typed-result failures.
//
// V2-2: tools execute on the LAUNCHER side via the IPC. The runner
// builds proxy [delegate.ToolDef] entries whose Execute closures
// emit tool_call envelopes; the launcher's multiplexer dispatches
// to the original ToolDef (bound to the engine's tool registry, MCP
// manager, etc.) and returns the result. This unblocks the MCP-tools-
// in-sandbox path V1 couldn't support.
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

// sandboxRunnerSessionID is the runID half of the (runID, nodeID) key
// the in-runner session store uses. Fixed because each runner process
// has exactly one store and no real runID travels over the IPC wire
// (it's a launcher-side concept). The value is opaque to the host
// store — the launcher's OnSessionCapture mirrors snapshots into the
// host store under the launcher's own runID.
const sandboxRunnerSessionID = "sandbox-runner"

// runClawRunner is the testable entry point — separated from the
// Cobra glue so tests can pipe synthetic stdin/stdout pairs without
// invoking the binary.
func runClawRunner(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) error {
	dispatcher := newProxyDispatcher(stdin, stdout)

	// Pre-result phase: read envelopes synchronously until we have a
	// task. session_replay envelopes (V2-4) seed the in-runner session
	// store before the LLM loop starts. Anything else before the task
	// envelope is a protocol error.
	var (
		ioTask          delegate.IOTask
		replaySnapshots [][]byte
	)
	for {
		env, err := dispatcher.readNextEnvelope()
		if err != nil {
			return emitFatal(dispatcher, stderr, fmt.Errorf("read pre-task envelope: %w", err))
		}
		if env.Type == delegate.EnvelopeTask {
			if uerr := unmarshalTaskEnvelope(env, &ioTask); uerr != nil {
				return emitFatal(dispatcher, stderr, uerr)
			}
			break
		}
		if env.Type == delegate.EnvelopeSessionReplay {
			// V2-4: stash the snapshot; we'll load it into the local
			// store once the task envelope arrives (NodeID is the key
			// half of the store entry).
			replaySnapshots = append(replaySnapshots, append([]byte(nil), env.Data...))
			continue
		}
		return emitFatal(dispatcher, stderr, fmt.Errorf("unexpected envelope %q before task", env.Type))
	}

	// start() must run AFTER the synchronous bootstrap loop above:
	// EnvelopeReader is not goroutine-safe, so the boot loop must
	// drain the pre-task envelopes (task + optional session_replay)
	// before handing the reader to the background reader goroutine.
	dispatcher.start()

	task := delegate.FromIOTask(ioTask)
	// Sandbox is intentionally nil — we ARE the sandbox now.
	task.Sandbox = nil
	// V2-2: hydrate task.ToolDefs from the wire-form IOToolDef list,
	// wrapping each in a proxy that forwards execution to the launcher.
	task.ToolDefs = makeProxyToolDefs(ioTask.ToolDefs, dispatcher)

	// V2-4: build a local session store, seed it from any
	// session_replay snapshots the launcher sent, and wire a sink that
	// mirrors every save back across the IPC. The runner-local store
	// is required so applySessionMessages prepends the replayed prior
	// messages to the LLM's first call (preserves compaction-retry
	// semantics across the sandbox boundary).
	sessionStore := model.NewNodeSessionStore()
	for _, snap := range replaySnapshots {
		if err := sessionStore.SaveSnapshot(sandboxRunnerSessionID, ioTask.NodeID, snap); err != nil {
			fmt.Fprintf(stderr, "iterion-claw-runner: warn: decode session_replay: %v\n", err)
		}
	}
	captureSink := &dispatcherCaptureSink{dispatcher: dispatcher}
	ctx = model.WithSandboxRunnerSession(ctx, sandboxRunnerSessionID, sessionStore, captureSink)

	// Build a minimal ClawBackend. The registry resolves the API
	// client from the standard ITERION_*_KEY env vars, which the
	// sandbox driver inherits from the host (subject to the env
	// scrubbing the engine applies before container start).
	registry := model.NewRegistry()
	backend := model.NewClawBackend(registry, model.EventHooks{}, model.RetryPolicy{})

	start := time.Now()
	result, err := backend.Execute(ctx, task)
	duration := time.Since(start)
	if duration > 0 && result.Duration == 0 {
		result.Duration = duration
	}

	ioRes := delegate.ToIOResult(result)
	if err != nil {
		ioRes.Error = err.Error()
	}
	resultEnv, marshalErr := delegate.NewResultEnvelope(ioRes)
	if marshalErr != nil {
		return emitFatal(dispatcher, stderr, marshalErr)
	}
	if writeErr := dispatcher.write(resultEnv); writeErr != nil {
		// Already losing the channel — best-effort stderr report.
		fmt.Fprintf(stderr, "iterion-claw-runner: write result envelope: %v\n", writeErr)
		return writeErr
	}
	if err != nil {
		fmt.Fprintf(stderr, "iterion-claw-runner: %v\n", err)
		return err
	}
	return nil
}

// unmarshalTaskEnvelope decodes a [delegate.EnvelopeTask] envelope
// into ioTask. Wraps the JSON error with a clear protocol-level
// message so the launcher's stderr surfaces a debuggable failure.
func unmarshalTaskEnvelope(env delegate.Envelope, ioTask *delegate.IOTask) error {
	if len(env.Data) == 0 {
		return errors.New("task envelope has empty Data field")
	}
	if err := json.Unmarshal(env.Data, ioTask); err != nil {
		return fmt.Errorf("decode task envelope: %w", err)
	}
	return nil
}

// emitFatal writes a terminal result envelope carrying the given
// error and returns it. Used for protocol-level failures (decode
// errors, missing task envelope) where the runner can't continue.
func emitFatal(dispatcher *proxyDispatcher, stderr io.Writer, err error) error {
	resultEnv, marshalErr := delegate.NewResultEnvelope(delegate.IOResult{Error: err.Error()})
	if marshalErr != nil {
		fmt.Fprintf(stderr, "iterion-claw-runner: marshal fatal: %v\n", marshalErr)
		return err
	}
	if writeErr := dispatcher.write(resultEnv); writeErr != nil {
		fmt.Fprintf(stderr, "iterion-claw-runner: write fatal: %v\n", writeErr)
	}
	fmt.Fprintf(stderr, "iterion-claw-runner: %v\n", err)
	return err
}
