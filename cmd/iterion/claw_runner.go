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
	"github.com/SocialGouv/iterion/pkg/backend/modelroute"
	"github.com/SocialGouv/iterion/pkg/backend/permission"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
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
// Tools are routed by [tool.SandboxPlacementOf]: the ones that act on a
// filesystem, a process or a model-supplied URL execute here, in the
// container; the launcher-owned ones (ask_user, MCP, the in-memory
// registries) are proxied — their Execute closures emit tool_call
// envelopes that the launcher's multiplexer dispatches to its own
// ToolDef, after its own placement and permission checks.
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
		sawGateway      bool
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
		if env.Type == delegate.EnvelopeGatewayV1 {
			// Empty capability marker: this host knows the gateway
			// factory. Values never ride this envelope — the forwarded
			// env carries them.
			sawGateway = true
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

	// The runner-side twin of the fail-closed position: a gateway task
	// that arrives WITHOUT the marker comes from a host that predates the
	// gateway — its registry would have no gateway factory either, and the
	// failure would be an opaque unknown-provider. Name the skew instead.
	if modelroute.Parse(ioTask.Model).Gateway() && !sawGateway {
		return emitFatal(dispatcher, stderr, fmt.Errorf("gateway task %q received without the gateway capability marker — the host binary predates gateway support; upgrade the host", ioTask.Model))
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
	// Every tool that starts a process, reaches a filesystem or opens a
	// model-supplied URL executes LOCALLY inside the runner, so its effects
	// land on the sandbox bind-mount and its requests leave through the
	// container's network. Only launcher-owned tools (MCP, ask_user, the
	// in-memory registries) IPC-proxy back; anything a sandboxed runner
	// cannot honour is refused here rather than silently executed on the
	// host — see [tool.SandboxPlacementOf].
	//
	// Without this split, a sandboxed `bash ls` from an LLM ran in the
	// launcher process's cwd — typically a wholly unrelated host
	// directory — and recipe runs that wrote files (commit_changes
	// downstream, fix_after_upgrade scaffolding, etc.) clobbered the
	// operator's environment instead of the run worktree. Worst observed
	// case: a `git clone` issued by an LLM "fixer" wiped the host cwd
	// (see SESSION-CONTINUITY trash post-mortem).
	workspace, _ := os.Getwd() // docker exec --workdir lands us at the
	// bind-mount target (default /workspace); fall back gracefully when
	// Getwd fails — bash/read_file relative paths still work via cwd.
	toolDefs, toolErr := makeHybridToolDefs(ioTask.ToolDefs, dispatcher, workspace)
	if toolErr != nil {
		return emitFatal(dispatcher, stderr, fmt.Errorf("build the in-container tool set: %w", toolErr))
	}
	task.ToolDefs = toolDefs

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
	// hooks relay what this loop observes to the launcher, which re-fires
	// them through its own hooks — what makes a sandboxed claw node
	// metered, auditable and forkable like an in-process one. The logger
	// carries the warnings EventHooks has no channel for (settings-hooks
	// diagnostics, subscription-spend notices) to stderr, which the
	// launcher captures into the node's error when the run fails. The
	// retry budget is the launcher's, carried by the task: this backend's
	// loop retries the provider calls made in here, as the host's claw
	// backend retries them for an unsandboxed node, and the budget bounds
	// billed attempts — so it is never read from this process's env, where
	// a variable a repository's devcontainer declares would set it.
	registry := model.NewRegistry()
	backend := model.NewClawBackend(registry, relayEventHooks(dispatcher, stderr), model.RetryPolicyFromWire(ioTask.Retry),
		model.WithClawLogger(iterlog.NewFromEnv(stderr)))

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
// backend: every observation of the in-container loop — its LLM steps,
// the tools it executes here, its retries and compactions, its per-turn
// fork anchors — crosses the IPC as an `event` envelope on the
// dispatcher's writer (see [model.SandboxRelayHooks]). A failed write is
// reported on stderr, which the launcher captures into the node's error
// when the channel is dead.
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
