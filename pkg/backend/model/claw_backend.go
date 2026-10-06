package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/claw-code-go/pkg/api/hooks"

	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
	"github.com/SocialGouv/iterion/pkg/backend/cost"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/modelroute"
	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
	"github.com/SocialGouv/iterion/pkg/backend/permission"
	"github.com/SocialGouv/iterion/pkg/backend/rewrite"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/knowledge"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/memory"
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
	// memStore overrides the workspace-memory backend. nil → the local
	// filesystem store (memory.DefaultFSStore). Cloud runners inject a
	// Mongo+inline store via WithMemoryStore so memory persists in the
	// tenant's document store rather than the pod's ephemeral disk.
	memStore knowledge.MemoryStore
	// logger carries operator-facing warnings the EventHooks surface has no
	// channel for (e.g. "this node is about to spend your subscription's
	// extra-usage balance"). nil silences them. See [WithClawLogger].
	logger *iterlog.Logger
}

func (*ClawBackend) SupportsAsyncQuestions() bool { return true }

// WithClawLogger attaches a leveled logger for operator-facing warnings.
func WithClawLogger(l *iterlog.Logger) ClawBackendOption {
	return func(b *ClawBackend) { b.logger = l }
}

// WithMemoryStore injects a non-default workspace-memory backend
// (e.g. the cloud Mongo store). nil leaves the filesystem default.
func WithMemoryStore(ms knowledge.MemoryStore) ClawBackendOption {
	return func(c *ClawBackend) { c.memStore = ms }
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
		store:      b.Store,
		publish:    b.Publish,
		runID:      runID,
		activeNode: NodeIDFromContext(ctx),
		versioner:  asInboxVersioner(b.Store),
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
	activeNode    string
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
	texts, ids, _ := store.DrainPendingForNode(ctx, h.store, h.publish, h.runID, h.activeNode)
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
// On the sandbox-routed path each tool runs where its
// [tool.SandboxPlacementOf] says: in the container, proxied back to the
// launcher, or not at all — docs/sandbox.md lists the refusals (tools with
// no in-container form, Ask-capable permission policies, async interaction,
// and an MCP server this launcher may not start for a sandboxed run).
func (b *ClawBackend) Execute(ctx context.Context, task delegate.Task) (result delegate.Result, err error) {
	defer func() {
		if err == nil && task.SessionSlot != "" {
			result.SessionID = task.SessionID
			result.SessionFingerprint = clawSessionFingerprint(task.Model)
		}
	}()
	// Carry the resolved compression mode + rewriter chain into the tool loop
	// so the bash builtin can compress command output (rewrite via context).
	// Off is a no-op. For the sandboxed path the mode + chain specs ride the
	// IOTask to the in-container runner, whose own Execute re-applies them here.
	mode, chain := rewrite.ParseMode(task.CompressMode), rewrite.NewChain(task.Rewriters)
	ctx = rewrite.WithMode(ctx, mode)
	ctx = rewrite.WithChain(ctx, chain)
	// Run-level env additions (devbox profile PATH on no-sandbox runs)
	// reach the in-process bash builtin via ctx — the tool registry is
	// built before the run's provisioning resolves, so the closure reads
	// the value per call. The shell also carries the chain's run env, last
	// so it wins, whatever the mode (the agent may run a rewriter itself): it
	// keeps what a command ran and printed out of the rewriter's own stores.
	ctx = tool.WithBashExtraEnv(ctx, append(slices.Clip(task.ExtraEnv), chain.RunEnv()...))

	// The node's credential-routing hint (the DSL `provider:` field) travels
	// with the context so the registry honours it at the resolution
	// chokepoint (ResolveWithContext). The sandboxed path's in-container
	// runner re-stamps it from its own IOTask-carried copy at this same line —
	// a runner binary that PREDATES the stamp silently resolves without the
	// hint (the pre-#1718 z.ai-synthesis behaviour), the same skew tolerance
	// as every other capability an older sandbox image lacks.
	ctx = WithProviderHint(ctx, task.ProviderHint)

	// claw is an in-process Anthropic SDK consumer rather than the vendor's
	// own CLI, which was once read as putting a Claude Pro/Max OAuth
	// subscription out of policy here. Anthropic's API settled it: the token
	// is ACCEPTED from a third-party app and billed against a separate
	// extra-usage balance instead of the plan's limits. So this warns — the
	// operator is spending a different pot than they may expect — and only
	// refuses when ITERION_FORBID_SUBSCRIPTION_OAUTH=1. See ADR-085.
	//
	// It runs BEFORE the sandbox dispatch, and must: sandboxing is on by
	// default, the in-container runner rebuilds its own registry from the
	// forwarded env, and it is built with no logger — so a guard placed after
	// the dispatch would neither refuse nor warn on the default path, silently
	// spending the balance the operator just closed.
	//
	// A GLM id on this provider is z.ai's (GLMOnAnthropicWire): it spends the
	// z.ai key, never the subscription, so neither the refusal nor the notice
	// concerns it — beside a Claude forfait it would refuse a node that was
	// never going to touch the forfait.
	if providerName, _, perr := ParseModelSpec(task.Model); perr == nil && providerName == "anthropic" && !GLMOnAnthropicWire(task.Model) {
		if err := secrets.GuardSubscriptionOAuth(ctx, secrets.ProviderAnthropic, secrets.OAuthKindClaudeCode); err != nil {
			return delegate.Result{}, fmt.Errorf("claw backend: %w", err)
		}
		if b.logger != nil && secrets.SubscriptionOAuthOnly(ctx, secrets.ProviderAnthropic, secrets.OAuthKindClaudeCode) {
			b.logger.Warn("[%s#%d/claw] %s", task.NodeID, task.Iteration,
				secrets.SubscriptionOAuthNotice(secrets.ProviderAnthropic))
		}
	}

	// A gateway-served model resolves its endpoint before anything is
	// dispatched — the same read the element builder makes for chain walks,
	// repeated here for the paths that never build through one (a direct
	// Execute, the sandbox runner below, a continued conversation). The
	// refusal names the variable, never a value.
	if modelroute.Parse(task.Model).Gateway() {
		if err := checkGatewayEnv(!secrets.LLMEndpointAllowPrivate()); err != nil {
			return delegate.Result{}, fmt.Errorf("claw backend: node %q: %w", task.NodeID, err)
		}
	}

	// A continued conversation is the executor's tool-less schema re-ask:
	// it runs in-process by design (see Task.ContinueConversation) and is
	// refused rather than forwarded to a runner that may predate the field.
	if len(task.ContinueConversation) > 0 && task.Sandbox != nil {
		return delegate.Result{}, fmt.Errorf("claw backend: node %q: a continued conversation runs in-process, never through the sandbox runner", task.NodeID)
	}

	// Outside the sandbox arm, deliberately. Resolution DROPS a refused
	// server's tools whatever the run's sandbox is, so the refusal that
	// compensates for the drop has to read the same fact: gated on
	// `task.Sandbox != nil` instead, the two disagreed whenever the policy
	// refused without a live sandbox — the manager's undecided state, which
	// is where a subbot child's executor starts — and the node's declared
	// MCP tool then vanished with no error, no event and no fallback.
	//
	// A populated map can only mean this launcher declined to start a server
	// the node asked for, which is a reason to refuse the ROUTE whether or
	// not a sandbox is why.
	if len(task.MCPServersRefusedOnLauncher) > 0 {
		return delegate.Result{}, &delegate.ErrCapabilityUnsupported{
			NodeID: task.NodeID, Backend: delegate.BackendClaw,
			Capability: "MCP " + refusedMCPServerSummary(task.MCPServersRefusedOnLauncher),
			// No tail appended: ServerNotStartableError.Error() already ends
			// with this exact advice, and saying it twice in one message
			// reads as two different remedies.
			Remedy: refusedMCPRemedy(task.MCPServersRefusedOnLauncher),
		}
	}

	if task.Sandbox != nil {
		// The permission gate crosses the sandbox IPC boundary as a
		// pre-task permission_policy envelope: the in-container
		// `iterion __claw-runner` rebuilds the policy through the same
		// parser and enforces it in its own tool loop (see
		// executeViaSandboxRunner). What CANNOT cross is an Ask
		// decision — the runner has no seam to pause the parent run for
		// a human — so a policy that can ever produce one (mode ask, or
		// any explicit ask rule, which outranks mode deny) is refused
		// loudly rather than silently degraded to deny; pi treats the
		// same combination as fail-not-degrade.
		if task.Permission.CanAsk() {
			return delegate.Result{}, fmt.Errorf(
				"claw backend: node %q declares a permission policy that can produce an Ask decision (mode %s), which a sandboxed runner cannot pause for — drop the ask rules / use deny, run the workflow unsandboxed (`sandbox: none` / `--sandbox none`), or route the node to claude_code",
				task.NodeID, task.Permission.Mode)
		}
		// The async question pair is served by the launcher, which does not
		// bind it to the run's question channel on this path: a task wired
		// to post async questions is refused by type — here, as one route's
		// failure, so the node's fallbacks can serve it.
		if task.PostAsyncQuestion != nil {
			return delegate.Result{}, &delegate.ErrCapabilityUnsupported{
				NodeID: task.NodeID, Backend: delegate.BackendClaw, Capability: "interaction: async in a sandboxed run"}
		}
		task.ToolDefs = withoutUnplaceableToolsThePolicyDenies(task)
		if err := refuseToolsWithNoSandboxPlacement(task); err != nil {
			return delegate.Result{}, err
		}
		return b.executeViaSandboxRunner(ctx, task)
	}

	// Resolve API client. Phase C: in cloud mode the runner stamps
	// per-tenant BYOK credentials into ctx, ResolveWithContext then
	// builds a fresh APIClient with the override key (no cache hit
	// across tenants). Local mode keeps the env-fallback path.
	client, err := b.registry.ResolveWithContext(ctx, task.Model)
	if err != nil {
		return delegate.Result{}, fmt.Errorf("claw backend: %w", err)
	}

	// The spec must name a provider; the prefix itself comes off in
	// buildRequest (wireModelID), exactly once — stripping it here too cut a
	// model id that holds slashes of its own ("meta-llama/…") short.
	if _, _, err := ParseModelSpec(task.Model); err != nil {
		return delegate.Result{}, fmt.Errorf("claw backend: %w", err)
	}
	route := modelroute.Parse(task.Model)

	// Build GenerationOptions.
	opts := GenerationOptions{
		Model:                 task.Model,
		MaxTokens:             task.MaxTokens,
		CompactThresholdRatio: task.CompactThresholdRatio,
		CompactPreserveRecent: task.CompactPreserveRecent,
		MaterializeSecrets:    task.MaterializeSecrets,
		UnmaterializeSecrets:  task.UnmaterializeSecrets,
	}

	// Reasoning effort via ProviderOptions. Coerce against the model's
	// supported matrix — claw-code-go does NOT clamp on its own, so a
	// recipe asking for "max" on an OpenAI model would otherwise reach
	// the API with an unsupported value and bounce as 400.
	if effort := coerceEffortForModel(task.ReasoningEffort, route.CapabilityID()); effort != "" {
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

	// The ambient-context policy (ADR-119): the instruction files the node
	// inherits, rendered by claw-code-go's own loader. Appended before the
	// memory blocks, whose cache_control re-mark covers the last block — this
	// one included, and it is as stable as they are.
	if ctx := clawAmbientSystemContext(task); ctx != "" {
		opts.SystemBlocks = append(opts.SystemBlocks, api.ContentBlock{Type: "text", Text: ctx})
	}

	// User message. A routing handoff composes here: the original
	// prompt's bytes stay the prefix (restart) or the closing task
	// statement (reuse), and the UserContent delta-slicing below gives
	// the section its own block when content blocks exist.
	userText := task.HandoffPrompt()

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
		// can't emit (it's appended after, not from the prompt body) - and
		// a routing handoff's section the same way: the composed text with
		// the original prompt removed ONCE, wherever it sits. A prefix
		// slice would assume the prompt opens the composition, which is
		// true for restart and false for reuse (revi R-finding on #2237).
		if userText != "" && task.UserPrompt != userText {
			if extra := delegate.HandoffExtra(userText, task.UserPrompt); extra != "" {
				blocks = append(blocks, api.ContentBlock{Type: "text", Text: extra})
			}
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

		resultText := task.ResumeAnswer
		if resultText == "" {
			resultText = "(no answer provided)"
		}
		isErr := false

		// Permission-gate resume: when the pending tool_use is a real
		// action the gate paused on (not the ask_user infra tool), the
		// operator's answer is an authorization decision, not a question
		// answer. The grant itself is applied to task.Permission by the
		// executor (from the runtime's GrantInputKey); here we just replace
		// the tool_result with the right instruction so the agent re-issues
		// the now-authorized call (which then runs through the normal tool
		// loop) or adapts on denial.
		if name, _, ok := findPendingToolUse(prior, task.ResumePendingToolUseID); ok &&
			!permission.IsInfrastructureTool(name) && task.Permission.Enabled() {
			if allow, _ := permission.ParseAnswer(task.ResumeAnswer); allow {
				resultText = "✅ The operator approved this action. Re-issue the exact same tool call now to perform it."
			} else {
				resultText = "⛔ The operator denied this action. Do not retry it; take a different approach or explain why it is needed."
				isErr = true
			}
		}

		prior = append(prior, api.Message{
			Role: "user",
			Content: []api.ContentBlock{api.ToolResult{
				ToolUseID: task.ResumePendingToolUseID,
				Content:   resultText,
				IsError:   isErr,
			}.ToContentBlock()},
		})
		opts.Messages = prior
	}

	// Continuation: a COMPLETED conversation the executor asks one more
	// question of — its schema re-ask. The prior messages are replayed as
	// they ended and the user text becomes the next turn; nothing is
	// pending, so it cannot combine with the resume form above (which
	// answers a pending tool_use). The store prepend is skipped for both
	// forms (applySessionMessagesForTask): they already carry the history
	// the store holds.
	if len(task.ContinueConversation) > 0 {
		if len(task.ResumeConversation) > 0 {
			return delegate.Result{}, fmt.Errorf("claw backend: node %q carries both a paused and a completed conversation", task.NodeID)
		}
		var prior []api.Message
		if err := json.Unmarshal(task.ContinueConversation, &prior); err != nil {
			return delegate.Result{}, fmt.Errorf("claw backend: decode continued conversation: %w", err)
		}
		if strings.TrimSpace(userText) == "" {
			return delegate.Result{}, fmt.Errorf("claw backend: node %q continues a conversation with an empty user turn", task.NodeID)
		}
		opts.Messages = append(prior, api.Message{
			Role:    "user",
			Content: []api.ContentBlock{{Type: "text", Text: userText}},
		})
	}

	// Tools.
	if len(task.ToolDefs) > 0 {
		opts.Tools = toolDefsToGeneration(task.ToolDefs)
		applyAsyncAskExecs(task, opts.Tools)
		maxSteps := task.ToolMaxSteps
		if maxSteps <= 0 {
			maxSteps = 5
		}
		opts.MaxSteps = maxSteps
		// A tool-equipped claw node is agentic by intent: it must ground its
		// answer in the workspace, not answer from priors. gpt-5.5 under the
		// default "auto" tool_choice skips its tools and fabricates a verdict
		// (0 tool calls) — the explore-mode façade that made whole_improve_loop's
		// GPT reviewer ungrounded. Force the first turn to be a tool call; the
		// loop reverts to auto once a tool lands so the model can still finish.
		opts.ForceInitialToolUse = true
	}

	// Observability hooks.
	applyHooks(task.NodeID, task.Iteration, b.hooks, &opts)

	// In-process lifecycle hooks (audit, safety, compaction
	// observability). Nil-safe at call sites in generation.go.
	opts.Hooks = b.lifecycleHooks
	// Plugin `hooks` parity: when the workspace's .claude/settings.json carries
	// command-type hooks (merged there from enabled hook plugins, the same file
	// claude_code reads via --setting-sources project), run them through claw's
	// hook Runner too. A fresh per-run runner = default lifecycle + settings
	// hooks (only when present), so the shared runner never accumulates per-run
	// handlers. On the launcher's sandboxed path Execute returned above; the
	// in-container runner re-enters HERE with the launcher-parsed document on
	// task.SettingsHooks — the host's WorkDir may not exist in the container
	// (a workspace not mounted at its host path), so the wired document takes
	// precedence over re-reading the file.
	if task.WorkDir != "" || len(task.SettingsHooks) > 0 {
		merged := NewDefaultLifecycleHooks(b.hooks)
		wired := 0
		if len(task.SettingsHooks) > 0 {
			wired = registerSettingsHooksJSON(merged, task.SettingsHooks, b.logger)
		} else {
			wired = registerSettingsHooks(merged, task.WorkDir, b.logger)
		}
		if wired > 0 {
			opts.Hooks = merged
		}
	}

	// Tool-permission gate (anti-prompt-injection boundary). The
	// resolved policy is evaluated before each tool executes; see
	// pkg/backend/permission and generation.go's executeToolsDirect.
	opts.Permission = task.Permission

	// Wire the operator-chatbox inbox if the runtime configured one.
	// Resolved per-call so a backend shared across runs picks up the
	// right hook for each run.
	if b.inbox != nil {
		if runID := RunIDFromContext(ctx); runID != "" {
			opts.Inbox = b.inbox.Bind(ctx, runID)
		}
	}

	if m := task.Memory; m != nil {
		// Resolve the memory base path: project_root re-roots the scope
		// under the run's RepoRoot so dispatcher worktrees + Nexie share
		// the same tree; falls back to WorkDir when RepoRoot is empty
		// (legacy / non-worktree runs). LegacyBotRef encodes that base
		// into a bot-visibility SpaceRef pointing at the identical
		// on-disk path the pre-knowledge layout used.
		memBase := task.WorkDir
		if (m.ProjectRoot || m.Visibility != "") && task.RepoRoot != "" {
			memBase = task.RepoRoot
		}
		var ref knowledge.SpaceRef
		if m.Visibility != "" {
			// Structured space: resolve the sharing axis against the run's
			// identity (tenant/owner from ctx, project from memBase).
			tenant, _ := store.TenantFromContext(ctx)
			owner, _ := store.OwnerFromContext(ctx)
			ref = memory.ResolveSpaceRef(knowledge.Visibility(m.Visibility), m.Scope, m.BotID, "", memory.SpaceRefInputs{
				TenantID:  tenant,
				UserID:    owner,
				ProjectID: memory.ProjectKey(memBase),
				BotID:     m.BotID,
			})
		} else {
			ref = memory.LegacyBotRef(memBase, m.Scope)
		}
		var memStore = b.memStore
		if memStore == nil {
			memStore = memory.DefaultFSStore()
		}
		if err := installWorkspaceMemory(ctx, &opts, memStore, ref, m); err != nil {
			return delegate.Result{}, fmt.Errorf("claw backend: memory: %w", err)
		}
	}

	// Dispatch to the appropriate generation strategy. A gateway route's
	// catalog record is resolved ONCE, HERE, before anything runs, and
	// carried immutable on everything the invocation produces (ADR-122
	// §resolution): the provenance rides the output (`_gateway_spec`) and
	// the window the gateway serves is the one the consumers see — never a
	// vendor's window borrowed by name.
	hasSchema := task.OutputSchema != nil
	gwRoute := modelroute.Parse(task.Model)
	var gwResolved compatgw.Resolved
	if gwRoute.Gateway() {
		gwResolved = compatgw.ResolveCatalog(gwRoute.Wire, modelspecs.Default(), os.Getenv)
	}
	var (
		res    delegate.Result
		genErr error
	)
	switch {
	case hasSchema && !task.HasTools:
		res, genErr = b.generateStructuredWithRetry(ctx, client, task, opts)
	case hasSchema && task.HasTools:
		res, genErr = b.generateTextWithToolsAndSchemaRetry(ctx, client, task, opts)
	default:
		res, genErr = b.generateTextWithRetry(ctx, client, task, opts)
	}
	if gwRoute.Gateway() {
		res.Output = stampGatewaySpec(res.Output, gwResolved)
		if gwResolved.Known() && gwResolved.Spec.ContextWindow > 0 && res.ContextWindow == 0 {
			res.ContextWindow = gwResolved.Spec.ContextWindow
		}
	}
	return res, genErr
}

// ---------------------------------------------------------------------------
// Retry
// ---------------------------------------------------------------------------

func (b *ClawBackend) retryLoop(ctx context.Context, nodeID string, fn func() (delegate.Result, error)) (delegate.Result, error) {
	result, err := fn()
	for attempt := 1; err != nil && isRetryable(err); attempt++ {
		// Error-adaptive budget: a connectivity failure gets the larger
		// transient budget to ride out a brief outage.
		maxAttempts := b.retry.effectiveMaxAttempts(err)
		if attempt >= maxAttempts {
			break
		}
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
			// The retry never happens, so the attempt waiting in `result` is
			// the last one and what it burned is the node's final bill.
			// Zeroing it here dropped exactly the spend the loop had just
			// finished metering (the cancel-during-backoff case the engine
			// books at its own frame).
			return result, ctx.Err()
		}

		prev := result
		result, err = fn()
		// The attempt that just failed was BILLED — that is what the
		// metered-failure results above are for — and overwriting it here
		// dropped exactly the figure this loop's own cancel arm goes to the
		// trouble of keeping. The shape that loses the most is the common
		// one: a tool loop that ran to its step limit and then hit a 429,
		// retried into an instant auth failure that billed nothing, reporting
		// the free attempt as the node's whole bill.
		//
		// SUMMED, never folded at a MAX: claw opens a fresh conversation per
		// attempt (it never reads SessionID — it replays from the run's own
		// store), so no attempt's figure contains another's, and
		// cost.Annotate prices each from its own tokens. `false` states that
		// invariant at the call site rather than inferring it. The frame
		// above applies the same rule (retryDelegateLoop).
		result = foldSpend(prev, result, false)
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
	questions := map[string]any{
		delegate.AskUserQuestionKey: ask.Question,
	}
	delegate.AddAskUserOptionKeys(questions, ask.Options, ask.AllowFreeText)
	if ask.PermissionMarker != nil {
		questions[permission.InteractionMarkerKey] = ask.PermissionMarker
	}
	if len(ask.AwaitPending) > 0 {
		questions[delegate.AwaitPendingInteractionsKey] = delegate.AwaitPendingToQuestions(ask.AwaitPending)
	}
	return delegate.Result{
		Output: map[string]any{
			"_needs_interaction":     true,
			"_interaction_questions": questions,
		},
		BackendName:         delegate.BackendClaw,
		PendingConversation: ask.Conversation,
		PendingToolUseID:    ask.PendingToolUseID,
	}, true
}

// meteredFailure renders what an ABANDONED generation already burned, in the
// shape the engine books from (`_tokens` / `_cost_usd` on the output map).
//
// claw is an in-process client, not a CLI: nothing outside this package sees
// its usage unless a delegate.Result carries it. GenerateTextDirect
// deliberately returns a partial TextResult beside its error — every step
// before the failing one was a real, billed request — and returning a bare
// delegate.Result{} threw that away, so a tool loop that died on its
// twentieth step reported the same zero as one that never reached the
// provider. The engine books a failed node's spend from this map, so an
// empty one is silently free work.
//
// Zero usage yields the zero Result: an empty output map with a `_tokens: 0`
// stamp would read as an output rather than as a bill, and the engine's own
// guard already skips a spendless failure. A call whose usage went
// unreported is no such zero: its bill is unknown, and the map says so.
func meteredFailure(task delegate.Task, usage Usage) delegate.Result {
	if usage.InputTokens == 0 && usage.OutputTokens == 0 && usage.UnreportedCalls == 0 {
		return delegate.Result{}
	}
	output := map[string]any{}
	tokens := annotateUsage(output, task.Model, usage)
	return delegate.Result{
		Output:         output,
		Tokens:         tokens,
		BackendName:    delegate.BackendClaw,
		ThinkingTokens: usage.ReasoningTokens,
		ThinkingMs:     usage.ThinkingMs,
	}
}

// annotateUsage stamps a generation's usage onto its output: the `_tokens` /
// `_model` / `_cost_usd` keys, and the count of calls whose usage the
// provider did not report, for which those figures are a lower bound.
func annotateUsage(output map[string]any, model string, u Usage) int {
	cost.SetUnreportedCalls(output, u.UnreportedCalls)
	return cost.Annotate(output, model, u.InputTokens, u.OutputTokens)
}

// partialUsage reads the usage off a best-effort partial result, tolerating
// the nil an early failure returns.
func partialUsage(r *TextResult) Usage {
	if r == nil {
		return Usage{}
	}
	return r.TotalUsage
}

// objectUsage is partialUsage for a structured generation.
func objectUsage[T any](r *ObjectResult[T]) Usage {
	if r == nil {
		return Usage{}
	}
	return r.TotalUsage
}

func (b *ClawBackend) generateStructured(ctx context.Context, client api.APIClient, task delegate.Task, opts GenerationOptions) (delegate.Result, error) {
	// Set the explicit schema for structured output.
	genOpts := opts
	genOpts.ExplicitSchema = task.OutputSchema
	genOpts = applySessionMessagesForTask(ctx, task, genOpts)

	result, err := GenerateObjectDirect[map[string]any](ctx, client, genOpts)
	if err != nil {
		if r, ok := askUserResult(err); ok {
			return r, nil
		}
		return meteredFailure(task, objectUsage(result)), fmt.Errorf("claw backend: structured generation: %w", err)
	}
	captureSessionMessages(ctx, taskSessionKey(task), &TextResult{Messages: result.Messages})

	output := result.Object
	if output == nil {
		output = make(map[string]any)
	}

	tokens := annotateUsage(output, task.Model, result.TotalUsage)

	return delegate.Result{
		Output:         output,
		Tokens:         tokens,
		BackendName:    delegate.BackendClaw,
		ThinkingTokens: result.TotalUsage.ReasoningTokens,
		ThinkingMs:     result.TotalUsage.ThinkingMs,
	}, nil
}

func (b *ClawBackend) generateTextWithRetry(ctx context.Context, client api.APIClient, task delegate.Task, opts GenerationOptions) (delegate.Result, error) {
	return b.retryLoop(ctx, task.NodeID, func() (delegate.Result, error) {
		return b.generateText(ctx, client, task, opts)
	})
}

func (b *ClawBackend) generateText(ctx context.Context, client api.APIClient, task delegate.Task, opts GenerationOptions) (delegate.Result, error) {
	opts = applySessionMessagesForTask(ctx, task, opts)
	result, err := GenerateTextDirect(ctx, client, opts)
	captureSessionMessages(ctx, taskSessionKey(task), result)
	if err != nil {
		if r, ok := askUserResult(err); ok {
			return r, nil
		}
		return meteredFailure(task, partialUsage(result)), fmt.Errorf("claw backend: text generation: %w", err)
	}

	output := map[string]any{"text": result.Text}
	tokens := annotateUsage(output, task.Model, result.TotalUsage)

	return delegate.Result{
		Output:         output,
		Tokens:         tokens,
		BackendName:    delegate.BackendClaw,
		ThinkingTokens: result.TotalUsage.ReasoningTokens,
		ThinkingMs:     result.TotalUsage.ThinkingMs,
	}, nil
}

// countToolCalls returns how many tool calls the model made across every
// step of an agentic-loop result.
func countToolCalls(r *TextResult) int {
	if r == nil {
		return 0
	}
	n := 0
	for i := range r.Steps {
		n += len(r.Steps[i].ToolCalls)
	}
	return n
}

// looksStructured reports whether text already carries a JSON object —
// i.e. the model committed to a structured verdict on its own. Used to
// skip the tool-use nudge for a node that answered directly from inline
// context rather than narrating an unfinished plan. extractJSON returns
// the {...} object substring (or "" when none is present, which json.Valid
// rejects), so a valid result here means a real structured payload.
func looksStructured(text string) bool {
	return json.Valid([]byte(extractJSON(text)))
}

// toolUseReminder is the one-shot nudge sent to a tool-equipped model
// that concluded without calling any tool, asking it to gather evidence
// before answering — or to finalize now if its context is already
// sufficient.
func toolUseReminder() api.Message {
	return api.Message{
		Role: "user",
		Content: []api.ContentBlock{{
			Type: "text",
			Text: systemReminder("You ended without using any of your available tools. If your task " +
				"requires inspecting files, running commands, or reading a diff that is " +
				"not already present in this conversation, you MUST use your tools now to " +
				"gather that evidence before producing your final answer. If everything " +
				"you need is already in the conversation above, output your final " +
				"structured result now."),
		}},
	}
}

// finalizeReminder is appended to the recovery (formatting) pass so a tool
// loop that ended without committing to JSON — whether it narrated instead of
// answering, or was cut off at the MaxSteps limit mid-task — is pushed to emit
// a FINAL, honest structured result rather than a provisional placeholder. The
// recovery pass is schema-forced with no tools, so the model cannot do more
// work; it must report the state it actually reached. A "work in progress" /
// "still validating" verdict otherwise freezes a placeholder into convergence
// loops (observed with a gpt-5.5 fixer cut off at the step limit mid-fix —
// run 019ec9d5: it reported applied=false + "validating… before finalizing"
// even though it had edited files).
func finalizeReminder() api.Message {
	return api.Message{
		Role: "user",
		Content: []api.ContentBlock{{
			Type: "text",
			Text: systemReminder("This is your FINAL turn — you cannot run any more tools. Output " +
				"your structured result now, reflecting the state you have ACTUALLY " +
				"reached. Do NOT return a provisional 'work in progress', 'still " +
				"validating', or 'will continue' placeholder. Fill every required " +
				"field with your real current result: if you applied changes, say so " +
				"and summarise exactly what you did and what (if anything) remains; if " +
				"you only inspected, report your actual findings."),
		}},
	}
}

func (b *ClawBackend) generateTextWithToolsAndSchemaRetry(ctx context.Context, client api.APIClient, task delegate.Task, opts GenerationOptions) (delegate.Result, error) {
	return b.retryLoop(ctx, task.NodeID, func() (delegate.Result, error) {
		return b.generateTextWithToolsAndSchema(ctx, client, task, opts)
	})
}

func (b *ClawBackend) generateTextWithToolsAndSchema(ctx context.Context, client api.APIClient, task delegate.Task, opts GenerationOptions) (delegate.Result, error) {
	opts = applySessionMessagesForTask(ctx, task, opts)
	result, err := GenerateTextDirect(ctx, client, opts)
	captureSessionMessages(ctx, taskSessionKey(task), result)
	if err != nil {
		if r, ok := askUserResult(err); ok {
			return r, nil
		}
		return meteredFailure(task, partialUsage(result)), fmt.Errorf("claw backend: text+tools generation: %w", err)
	}

	// A tool-equipped reviewer/judge that ended the loop WITHOUT calling a
	// single tool — and without already committing to a JSON verdict — has
	// almost certainly narrated a plan ("I'll review the diff…") and
	// stopped before doing the work (observed with gpt-5.5 at high
	// reasoning, and a likely outcome of a stalled/truncated stream).
	// Letting the recovery pass below coerce that narration into the
	// schema freezes a "still in progress" placeholder into the output —
	// fatal for any convergence loop that needs a real cross-family
	// approval. Give the model ONE explicit nudge to use its tools, then
	// re-run the loop. No-op on the healthy path: a model that already
	// emitted a JSON verdict (looksStructured) or that used its tools is
	// left untouched, so a reviewer whose data is inline (e.g.
	// whole_improve_loop's chunk_content) never pays for it.
	if task.HasTools && countToolCalls(result) == 0 && !looksStructured(result.Text) {
		nudged := opts
		// The nudge is the harness's re-ask, not the operator's prompt —
		// already screened on the first pass.
		nudged.SkipUserPromptSubmit = true
		nudged.Messages = append(append([]api.Message(nil), result.Messages...), toolUseReminder())
		if b.hooks.OnLLMRequest != nil {
			b.hooks.OnLLMRequest(task.NodeID, LLMRequestInfo{
				Model:        task.Model,
				WireModel:    wireModelIfDistinct(task.Model),
				MessageCount: len(nudged.Messages),
				Timestamp:    time.Now(),
			})
		}
		reRun, reErr := GenerateTextDirect(ctx, client, nudged)
		switch reErr {
		case nil:
			// Carry the wasted first-pass usage into the re-run so cost
			// accounting reflects both turns.
			accumulateUsage(&reRun.TotalUsage, result.TotalUsage)
			result = reRun
			captureSessionMessages(ctx, taskSessionKey(task), result)
		default:
			// A failure DURING the nudge must not be silently swallowed:
			// otherwise the degenerate first-pass result falls through to
			// the recovery coercion below and re-creates the placeholder
			// verdict this guard exists to prevent. An ask_user surfaces as
			// a pause; a transient/network failure propagates so the outer
			// retryLoop re-issues the whole turn; only a permanent failure
			// (e.g. context overflow — retrying won't help) falls through to
			// recovery with the original result.
			if r, ok := askUserResult(reErr); ok {
				return r, nil
			}
			if isRetryable(reErr) {
				// The tool loop AND the nudge were both billed before this
				// gave up; the outer retryLoop re-issues the whole turn, and
				// if that one fails too this is the figure the node reports.
				abandoned := result.TotalUsage
				accumulateUsage(&abandoned, partialUsage(reRun))
				return meteredFailure(task, abandoned), fmt.Errorf("claw backend: nudge re-run: %w", reErr)
			}
			// Falling through to recovery: the nudge was billed too, so its
			// partial usage rides the first pass's into every exit below.
			accumulateUsage(&result.TotalUsage, partialUsage(reRun))
		}
	}

	text := strings.TrimSpace(result.Text)
	text = extractJSON(text)

	// Try the cheap path first: parse the tool-loop's final text as JSON.
	// If the model already committed to structured output, we're done.
	if text != "" {
		var output map[string]any
		if err := json.Unmarshal([]byte(text), &output); err == nil {
			tokens := annotateUsage(output, task.Model, result.TotalUsage)
			return delegate.Result{
				Output:         output,
				Tokens:         tokens,
				BackendName:    delegate.BackendClaw,
				ThinkingTokens: result.TotalUsage.ReasoningTokens,
				ThinkingMs:     result.TotalUsage.ThinkingMs,
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
	// The recovery pass is the harness's formatting re-ask: screening it
	// would fire UserPromptSubmit a second time on text the operator never
	// wrote, and a Block there is swallowed by the fall-through below.
	recoveryOpts.SkipUserPromptSubmit = true
	// Append finalizeReminder so the schema-forced pass reports the state the
	// model actually reached instead of coercing a "work in progress"
	// placeholder (run 019ec9d5). Copy result.Messages rather than mutate it.
	recoveryOpts.Messages = append(append([]api.Message(nil), result.Messages...), finalizeReminder())
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
			WireModel:    wireModelIfDistinct(task.Model),
			MessageCount: len(recoveryOpts.Messages),
			Timestamp:    time.Now(),
		})
	}
	obj, recErr := GenerateObjectDirect[map[string]any](ctx, client, recoveryOpts)
	if recErr == nil && obj != nil && obj.Object != nil {
		both := result.TotalUsage
		accumulateUsage(&both, obj.TotalUsage)
		tokens := annotateUsage(obj.Object, task.Model, both)
		return delegate.Result{
			Output:             obj.Object,
			Tokens:             tokens,
			BackendName:        delegate.BackendClaw,
			FormattingPassUsed: true,
			ThinkingTokens:     result.TotalUsage.ReasoningTokens + obj.TotalUsage.ReasoningTokens,
			ThinkingMs:         result.TotalUsage.ThinkingMs + obj.TotalUsage.ThinkingMs,
		}, nil
	}

	// BOTH exits below are past the recovery pass, and both owe its bill: it
	// is a separate, fully-billed provider call, and its usage reaches here
	// only because GenerateObjectDirect hands back a partial beside its error
	// (a bare nil made a billed call indistinguishable from one that never
	// left the process). The success path above sums the two for the same
	// reason. ONE figure for both exits — deriving it twice is how the
	// text-bearing one came to be priced from the tool loop alone.
	billed := result.TotalUsage
	if obj != nil {
		accumulateUsage(&billed, obj.TotalUsage)
	}

	// Last-ditch: surface whatever text we got as a parse-fallback so
	// the runtime's existing structured-output retry path can decide
	// what to do. The error from the recovery pass is logged for
	// post-mortem.
	if text == "" {
		// The most expensive failure claw has: a whole agentic tool loop ran
		// (possibly to MaxSteps) and the schema-forced recovery pass ran on
		// top of it, and the node has nothing to show for either.
		return meteredFailure(task, billed), fmt.Errorf("claw backend: text+tools generation produced empty response after tool loop and structured-output recovery failed: %v", recErr)
	}
	output := map[string]any{"text": text}
	tokens := annotateUsage(output, task.Model, billed)
	return delegate.Result{
		Output:         output,
		Tokens:         tokens,
		BackendName:    delegate.BackendClaw,
		ParseFallback:  true,
		ThinkingTokens: billed.ReasoningTokens,
		ThinkingMs:     billed.ThinkingMs,
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
	runnerEnv, err := forwardableProviderEnv(ctx, task.Model)
	if err != nil {
		return delegate.Result{}, err
	}
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

	// A gated node ships its policy BEFORE the task envelope. The
	// position is the fail-closed guarantee on a mixed-version fleet: a
	// runner binary too old to know the type fatals on "unexpected
	// envelope before task" instead of executing the node with an empty
	// policy. Execute() already refused any policy that can Ask.
	if task.Permission.Enabled() {
		permEnv, err := delegate.NewPermissionPolicyEnvelope(task.Permission.Config())
		if err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return delegate.Result{}, fmt.Errorf("claw backend: build permission_policy envelope: %w", err)
		}
		if err := mux.Send(permEnv); err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return delegate.Result{}, fmt.Errorf("claw backend: send permission_policy: %w", err)
		}
	}

	// The gateway capability marker, before the task (F19): a runner too
	// old to know the type fatals on "unexpected envelope before task" —
	// every gateway node on a stale image fails closed, opaquely no more.
	// An old host cannot produce gateway tasks at all (no factory), so the
	// reverse skew is vacuous.
	if modelroute.Parse(task.Model).Gateway() {
		if err := mux.Send(delegate.NewGatewayV1Envelope()); err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return delegate.Result{}, fmt.Errorf("claw backend: send gateway_v1: %w", err)
		}
	}

	// Send the task envelope. The runner blocks on its
	// EnvelopeReader.Read() until this arrives. The workspace's
	// .claude/settings.json hooks ride along: the launcher reads them on the
	// host and the in-container runner registers the document instead of
	// re-reading a WorkDir that may not exist inside the container (a
	// workspace not mounted at its host path would otherwise fire NO hooks
	// in silence — the parity gap of #1715). An old runner ignores the
	// unknown field and falls back to its own WorkDir read.
	ioTask := delegate.ToIOTask(task)
	ioTask.SettingsHooks = settingsHooksForWire(task.WorkDir, task.NodeID, task.Iteration, b.logger)
	taskEnv, err := delegate.NewTaskEnvelope(ioTask)
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

// settingsHooksForWire reads the host's .claude/settings.json hooks document
// for the sandbox crossing and emits every diagnostic on the LAUNCHER side —
// the in-container registration's warnings go to the container's stderr,
// which the launcher surfaces only on failure, so on a successful sandboxed
// run this pass is the diagnostic's only channel. Returns nil when there is
// nothing useful to ship (no document, or one claw cannot parse — a document
// of the wrong JSON type is warned about here, never shipped in silence).
func settingsHooksForWire(workDir, nodeID string, iteration int, logger *iterlog.Logger) json.RawMessage {
	doc, err := readSettingsHooksDoc(workDir)
	if err != nil {
		if logger != nil {
			logger.Warn("[%s#%d/claw] parse .claude/settings.json hooks: %v — the sandboxed node fires no settings hooks",
				nodeID, iteration, err)
		}
		return nil
	}
	if len(doc) == 0 {
		return nil
	}
	var hooksDoc map[string][]settingsHookGroup
	if err := json.Unmarshal(doc, &hooksDoc); err != nil {
		if logger != nil {
			logger.Warn("[%s#%d/claw] .claude/settings.json hooks: %v — the sandboxed node fires no settings hooks",
				nodeID, iteration, err)
		}
		return nil
	}
	diagnoseSettingsHooks(hooksDoc, logger)
	return doc
}

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
	// z.ai (GLM) drives claw's Anthropic provider through an
	// Anthropic-compatible endpoint, so a sandboxed claw reviewer (e.g.
	// anthropic/glm-5.2) needs the z.ai creds inside the container.
	// registry.go synthesises the bearer + ZAIDefaultBaseURL from
	// ZAI_API_KEY when no other anthropic auth is present;
	// ANTHROPIC_AUTH_TOKEN/ANTHROPIC_BASE_URL cover the explicit BYOK path.
	//
	// claw CAN use a Claude Code OAuth forfait — its anthropic provider takes
	// the token as an OAuth bearer, and registry.go feeds it one from the env
	// (desktop) or from the run's materialised OAuthDir (pod). What is missing
	// is a channel to carry it ACROSS this boundary: the in-container
	// __claw-runner rebuilds its registry from env alone, and this list is the
	// only credential channel, so a sandboxed claw anthropic node still falls
	// back to the ambient ANTHROPIC_API_KEY. Tracked as the sandbox seam of
	// the forfait work; seeding an in-container CLAUDE_CONFIG_DIR from the
	// existing ClaudeCodeSandboxConfigDir mount (mirroring CODEX_HOME) is the
	// shape that closes it.
	"ZAI_API_KEY",
	// Moonshot's factory reads BOTH the key and the base URL that decides
	// WHICH gateway it is spent on (registry.go moonshotBaseURL: the .cn
	// endpoint, an operator proxy). A missing key is loud — the in-container
	// factory refuses by name — but a missing base URL is not: the node keeps
	// working against the published endpoint while the host talks to the
	// operator's, one node on two vendors' infrastructure with nothing said.
	"MOONSHOT_API_KEY",
	"MOONSHOT_BASE_URL",
	// The OpenAI twin: the endpoint a key is spent on, and the one setting
	// that keeps a ChatGPT forfait off a third-party gateway
	// (openAIOAuthAllowed) — the host and the container must read the same.
	"OPENAI_BASE_URL",
	// xai's provider reads XAI_API_KEY from env, so this is the only
	// channel into the container — and the pool can grant a donated xai
	// key, which is METERED and billed to its lender.
	"XAI_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
	"ANTHROPIC_BASE_URL",
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
	// The subscription opt-out is a spend policy, so it must hold inside the
	// container too — the in-container runner rebuilds its own registry and
	// would otherwise resolve the forwarded subscription token as if the
	// switch were off.
	"ITERION_FORBID_SUBSCRIPTION_OAUTH",
	// Client-identity overrides — must reach the in-container runner too
	// (docs/backends.md § Client identity).
	"ITERION_LLM_USER_AGENT",
	"CLAW_USER_AGENT",
	"ANTHROPIC_CUSTOM_HEADERS",
	// The ChatGPT-forfait wire gates model availability on the codex-cli
	// `version:` header. The sandbox image ships no codex binary, so the
	// in-container runner's `codex --version` probe finds nothing and
	// falls back to claw's baked-in version — which the backend then
	// refuses for newer models ("gpt-5.6-sol requires a newer codex-cli").
	// The operator override must therefore cross the boundary.
	"ITERION_CODEX_VERSION",
}

// hostCodexVersion resolves the codex-cli version on the HOST side of the
// sandbox boundary (env override, then `codex --version`). A var so tests
// can stub the probe.
var hostCodexVersion = codexCLIVersion

// byokEnvVar names the environment variable claw's registry reads for a
// provider's key inside the container. A provider absent here has no
// env-based BYOK path, so a tenant key for it cannot be forwarded.
var byokEnvVar = map[secrets.Provider]string{
	secrets.ProviderOpenAI:    "OPENAI_API_KEY",
	secrets.ProviderAnthropic: "ANTHROPIC_API_KEY",
	secrets.ProviderAzure:     "AZURE_OPENAI_API_KEY",
	secrets.ProviderZAI:       "ZAI_API_KEY",
	secrets.ProviderMoonshot:  "MOONSHOT_API_KEY",
	secrets.ProviderXAI:       "XAI_API_KEY",
}

// forwardableProviderEnv builds the env map ClawBackend hands to
// sandbox.Run.Command so the in-container runner can reach the same
// provider APIs as the host. Empty entries are skipped — we never
// inject a name=<empty> pair, since some providers treat that as
// "auth attempted but invalid" instead of "no auth".
//
// The RUN's own resolved credentials override the ambient ones. Inside
// the container the runner rebuilds its registry from env alone — ctx
// never crosses the process boundary — so without this a sandboxed node
// silently authenticates as the HOST: a tenant's BYOK key is ignored in
// favour of the pod's platform key, and a lent subscription is bypassed
// entirely while its donor's lease has already been taken. The failure is
// invisible whenever the ambient key happens to work, which is the worst
// possible shape for a billing boundary.
func forwardableProviderEnv(ctx context.Context, model string) (map[string]string, error) {
	env := map[string]string{}
	for _, name := range providerCredentialEnvVars {
		if v := os.Getenv(name); v != "" {
			env[name] = v
		}
	}
	// A gateway-served node carries the gateway env, and ONLY a gateway
	// node does: the endpoint is operator infrastructure, the key to it is
	// not a vendor credential, and no other node has business seeing
	// either. The endpoint is validated (the host already refused an
	// unusable one at Execute's head — this is the same read, on the map
	// that crosses the IPC), and the in-container factory's guard is told
	// the operator's choice explicitly rather than left to re-derive it
	// from whatever the sandbox env happens to carry.
	if modelroute.Parse(model).Gateway() {
		cfg, err := compatgw.FromEnv(os.Getenv)
		if err != nil {
			return nil, err
		}
		allowPrivate := secrets.LLMEndpointAllowPrivate()
		if err := cfg.Validate(!allowPrivate); err != nil {
			return nil, err
		}
		env[compatgw.BaseURLEnv] = cfg.BaseURL
		if cfg.APIKey != "" {
			env[compatgw.APIKeyEnv] = cfg.APIKey
		}
		if allowPrivate {
			env["ITERION_LLM_ENDPOINT_ALLOW_PRIVATE"] = "1"
		}
		// The host's OWN catalog resolution rides along, so the in-container
		// catalog answers with the same record the host answered with — the
		// image's snapshot can be older than the host's, and a window or a
		// price quietly differing across the IPC is a lie one side tells the
		// other. Top precedence container-side (compatgw.ResolvedEnvRecord).
		if resolved := compatgw.ResolveCatalog(modelroute.Parse(model).Wire, modelspecs.Default(), os.Getenv); resolved.Known() {
			if raw, merr := json.Marshal(struct {
				Model            string  `json:"model"`
				Source           string  `json:"source"`
				ContextWindow    int     `json:"context_window,omitempty"`
				MaxOutputTokens  int     `json:"max_output_tokens,omitempty"`
				InputUSDPerMTok  float64 `json:"input_usd_per_mtok,omitempty"`
				OutputUSDPerMTok float64 `json:"output_usd_per_mtok,omitempty"`
			}{
				Model:            modelroute.Parse(model).Wire,
				Source:           resolved.Source,
				ContextWindow:    resolved.Spec.ContextWindow,
				MaxOutputTokens:  resolved.Spec.MaxOutputTokens,
				InputUSDPerMTok:  resolved.Spec.InputCostPerM,
				OutputUSDPerMTok: resolved.Spec.OutputCostPerM,
			}); merr == nil {
				env["ITERION_OPENAI_COMPATIBLE_RESOLVED"] = string(raw)
			}
		}
	}
	// No ITERION_CODEX_VERSION override set: forward the HOST-resolved
	// codex-cli version as a PROBE (ITERION_CODEX_HOST_VERSION), not as the
	// decision. The in-container runner cannot probe `codex --version`
	// itself (the sandbox image ships no codex binary); it keeps the newer
	// of this probe and its own baked release, so neither a stale host
	// binary nor a stale image can pin the ChatGPT-forfait identity below
	// what either side would present alone.
	if env["ITERION_CODEX_VERSION"] == "" {
		if v := hostCodexVersion(); v != "" {
			env[codexHostVersionEnv] = v
		}
	}
	creds, ok := secrets.CredentialsFromContext(ctx)
	if !ok {
		return env, nil
	}
	for provider, key := range creds.APIKeys {
		if key == "" {
			continue
		}
		if name := byokEnvVar[provider]; name != "" {
			env[name] = key
		}
	}
	// A PINNED key (secrets.RunBundle.PinnedAPIKeys) crosses only for the
	// node that names its provider in its model spec — `moonshot/kimi-k2`
	// carries MOONSHOT_API_KEY into the container, an `anthropic/…` node in
	// the same run does not see it. The funded provider stays the one
	// clawPinnedProvider derives from the spec: claw's `provider:` hint
	// narrows an anthropic route to Anthropic-direct for models Anthropic
	// serves, but it never names a different credential slot — and a GLM id
	// on the anthropic wire is a no-op for it (Anthropic does not serve
	// GLM): z.ai-funded when a z.ai key is reachable, the anthropic-wire
	// env auth otherwise, exactly as without a hint (registry.go's hint
	// branch). Forwarding the key to every
	// node would put a credential provisioned for one route into the
	// environment of all of them. After the run's own key of that
	// provider, never over it: a tenant's instrument outranks the
	// deployment's, as it does in process (APIKeyForRoute).
	if prov := clawPinnedProvider(model); prov != "" && creds.APIKey(prov) == "" {
		if k := creds.PinnedAPIKey(prov); k != "" {
			if name := byokEnvVar[prov]; name != "" {
				env[name] = k
			}
		}
	}
	// A resolved ChatGPT forfait (the tenant's own, or one lent through the
	// credential pool) is delivered into the sandbox as a file by
	// runtime.addCodexOAuthSecretFile. Point the in-container runner at it
	// and force the OAuth path: the ambient OPENAI_API_KEY would otherwise
	// win by the documented "an explicit env key is deliberate" rule, which
	// is true of a machine default and false of a credential resolved FOR
	// THIS RUN.
	if creds.OAuthDir(string(secrets.OAuthKindCodex)) != "" {
		env["CODEX_HOME"] = secrets.CodexSandboxConfigDir
		// Forced ONLY when the run resolved no OpenAI key of its own. A
		// tenant can hold both a BYOK key and a connected forfait, and the
		// host resolver spends the KEY in that case — forcing here would
		// make the same run spend the opposite instrument depending on
		// whether it happened to be sandboxed, quietly draining a
		// subscription the tenant did not choose to use.
		// …and never against the operator's explicit kill switch:
		// ITERION_OPENAI_USE_OAUTH=0 is a machine-wide refusal to spend any
		// subscription, which a per-run credential does not get to overrule.
		// The run's DEFAULT openai key only: a key a shared tier sealed for
		// the route alone comes after the ChatGPT forfait, which claw spends
		// on its plan — the order ResolveWithContext applies in process,
		// under the same openAIOAuthAllowed.
		nodeOwnKey := creds.APIKeys[secrets.ProviderOpenAI]
		if nodeOwnKey == "" && openAIOAuthAllowed() {
			env["ITERION_OPENAI_USE_OAUTH"] = "1"
		}
	}
	// The refusal crosses whatever the run holds — the in-container factory
	// would otherwise spend the forfait the host was told never to. Only the
	// refusal: a host-wide "1" forces the forfait over the ENV key, and here
	// the run's own key crosses as that env key.
	if os.Getenv("ITERION_OPENAI_USE_OAUTH") == "0" {
		env["ITERION_OPENAI_USE_OAUTH"] = "0"
	}
	// The Anthropic twin (#736), and only for a node the forfait can actually
	// serve: a claude model on claw's anthropic provider. A z.ai/GLM model
	// rides that provider too — it arrives as "anthropic/glm-X" and registry.go
	// SYNTHESISES z.ai's base URL from a bare ZAI_API_KEY — so the wire check
	// below cannot see it: there is no ANTHROPIC_BASE_URL to inspect. Clearing
	// ZAI_API_KEY for such a node would remove its only credential channel and
	// leave the forfait bearer asking api.anthropic.com for a GLM model it
	// cannot serve; a node on another provider (openrouter/…) has no use for
	// the Claude forfait at all, and an expired one must not fail it.
	// clawPinnedProvider answers both: zai for a GLM spec, the spec's own
	// provider otherwise.
	if clawPinnedProvider(model) == secrets.ProviderAnthropic {
		if err := applyForfaitAcrossSandbox(env, creds, model); err != nil {
			return nil, err
		}
	}
	return env, nil
}

// claw declares no forfait-first provider (delegate.RegisterForfaitFirst).
// On `openai/…` it does spend the ChatGPT forfait before a key pinned for the
// route — but only while the RUNNER lets it (openAIOAuthAllowed, a
// ChatGPT-mode blob), which the server's accounting cannot see; a declaration
// there would leave a key the runner spends unstamped. On `anthropic/…` the
// key comes first: a Claude forfait on claw is billed as extra usage.

// clawPinnedProvider names the provider a claw model spec PINS — the
// `<provider>/` prefix, lower-cased — or "" when the spec carries none or
// names something no credential slot answers to. It is deliberately strict:
// it gates a credential, so an unreadable spec must yield nothing rather
// than a guess.
func clawPinnedProvider(model string) secrets.Provider {
	name, _, err := ParseModelSpec(strings.TrimSpace(model))
	if err != nil {
		return ""
	}
	// `anthropic/glm-*` rides claw's anthropic provider but is served by
	// z.ai: the key its route is funded with is z.ai's (prefixOrWiden pins
	// zai for it), and ZAI_API_KEY is what the in-container registry
	// synthesises z.ai's base URL from.
	if GLMOnAnthropicWire(model) {
		return secrets.ProviderZAI
	}
	prov := secrets.Provider(strings.ToLower(strings.TrimSpace(name)))
	if !prov.Valid() {
		return ""
	}
	return prov
}

// modelServedByZAI reports whether a model pinned on claw's anthropic provider
// is actually served by z.ai's Anthropic-compatible endpoint. Same predicate
// anthropicCapabilities uses to split the two families apart. It reads a model
// ID, not a spec: GLMOnAnthropicWire is the spec-level question.
// Anchored on the PREFIX: every z.ai id starts with "glm" (glm-4.6, glm-5.3),
// and a substring match misrouted ids that merely contain it
// ("notglm", "claude-glm-experimental") onto a vendor that does not serve them.
func modelServedByZAI(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "glm")
}

// GLMOnAnthropicWire reports whether a model spec names a GLM model on the
// anthropic wire — bare (`glm-5.3`, as claude_code and pi take it) or on
// claw's anthropic provider (`anthropic/glm-5.3`). z.ai serves exactly those,
// through its Anthropic-compatible endpoint, so the credential they spend is
// the z.ai key whatever holds the wire. A spec naming another provider
// (`openrouter/z-ai/glm-4.6`) is that provider's route and stays its own.
func GLMOnAnthropicWire(spec string) bool {
	spec = strings.TrimSpace(spec)
	if prefix, id, cut := strings.Cut(spec, "/"); cut {
		if !strings.EqualFold(strings.TrimSpace(prefix), string(secrets.ProviderAnthropic)) {
			return false
		}
		spec = id
	}
	return modelServedByZAI(spec)
}

// zaiKeyReachable reports whether a route naming zai has a key to spend: the
// run's own (default, or pinned for the route) or the process's ZAI_API_KEY —
// the sources the delegates' zai branches read, in that order.
func zaiKeyReachable(ctx context.Context) bool {
	if creds, ok := secrets.CredentialsFromContext(ctx); ok && creds.APIKeyForRoute(secrets.ProviderZAI) != "" {
		return true
	}
	return os.Getenv("ZAI_API_KEY") != ""
}

// RouteProviderHint is the provider hint the executor hands a delegate for
// one chain element: the element's own, except that a GLM id on the anthropic
// wire with no hint goes to z.ai on claude_code and pi when a z.ai key is
// reachable. With no hint the wire's default precedence would hand it to
// whatever holds the family, and a Claude forfait sends it to
// api.anthropic.com, which does not serve it; without a reachable key the
// zai branch would refuse the node before it spawns and leave the ambient
// z.ai setup (ANTHROPIC_BASE_URL on api.z.ai + ANTHROPIC_AUTH_TOKEN, what
// z.ai documents for Claude Code) untried, which the default path inherits.
//
// The dispatch and the usage-cap pre-flight both ask it, so the pre-flight
// reads the ledger of the credential the session will actually spend.
func RouteProviderHint(ctx context.Context, backend, hint, model string) string {
	h := strings.ToLower(strings.TrimSpace(hint))
	if (h == "" || h == "auto") &&
		(backend == delegate.BackendClaudeCode || backend == delegate.BackendPi) &&
		GLMOnAnthropicWire(model) && zaiKeyReachable(ctx) {
		return string(secrets.ProviderZAI)
	}
	return hint
}

// applyForfaitAcrossSandbox is the body of the forfait crossing, split out so
// the model gate above reads as one line.
func applyForfaitAcrossSandbox(env map[string]string, creds secrets.Credentials, model string) error {
	// A resolved Claude Code forfait is mounted by
	// runtime.addClaudeOAuthSecretFile and copied into a writable config dir by
	// seedClaudeConfigDir — both per RUN, not per backend, so the dir is
	// populated for a claw node too. Pointing CLAUDE_CONFIG_DIR at it is what
	// lets the in-container registry find it: its desktop path reads exactly
	// that variable.
	//
	// Clearing the ambient anthropic-wire vars is not a detail, it is the fix.
	// The registry reads the disk forfait only when no env credential precedes
	// it, and the ambient ANTHROPIC_API_KEY forwarded above is the POD's — the
	// platform's — a machine default rather than a credential resolved FOR THIS
	// RUN, which is the distinction this function exists to enforce. Left in
	// place, a forfait-only tenant's sandboxed node authenticates and bills
	// against the platform account, and does so invisibly, because the ambient
	// key works.
	//
	// Two limits, both deliberate: a BYOK key claw's anthropic provider would
	// spend — the Anthropic one, or z.ai's, which the env factory synthesises
	// onto z.ai's base URL — is the tenant's own explicit instrument and keeps
	// precedence (otherwise the same run would spend a different one depending
	// on whether it happened to be sandboxed); and a redirected wire is a
	// destination the operator chose, so a bearer carrying the whole Claude
	// account does not travel there. clawAnthropicProviderSlots says which keys
	// those are.
	// A key pinned for THIS node's own route is as held as a BYOK slot: the
	// pinned-key block above injected it into the env, and applying the
	// forfait here deleted it right back — the container spent the forfait
	// while the in-process path spent the pin, and an expired forfait
	// refused a node whose key was good. The route-less predicate below
	// stays pin-blind on purpose (#736): a pin for ANOTHER route must not
	// keep the forfait out of an unpinned node.
	for _, slot := range clawAnthropicProviderSlots {
		if slot == clawPinnedProvider(model) && creds.PinnedAPIKey(slot) != "" {
			return nil
		}
	}
	if creds.OAuthDir(string(secrets.OAuthKindClaudeCode)) != "" &&
		!heldAnthropicWireAPIKey(creds) &&
		secrets.AnthropicForfaitWireOK(os.Getenv("ANTHROPIC_BASE_URL")) {
		dir := creds.OAuthDir(string(secrets.OAuthKindClaudeCode))
		// Validate BEFORE clearing. Once the shadows are gone the forfait is
		// the node's ONLY credential in the container, and the in-container
		// resolver is the env factory, which swallows expiry — it returns ""
		// and builds a client with no credential at all, i.e. #687's opaque
		// 401 loop with nothing naming the forfait. The in-process twin
		// (anthropicFromCtxForfait) already refuses rather than degrade there;
		// this seam must decide the same way, or the two disagree again.
		if _, terr := secrets.AnthropicForfaitToken(dir); terr != nil {
			return fmt.Errorf("claw backend: sandboxed anthropic node cannot use the run's forfait: %w", terr)
		}
		env["CLAUDE_CONFIG_DIR"] = secrets.ClaudeCodeSandboxConfigDir
		for _, shadow := range anthropicWireShadowEnv() {
			delete(env, shadow)
		}
	}
	return nil
}

// clawAnthropicProviderSlots are the BYOK slots whose key claw's `anthropic`
// provider spends: the Anthropic key, and z.ai's, which the registry's env
// factory reads when no Anthropic credential precedes it and points at z.ai's
// base URL. They are the keys that outrank the forfait inside the container,
// and so the only ones that decide the forfait crossing.
//
// NOT every slot of secrets.AnthropicWireSlotOrder. That list is the claude_code
// delegate's precedence, where each facade key reroutes the CLI. claw names
// its provider in the model spec instead, and a facade with a provider of its
// own — moonshot, reached as `moonshot/…` — funds no `anthropic/…` node: the
// env factory never reads its key. Counting it here kept a tenant's forfait
// out of its sandboxed anthropic nodes whenever the tenant also held a
// Moonshot key, so the platform's ambient Anthropic key served them; and
// listing its variable as a shadow deleted the key a moonshot node needs.
// TestClawAnthropicProviderSlots_MatchWhatTheFactorySpends holds this list to
// the factory.
var clawAnthropicProviderSlots = []secrets.Provider{
	secrets.ProviderAnthropic,
	secrets.ProviderZAI,
}

// heldAnthropicWireAPIKey reports whether the run carries a BYOK key claw's
// anthropic provider would spend (clawAnthropicProviderSlots).
func heldAnthropicWireAPIKey(creds secrets.Credentials) bool {
	for _, slot := range clawAnthropicProviderSlots {
		if creds.APIKeys[slot] != "" {
			return true
		}
	}
	return false
}

// anthropicWireShadowEnv names the ambient variables that would outrank the
// forfait inside the container: the two Anthropic-flavoured ones the env
// factory reads directly, plus the forwarded key of every other slot it
// spends.
func anthropicWireShadowEnv() []string {
	shadows := []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"}
	for _, slot := range clawAnthropicProviderSlots {
		if name := byokEnvVar[slot]; name != "" && name != "ANTHROPIC_API_KEY" {
			shadows = append(shadows, name)
		}
	}
	return shadows
}

// forwardedToolSpellings lists the names the permission gate may know one
// tool by: the name it was advertised under and, for an MCP tool, the
// claude_code FQN (`mcp__<server>__<tool>`) that rules are commonly written in
// and that a model may emit.
func forwardedToolSpellings(td delegate.ToolDef) []string {
	spellings := []string{td.Name}
	if rest, ok := strings.CutPrefix(td.QualifiedName, "mcp."); ok {
		if server, name, found := strings.Cut(rest, "."); found && server != "" && name != "" {
			spellings = append(spellings, "mcp__"+server+"__"+name)
		}
	}
	return spellings
}

// evaluateForwardedCall gates a forwarded call on the tool's identity. The
// runner gates the spelling the model emitted and forwards the advertised
// one, and rules match spellings literally, so one spelling is not enough:
// an explicit deny on ANY spelling denies — a forged call cannot pick the
// spelling no deny names — and otherwise an allow on any spelling allows, so
// an honest call the runner allowed through the FQN form is not refused here.
// Otherwise the first spelling's verdict (the mode default) holds. The
// returned spelling is the one the verdict was taken on.
func evaluateForwardedCall(p *permission.Policy, spellings []string, args map[string]any) (permission.Decision, string, string) {
	first, firstRule := p.Evaluate(spellings[0], args)
	if first == permission.Deny && firstRule != "" {
		return first, firstRule, spellings[0]
	}
	verdict, rule, spelling := first, firstRule, spellings[0]
	for _, s := range spellings[1:] {
		dec, r := p.Evaluate(s, args)
		if dec == permission.Deny && r != "" {
			return dec, r, s
		}
		if dec == permission.Allow && verdict != permission.Allow {
			verdict, rule, spelling = dec, r, s
		}
	}
	return verdict, rule, spelling
}

// refuseForwardedCall records a forwarded tool call the launcher refuses and
// returns err. The refusal happens on the host's side of the boundary, so it
// is logged and emitted from here — not left to the runner's event relay,
// which whatever forged the call also controls.
func (b *ClawBackend) refuseForwardedCall(task delegate.Task, name string, err error) error {
	if b.logger != nil {
		b.logger.Warn("[%s/claw] %v", task.NodeID, err)
	}
	if b.hooks.OnToolCall != nil {
		b.hooks.OnToolCall(task.NodeID, LLMToolCallInfo{ToolName: name, Error: err})
	}
	return err
}

// withoutUnplaceableToolsThePolicyDenies drops a tool no side of the sandbox
// can serve when the run's permission policy denies it outright: the node
// could never have called it, so it neither refuses the node nor reaches the
// runner, which stops on any tool it cannot place. The tool_policy
// counterpart withholds such a tool at resolution.
func withoutUnplaceableToolsThePolicyDenies(task delegate.Task) []delegate.ToolDef {
	if !task.Permission.Enabled() {
		return task.ToolDefs
	}
	kept := make([]delegate.ToolDef, 0, len(task.ToolDefs))
	for _, td := range task.ToolDefs {
		if placement, _ := tool.SandboxPlacementOf(td.Name); placement == tool.PlacementRefused && task.Permission.HasExplicitDeny(td.Name, nil) {
			continue
		}
		kept = append(kept, td)
	}
	return kept
}

// refusedMCPServerSummary names the refused servers in a stable order, so
// the capability string a fallback decision is logged under does not change
// from run to run over one map's iteration order.
func refusedMCPServerSummary(refused map[string]delegate.MCPLauncherRefusal) string {
	names := make([]string, 0, len(refused))
	for name := range refused {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 1 {
		return fmt.Sprintf("server %q", names[0])
	}
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = fmt.Sprintf("%q", name)
	}
	return "servers " + strings.Join(quoted, ", ")
}

// refuseToolsWithNoSandboxPlacement refuses a sandboxed task carrying a tool
// neither side of the sandbox may serve: the container has no form of it and
// the launcher would run it on the host (tool.PlacementRefused, which every
// unclassified name falls into). It runs in Execute, beside the Ask refusal,
// so the node's `fallbacks:` still get their turn; the build-time effects
// (llm_prompt, board token) have fired, but no runner starts and no token is
// spent.
// refusedMCPRemedy opens the remedy with the launcher's OWN reason for
// declining, in a stable order. Asserting "outside this run's sandbox"
// instead was wrong wherever the policy refuses without one.
func refusedMCPRemedy(refused map[string]delegate.MCPLauncherRefusal) string {
	names := make([]string, 0, len(refused))
	for name := range refused {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "this launcher declined to start the MCP servers this node asked for"
	}
	// EVERY reason, not the alphabetically first. A node with no fallback
	// shows this text and nothing else (see delegate.ErrCapabilityUnsupported),
	// so a second server refused for a different reason — or carrying its own
	// health `Cause` — had no other way to reach the operator.
	reasons := make([]string, 0, len(names))
	carriesAdvice := false
	for _, name := range names {
		reasons = append(reasons, refused[name].Reason)
		if refused[name].CarriesRouteAdvice {
			carriesAdvice = true
		}
	}
	remedy := "claw connects MCP servers in the launcher process, and " + strings.Join(reasons, "; ")
	// The typed refusal ends with the route-it-elsewhere advice — but only
	// when it has no cause to report instead. So append it when no reason
	// carried it, and never when one did: saying it twice reads as two
	// remedies, and saying it never leaves a refused-AND-broken server with
	// no way forward at all.
	//
	// The flag comes from the refusal's writer. Searching the reason for
	// "inside the container" put this branch at the mercy of a reword in
	// another package, with nothing to fail if it happened.
	if !carriesAdvice {
		remedy += "; route the node to a backend that starts them inside the container (claude_code, pi), " +
			"or run the workflow unsandboxed (`sandbox: none` / `--sandbox none`)"
	}
	return remedy
}

func refuseToolsWithNoSandboxPlacement(task delegate.Task) error {
	for _, td := range task.ToolDefs {
		if placement, reason := tool.SandboxPlacementOf(td.Name); placement == tool.PlacementRefused {
			return fmt.Errorf(
				"claw backend: node %q declares tool %q, which a sandboxed runner cannot execute in-container (%s) — "+
					"drop it, or run the workflow unsandboxed (`sandbox: none` / `--sandbox none`)", task.NodeID, td.Name, reason)
		}
	}
	return nil
}

// canonicalMCPToolName maps an MCP tool name the model emitted in the
// claude_code FQN convention ("mcp__server__tool") to the sanitized
// single-underscore form ("mcp_server_tool") that iterion advertises to
// the provider (see (*tool.ToolDef).sanitizedName, which turns the
// dot-delimited qualified name into underscores). Names without the
// "mcp__" FQN prefix are returned unchanged.
//
// Bot prompts name board/MCP tools in the double-underscore form for
// cross-backend parity with claude_code, but every claw dispatch path
// advertises them sanitized — so the model's call can arrive in either
// spelling. Normalising at lookup time lets both dispatch. The collapse
// is unambiguous: a sanitized key never contains "__" (qualified-name
// dots each become a single "_"), so reducing "__"→"_" only ever maps an
// FQN onto its registered key, never onto a different tool.
func canonicalMCPToolName(name string) string {
	const fqnPrefix = "mcp__"
	if !strings.HasPrefix(name, fqnPrefix) {
		return name
	}
	return strings.ReplaceAll(name, "__", "_")
}

// multiplexerHandler builds the launcher-side envelope dispatch table
// for a specific task.
//
//   - V2-2: OnToolCall dispatches via the task's ToolDefs map. The
//     runner emits a tool_call envelope for each call to a
//     launcher-placed tool; the launcher checks the call (advertised
//     name, placement, permission gate), invokes the original closure
//     (which has access to the engine's tool registry, MCP manager,
//     ask_user channel, etc.) and forwards the result. *ErrAskUser
//     returns are preserved typed by the multiplexer (V2-3).
//   - V2-4: OnSessionCapture mirrors runner-emitted session snapshots
//     into the host's nodeSessionStore so CompactAndRetry compacts the
//     latest history. Pre-spawn, the launcher seeds a session_replay
//     envelope from the host store (see [executeViaSandboxRunner]).
func (b *ClawBackend) multiplexerHandler(ctx context.Context, task delegate.Task) delegate.MultiplexerHandler {
	// Index ToolDefs by name once so OnToolCall is O(1) instead of
	// scanning the slice on each runner-initiated tool_call.
	toolByName := make(map[string]delegate.ToolDef, len(task.ToolDefs))
	for _, td := range task.ToolDefs {
		toolByName[td.Name] = td
	}
	hostRunID, hostStore := runtimeContextFrom(ctx)
	return delegate.MultiplexerHandler{
		OnToolCall: func(toolCtx context.Context, name string, input json.RawMessage) (string, error) {
			// A runner forwards the name the tool was advertised under — its
			// proxy closure captures it — so any other spelling is not one an
			// honest runner sends, and is refused rather than mapped.
			td, ok := toolByName[name]
			if !ok {
				return "", b.refuseForwardedCall(task, name, fmt.Errorf("launcher: tool %q was not advertised to this node", name))
			}
			// The sandbox boundary is held on THIS side. The runner is the
			// contained process — possibly an older binary baked into the
			// sandbox image, and its IPC stdout is writable by anything in the
			// container running as the same uid — so its routing is a request,
			// never a verdict: only a tool whose home is the launcher's own
			// state executes here.
			if placement, reason := tool.SandboxPlacementOf(td.Name); placement != tool.PlacementLauncher {
				return "", b.refuseForwardedCall(task, name, fmt.Errorf(
					"launcher: refusing to execute tool %q on the host for a sandboxed node: its placement is %s (%s) — "+
						"a current runner never forwards it: either the sandbox image carries an older iterion, or something else in the container wrote this call",
					name, placement, reason))
			}
			// The permission gate the runner applies before forwarding is applied
			// again here: a call that reached this handler without passing it —
			// an older runner, a forged envelope — is gated all the same, on the
			// tool's identity rather than on one spelling of it.
			if task.Permission.Enabled() {
				var args map[string]any
				if len(input) > 0 {
					if err := json.Unmarshal(input, &args); err != nil {
						return "", b.refuseForwardedCall(task, name, fmt.Errorf("launcher: tool %q: decode input for the permission gate: %w", name, err))
					}
				}
				if dec, rule, spelling := evaluateForwardedCall(task.Permission, forwardedToolSpellings(td), args); dec != permission.Allow {
					return "", b.refuseForwardedCall(task, name, fmt.Errorf("launcher: %s", permission.DenyMessage(spelling, args, rule)))
				}
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
		OnEvent: func(eventType string, payload map[string]any) {
			// What the in-container loop observed — its LLM steps, the
			// tools it ran, its retries and compactions, its per-turn
			// anchors — re-fired through THIS process's hooks so a
			// sandboxed claw node is persisted, priced, metered and
			// forkable like an in-process one — see sandbox_relay.go.
			handled, err := ApplyRelayedEvent(b.hooks, task.NodeID, eventType, payload)
			if b.logger == nil {
				return
			}
			switch {
			case err != nil:
				b.logger.Warn("[%s#%d/claw] the sandbox runner relayed a %s event this host cannot decode — the node's per-step metering is incomplete: %v",
					task.NodeID, task.Iteration, eventType, err)
			case !handled:
				b.logger.Debug("[%s#%d/claw] the sandbox runner relayed a %s event this host does not consume",
					task.NodeID, task.Iteration, eventType)
			}
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

// applyAsyncAskExecs swaps the registry's explicit-error default execs of
// ask_user_async / await_answers for the task-bound behaviour (ADR-081)
// when the node is interaction: async. No-op for other nodes — the tools
// then keep their loud "not bound" default if they somehow resolve.
func applyAsyncAskExecs(task delegate.Task, tools []GenerationTool) {
	if task.PostAsyncQuestion == nil {
		return
	}
	for i := range tools {
		switch tools[i].Name {
		case delegate.AskUserAsyncToolName:
			tools[i].Execute = func(_ context.Context, input json.RawMessage) (string, error) {
				var in map[string]any
				if err := json.Unmarshal(input, &in); err != nil {
					return "", fmt.Errorf("ask_user_async: decode input: %w", err)
				}
				q, _ := in["question"].(string)
				options, allowFree := delegate.ParseAskUserToolInput(in)
				if _, err := task.PostAsyncQuestion(delegate.AsyncQuestion{Question: q, Options: options, AllowFreeText: allowFree}); err != nil {
					return "", err
				}
				return delegate.AsyncQuestionPostedText, nil
			}
		case delegate.AwaitAnswersToolName:
			tools[i].Execute = func(_ context.Context, _ json.RawMessage) (string, error) {
				pending, err := task.PendingAsyncQuestions()
				if err != nil {
					return "", fmt.Errorf("await_answers: %w", err)
				}
				if len(pending) == 0 {
					answers, err := task.CollectAsyncAnswers()
					if err != nil {
						return "", fmt.Errorf("await_answers: %w", err)
					}
					return answers, nil
				}
				// Pending questions remain: suspend the node through the
				// standard ask_user pause machinery. The generation loop
				// stashes the conversation + pending tool_use so resume
				// answers THIS call with the collected answers.
				return "", &delegate.ErrAskUser{
					Question:     delegate.AwaitPauseQuestion(pending),
					AwaitPending: pending,
				}
			}
		}
	}
}
