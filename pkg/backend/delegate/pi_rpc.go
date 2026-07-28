package delegate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/cost"
	"github.com/SocialGouv/iterion/pkg/backend/delegate/piext"
	"github.com/SocialGouv/iterion/pkg/backend/delegate/pisdk"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// PiRPCBackend drives `pi --mode rpc` — a long-lived process iterion talks to
// over JSONL instead of a one-shot invocation.
//
// The transport is what unlocks everything print mode cannot do, and each item
// is otherwise unavailable on ANY iterion CLI backend:
//
//   - **Tool events.** pi emits tool_execution_start/end natively, so
//     TaskHooks fire and the studio timeline and files-touched panel light up.
//   - **Real steering.** `steer` is a first-class command; claude_code fakes
//     the same thing with a PostToolUse AdditionalContext plus a Stop-blocking
//     hook.
//   - **Authoritative accounting.** get_session_stats replaces per-message
//     accumulation, and carries context usage.
//   - **A pre-flight handshake.** get_state reveals the resolved model and
//     session id in ~200ms, before a token is spent, so a bad `--model` fails
//     fast instead of after a full process start and a wasted API call.
//   - **Abort on cancellation**, rather than killing a process mid-call.
//
// Every mapping (model, effort, argv, credentials) is shared verbatim with
// print mode, so the two transports are observationally identical apart from
// fidelity — which is why they share one backend name. See ADR-085.
type PiRPCBackend struct {
	// Command overrides the default `pi` binary.
	Command string
	// ExtraArgs are appended to every invocation. This is the seam the
	// iterion pi extension loads through (`-e <path>`), since the capabilities
	// pi lacks natively — permission gate, ask_user, board tools, MCP — all
	// arrive as one extension rather than as flags.
	ExtraArgs []string
	Logger    *iterlog.Logger
}

// Stream timers, mirroring the claude_code stream guards with pi-scoped env
// names so an operator can tune the two independently.
var (
	piColdTimeout       = envDuration("ITERION_PI_STREAM_COLD_TIMEOUT", 90*time.Second)
	piIdleTimeout       = envDuration("ITERION_PI_STREAM_IDLE_TIMEOUT", 15*time.Minute)
	piNoProgressTimeout = envDuration("ITERION_PI_NO_PROGRESS_TIMEOUT", 25*time.Minute)
	piSettleGrace       = envDuration("ITERION_PI_SETTLE_GRACE", 30*time.Second)
)

func envDuration(key string, def time.Duration) time.Duration {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}

// Execute runs one node through a live pi session.
func (b *PiRPCBackend) Execute(ctx context.Context, task Task) (Result, error) {
	if task.WorkDir != "" {
		if err := validateWorkDir(task.WorkDir, task.BaseDir); err != nil {
			return Result{}, err
		}
	}

	binary := b.Command
	if binary == "" {
		binary = "pi"
	}

	systemPrompt := task.BuildSystemPrompt()
	promptFile, cleanupPrompt, err := piWriteSystemPrompt(task, systemPrompt)
	if err != nil {
		return Result{BackendName: BackendPi, ExitCode: -1}, err
	}
	defer cleanupPrompt()

	argv := append(piRPCArgs(task, promptFile), b.ExtraArgs...)

	// The iterion extension supplies what pi has no native surface for — today
	// the permission gate. Loaded via `-e`, which bypasses pi's project-trust
	// gate (a `.pi/extensions/` drop would silently never load in a headless
	// run, and never say so).
	if extPath, cleanupExt, extErr := piext.Materialise(task.WorkDir); extErr != nil {
		// A permission-gated node without its gate is a false sense of
		// security, so that specific combination fails rather than degrades.
		if task.Permission.Enabled() {
			return Result{BackendName: BackendPi, ExitCode: -1},
				fmt.Errorf("pi backend: this node declares a permission gate but the iterion "+
					"extension could not be installed, which would leave the gate INACTIVE: %w", extErr)
		}
		if b.Logger != nil {
			b.Logger.Warn("[%s#%d/%s] iterion extension unavailable (%v); "+
				"iterion-specific capabilities are inactive for this node",
				task.NodeID, task.Iteration, BackendPi, extErr)
		}
	} else {
		defer cleanupExt()
		argv = append(argv, "-e", extPath)
	}

	collector := &piCollector{task: task, logger: b.Logger, settled: make(chan struct{})}
	client := pisdk.NewClient(pisdk.ClientOptions{
		Binary:  binary,
		Args:    argv,
		Dir:     task.WorkDir,
		Env:     piRPCEnv(ctx, task),
		Spawn:   b.spawner(task, binary),
		OnEvent: collector.onEvent,
		OnStderr: func(line string) {
			if b.Logger != nil {
				b.Logger.Info("[%s#%d/%s:err] %s", task.NodeID, task.Iteration, BackendPi, line)
			}
		},
		OnUIRequest: func(req pisdk.UIRequest) *pisdk.UIResponse {
			return b.handleUIRequest(task, collector, req)
		},
	})

	start := time.Now()
	if err := client.Start(ctx); err != nil {
		return Result{BackendName: BackendPi, ExitCode: -1, Duration: time.Since(start)}, err
	}
	defer func() { _ = client.Close() }()

	result := Result{BackendName: BackendPi}

	// Handshake. A successful get_state proves the JSONL loop is up AND fills
	// in the model/session/context-window before any tokens are spent.
	handshakeCtx, cancelHandshake := context.WithTimeout(ctx, piColdTimeout)
	state, err := client.GetState(handshakeCtx)
	cancelHandshake()
	if err != nil {
		result.Duration = time.Since(start)
		result.Stderr = truncate(client.Stderr(), 8192)
		return result, &ErrTransient{
			Provider: BackendPi,
			Reason:   "handshake failed",
			Detail:   err.Error(),
		}
	}
	result.SessionID = state.SessionID
	if state.Model != nil {
		result.EffectiveModel = state.Model.ID
		result.ContextWindow = state.Model.ContextWindow
	}
	b.warnOnModelDrift(task, result.EffectiveModel)

	// iterion owns retry policy. pi's own loop hides upstream failures from
	// the executor's classifier and, because only the final attempt's
	// transcript survives, silently under-reports what was billed.
	if err := client.SetAutoRetry(ctx, false); err != nil && b.Logger != nil {
		b.Logger.Warn("[%s#%d/%s] could not disable pi's own retry loop: %v",
			task.NodeID, task.Iteration, BackendPi, err)
	}

	if err := client.Prompt(ctx, task.UserPrompt); err != nil {
		result.Duration = time.Since(start)
		result.Stderr = truncate(client.Stderr(), 8192)
		return result, &ErrTransient{Provider: BackendPi, Reason: "prompt rejected", Detail: err.Error()}
	}

	// The prompt reply meant "accepted". Completion is agent_settled.
	waitErr := b.awaitSettle(ctx, client, collector, task)
	result.Duration = time.Since(start)
	result.Stderr = truncate(client.Stderr(), 8192)

	// Accounting: get_session_stats is authoritative and supersedes the
	// per-message accumulation the collector did as a fallback.
	inTok, outTok, costUSD, peak, thinkTok := collector.usage()
	statsCtx, cancelStats := context.WithTimeout(ctx, 10*time.Second)
	if stats, err := client.SessionStats(statsCtx); err == nil {
		// Cache reads/writes stay OUT of the billed token count, matching
		// claude_code (which sums Usage.InputTokens + OutputTokens and routes
		// input+cache_creation+cache_read to the context gauge instead). Every
		// backend has to agree here or a workflow's `max_tokens` budget would
		// mean something different depending on which one ran the node.
		inTok = stats.Tokens.Input
		outTok = stats.Tokens.Output
		costUSD = stats.Cost
		// Context load — input plus everything served from or written to the
		// cache — is the quantity the context-usage gauge tracks.
		if loaded := stats.Tokens.Input + stats.Tokens.CacheRead + stats.Tokens.CacheWrite; loaded > peak {
			peak = loaded
		}
		if stats.ContextUsage != nil {
			if stats.ContextUsage.Tokens > peak {
				peak = stats.ContextUsage.Tokens
			}
			if stats.ContextUsage.ContextWindow > 0 {
				result.ContextWindow = stats.ContextUsage.ContextWindow
			}
		}
	}
	cancelStats()

	result.Tokens = inTok + outTok
	result.ThinkingTokens = thinkTok
	result.PeakInputTokens = peak

	text := collector.text()
	output, rawLen, fallback := parseSDKOutput(&text, nil, task.OutputSchema)
	result.Output = output
	result.RawOutputLen = rawLen
	result.ParseFallback = fallback
	cost.AnnotateWithUSD(result.Output, task.Model, inTok, outTok, costUSD)

	for _, notice := range collector.notices() {
		if b.Logger != nil {
			b.Logger.Warn("[%s#%d/%s] %s", task.NodeID, task.Iteration, BackendPi, notice)
		}
	}

	// A suspension outranks everything else: the run is pausing for a human,
	// not failing. The partial transcript and accounting above are still
	// returned so the operator sees what the agent did before it asked.
	if pause := collector.pending(); pause != nil {
		return result, pause.err()
	}
	if waitErr != nil {
		return result, waitErr
	}
	if err := collector.failure(); err != nil {
		return result, err
	}

	if task.Hooks.OnTurnFinished != nil {
		task.Hooks.OnTurnFinished(TurnFinishedInfo{
			SessionID:    result.SessionID,
			FinishReason: collector.stopReason(),
			Text:         text,
			InputTokens:  inTok,
			OutputTokens: outTok,
		})
	}
	return result, nil
}

// warnOnModelDrift surfaces pi resolving a different model than requested. pi
// fuzzy-matches an unknown pattern against its catalogue rather than failing,
// so a typo silently runs something else.
func (b *PiRPCBackend) warnOnModelDrift(task Task, effective string) {
	if b.Logger == nil || effective == "" || task.Model == "" {
		return
	}
	if !sameModelID(effective, task.Model) {
		b.Logger.Warn("[%s#%d/%s] requested model %q resolved to %q",
			task.NodeID, task.Iteration, BackendPi, task.Model, effective)
	}
}

// spawner routes the process into the run's container when sandboxed.
//
// The pidfile wrapper matters more here than in print mode: an RPC pi lives
// forever by design (its mode returns a never-resolving promise), so a leaked
// in-container process would hold a model session indefinitely. Killing the
// host-side `docker exec` client has no signal path to the exec'd process.
func (b *PiRPCBackend) spawner(task Task, binary string) pisdk.Spawner {
	if task.Sandbox == nil {
		return nil
	}
	return func(ctx context.Context, argv []string) *exec.Cmd {
		full := append([]string{binary}, argv...)
		mark := sandboxDelegateMark(task)
		return task.Sandbox.Command(ctx, wrapSandboxDelegateArgv(mark, full), sandbox.ExecOpts{
			// The container's environment is ONLY what we pass here, so the
			// host's provider credentials must be forwarded by name.
			Env:     piSandboxEnv(ctx, task),
			WorkDir: task.WorkDir,
			// Mandatory: the docker/k8s drivers only allocate a forwarded
			// stdin when Stdin or KeepStdinOpen is set, and an RPC session is
			// nothing but a conversation over stdin.
			KeepStdinOpen: true,
		})
	}
}

// awaitSettle blocks until the turn settles, or a guard trips.
func (b *PiRPCBackend) awaitSettle(ctx context.Context, client *pisdk.Client, collector *piCollector, task Task) error {
	cold := time.NewTimer(piColdTimeout)
	defer cold.Stop()
	poll := time.NewTicker(2 * time.Second)
	defer poll.Stop()
	pauseTick := time.NewTicker(200 * time.Millisecond)
	defer pauseTick.Stop()

	sawFirstEvent := false

	abortAndGrace := func() {
		abortCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = client.Abort(abortCtx)
		cancel()
		select {
		case <-collector.settled:
		case <-time.After(piSettleGrace):
		}
	}

	for {
		select {
		case <-collector.settled:
			return nil

		case <-pauseTick.C:
			// A raised suspension will not settle on its own: the agent is
			// blocked on an answer that only a human can give. Abort so the
			// turn unwinds and Execute can surface the pause.
			if collector.pending() != nil {
				abortAndGrace()
				return nil
			}

		case <-ctx.Done():
			// Ask pi to stop rather than killing it mid-call: an aborted turn
			// still flushes its transcript, so the partial result survives.
			abortAndGrace()
			return ctx.Err()

		case <-cold.C:
			if !sawFirstEvent {
				abortAndGrace()
				return &ErrTransient{
					Provider: BackendPi,
					Reason:   "no output",
					Detail:   fmt.Sprintf("pi produced no event within %s", piColdTimeout),
				}
			}

		case <-poll.C:
			last, lastProgress := collector.marks()
			if !sawFirstEvent && !last.IsZero() {
				sawFirstEvent = true
			}
			// Drain the operator inbox at a safe boundary: pi delivers a
			// steered message at the agent's next turn.
			b.drainInbox(ctx, client, task)

			if sawFirstEvent && time.Since(last) > piIdleTimeout {
				abortAndGrace()
				return &ErrTransient{
					Provider: BackendPi,
					Reason:   "stream idle",
					Detail:   fmt.Sprintf("no event for %s", piIdleTimeout),
				}
			}
			// No-progress catches the pathology idle cannot: a model looping
			// on streaming deltas without ever finishing a message.
			if sawFirstEvent && time.Since(lastProgress) > piNoProgressTimeout {
				abortAndGrace()
				return &ErrTransient{
					Provider: BackendPi,
					Reason:   "no progress",
					Detail:   fmt.Sprintf("no completed message or tool call for %s", piNoProgressTimeout),
				}
			}
		}
	}
}

// drainInbox forwards operator-typed messages into the live session.
func (b *PiRPCBackend) drainInbox(ctx context.Context, client *pisdk.Client, task Task) {
	if task.InboxDrain == nil {
		return
	}
	for _, msg := range task.InboxDrain() {
		if strings.TrimSpace(msg) == "" {
			continue
		}
		if err := client.Steer(ctx, msg); err != nil {
			if b.Logger != nil {
				b.Logger.Warn("[%s#%d/%s] steering message dropped: %v",
					task.NodeID, task.Iteration, BackendPi, err)
			}
			return
		}
		if b.Logger != nil {
			b.Logger.Info("[%s#%d/%s] steered: %s", task.NodeID, task.Iteration, BackendPi, truncate(msg, 200))
		}
	}
}

// piRPCArgs builds the argv for RPC mode: the shared per-task flags, minus the
// print-mode output selection that pisdk supplies itself.
func piRPCArgs(task Task, promptFile string) []string {
	var args []string
	for i := 0; i < len(piProtocol.ExtraArgs); i++ {
		if piProtocol.ExtraArgs[i] == "--mode" {
			i++ // skip the mode's value too; pisdk emits `--mode rpc`
			continue
		}
		args = append(args, piProtocol.ExtraArgs[i])
	}
	if promptFile != "" {
		args = append(args, piProtocol.SystemPromptFlag, promptFile)
	}
	if task.ReasoningEffort != "" {
		args = append(args, piMapEffort(task.ReasoningEffort)...)
	}
	return append(args, piExtraArgsFor(task)...)
}

// piRPCEnv assembles the child environment: the host's, plus the shared
// credential overrides, plus run-level provisioning (devbox PATH) last so it
// wins on a duplicate key.
func piRPCEnv(ctx context.Context, task Task) []string {
	env := os.Environ()
	for k, v := range piResolveEnv(ctx) {
		env = append(env, k+"="+v)
	}
	for k, v := range piExtensionEnv(task) {
		env = append(env, k+"="+v)
	}
	return append(env, task.ExtraEnv...)
}

// piExtensionEnv is the whole configuration surface of the iterion extension.
//
// Every variable is optional by contract: absent means the capability is off
// and the extension degrades to a no-op, so a run never fails because iterion
// passed one fewer variable than the extension build expected. Secret VALUES
// never appear here.
func piExtensionEnv(task Task) map[string]string {
	env := map[string]string{
		"ITERION_PI_CONTRACT":  piext.ContractVersion,
		"ITERION_PI_CTRL":      "rpc",
		"ITERION_PI_ITERATION": strconv.Itoa(task.Iteration),
	}
	if task.NodeID != "" {
		env["ITERION_PI_NODE_ID"] = task.NodeID
	}
	// Set only when a gate is actually configured, so the extension registers
	// no hook — and the node pays no per-tool-call round-trip — otherwise.
	if task.Permission.Enabled() {
		env["ITERION_PI_PERMISSION"] = task.Permission.Mode.String()
	}
	// Likewise for ask_user: a node that cannot reach a human should not be
	// offered a tool that pauses the run, or it will call it and stall.
	if task.InteractionEnabled {
		env["ITERION_PI_INTERACTION"] = "sync"
	}
	if servers := piMCPServers(task); len(servers) > 0 {
		if raw, err := json.Marshal(servers); err == nil {
			env["ITERION_PI_MCP_SERVERS"] = string(raw)
		}
	}
	return env
}

// piMCPServerSpec is one MCP server the extension bridges onto pi.
type piMCPServerSpec struct {
	Name    string            `json:"name"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

// piMCPServers lists the MCP servers this node should reach.
//
// pi has no MCP client, so every one of iterion's MCP surfaces is invisible to
// it until the extension bridges them. Today that is the board; workflow-declared
// `mcp_server:` blocks join the same list once non-HTTP transports land.
//
// The board is included only when the run actually has capabilities AND the
// HTTP endpoint is wired. Registering it otherwise would hand the agent tools
// that fail on every call, which is worse than not having them: the model
// burns turns discovering they do not work.
//
// This is also the ONLY place a token appears in the extension's configuration
// — inside a server's headers, never as a standalone variable — so a generic
// environment dump cannot log it.
func piMCPServers(task Task) []piMCPServerSpec {
	var out []piMCPServerSpec
	if len(task.Capabilities) > 0 && task.BoardHTTPEndpoint != "" && task.BoardRunToken != "" {
		out = append(out, piMCPServerSpec{
			Name:    "iterion_board",
			URL:     task.BoardHTTPEndpoint,
			Headers: map[string]string{"X-Iterion-Run": task.BoardRunToken},
		})
	}
	return out
}

// handleUIRequest answers an extension UI request.
//
// The channel is shared with any other extension the operator installed, so a
// request is only iterion's if it carries the control envelope. A genuine
// third-party dialog is cancelled — its documented safe default — because
// iterion has no operator at the other end of pi's UI and must neither block
// the agent nor invent an answer on the operator's behalf.
func (b *PiRPCBackend) handleUIRequest(task Task, collector *piCollector, req pisdk.UIRequest) *pisdk.UIResponse {
	env, ok := piParseCtrl(req)
	if !ok {
		if b.Logger != nil {
			b.Logger.Warn("[%s#%d/%s] declining an unrecognised extension UI request (%s: %q)",
				task.NodeID, task.Iteration, BackendPi, req.Method, truncate(req.Prompt(), 120))
		}
		return nil
	}

	switch env.Op {
	case piOpPermissionEvaluate:
		verdict, marker := piEvaluatePermission(task, env.Data)
		if b.Logger != nil && verdict.Decision != "allow" {
			b.Logger.Warn("[%s#%d/%s] permission gate: %s", task.NodeID, task.Iteration, BackendPi, verdict.Reason)
		}
		// `ask` suspends the run for a human. The call stays blocked in the
		// meantime, so the tool cannot run in the window before the pause
		// lands.
		if marker != nil {
			collector.setPause(&piPendingPause{
				question:         verdict.Reason,
				allowFreeText:    false,
				permissionMarker: marker,
			})
		}
		return piCtrlAnswer(req.ID, verdict)

	case piOpAskUser:
		pause, err := piParseAskUser(env.Data)
		if err != nil {
			if b.Logger != nil {
				b.Logger.Warn("[%s#%d/%s] %v", task.NodeID, task.Iteration, BackendPi, err)
			}
			return piCtrlFail(req.ID, err.Error())
		}
		collector.setPause(pause)
		if b.Logger != nil {
			b.Logger.Info("[%s#%d/%s] ask_user: %s", task.NodeID, task.Iteration, BackendPi,
				truncate(pause.question, 200))
		}
		return piCtrlAnswer(req.ID, map[string]any{"escalated": true})
	default:
		if b.Logger != nil {
			b.Logger.Warn("[%s#%d/%s] unknown control op %q from the iterion extension "+
				"(extension newer than this engine?)", task.NodeID, task.Iteration, BackendPi, env.Op)
		}
		return piCtrlFail(req.ID, "unknown op "+env.Op)
	}
}

// piWriteSystemPrompt materialises the composed prompt, reusing the print-mode
// helper so both transports place it identically.
func piWriteSystemPrompt(task Task, systemPrompt string) (string, func(), error) {
	if systemPrompt == "" {
		return "", func() {}, nil
	}
	return writeSystemPromptFile(task, BackendPi, systemPrompt)
}

// ---------------------------------------------------------------------------
// piCollector turns the event stream into hooks, text and accounting.
// ---------------------------------------------------------------------------

type piCollector struct {
	task    Task
	logger  *iterlog.Logger
	settled chan struct{}

	mu           sync.Mutex
	closeOnce    sync.Once
	lastEvent    time.Time
	lastProgress time.Time

	assistantText string
	stop          pisdk.StopReason
	errMessage    string
	httpStatus    int

	inTokens  int
	outTokens int
	thinking  int
	costUSD   float64
	peakInput int
	seen      map[string]bool

	retries []pisdk.Event

	// pause is set when the extension raised a suspension (ask_user, or a
	// permission escalation). Execute turns it into ErrAskUser after the turn
	// unwinds; it cannot be raised inline because the control-channel handler
	// must reply promptly and a pause has to travel out through Execute.
	pause *piPendingPause
}

// setPause records the first suspension of the turn. Later ones are dropped:
// one pause resolves one question, and the agent re-asks on resume if it still
// needs to.
func (c *piCollector) setPause(p *piPendingPause) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pause == nil {
		c.pause = p
	}
}

func (c *piCollector) pending() *piPendingPause {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pause
}

func (c *piCollector) onEvent(ev pisdk.Event) {
	now := time.Now()
	c.mu.Lock()
	c.lastEvent = now
	if c.seen == nil {
		c.seen = map[string]bool{}
	}

	switch ev.Type {
	case pisdk.EventMessageEnd:
		c.lastProgress = now
		if ev.Message != nil && ev.Message.IsAssistant() {
			c.absorbAssistant(*ev.Message)
		}

	case pisdk.EventToolExecutionEnd, pisdk.EventCompactionEnd:
		c.lastProgress = now

	case pisdk.EventAutoRetryStart:
		// Should not fire — retries are disabled at handshake. If it does, the
		// attempt is billed and will be absent from the surviving transcript.
		c.retries = append(c.retries, ev)

	case pisdk.EventAgentSettled:
		c.mu.Unlock()
		c.closeOnce.Do(func() { close(c.settled) })
		c.fireHooks(ev)
		return
	}
	c.mu.Unlock()

	c.fireHooks(ev)
}

// absorbAssistant accumulates one completed assistant message. Called with the
// lock held.
func (c *piCollector) absorbAssistant(m pisdk.Message) {
	if text := m.Text(); text != "" {
		c.assistantText = text
	}
	if m.StopReason != "" {
		c.stop = m.StopReason
	}
	if m.ErrorMessage != "" {
		c.errMessage = m.ErrorMessage
	}
	if status := m.HTTPStatus(); status != 0 {
		c.httpStatus = status
	}
	// De-dup: the same message is re-emitted across streaming deltas, and
	// summing it per delta would multiply the reported bill.
	if m.Usage == nil {
		return
	}
	id := m.Identity()
	if c.seen[id] {
		return
	}
	c.seen[id] = true
	c.inTokens += m.Usage.Input
	c.outTokens += m.Usage.Output
	c.thinking += m.Usage.ReasoningTokens()
	c.costUSD += m.Usage.Cost.Total
	if ctxTokens := m.Usage.ContextTokens(); ctxTokens > c.peakInput {
		c.peakInput = ctxTokens
	}
}

// fireHooks bridges pi's events onto iterion's TaskHooks. Runs on the client's
// dispatcher goroutine, never the reader, so a slow hook cannot stall pi.
func (c *piCollector) fireHooks(ev pisdk.Event) {
	h := c.task.Hooks
	switch ev.Type {
	case pisdk.EventToolExecutionStart:
		if h.OnToolStarted != nil {
			h.OnToolStarted(ev.ToolName, ev.ToolCallID, ev.Args)
		}
	case pisdk.EventToolExecutionEnd:
		if h.OnToolCalled != nil {
			h.OnToolCalled(ev.ToolName, ev.ToolCallID, ev.IsError, flattenPiResult(ev.Result))
		}
	case pisdk.EventMessageEnd:
		if h.OnAssistantText != nil && ev.Message != nil && ev.Message.IsAssistant() {
			if text := ev.Message.Text(); text != "" {
				h.OnAssistantText(text)
			}
		}
	}
}

// flattenPiResult renders a tool result for the store. pi types it as `any`,
// so a bare string arrives quoted and is unwrapped.
func flattenPiResult(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

func (c *piCollector) marks() (lastEvent, lastProgress time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastEvent, c.lastProgress
}

func (c *piCollector) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.assistantText
}

func (c *piCollector) stopReason() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.stop)
}

// usage returns the accumulated accounting. Every field is guarded: the
// dispatcher goroutine writes them while Execute reads.
func (c *piCollector) usage() (in, out int, costUSD float64, peak, thinking int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inTokens, c.outTokens, c.costUSD, c.peakInput, c.thinking
}

func (c *piCollector) notices() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.retries) == 0 {
		return nil
	}
	last := c.retries[len(c.retries)-1]
	return []string{fmt.Sprintf(
		"pi retried upstream %d time(s) despite retries being disabled at handshake "+
			"(last: attempt %d/%d after %dms — %q). Those attempts were billed but are "+
			"absent from this node's accounting.",
		len(c.retries), last.Attempt, last.MaxAttempts, last.DelayMs, last.ErrorMessage)}
}

// failure re-types a failed turn onto iterion's error taxonomy, sharing the
// classification with print mode so a pi rate limit behaves identically on
// either transport.
func (c *piCollector) failure() error {
	c.mu.Lock()
	stop, msg, status := c.stop, c.errMessage, c.httpStatus
	c.mu.Unlock()

	if !stop.Failed() {
		return nil
	}
	return piClassifyFailure(pisdk.Message{
		Role:         "assistant",
		StopReason:   stop,
		ErrorMessage: msg,
		Diagnostics:  piStatusDiagnostics(status),
	})
}

// piStatusDiagnostics rebuilds the minimal diagnostics shape piClassifyFailure
// reads the upstream HTTP status from.
func piStatusDiagnostics(status int) []pisdk.Diagnostic {
	if status == 0 {
		return nil
	}
	code, err := json.Marshal(status)
	if err != nil {
		return nil
	}
	return []pisdk.Diagnostic{{
		Type:  "provider_error",
		Error: &pisdk.DiagnosticError{Code: code},
	}}
}
