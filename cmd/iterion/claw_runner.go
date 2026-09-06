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
	"github.com/SocialGouv/iterion/pkg/backend/permission"
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
		policyCfg       *permission.PolicyConfig
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
		if env.Type == delegate.EnvelopePermissionPolicy {
			// The node's permission gate, in serialisable form. A
			// malformed payload is fatal BEFORE any model call: a gate
			// the author declared must never silently not exist.
			var cfg permission.PolicyConfig
			if uerr := json.Unmarshal(env.Data, &cfg); uerr != nil {
				return emitFatal(dispatcher, stderr, fmt.Errorf("decode permission_policy envelope: %w", uerr))
			}
			policyCfg = &cfg
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
	// Rebuild the permission gate the launcher shipped pre-task, through
	// the same parser it was authored against. From here the ordinary
	// unsandboxed Execute path applies it (opts.Permission), so builtins
	// running locally in this container and proxied tools alike hit the
	// same gate as an unsandboxed run.
	if policyCfg != nil {
		pol, perr := permission.NewPolicyFromConfig(*policyCfg)
		if perr != nil {
			return emitFatal(dispatcher, stderr, fmt.Errorf("rebuild permission policy: %w", perr))
		}
		task.Permission = pol
	}
	// V2-2 (refined): builtins (bash, read_file, glob, grep, file_edit,
	// web_fetch, write_file) execute LOCALLY inside the runner so their
	// filesystem effects land on the sandbox bind-mount, not on the
	// launcher's host cwd. Everything else (MCP tools, ask_user, custom
	// engine-side tools) still IPC-proxies back to the launcher.
	//
	// Without this hybrid, a sandboxed `bash ls` from an LLM ran in the
	// launcher process's cwd — typically a wholly unrelated host
	// directory — and recipe runs that wrote files (commit_changes
	// downstream, fix_after_upgrade scaffolding, etc.) clobbered the
	// operator's environment instead of the run worktree. Worst observed
	// case: a `git clone` issued by an LLM "fixer" wiped the host cwd
	// (see SESSION-CONTINUITY trash post-mortem).
	workspace, _ := os.Getwd() // docker exec --workdir lands us at the
	// bind-mount target (default /workspace); fall back gracefully when
	// Getwd fails — bash/read_file relative paths still work via cwd.
	task.ToolDefs = makeHybridToolDefs(ioTask.ToolDefs, dispatcher, workspace, stderr)

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
	// scrubbing the engine applies before container start). Its event
	// hooks relay the per-step LLM observations to the launcher, which
	// re-fires them through its own hooks — what meters a sandboxed claw
	// node like an in-process one.
	registry := model.NewRegistry()
	backend := model.NewClawBackend(registry, relayEventHooks(dispatcher, stderr), model.RetryPolicy{})

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

// relayEventHooks builds the event hooks the runner installs on its claw
// backend: llm_request and llm_step_finished cross the IPC as `event`
// envelopes on the dispatcher's writer (see [model.SandboxRelayHooks]). A
// failed write is reported on stderr, which the launcher captures into the
// node's error when the channel is dead.
func relayEventHooks(d *proxyDispatcher, stderr io.Writer) model.EventHooks {
	return model.SandboxRelayHooks(d.write, func(err error) {
		fmt.Fprintf(stderr, "iterion-claw-runner: %v\n", err)
	})
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
