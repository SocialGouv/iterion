package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
)

// proxyDispatcher is the runner-side counterpart to the launcher-side
// [delegate.Multiplexer]. It owns the envelope channel back to the
// launcher: a single reader goroutine consumes envelopes from stdin
// and routes them to per-correlation-ID response channels; a writer
// (mutex-protected) serialises envelopes to stdout.
//
// The proxy ToolDef closures (and V2-3's ask_user closure) call the
// dispatcher to round-trip a request → response pair across the IPC.
// Each call gets a fresh ID so concurrent tool_use blocks (parallel
// tool calls in Anthropic / OpenAI loops) don't collide.
type proxyDispatcher struct {
	reader *delegate.EnvelopeReader
	writer *delegate.EnvelopeWriter

	mu      sync.Mutex
	pending map[string]chan delegate.Envelope

	nextID atomic.Uint64

	done chan struct{} // closed AFTER failAllPending — see start()
}

// newProxyDispatcher wraps the runner's stdin/stdout pair as a
// dispatcher. Caller must invoke [proxyDispatcher.start] exactly once
// before any [proxyDispatcher.callTool] (or future [callAskUser])
// invocation.
func newProxyDispatcher(stdin io.Reader, stdout io.Writer) *proxyDispatcher {
	return &proxyDispatcher{
		reader:  delegate.NewEnvelopeReader(stdin),
		writer:  delegate.NewEnvelopeWriter(stdout),
		pending: make(map[string]chan delegate.Envelope),
		done:    make(chan struct{}),
	}
}

// readNextEnvelope is exposed to the bootstrap path (which reads the
// initial task envelope synchronously before [start] kicks in).
func (d *proxyDispatcher) readNextEnvelope() (delegate.Envelope, error) {
	return d.reader.Read()
}

// write emits env on stdout. Used by the bootstrap path (terminal
// result envelope) and the proxy ToolDef closures (tool_call
// envelopes). Concurrent-safe.
func (d *proxyDispatcher) write(env delegate.Envelope) error {
	return d.writer.Write(env)
}

// start spawns the reader goroutine. It runs until the launcher
// closes stdin (clean shutdown after the runner emits the terminal
// result envelope) or a wire-format error occurs. Either way, all
// outstanding pending channels receive a synthetic error envelope so
// in-flight proxy calls unblock with a useful message rather than
// hang.
func (d *proxyDispatcher) start() {
	go func() {
		var loopErr error
		// Drain pending channels BEFORE closing d.done so callers
		// racing on `<-d.done` see the synthetic error envelope in
		// their reply channel rather than the bare "channel closed"
		// fallback (each pending call is a buffered-1 channel that
		// failAllPending writes into under d.mu).
		defer func() {
			d.failAllPending(loopErr)
			close(d.done)
		}()
		for {
			env, err := d.reader.Read()
			if err != nil {
				if !errors.Is(err, io.EOF) {
					loopErr = err
				}
				return
			}
			d.route(env)
		}
	}()
}

// route delivers env to the channel registered for env.ID, if any.
// Envelopes without an ID, or with an unknown ID, are dropped — the
// only legitimate sources are tool_result, ask_user_answer, and
// (V2-4) session_replay; all carry the ID of their corresponding
// outbound request.
func (d *proxyDispatcher) route(env delegate.Envelope) {
	if env.ID == "" {
		return
	}
	d.mu.Lock()
	ch, ok := d.pending[env.ID]
	if ok {
		delete(d.pending, env.ID)
	}
	d.mu.Unlock()
	if ok {
		ch <- env
	}
}

// failAllPending unblocks every outstanding proxy call with a
// synthetic envelope carrying err. Used when the reader goroutine
// exits unexpectedly.
func (d *proxyDispatcher) failAllPending(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err == nil {
		err = errors.New("runner: launcher closed envelope channel")
	}
	for id, ch := range d.pending {
		// Synthetic tool_result with the error; the proxy closure
		// translates it into a Go error inside the LLM loop.
		env, _ := delegate.NewToolResultEnvelope(id, "", err.Error())
		ch <- env
		delete(d.pending, id)
	}
}

// call sends env (must have a non-empty ID) and blocks until the
// launcher's correlated response arrives, ctx is cancelled, or the
// reader goroutine exits.
func (d *proxyDispatcher) call(ctx context.Context, env delegate.Envelope) (delegate.Envelope, error) {
	if env.ID == "" {
		return delegate.Envelope{}, errors.New("dispatcher: envelope ID required")
	}
	ch := make(chan delegate.Envelope, 1)
	d.mu.Lock()
	d.pending[env.ID] = ch
	d.mu.Unlock()

	if err := d.writer.Write(env); err != nil {
		d.mu.Lock()
		delete(d.pending, env.ID)
		d.mu.Unlock()
		return delegate.Envelope{}, err
	}

	select {
	case reply := <-ch:
		return reply, nil
	case <-ctx.Done():
		d.mu.Lock()
		delete(d.pending, env.ID)
		d.mu.Unlock()
		return delegate.Envelope{}, ctx.Err()
	case <-d.done:
		// failAllPending already populated ch with a synthetic error
		// envelope; pull it so callers see the error rather than the
		// generic "channel closed" message.
		select {
		case reply := <-ch:
			return reply, nil
		default:
			return delegate.Envelope{}, errors.New("runner: envelope channel closed before reply")
		}
	}
}

// callTool issues an EnvelopeToolCall and returns the tool_result's
// (output, error) pair. Used by proxy ToolDefs to forward LLM-driven
// tool execution to the launcher.
//
// V2-3: when the tool_result envelope carries an [AskUserToolFail]
// payload, this rebuilds a typed [*delegate.ErrAskUser] so the LLM
// loop's existing pause/resume flow triggers identically to the
// unsandboxed case (without this the ask_user tool could only ever
// surface as a generic error and the engine wouldn't know to pause).
func (d *proxyDispatcher) callTool(ctx context.Context, name string, input json.RawMessage) (string, error) {
	id := "call-" + strconv.FormatUint(d.nextID.Add(1), 10)
	env, err := delegate.NewToolCallEnvelope(id, name, input)
	if err != nil {
		return "", err
	}
	reply, err := d.call(ctx, env)
	if err != nil {
		return "", fmt.Errorf("dispatcher: tool_call: %w", err)
	}
	var data delegate.ToolResultData
	if err := json.Unmarshal(reply.Data, &data); err != nil {
		return "", fmt.Errorf("dispatcher: decode tool_result: %w", err)
	}
	if data.AskUser != nil {
		return "", &delegate.ErrAskUser{
			Question:         data.AskUser.Question,
			PendingToolUseID: data.AskUser.PendingToolUseID,
			Conversation:     data.AskUser.Conversation,
			Options:          data.AskUser.Options,
			AllowFreeText:    data.AskUser.AllowFreeText,
		}
	}
	if data.Error != "" {
		return "", errors.New(data.Error)
	}
	return data.Output, nil
}

// dispatcherCaptureSink implements [model.SessionCaptureSink] by
// emitting [delegate.EnvelopeSessionCapture] envelopes through the
// proxy dispatcher's writer. V2-4 — the launcher's
// [delegate.MultiplexerHandler.OnSessionCapture] receives them and
// mirrors the snapshot into the host's nodeSessionStore so
// CompactAndRetry compacts the LATEST history rather than the
// last-known-good one.
type dispatcherCaptureSink struct {
	dispatcher *proxyDispatcher
	stderr     io.Writer
}

// Capture never fails the node, and it is not free: an in-budget snapshot
// costs a length check, while one over the budget pays a full compaction pass
// over the whole conversation on every save — measured at 45 ms for five
// megabyte-sized turns and seconds for tens of thousands of short ones, on
// the LLM loop's hot path, since a conversation only grows and every later
// save pays it again — the cost is accepted here, not hidden. A mirror that stopped mirroring is what makes
// CompactAndRetry shrink the wrong conversation, so the cost buys the host's
// copy staying current, and what it cannot buy it reports (see [gap]).
//
// The (runID, session key) pair the local store used is dropped rather than
// put on the wire: the session_capture envelope carries the snapshot alone,
// and the launcher keys the mirror from the task it sent — the host's runID
// and that task's session key are the launcher's to own.
//
// The snapshot is bounded first, by the same rule the launcher's replay
// obeys in the other direction: the store it comes from is uncapped, while
// the channel's reader fails the WHOLE channel on a line over
// [delegate.MaxEnvelopeLineBytes]. Unbounded, a conversation past the cap
// is refused on this save AND on every later one — it only grows — so the
// host would hold a stale, smaller conversation for the rest of the run.
func (s *dispatcherCaptureSink) Capture(_ string, _ string, snapshot []byte) {
	if s == nil || s.dispatcher == nil {
		return
	}
	bounded, reduced, err := model.ConstrainSessionSnapshotForWire(snapshot)
	if err != nil {
		s.gap(model.MirrorGapSnapshotRefused, len(snapshot),
			"the host's mirror of this conversation is now behind: %v", err)
		return
	}
	if reduced {
		// Not a gap: the host then holds a compacted form of the SAME
		// conversation — the shape its own retry path produces — so this
		// line stays on stderr and nothing crosses but the snapshot.
		s.warn("compacted the session snapshot to cross the channel (%d -> %d bytes)", len(snapshot), len(bounded))
	}
	if err := s.dispatcher.write(delegate.NewSessionCaptureEnvelope(bounded)); err != nil {
		s.gap(model.MirrorGapWriteRefused, len(bounded),
			"the host's mirror of this conversation is now behind: %v", err)
	}
}

// gap is [relaySessionMirrorGap] for this sink's own channels.
func (s *dispatcherCaptureSink) gap(reason string, size int, format string, args ...any) {
	relaySessionMirrorGap(s.dispatcher, s.stderr, reason, size, format, args...)
}

// warn names a diagnostic on the runner's stderr, the runner's one channel
// for something non-fatal that must not fail the node.
func (s *dispatcherCaptureSink) warn(format string, args ...any) {
	runnerWarn(s.stderr, format, args...)
}

// runnerWarn writes one non-fatal diagnostic to the runner's stderr: the
// container's own log, which this process writes without knowing what the
// launcher reads of it.
func runnerWarn(stderr io.Writer, format string, args ...any) {
	if stderr == nil {
		return
	}
	fmt.Fprintf(stderr, "iterion-claw-runner: warn: "+format+"\n", args...)
}

// relaySessionMirrorGap reports one gap in the HOST's copy of this crossing's
// conversation — a snapshot the channel's budget refuses, a write it refuses,
// a replay that did not decode — on BOTH channels this process has, because
// neither alone is enough: the relayed event is logged by the launcher
// whatever the crossing's outcome, but travels on the channel a refused
// write has just shown to be broken, while the stderr line survives that
// break as the container's own log.
//
// The event carries the reason and the size only — the launcher composes its
// own sentence from them, so the far side's own words stay in the container's
// log rather than landing in the operator's unscreened.
//
// Every gap in the host's copy goes through here: two in the capture sink,
// one on the seeded replay this runner could not decode.
func relaySessionMirrorGap(d *proxyDispatcher, stderr io.Writer, reason string, size int, format string, args ...any) {
	runnerWarn(stderr, format, args...)
	if d == nil {
		return
	}
	env, err := model.NewSessionMirrorGapEvent(reason, size)
	if err != nil {
		runnerWarn(stderr, "relaying that gap to the launcher failed too: %v", err)
		return
	}
	if err := d.write(env); err != nil {
		runnerWarn(stderr, "relaying that gap to the launcher failed too: %v — the line above is its only account", err)
	}
}

// registerSandboxLocalTools fills reg with every tool
// [tool.SandboxPlacementOf] classifies [tool.PlacementSandbox] — the ones
// that start a process, reach a filesystem or open a model-supplied URL, and
// so must run against the CONTAINER's resources rather than the launcher's.
//
// workspace is the runner's cwd (docker exec lands at the bind-mount target).
//
// bashExtraEnv is nil here on purpose: a sandboxed task carries no env
// additions (delegate.Task.ExtraEnv is host-only, and IOTask has no such
// field) — bash sees the container's environment, settled at its creation.
func registerSandboxLocalTools(reg *tool.Registry, workspace string, subagent tool.SubagentRunner) error {
	if err := tool.RegisterClawBuiltinsWithEnv(reg, workspace, nil); err != nil {
		return err
	}
	if err := tool.RegisterClawWorkspaceDiagnostics(reg, workspace, nil); err != nil {
		return err
	}
	if err := tool.RegisterClawSimple(reg); err != nil {
		return err
	}
	if err := tool.RegisterClawSkill(reg, workspace); err != nil {
		return err
	}
	// The subagent runner spawns its child conversation against THIS
	// container's tools and this container's API keys, which is what the
	// placement of `agent` asks for. A nil one still registers the tool and
	// refuses every call in the open — the shape a host that wires none
	// keeps.
	if err := tool.RegisterClawSubagents(reg, subagent); err != nil {
		return err
	}
	return tool.RegisterClawReadImage(reg)
}

// makeHybridToolDefs builds the runner's tool set, routing each tool the
// launcher advertised by its [tool.SandboxPlacement]:
//
//   - PlacementSandbox → executed here, in the container, from the local
//     registry. It MUST resolve; a miss is an error, never a proxy.
//   - PlacementLauncher → an IPC proxy: the launcher's
//     [delegate.MultiplexerHandler.OnToolCall] runs its own closure (ask_user,
//     MCP, the in-memory registries) and sends the result back.
//   - PlacementRefused (including every unclassified name) → an error.
//
// A tool handed back to the launcher executes with the launcher's process,
// cwd and filesystem, so every failure here is fatal rather than a fallback to
// the proxy. The boundary itself is held on the launcher side, which refuses
// any tool that is not launcher-placed whatever this routing asks for.
func makeHybridToolDefs(
	defs []delegate.IOToolDef,
	d *proxyDispatcher,
	workspace string,
	modelReg *model.Registry,
	hooks model.EventHooks,
) ([]delegate.ToolDef, error) {
	return buildHybridToolDefs(defs, d, workspace, sandboxLocalToolRegistrar(modelReg, hooks))
}

// sandboxLocalToolRegistrar closes the in-container registration over what a
// child conversation needs: the model registry that resolves this container's
// API clients, and the hooks that relay the child's turns to the launcher so
// a sandboxed delegation is metered and auditable like an unsandboxed one.
//
// The subagent runner is built per registration, over the very registry it is
// registered into: a child spawned in here draws from the container's own
// tools when no parent node bounds it, and from the node's effective set when
// one does (model.ParentLoop, which the in-container Execute fills from the
// forwarded task). Lifecycle hooks are nil, as they are for this runner's own
// loop — the launcher owns that layer.
func sandboxLocalToolRegistrar(modelReg *model.Registry, hooks model.EventHooks) func(*tool.Registry, string) error {
	return func(reg *tool.Registry, workspace string) error {
		return registerSandboxLocalTools(reg, workspace,
			model.NewSubagentRunner(modelReg, reg, hooks, nil, ""))
	}
}

// buildHybridToolDefs is makeHybridToolDefs with the local registration
// injected, so a test can drive the registration-failure and
// missing-local-tool branches without a container.
func buildHybridToolDefs(
	defs []delegate.IOToolDef,
	d *proxyDispatcher,
	workspace string,
	register func(*tool.Registry, string) error,
) ([]delegate.ToolDef, error) {
	if len(defs) == 0 {
		return nil, nil
	}
	localReg := tool.NewRegistry()
	if err := register(localReg, workspace); err != nil {
		return nil, fmt.Errorf("register the in-container tool set (workspace=%q): %w", workspace, err)
	}
	out := make([]delegate.ToolDef, len(defs))
	for i, td := range defs {
		name := td.Name
		placement, reason := tool.SandboxPlacementOf(name)
		switch placement {
		case tool.PlacementSandbox:
			local, err := localReg.Resolve(name)
			if err != nil || local == nil || local.Origin.Kind != tool.OriginBuiltin {
				return nil, fmt.Errorf(
					"tool %q must execute inside the sandbox (%s) but the runner has no local registration for it — "+
						"proxying it would run it on the launcher's host", name, reason)
			}
			localExec := local.Execute
			out[i] = delegate.ToolDef{
				Name:        name,
				Description: td.Description, // keep launcher's description verbatim — it carries any per-recipe wording
				InputSchema: td.InputSchema, // and the launcher's schema is the contract the LLM was prompted against
				Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
					return localExec(ctx, input)
				},
			}
		case tool.PlacementLauncher:
			out[i] = delegate.ToolDef{
				Name:        name,
				Description: td.Description,
				InputSchema: td.InputSchema,
				Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
					return d.callTool(ctx, name, input)
				},
			}
		default:
			return nil, fmt.Errorf(
				"tool %q cannot be executed by a sandboxed runner (%s) — "+
					"drop it, or run the workflow unsandboxed (`sandbox: none` / `--sandbox none`)", name, reason)
		}
	}
	return out, nil
}
