package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/claw-code-go/pkg/api/hooks"

	"github.com/SocialGouv/iterion/pkg/backend/cost"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ClawBackend implements delegate.Backend by calling GenerateTextDirect and
// GenerateObjectDirect against api.APIClient. It wraps the direct LLM path
// into the unified Backend interface.
type ClawBackend struct {
	registry       *Registry
	hooks          EventHooks
	retry          RetryPolicy
	lifecycleHooks *hooks.Runner
	// inbox, when non-nil, plumbs the run's user-message inbox into
	// the generation loop: operator-typed chat messages are appended
	// to the conversation between tool iterations. See [WithInbox].
	inbox InboxBinder
}

// InboxHook is invoked by the generation tool-loop between
// iterations: Consume marks the previous round's delivered messages
// as consumed, Drain returns any new operator-typed texts to inject
// before the next LLM call. Both run cooperatively at safe
// boundaries — never mid-stream.
type InboxHook interface {
	Consume(ctx context.Context)
	Drain(ctx context.Context) []string
}

// InboxBinder constructs a per-run InboxHook. The runtime supplies a
// store-backed implementation; tests can plug in a stub. Returning
// nil disables the inbox plumbing for that specific run.
type InboxBinder interface {
	Bind(ctx context.Context, runID string) InboxHook
}

// WithInbox wires an InboxBinder into the backend so the generation
// engine drains the operator chatbox between tool iterations.
func WithInbox(b InboxBinder) ClawBackendOption {
	return func(c *ClawBackend) { c.inbox = b }
}

// StoreInboxBinder is the production InboxBinder, backed by a
// store.RunStore + an event-broker publish callback. The hooks it
// returns reuse the shared store.DrainPending / store.MarkConsumed
// helpers so the runtime's pauseAtHuman drainer and the agent-loop
// drainer emit identical event payloads.
type StoreInboxBinder struct {
	Store store.RunStore
	// Publish receives each user_message_* event after store-side
	// persistence. Local mode passes EventBroker.Publish; cloud mode
	// passes nil (the Mongo change-stream surfaces transitions).
	Publish func(store.Event)
}

// Bind returns the hook scoped to runID, or nil when the binder is
// not configured for this run.
func (b *StoreInboxBinder) Bind(ctx context.Context, runID string) InboxHook {
	if b == nil || b.Store == nil || runID == "" {
		return nil
	}
	return &storeInboxHook{
		store:     b.Store,
		publish:   b.Publish,
		runID:     runID,
		versioner: asInboxVersioner(b.Store),
	}
}

// asInboxVersioner type-asserts the store onto the optional
// QueuedInboxVersioner interface so callers can fast-skip a load
// when the doorbell counter is unchanged. Returns nil when the
// backend can't supply a counter (forces every Drain to reload).
func asInboxVersioner(s store.RunStore) store.QueuedInboxVersioner {
	v, _ := s.(store.QueuedInboxVersioner)
	return v
}

// storeInboxHook is one bound-to-a-run InboxHook. The struct is the
// stable home for the cross-iteration state (last delivered IDs +
// last observed inbox version) so the closures stay garbage-free.
type storeInboxHook struct {
	store         store.RunStore
	publish       func(store.Event)
	runID         string
	versioner     store.QueuedInboxVersioner
	lastDelivered []string
	lastVersion   uint64
	versionSeen   bool
}

// Drain transitions queued→delivered and returns texts in FIFO order.
// When the underlying store implements QueuedInboxVersioner the hook
// fast-skips loading the JSONL when the counter is unchanged — the
// common case for a busy tool loop on a run with no queued messages.
func (h *storeInboxHook) Drain(ctx context.Context) []string {
	if h.versioner != nil {
		v := h.versioner.QueuedInboxVersion(h.runID)
		if h.versionSeen && v == h.lastVersion {
			return nil
		}
		h.lastVersion = v
		h.versionSeen = true
	}
	texts, ids, _ := store.DrainPending(ctx, h.store, h.publish, h.runID)
	if len(ids) > 0 {
		h.lastDelivered = ids
	}
	return texts
}

// Consume marks the previously-drained messages as consumed.
func (h *storeInboxHook) Consume(ctx context.Context) {
	if len(h.lastDelivered) == 0 {
		return
	}
	store.MarkConsumed(ctx, h.store, h.publish, h.runID, h.lastDelivered)
	h.lastDelivered = nil
}

// ClawBackendOption configures a ClawBackend at construction time.
type ClawBackendOption func(*ClawBackend)

// WithBackendLifecycleHooks installs an in-process hook runner fired
// around tool execution and at session end. A nil runner is a no-op.
func WithBackendLifecycleHooks(r *hooks.Runner) ClawBackendOption {
	return func(b *ClawBackend) { b.lifecycleHooks = r }
}

// NewClawBackend creates a new ClawBackend.
func NewClawBackend(registry *Registry, hk EventHooks, retry RetryPolicy, opts ...ClawBackendOption) *ClawBackend {
	b := &ClawBackend{
		registry: registry,
		hooks:    hk,
		retry:    retry,
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// Execute implements delegate.Backend.
//
// When the run is sandboxed (task.Sandbox != nil), the call is
// forwarded to the iterion-claw-runner sub-process inside the
// container — see [executeViaSandboxRunner]. This keeps the LLM's
// in-process tool execution (Bash, file edits) inside the sandbox
// rather than escaping to the host. The unsandboxed path below is
// the historical in-process implementation.
//
// V1 limitations of the sandbox-routed path are documented on
// [delegate.IOTask] and in docs/sandbox.md: no MCP servers, no
// mid-tool-loop ask_user resume.
func (b *ClawBackend) Execute(ctx context.Context, task delegate.Task) (delegate.Result, error) {
	if task.Sandbox != nil {
		return b.executeViaSandboxRunner(ctx, task)
	}

	// CGU guard: claw is an in-process Anthropic SDK consumer.
	// Anthropic's Consumer Terms scope the Claude Pro/Max OAuth
	// forfait to the official Claude Code CLI surface, so iterion
	// MUST refuse to drive an Anthropic call from claw using a
	// stored OAuth-forfait credential when no API key is available.
	// The claude_code delegate backend (which spawns the official
	// CLI) is exempt and lives in pkg/backend/delegate.
	if providerName, _, perr := ParseModelSpec(task.Model); perr == nil && providerName == "anthropic" {
		if err := secrets.GuardThirdPartyOAuth(ctx, secrets.ProviderAnthropic, secrets.OAuthKindClaudeCode); err != nil {
			return delegate.Result{}, fmt.Errorf("claw backend: %w", err)
		}
	}

	// Resolve API client. Phase C: in cloud mode the runner stamps
	// per-tenant BYOK credentials into ctx, ResolveWithContext then
	// builds a fresh APIClient with the override key (no cache hit
	// across tenants). Local mode keeps the env-fallback path.
	client, err := b.registry.ResolveWithContext(ctx, task.Model)
	if err != nil {
		return delegate.Result{}, fmt.Errorf("claw backend: %w", err)
	}

	// Strip the "provider/" prefix so the request body carries the bare
	// model ID. Provider routing is already done at this point (via
	// Resolve), and provider APIs (Anthropic, OpenAI) don't recognize the
	// prefixed form in the JSON body — Anthropic returns 404, OpenAI may
	// silently coerce or also reject depending on the model.
	_, modelID, err := ParseModelSpec(task.Model)
	if err != nil {
		return delegate.Result{}, fmt.Errorf("claw backend: %w", err)
	}

	// Build GenerationOptions.
	opts := GenerationOptions{
		Model:                 modelID,
		MaxTokens:             task.MaxTokens,
		CompactThresholdRatio: task.CompactThresholdRatio,
		CompactPreserveRecent: task.CompactPreserveRecent,
	}

	// Reasoning effort via ProviderOptions. Coerce against the model's
	// supported matrix — claw-code-go does NOT clamp on its own, so a
	// recipe asking for "max" on an OpenAI model would otherwise reach
	// the API with an unsupported value and bounce as 400.
	if effort := coerceEffortForModel(task.ReasoningEffort, modelID); effort != "" {
		opts.ProviderOptions = providerOptsForNode(effort)
	}

	// System prompt (optionally augmented with the interaction protocol)
	// with ephemeral cache_control marker.
	systemText := task.BuildSystemPrompt()
	if systemText != "" {
		opts.SystemBlocks = []api.ContentBlock{{
			Type:         "text",
			Text:         systemText,
			CacheControl: api.EphemeralCacheControl(),
		}}
	}

	// User message.
	userText := task.UserPrompt

	// When both tools AND output schema are present, inject schema format
	// instruction into user text (GenerateText supports tool loop,
	// GenerateObject does not).
	if task.OutputSchema != nil && task.HasTools {
		schemaJSON, _ := json.MarshalIndent(task.OutputSchema, "", "  ")
		userText += fmt.Sprintf(
			"\n\nOUTPUT FORMAT: After completing all tool operations, your final message MUST be a raw JSON object matching this schema:\n%s\nNo markdown fences, no extra text — ONLY the JSON object.",
			string(schemaJSON),
		)
	}

	if len(task.UserContent) > 0 {
		blocks := make([]api.ContentBlock, 0, len(task.UserContent))
		for _, c := range task.UserContent {
			switch c.Type {
			case "text":
				if c.Text == "" {
					continue
				}
				blocks = append(blocks, api.ContentBlock{Type: "text", Text: c.Text})
			case "image":
				switch {
				case c.Data != "":
					blocks = append(blocks, api.ContentBlock{
						Type: "image",
						Source: &api.ImageSource{
							Type:      "base64",
							MediaType: c.MediaType,
							Data:      c.Data,
						},
					})
				case c.URL != "":
					blocks = append(blocks, api.ContentBlock{
						Type: "image",
						Source: &api.ImageSource{
							Type: "url",
							URL:  c.URL,
						},
					})
				}
			}
		}
		// Surface the schema-injection suffix that buildUserContent
		// can't emit (it's appended after, not from the prompt body).
		if userText != "" && task.UserPrompt != userText {
			blocks = append(blocks, api.ContentBlock{Type: "text", Text: userText[len(task.UserPrompt):]})
		}
		if len(blocks) > 0 {
			opts.Messages = []api.Message{{Role: "user", Content: blocks}}
		}
	} else if userText != "" {
		opts.Messages = []api.Message{
			{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: userText}}},
		}
	}

	// Resume mode: the persisted conversation already contains the original
	// user prompt and the assistant message with the pending tool_use block.
	// Replace opts.Messages with that conversation plus a new user-role
	// message carrying a tool_result that answers the captured ask_user
	// call. This rehydrates the LLM's mid-tool-loop state across the pause
	// without re-rendering the prompt or relying on the [PRIOR INTERACTION]
	// suffix.
	if len(task.ResumeConversation) > 0 {
		var prior []api.Message
		if err := json.Unmarshal(task.ResumeConversation, &prior); err != nil {
			return delegate.Result{}, fmt.Errorf("claw backend: decode resume conversation: %w", err)
		}
		if task.ResumePendingToolUseID == "" {
			return delegate.Result{}, fmt.Errorf("claw backend: resume conversation set but pending tool_use ID is empty")
		}
		answer := task.ResumeAnswer
		if answer == "" {
			answer = "(no answer provided)"
		}
		prior = append(prior, api.Message{
			Role: "user",
			Content: []api.ContentBlock{api.ToolResult{
				ToolUseID: task.ResumePendingToolUseID,
				Content:   answer,
			}.ToContentBlock()},
		})
		opts.Messages = prior
	}

	// Tools.
	if len(task.ToolDefs) > 0 {
		opts.Tools = toolDefsToGeneration(task.ToolDefs)
		maxSteps := task.ToolMaxSteps
		if maxSteps <= 0 {
			maxSteps = 5
		}
		opts.MaxSteps = maxSteps
	}

	// Observability hooks.
	applyHooks(task.NodeID, b.hooks, &opts)

	// In-process lifecycle hooks (audit, safety, compaction
	// observability). Nil-safe at call sites in generation.go.
	opts.Hooks = b.lifecycleHooks

	// Wire the operator-chatbox inbox if the runtime configured one.
	// Resolved per-call so a backend shared across runs picks up the
	// right hook for each run.
	if b.inbox != nil {
		if runID := RunIDFromContext(ctx); runID != "" {
			opts.Inbox = b.inbox.Bind(ctx, runID)
		}
	}

	if m := task.Memory; m != nil {
		if err := installWorkspaceMemory(&opts, task.WorkDir, m); err != nil {
			return delegate.Result{}, fmt.Errorf("claw backend: memory: %w", err)
		}
	}

	// Dispatch to the appropriate generation strategy.
	hasSchema := task.OutputSchema != nil
	if hasSchema && !task.HasTools {
		return b.generateStructuredWithRetry(ctx, client, task, opts)
	}
	if hasSchema && task.HasTools {
		return b.generateTextWithToolsAndSchemaRetry(ctx, client, task, opts)
	}
	return b.generateTextWithRetry(ctx, client, task, opts)
}

// ---------------------------------------------------------------------------
// Retry
// ---------------------------------------------------------------------------

func (b *ClawBackend) retryLoop(ctx context.Context, nodeID string, fn func() (delegate.Result, error)) (delegate.Result, error) {
	maxAttempts := b.retry.maxAttempts()
	result, err := fn()
	for attempt := 1; err != nil && isRetryable(err) && attempt < maxAttempts; attempt++ {
		delay := b.retry.backoff(attempt - 1)

		if b.hooks.OnLLMRetry != nil {
			b.hooks.OnLLMRetry(nodeID, RetryInfo{
				Attempt:    attempt,
				Error:      err,
				StatusCode: statusCodeOf(err),
				Delay:      delay,
			})
		}

		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return delegate.Result{}, ctx.Err()
		}

		result, err = fn()
	}
	return result, err
}

// ---------------------------------------------------------------------------
// Generation strategies
// ---------------------------------------------------------------------------

func (b *ClawBackend) generateStructuredWithRetry(ctx context.Context, client api.APIClient, task delegate.Task, opts GenerationOptions) (delegate.Result, error) {
	return b.retryLoop(ctx, task.NodeID, func() (delegate.Result, error) {
		return b.generateStructured(ctx, client, task, opts)
	})
}

// askUserResult converts a *delegate.ErrAskUser into the standard
// _needs_interaction Result iterion's executor expects. Used by every
// generation path so an LLM-issued ask_user call surfaces uniformly
// regardless of which generation strategy ran (structured / text /
// text+tools+schema). Conversation + PendingToolUseID propagate through
// Result so the runtime can persist them in the checkpoint, enabling
// mid-tool-loop resume on the next turn.
func askUserResult(err error) (delegate.Result, bool) {
	var ask *delegate.ErrAskUser
	if !errors.As(err, &ask) {
		return delegate.Result{}, false
	}
	return delegate.Result{
		Output: map[string]interface{}{
			"_needs_interaction": true,
			"_interaction_questions": map[string]interface{}{
				delegate.AskUserQuestionKey: ask.Question,
			},
		},
		BackendName:         delegate.BackendClaw,
		PendingConversation: ask.Conversation,
		PendingToolUseID:    ask.PendingToolUseID,
	}, true
}

func (b *ClawBackend) generateStructured(ctx context.Context, client api.APIClient, task delegate.Task, opts GenerationOptions) (delegate.Result, error) {
	// Set the explicit schema for structured output.
	genOpts := opts
	genOpts.ExplicitSchema = task.OutputSchema

	result, err := GenerateObjectDirect[map[string]interface{}](ctx, client, genOpts)
	if err != nil {
		if r, ok := askUserResult(err); ok {
			return r, nil
		}
		return delegate.Result{}, fmt.Errorf("claw backend: structured generation: %w", err)
	}

	output := result.Object
	if output == nil {
		output = make(map[string]interface{})
	}

	tokens := cost.Annotate(output, task.Model, result.TotalUsage.InputTokens, result.TotalUsage.OutputTokens)

	return delegate.Result{
		Output:      output,
		Tokens:      tokens,
		BackendName: delegate.BackendClaw,
	}, nil
}

func (b *ClawBackend) generateTextWithRetry(ctx context.Context, client api.APIClient, task delegate.Task, opts GenerationOptions) (delegate.Result, error) {
	return b.retryLoop(ctx, task.NodeID, func() (delegate.Result, error) {
		return b.generateText(ctx, client, task, opts)
	})
}

func (b *ClawBackend) generateText(ctx context.Context, client api.APIClient, task delegate.Task, opts GenerationOptions) (delegate.Result, error) {
	opts = applySessionMessages(ctx, task.NodeID, opts)
	result, err := GenerateTextDirect(ctx, client, opts)
	captureSessionMessages(ctx, task.NodeID, result)
	if err != nil {
		if r, ok := askUserResult(err); ok {
			return r, nil
		}
		return delegate.Result{}, fmt.Errorf("claw backend: text generation: %w", err)
	}

	output := map[string]interface{}{"text": result.Text}
	tokens := cost.Annotate(output, task.Model, result.TotalUsage.InputTokens, result.TotalUsage.OutputTokens)

	return delegate.Result{
		Output:      output,
		Tokens:      tokens,
		BackendName: delegate.BackendClaw,
	}, nil
}

func (b *ClawBackend) generateTextWithToolsAndSchemaRetry(ctx context.Context, client api.APIClient, task delegate.Task, opts GenerationOptions) (delegate.Result, error) {
	return b.retryLoop(ctx, task.NodeID, func() (delegate.Result, error) {
		return b.generateTextWithToolsAndSchema(ctx, client, task, opts)
	})
}

func (b *ClawBackend) generateTextWithToolsAndSchema(ctx context.Context, client api.APIClient, task delegate.Task, opts GenerationOptions) (delegate.Result, error) {
	opts = applySessionMessages(ctx, task.NodeID, opts)
	result, err := GenerateTextDirect(ctx, client, opts)
	captureSessionMessages(ctx, task.NodeID, result)
	if err != nil {
		if r, ok := askUserResult(err); ok {
			return r, nil
		}
		return delegate.Result{}, fmt.Errorf("claw backend: text+tools generation: %w", err)
	}

	text := strings.TrimSpace(result.Text)
	text = extractJSON(text)

	// Try the cheap path first: parse the tool-loop's final text as JSON.
	// If the model already committed to structured output, we're done.
	if text != "" {
		var output map[string]interface{}
		if err := json.Unmarshal([]byte(text), &output); err == nil {
			tokens := cost.Annotate(output, task.Model, result.TotalUsage.InputTokens, result.TotalUsage.OutputTokens)
			return delegate.Result{
				Output:      output,
				Tokens:      tokens,
				BackendName: delegate.BackendClaw,
			}, nil
		}
	}

	// Recovery pass — fires when the tool loop produced either no
	// final text (MaxSteps exhausted, model kept calling tools) OR a
	// non-JSON narrative response ("No findings.", "I reviewed X..."
	// — common with gpt-5.5 when the schema feels heavy). Same
	// conversation history, NO tools, schema enforced via
	// GenerateObjectDirect. The model is now obliged to produce
	// structured output on its next turn. Mirrors claude_code's
	// two-pass formatting.
	recoveryOpts := opts
	recoveryOpts.Messages = result.Messages
	recoveryOpts.Tools = nil
	recoveryOpts.MaxSteps = 1
	recoveryOpts.ExplicitSchema = task.OutputSchema

	// Emit an OnLLMRequest before the recovery pass so the timeline /
	// Prometheus exporter sees two distinct LLM steps for a recovered
	// tool loop instead of one (the recovery's tokens then attach to
	// the original step in the aggregate, with no per-step accounting).
	if b.hooks.OnLLMRequest != nil {
		b.hooks.OnLLMRequest(task.NodeID, LLMRequestInfo{
			Model:        task.Model,
			MessageCount: len(recoveryOpts.Messages),
			Timestamp:    time.Now(),
		})
	}
	obj, recErr := GenerateObjectDirect[map[string]interface{}](ctx, client, recoveryOpts)
	if recErr == nil && obj != nil && obj.Object != nil {
		tokens := cost.Annotate(obj.Object, task.Model,
			result.TotalUsage.InputTokens+obj.TotalUsage.InputTokens,
			result.TotalUsage.OutputTokens+obj.TotalUsage.OutputTokens)
		return delegate.Result{
			Output:             obj.Object,
			Tokens:             tokens,
			BackendName:        delegate.BackendClaw,
			FormattingPassUsed: true,
		}, nil
	}

	// Last-ditch: surface whatever text we got as a parse-fallback so
	// the runtime's existing structured-output retry path can decide
	// what to do. The error from the recovery pass is logged for
	// post-mortem.
	if text == "" {
		return delegate.Result{}, fmt.Errorf("claw backend: text+tools generation produced empty response after tool loop and structured-output recovery failed: %v", recErr)
	}
	output := map[string]interface{}{"text": text}
	tokens := cost.Annotate(output, task.Model, result.TotalUsage.InputTokens, result.TotalUsage.OutputTokens)
	return delegate.Result{
		Output:        output,
		Tokens:        tokens,
		BackendName:   delegate.BackendClaw,
		ParseFallback: true,
	}, nil
}

// ---------------------------------------------------------------------------
// Sandboxed execution — Phase 4 V1
// ---------------------------------------------------------------------------

// executeViaSandboxRunner forwards the task to the iterion-claw-runner
// sub-process inside the sandbox container.
//
// Wire format (V2-1+, NDJSON envelopes on stdin/stdout — see
// [delegate.Envelope]):
//
//	stdin  : EnvelopeTask, then EnvelopeToolResult / EnvelopeAskUserAnswer
//	         / EnvelopeSessionReplay as the multiplexer drives them in
//	         response to runner-initiated envelopes
//	stdout : intermediate envelopes (tool_call / ask_user /
//	         session_capture / event), terminated by EnvelopeResult
//
// The runner re-builds the claw backend in-container with a default
// tool set, executes the task, and returns the structured result.
// Errors come back two ways: a non-zero exit code and a non-empty
// IOResult.Error field — both are surfaced to the caller.
//
// V2-1 ships the multiplexer with a no-op handler set; the runner
// today emits only the terminal result envelope. V2-2 wires
// OnToolCall to the in-process tool registry + MCP manager so MCP
// tools become reachable across the IPC; V2-3 wires OnAskUser to the
// engine pause path; V2-4 wires OnSessionCapture for compaction-retry.
func (b *ClawBackend) executeViaSandboxRunner(ctx context.Context, task delegate.Task) (delegate.Result, error) {
	run := task.Sandbox
	if run == nil {
		return delegate.Result{}, fmt.Errorf("claw backend: executeViaSandboxRunner called without a sandbox handle")
	}

	// The runner is the same iterion binary inside the container,
	// invoked via a hidden subcommand. The container image is
	// expected to ship `iterion` on PATH (the production Dockerfile
	// installs it; bind-mount workflows on local hosts can mount
	// the host binary into /usr/local/bin/iterion when arches match).
	//
	// KeepStdinOpen tells the docker driver to add `--interactive`
	// to the docker exec invocation. Without it, the container's
	// stdin is closed before we get to wire cmd.StdinPipe() below,
	// the runner reads EOF on its very first envelope read, and
	// dies with "read pre-task envelope: EOF (exit: exit status 1)"
	// — the same class of failure the claudesdk Session path hit
	// before 0ab267c.
	//
	// Forward provider credentials from the host iterion-desktop
	// process env into the runner. The runner re-builds its own
	// model registry inside the container, which calls
	// os.Getenv("OPENAI_API_KEY") etc. — without forwarding, it
	// finds nothing and bails with "API key required for OpenAI-
	// compatible provider". Pass through only the keys we know
	// providers consume: anything else stays on the host.
	runnerEnv := forwardableProviderEnv()
	cmd := run.Command(ctx, []string{"iterion", "__claw-runner"}, sandbox.ExecOpts{
		KeepStdinOpen: true,
		Env:           runnerEnv,
	})

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return delegate.Result{}, fmt.Errorf("claw backend: stdin pipe: %w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return delegate.Result{}, fmt.Errorf("claw backend: stdout pipe: %w", err)
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return delegate.Result{}, fmt.Errorf("claw backend: spawn runner: %w", err)
	}

	mux := delegate.NewMultiplexer(stdoutPipe, stdinPipe, b.multiplexerHandler(ctx, task))

	// V2-4: when the host's session store has prior messages for this
	// (runID, nodeID), seed the runner with a session_replay envelope
	// BEFORE the task envelope. The runner stashes the snapshot until
	// the task arrives, then loads it into its local store so
	// applySessionMessages prepends the replayed history to the LLM's
	// first call. This preserves CompactAndRetry semantics across the
	// sandbox boundary.
	hostRunID, hostStore := runtimeContextFrom(ctx)
	if hostStore != nil && hostRunID != "" && task.NodeID != "" {
		if snapshot := hostStore.LoadSnapshot(hostRunID, task.NodeID); len(snapshot) > 0 {
			replayEnv := delegate.NewSessionReplayEnvelope(snapshot)
			if err := mux.Send(replayEnv); err != nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				return delegate.Result{}, fmt.Errorf("claw backend: send session_replay: %w", err)
			}
		}
	}

	// Send the task envelope. The runner blocks on its
	// EnvelopeReader.Read() until this arrives.
	taskEnv, err := delegate.NewTaskEnvelope(delegate.ToIOTask(task))
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return delegate.Result{}, fmt.Errorf("claw backend: build task envelope: %w", err)
	}
	if err := mux.Send(taskEnv); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return delegate.Result{}, fmt.Errorf("claw backend: send task envelope: %w", err)
	}

	// Drive the multiplexer loop. Returns the terminal IOResult.
	ioRes, runErr := mux.Run(ctx)

	// Close stdin so the runner sees EOF if it's still reading; the
	// terminal result envelope is supposed to be its last write, so
	// closing stdin after Run returns is purely belt-and-suspenders.
	_ = stdinPipe.Close()

	waitErr := cmd.Wait()

	if runErr != nil && !errors.Is(runErr, io.EOF) {
		return delegate.Result{}, fmt.Errorf("claw backend: multiplexer: %w (stderr: %s)", runErr, stderrBuf.String())
	}
	if errors.Is(runErr, io.EOF) && ioRes.Error == "" && waitErr == nil {
		// Runner closed stdout without sending a result envelope and
		// exited cleanly — this should not happen with a well-formed
		// runner, but surface a clear diagnostic instead of a misleading
		// success.
		return delegate.Result{}, fmt.Errorf("claw backend: runner exited without sending result envelope (stderr: %s)", stderrBuf.String())
	}

	if waitErr != nil && ioRes.Error == "" {
		return delegate.Result{}, fmt.Errorf("claw backend: runner exited with error: %w (stderr: %s)", waitErr, stderrBuf.String())
	}
	if ioRes.Error != "" {
		// Preserve waitErr for errors.Is / errors.As consumers when
		// the runner emitted a structured error AND exited non-zero
		// (the normal error path). Without %w on waitErr, downstream
		// classifiers would lose the exec.ExitError typing.
		if waitErr != nil {
			return delegate.FromIOResult(ioRes), fmt.Errorf("claw backend: runner: %s (exit: %w)", ioRes.Error, waitErr)
		}
		return delegate.FromIOResult(ioRes), fmt.Errorf("claw backend: runner: %s", ioRes.Error)
	}

	res := delegate.FromIOResult(ioRes)
	if res.BackendName == "" {
		res.BackendName = delegate.BackendClaw
	}
	return res, nil
}

// multiplexerHandler builds the launcher-side envelope dispatch table
// for a specific task.
//
//   - V2-2: OnToolCall dispatches via the task's ToolDefs map. The
//     runner emits a tool_call envelope for each LLM-driven tool
//     invocation; the launcher invokes the original closure (which has
//     access to the engine's tool registry, MCP manager, ask_user
//     channel, etc.) and forwards the result. *ErrAskUser returns are
//     preserved typed by the multiplexer (V2-3).
//   - V2-4: OnSessionCapture mirrors runner-emitted session snapshots
//     into the host's nodeSessionStore so CompactAndRetry compacts the
//     latest history. Pre-spawn, the launcher seeds a session_replay
//     envelope from the host store (see [executeViaSandboxRunner]).
//
// providerCredentialEnvVars enumerates the env-var names the in-runner
// model registry consults to authenticate against each provider. Listed
// explicitly (rather than forwarding the full host env) so the sandbox
// stays isolated from the operator's shell — only the keys the runner
// actually needs cross the boundary.
//
// Keep this in sync with pkg/backend/model/registry.go's per-provider
// auth code: any new provider whose Resolve() reads os.Getenv(...) for
// credentials must append its env-var name here, otherwise the runner
// inside the sandbox will surface "API key required for <provider>".
var providerCredentialEnvVars = []string{
	"OPENAI_API_KEY",
	"AZURE_OPENAI_API_KEY",
	"AZURE_OPENAI_ENDPOINT",
	"ANTHROPIC_API_KEY",
	"GEMINI_API_KEY",
	"GOOGLE_API_KEY",
	"GROQ_API_KEY",
	"DEEPSEEK_API_KEY",
	"MISTRAL_API_KEY",
	"BEDROCK_REGION",
	"AWS_REGION",
	"AWS_ACCESS_KEY_ID",
	"AWS_SECRET_ACCESS_KEY",
	"AWS_SESSION_TOKEN",
}

// forwardableProviderEnv builds the env map ClawBackend hands to
// sandbox.Run.Command so the in-container runner can reach the same
// provider APIs as the host. Empty entries are skipped — we never
// inject a name=<empty> pair, since some providers treat that as
// "auth attempted but invalid" instead of "no auth".
func forwardableProviderEnv() map[string]string {
	env := map[string]string{}
	for _, name := range providerCredentialEnvVars {
		if v := os.Getenv(name); v != "" {
			env[name] = v
		}
	}
	return env
}

// mcpDotToUnderscore returns the claude_code-style FQN alias for a
// dot-delimited MCP tool name ("mcp.server.tool" → "mcp__server__tool")
// or "" when the input is not an MCP tool. Used by the claw launcher
// + Registry.Resolve so a prompt that names tools in either convention
// resolves under either backend.
func mcpDotToUnderscore(name string) string {
	const prefix = "mcp."
	if !strings.HasPrefix(name, prefix) {
		return ""
	}
	body := name[len(prefix):]
	// Find LAST dot so tool names with embedded dots (none today, but
	// don't break on them) split server/tool the same way Resolve does.
	idx := strings.LastIndex(body, ".")
	if idx <= 0 {
		return ""
	}
	server := body[:idx]
	tool := body[idx+1:]
	if server == "" || tool == "" {
		return ""
	}
	return "mcp__" + server + "__" + tool
}

func (b *ClawBackend) multiplexerHandler(ctx context.Context, task delegate.Task) delegate.MultiplexerHandler {
	// Index ToolDefs by name once so OnToolCall is O(1) instead of
	// scanning the slice on each runner-initiated tool_call. MCP tools
	// also get a claude_code-style FQN alias ("mcp__server__tool" →
	// same ToolDef as "mcp.server.tool") so a bot prompt written for
	// claude_code calls land here regardless of which backend is
	// resolved. Without the alias, claw's strict ToolDefs check
	// rejects the call before iterion's Registry.Resolve (which has
	// the same normalisation) ever sees it.
	toolByName := make(map[string]delegate.ToolDef, len(task.ToolDefs)*2)
	for _, td := range task.ToolDefs {
		toolByName[td.Name] = td
		if alias := mcpDotToUnderscore(td.Name); alias != "" {
			toolByName[alias] = td
		}
	}
	hostRunID, hostStore := runtimeContextFrom(ctx)
	return delegate.MultiplexerHandler{
		OnToolCall: func(toolCtx context.Context, name string, input json.RawMessage) (string, error) {
			td, ok := toolByName[name]
			if !ok {
				return "", fmt.Errorf("launcher: tool %q not in task.ToolDefs (runner asked for an unknown tool)", name)
			}
			if td.Execute == nil {
				return "", fmt.Errorf("launcher: tool %q has no Execute closure (engine misconfiguration)", name)
			}
			return td.Execute(toolCtx, input)
		},
		OnSessionCapture: func(snapshot json.RawMessage) {
			if hostStore == nil || hostRunID == "" || task.NodeID == "" {
				return
			}
			// Best-effort mirror — failures keep the host store one
			// snapshot behind but the next capture will reconcile.
			_ = hostStore.SaveSnapshot(hostRunID, task.NodeID, snapshot)
		},
	}
}

// ---------------------------------------------------------------------------
// Tool conversion
// ---------------------------------------------------------------------------

// toolDefsToGeneration converts delegate.ToolDef slices to GenerationTool slices.
func toolDefsToGeneration(defs []delegate.ToolDef) []GenerationTool {
	tools := make([]GenerationTool, len(defs))
	for i, d := range defs {
		tools[i] = GenerationTool{
			Name:        d.Name,
			Description: d.Description,
			InputSchema: d.InputSchema,
			Execute:     d.Execute,
		}
	}
	return tools
}
