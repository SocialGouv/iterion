package delegate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/SocialGouv/iterion/pkg/backend/delegate/claudesdk"
	"github.com/SocialGouv/iterion/pkg/backend/permission"
	"github.com/SocialGouv/iterion/pkg/backend/rewrite"
	"github.com/SocialGouv/iterion/pkg/internal/proc"
)

// askUserMCPServerName is the name under which iterion registers itself as an
// MCP server exposing the ask_user tool. The CLI prefixes MCP tool names as
// "mcp__<server>__<tool>", so the LLM sees the tool as "mcp__iterion__ask_user".
const askUserMCPServerName = "iterion"

// askUserMCPToolName is the fully-qualified name of the ask_user tool as the
// CLI exposes it to the LLM.
const askUserMCPToolName = "mcp__iterion__ask_user"

// askUserMCPSubcommand is the hidden iterion subcommand that runs an MCP stdio
// server exposing only the ask_user tool. See cmd/iterion/mcp_ask_user.go.
const askUserMCPSubcommand = "__mcp-ask-user"

// editMissHintAfter is how many consecutive Edit/MultiEdit "String to
// replace not found in file" failures trigger a re-Read hint injection
// (see the PostToolUse Edit-resilience hook). The model otherwise tends to
// blind-retry a mismatching old_string until defaultMaxConsecutiveToolErrors
// aborts the whole session — and the runtime's recovery re-runs the node
// straight back into the same wedge. Small enough to break the loop early;
// >1 so a single self-correcting miss isn't nagged.
const editMissHintAfter = 2

// editMissCount updates the running tally of consecutive Edit/MultiEdit
// "String to replace not found" failures given the latest tool call:
//   - a non-Edit tool leaves the tally unchanged (a Read between two
//     misses is the model trying to recover — it shouldn't reset the
//     wedge signal),
//   - an Edit/MultiEdit whose response carries the not-found error bumps
//     the tally,
//   - any other Edit/MultiEdit result (success, or a different error)
//     resets it to 0.
//
// Extracted from the PostToolUse hook so the wedge-detection is unit-
// testable without driving a live claude session.
func editMissCount(toolName, response string, prev int) int {
	if toolName != "Edit" && toolName != "MultiEdit" {
		return prev
	}
	if strings.Contains(response, "to replace not found") {
		return prev + 1
	}
	return 0
}

// installEditMissResilience appends the PostToolUse hook that breaks the
// Edit/MultiEdit blind-retry wedge: claude_code's Edit fails with "String
// to replace not found in file" when old_string doesn't match the file
// verbatim (a stale read or whitespace drift). The model tends to
// blind-retry a mismatching edit until defaultMaxConsecutiveToolErrors
// aborts the session — and recovery re-runs the node into the same wedge
// (observed: a feature_dev act burned 4 recovery attempts integrating into
// existing server files). After editMissHintAfter consecutive Edit-misses,
// inject a corrective system message so the model re-Reads the verbatim
// current text before editing. editMisses is closure-local: a session's
// tool calls are sequential, so no synchronisation is needed. Counts misses
// across intervening non-Edit tools (a Read between two misses doesn't reset
// — the model still hasn't landed the edit); resets only on a successful Edit.
func (b *ClaudeCodeBackend) installEditMissResilience(opts []claudesdk.Option, task Task) []claudesdk.Option {
	editMisses := 0
	return append(opts, claudesdk.WithHook(claudesdk.HookPostToolUse, claudesdk.HookMatcher{
		Handler: func(_ context.Context, in claudesdk.HookCallbackInput) (claudesdk.HookOutput, error) {
			editMisses = editMissCount(in.ToolName, fmt.Sprintf("%v", in.ToolResponse), editMisses)
			if editMisses < editMissHintAfter {
				return claudesdk.HookOutput{}, nil
			}
			b.Logger.Info("[%s#%d/claude-code] 🩹 %d consecutive Edit-misses — injecting re-Read hint", task.NodeID, task.Iteration, editMisses)
			hint := "Your Edit/MultiEdit failed: \"String to replace not found in file\". " +
				"The old_string does not match the file's CURRENT content verbatim (usually a whitespace or stale-read mismatch). " +
				"Do NOT retry the same edit. First Read the exact lines you intend to change to capture their verbatim current text (including leading whitespace), then issue the edit with that exact old_string. " +
				"If edits keep failing on a file, Read the whole surrounding region before editing again."
			return claudesdk.HookOutput{SystemMessage: hint, AdditionalContext: hint}, nil
		},
	}))
}

// pendingAskUser carries an intercepted ask_user call (or a
// tool-permission Ask prompt) from a PreToolUse hook to the
// post-session escalation check in Execute. Options/AllowFreeText
// mirror the ask_user tool's structured input; a permission pause
// carries only Question.
type pendingAskUser struct {
	Question      string
	Options       []AskUserOption
	AllowFreeText bool
}

// wireAskUserHook registers iterion's native ask_user MCP server and a
// PreToolUse hook that captures the question and cancels the stream the
// moment the LLM calls ask_user (mirrors the claw backend's in-process
// path). Disabled when sandboxed: the stdio MCP server's host binary path
// (os.Executable) and the host /tmp --mcp-config are both invisible inside
// the container, so claude would reject the missing config and exit before
// producing a result — the [INTERACTION PROTOCOL] JSON fallback covers that
// case. Stores the captured question (with any structured options) into
// pendingQuestion and extends extras with the ask_user tool name only when
// the node already restricts its toolset (an empty AllowedTools means "no
// restriction").
func (b *ClaudeCodeBackend) wireAskUserHook(task Task, opts []claudesdk.Option, extras *[]string, pendingQuestion *atomic.Value, cancelStream context.CancelFunc) []claudesdk.Option {
	if !task.InteractionEnabled || task.Sandbox != nil {
		return opts
	}
	selfPath := proc.LocateIterionBinary()
	if selfPath == "" {
		b.Logger.Warn("[%s#%d/claude-code] could not resolve iterion CLI binary path; native ask_user MCP server disabled (falling back to JSON _needs_interaction protocol)", task.NodeID, task.Iteration)
		return opts
	}
	opts = append(opts, claudesdk.WithMCPServer(askUserMCPServerName, &claudesdk.MCPStdioServer{
		Command: selfPath,
		Args:    []string{askUserMCPSubcommand},
	}))
	if len(task.AllowedTools) > 0 {
		*extras = append(*extras, askUserMCPToolName)
	}
	matcher := "^" + askUserMCPToolName + "$"
	noContinue := false
	opts = append(opts, claudesdk.WithHook(claudesdk.HookPreToolUse, claudesdk.HookMatcher{
		Matcher: &matcher,
		Handler: func(_ context.Context, in claudesdk.HookCallbackInput) (claudesdk.HookOutput, error) {
			if q, ok := in.ToolInput["question"].(string); ok && q != "" {
				options, allowFree := ParseAskUserToolInput(in.ToolInput)
				pendingQuestion.Store(pendingAskUser{Question: q, Options: options, AllowFreeText: allowFree})
				cancelStream()
			}
			return claudesdk.HookOutput{
				Decision:      "deny",
				Continue:      &noContinue,
				SystemMessage: "ask_user has been escalated to the iterion runtime; stop generating.",
			}, nil
		},
	}))
	return opts
}

// wirePermissionHook installs the tool-permission gate for claude_code:
// a broad PreToolUse hook (Matcher nil = every tool) that evaluates the
// resolved policy before the CLI runs a tool. This is claude_code's half
// of cross-backend parity with claw's executeToolsDirect gate — both
// honour the SAME permission.Policy.
//
// Under the always-on --permission-mode bypassPermissions, PreToolUse
// hooks STILL run and a "deny" decision STILL blocks the tool (per the
// Agent SDK permission-evaluation order: hooks run first). So the gate
// needs no --permission-mode change:
//   - Allow → empty HookOutput → falls through → bypass approves.
//   - Deny  → permissionDecision "deny" with a reason the model adapts to.
//   - Ask   → capture the approval prompt + cancel the stream so the run
//     PAUSES for the human (reuses the ask_user pause path:
//     pendingQuestion + buildAskUserPendingResult). On resume the operator
//     grants and the model re-issues the now-authorized call.
//
// Infrastructure tools (ask_user, board.*) are exempt inside
// permission.Policy.Evaluate, so this hook never blocks iterion's own
// interaction plumbing.
func (b *ClaudeCodeBackend) wirePermissionHook(task Task, opts []claudesdk.Option, pendingQuestion, pendingPermission *atomic.Value, cancelStream context.CancelFunc) []claudesdk.Option {
	policy := task.Permission
	if !policy.Enabled() {
		return opts
	}
	noContinue := false
	return append(opts, claudesdk.WithHook(claudesdk.HookPreToolUse, claudesdk.HookMatcher{
		Handler: func(_ context.Context, in claudesdk.HookCallbackInput) (claudesdk.HookOutput, error) {
			dec, rule := policy.Evaluate(in.ToolName, in.ToolInput)
			switch dec {
			case permission.Deny:
				return claudesdk.HookOutput{
					Decision:       "deny",
					DecisionReason: permission.DenyMessage(in.ToolName, in.ToolInput, rule),
				}, nil
			case permission.Ask:
				// Surface the approval request to the human and stop the
				// stream — the post-session check reuses the ask_user
				// pending path to pause the run. The marker carries the
				// structured request so the runtime can auto-grant on resume.
				pendingQuestion.Store(pendingAskUser{Question: permission.AskPrompt(in.ToolName, in.ToolInput, rule)})
				pendingPermission.Store(permission.Marker(in.ToolName, in.ToolInput, rule))
				cancelStream()
				return claudesdk.HookOutput{
					Decision:      "deny",
					Continue:      &noContinue,
					SystemMessage: "This action requires operator approval; it has been escalated to the iterion runtime. Stop generating.",
				}, nil
			default: // permission.Allow
				return claudesdk.HookOutput{}, nil
			}
		},
	}))
}

// installMaterializeSecretsHook adds a PreToolUse hook that swaps
// __ITERION_SECRET_<name>__ placeholders for their real values in
// agent-emitted tool input, immediately before the CLI runs the tool
// (Layer 1, structural). The placeholder is all the model ever emits/sees;
// the real value is spliced in here and never enters the prompt, the event
// stream, or the run store. Matches all tools (Matcher nil); a no-op for
// input that carries no placeholder.
func installMaterializeSecretsHook(task Task, opts []claudesdk.Option) []claudesdk.Option {
	materialize := task.MaterializeSecrets
	if materialize == nil {
		return opts
	}
	return append(opts, claudesdk.WithHook(claudesdk.HookPreToolUse, claudesdk.HookMatcher{
		Handler: func(_ context.Context, in claudesdk.HookCallbackInput) (claudesdk.HookOutput, error) {
			if len(in.ToolInput) == 0 {
				return claudesdk.HookOutput{}, nil
			}
			raw, err := json.Marshal(in.ToolInput)
			if err != nil {
				return claudesdk.HookOutput{}, nil
			}
			swapped := materialize(string(raw))
			if swapped == string(raw) {
				return claudesdk.HookOutput{}, nil // no placeholder present
			}
			var updated map[string]any
			if err := json.Unmarshal([]byte(swapped), &updated); err != nil {
				return claudesdk.HookOutput{}, nil
			}
			return claudesdk.HookOutput{Decision: "allow", UpdatedInput: updated}, nil
		},
	}))
}

// installRewriteHook adds a PreToolUse hook on the Bash tool that rewrites
// commands to their compressed equivalent (e.g. "git status" → "rtk git
// status"), saving 60–90% of output tokens, when compression is enabled for
// this node and at least one rewriter plugin's binary is present. The rewrite
// decision is delegated to each rewriter's own contract (single source of
// truth); iterion uses rewriters purely as compressors — never a permission
// gate — so it always auto-allows the rewritten command. The rewrite runs
// host-side; the (sandboxed) CLI runs the rewritten command in-container
// against the bind-mounted rewriter binary.
func installRewriteHook(task Task, opts []claudesdk.Option) []claudesdk.Option {
	mode := rewrite.ParseMode(task.CompressMode)
	chain := rewrite.NewChain(task.Rewriters)
	if !mode.Enabled() || !chain.Available() {
		return opts
	}
	bashMatcher := "^Bash$"
	return append(opts, claudesdk.WithHook(claudesdk.HookPreToolUse, claudesdk.HookMatcher{
		Matcher: &bashMatcher,
		Handler: func(hookCtx context.Context, in claudesdk.HookCallbackInput) (claudesdk.HookOutput, error) {
			updated, changed := chain.RewriteCommandField(hookCtx, mode, in.ToolInput)
			if !changed {
				return claudesdk.HookOutput{}, nil
			}
			return claudesdk.HookOutput{
				Decision:       "allow",
				DecisionReason: "compress auto-rewrite",
				UpdatedInput:   updated,
			}, nil
		},
	}))
}

// wireBoardMCP registers the internal __mcp-board MCP server when the node
// holds any board.* capability, so the bot can mutate the kanban from inside
// its reasoning loop. Non-sandboxed runs use the stdio transport (iterion
// binary subcommand); sandboxed runs use the HTTP transport when the runtime
// configured BoardHTTPEndpoint+BoardRunToken (else the capability is disabled
// with a warning). Extends extras with the granted board tool names when the
// node already restricts its toolset.
func (b *ClaudeCodeBackend) wireBoardMCP(task Task, opts []claudesdk.Option, extras *[]string) []claudesdk.Option {
	if !HasBoardCapability(task.Capabilities) {
		return opts
	}
	if task.Sandbox == nil {
		selfPath := proc.LocateIterionBinary()
		if selfPath == "" {
			b.Logger.Warn("[%s#%d/claude-code] could not resolve iterion CLI binary path; board MCP server disabled", task.NodeID, task.Iteration)
			return opts
		}
		env := map[string]string{
			"ITERION_BOARD_CAPS": strings.Join(task.Capabilities, ","),
		}
		if task.StoreDir != "" {
			env["ITERION_STORE_DIR"] = task.StoreDir
		}
		if task.SourceIssueID != "" {
			env["ITERION_SOURCE_ISSUE_ID"] = task.SourceIssueID
		}
		opts = append(opts, claudesdk.WithMCPServer(boardMCPServerName, &claudesdk.MCPStdioServer{
			Command: selfPath,
			Args:    []string{boardMCPSubcommand},
			Env:     env,
		}))
		if len(task.AllowedTools) > 0 {
			*extras = append(*extras, BoardToolsFor(task.Capabilities)...)
		}
		return opts
	}
	// Sandboxed: HTTP transport (Phase 2 of board's stdio-then-HTTP rollout).
	if task.BoardHTTPEndpoint != "" && task.BoardRunToken != "" {
		opts = append(opts, claudesdk.WithMCPServer(boardMCPServerName, &claudesdk.MCPHTTPServer{
			URL: task.BoardHTTPEndpoint,
			Headers: map[string]string{
				"X-Iterion-Run": task.BoardRunToken,
			},
			// Force the board server past claude-code's tool-search deferral
			// so board.* tools surface without a ToolSearch hit, and fail
			// loudly at startup if unreachable (C082).
			AlwaysLoad: true,
		}))
		if len(task.AllowedTools) > 0 {
			*extras = append(*extras, BoardToolsFor(task.Capabilities)...)
		}
		return opts
	}
	b.Logger.Warn("[%s#%d/claude-code] board capabilities granted but workflow is sandboxed and BoardHTTPEndpoint/BoardRunToken not configured; board MCP disabled for this node", task.NodeID, task.Iteration)
	return opts
}

// wireUserMCP forwards the node's active user/plugin-declared MCP servers
// (Task.MCPServers) to the claude_code CLI via --mcp-config, so their tools
// are available to the agent — the claude_code half of the parity claw
// already had. It is purely ADDITIVE: it registers servers only, never
// passes --tools, so the native toolset (WebSearch/WebFetch, …) stays on by
// default. AlwaysLoad is set so an http/sse server's tools surface past
// claude-code's tool-search deferral (and a stdio server's tools are always
// eager). When the node restricts its toolset (non-empty AllowedTools), each
// server's tools are allow-listed by wildcard FQN (mcp__<server>__*) so the
// declared MCP tools are not filtered out.
//
// Sandboxed stdio servers whose Command is a host path are left to the CLI:
// the same host-path caveat as ask_user/board applies, but unlike those we
// have no HTTP fallback for arbitrary user servers — an http/sse server is
// reachable from inside the container, a stdio one only if its Command
// resolves in-container.
func (b *ClaudeCodeBackend) wireUserMCP(task Task, opts []claudesdk.Option, extras *[]string) []claudesdk.Option {
	for _, s := range task.MCPServers {
		var srv claudesdk.MCPServerConfig
		switch strings.ToLower(strings.TrimSpace(s.Transport)) {
		case "http":
			if s.URL == "" {
				b.Logger.Warn("[%s#%d/claude-code] MCP server %q: http transport with empty url; skipped", task.NodeID, task.Iteration, s.Name)
				continue
			}
			srv = &claudesdk.MCPHTTPServer{URL: s.URL, Headers: s.Headers, AlwaysLoad: true}
		case "sse":
			if s.URL == "" {
				b.Logger.Warn("[%s#%d/claude-code] MCP server %q: sse transport with empty url; skipped", task.NodeID, task.Iteration, s.Name)
				continue
			}
			srv = &claudesdk.MCPSSEServer{URL: s.URL, Headers: s.Headers}
		default: // stdio
			if s.Command == "" {
				b.Logger.Warn("[%s#%d/claude-code] MCP server %q: stdio transport with empty command; skipped", task.NodeID, task.Iteration, s.Name)
				continue
			}
			srv = &claudesdk.MCPStdioServer{Command: s.Command, Args: s.Args, Env: s.Env}
		}
		opts = append(opts, claudesdk.WithMCPServer(s.Name, srv))
		if len(task.AllowedTools) > 0 {
			*extras = append(*extras, "mcp__"+s.Name+"__*")
		}
	}
	return opts
}

// installInboxDrainHooks delivers operator-chatbox messages mid-session
// (parity with the claw backend's per-iteration drain). PostToolUse fires
// after every tool call and Stop fires when the LLM tries to end the turn;
// both consult the same drain closure and surface queued operator messages
// so the LLM sees operator input on its next turn without waiting for the
// run to finish or pause at a human boundary.
func (b *ClaudeCodeBackend) installInboxDrainHooks(task Task, opts []claudesdk.Option) []claudesdk.Option {
	if task.InboxDrain == nil {
		return opts
	}
	drainAndFormat := func() string {
		texts := task.InboxDrain()
		if len(texts) == 0 {
			return ""
		}
		var sb strings.Builder
		sb.WriteString("Operator queued message")
		if len(texts) > 1 {
			sb.WriteString("s")
		}
		sb.WriteString(":\n\n")
		for i, t := range texts {
			if i > 0 {
				sb.WriteString("\n---\n")
			}
			sb.WriteString(t)
		}
		return sb.String()
	}
	opts = append(opts, claudesdk.WithHook(claudesdk.HookPostToolUse, claudesdk.HookMatcher{
		Handler: func(_ context.Context, _ claudesdk.HookCallbackInput) (claudesdk.HookOutput, error) {
			msg := drainAndFormat()
			if msg == "" {
				return claudesdk.HookOutput{}, nil
			}
			b.Logger.Info("[%s#%d/claude-code] 📥 delivered queued operator message via PostToolUse", task.NodeID, task.Iteration)
			return claudesdk.HookOutput{AdditionalContext: msg, SystemMessage: msg}, nil
		},
	}))
	opts = append(opts, claudesdk.WithHook(claudesdk.HookStop, claudesdk.HookMatcher{
		Handler: func(_ context.Context, _ claudesdk.HookCallbackInput) (claudesdk.HookOutput, error) {
			msg := drainAndFormat()
			if msg == "" {
				return claudesdk.HookOutput{}, nil
			}
			b.Logger.Info("[%s#%d/claude-code] 📥 delivered queued operator message via Stop (blocking stop)", task.NodeID, task.Iteration)
			return claudesdk.HookOutput{BlockStop: true, Reason: msg, SystemMessage: msg}, nil
		},
	}))
	return opts
}
