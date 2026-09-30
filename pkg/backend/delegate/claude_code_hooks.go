package delegate

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"

	"strings"
	"sync/atomic"

	"github.com/SocialGouv/iterion/pkg/backend/delegate/claudesdk"
	"github.com/SocialGouv/iterion/pkg/backend/permission"
	"github.com/SocialGouv/iterion/pkg/backend/rewrite"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/internal/proc"
)

// askUserMCPServerName is the name under which iterion registers itself as an
// MCP server exposing the ask_user tool. The CLI prefixes MCP tool names as
// "mcp__<server>__<tool>", so the LLM sees the tool as "mcp__iterion__ask_user".
const askUserMCPServerName = "iterion"

// askUserMCPToolName is the fully-qualified name of the ask_user tool as the
// CLI exposes it to the LLM.
const askUserMCPToolName = "mcp__iterion__ask_user"

// Fully-qualified names of the async-interaction pair (ADR-081), served
// by the same iterion MCP server.
const (
	askUserAsyncMCPToolName = "mcp__iterion__" + AskUserAsyncToolName
	awaitAnswersMCPToolName = "mcp__iterion__" + AwaitAnswersToolName
)

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
	// AwaitPending marks an await_answers escalation (ADR-081): the
	// still-unanswered async questions the agent chose to block on.
	AwaitPending []PendingAsync
}

// wireAskUserHook registers iterion's native ask_user MCP server and a
// PreToolUse hook that captures the question and cancels the stream the
// moment the LLM calls ask_user (mirrors the claw backend's in-process
// path). Stores the captured question (with any structured options) into
// pendingQuestion and extends extras with the ask_user tool name only when
// the node already restricts its toolset (an empty AllowedTools means "no
// restriction").
//
// Transport is sandbox-dependent (ADR-082 Phase 3, mirroring wireBoardMCP):
//
//   - unsandboxed → the stdio `iterion __mcp-ask-user` subcommand
//     (host binary path via proc.LocateIterionBinary);
//   - sandboxed → an HTTP MCP server pointing at the per-run
//     gateway-reachable listener the engine bound at sandbox start
//     (Task.AskUserHTTPEndpoint + AskUserRunToken), since the stdio
//     server's host binary path is invisible inside the container.
//     When the runtime didn't bind the endpoint the tools are disabled
//     LOUDLY (never silently); the [INTERACTION PROTOCOL] JSON
//     fallback still allows a blocking escalation.
//
// The PreToolUse hooks below run host-side over the SDK control channel
// on BOTH transports, so the interception semantics — and the
// interaction-store paths behind Task.PostAsyncQuestion — are identical.
func (b *ClaudeCodeBackend) wireAskUserHook(task Task, opts []claudesdk.Option, extras *[]string, pendingQuestion *atomic.Value, cancelStream context.CancelFunc) []claudesdk.Option {
	if !task.InteractionEnabled {
		return opts
	}
	if task.Sandbox != nil {
		srv := askUserSandboxHTTPServer(task)
		if srv == nil {
			b.Logger.Warn("[%s#%d/claude-code] sandboxed run has no ask-user MCP HTTP endpoint (listener not bound); native ask_user disabled for this node (falling back to JSON _needs_interaction protocol)", task.NodeID, task.Iteration)
			if task.PostAsyncQuestion != nil {
				b.Logger.Warn("[%s#%d/claude-code] interaction: async unavailable without the ask-user HTTP endpoint — ask_user_async/await_answers disabled for this node; the [INTERACTION PROTOCOL] JSON fallback still allows a blocking escalation", task.NodeID, task.Iteration)
			}
			return opts
		}
		opts = append(opts, claudesdk.WithMCPServer(askUserMCPServerName, srv))
	} else {
		selfPath := proc.LocateIterionBinary()
		if selfPath == "" {
			b.Logger.Warn("[%s#%d/claude-code] could not resolve iterion CLI binary path; native ask_user MCP server disabled (falling back to JSON _needs_interaction protocol)", task.NodeID, task.Iteration)
			return opts
		}
		opts = append(opts, claudesdk.WithMCPServer(askUserMCPServerName, &claudesdk.MCPStdioServer{
			Command: selfPath,
			Args:    []string{askUserMCPSubcommand},
		}))
	}
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
	return b.wireAsyncAskHooks(task, opts, extras, pendingQuestion, cancelStream)
}

// askUserSandboxHTTPServer builds the HTTP MCP server config a sandboxed
// task uses to reach the per-run ask-user listener, or nil when the
// runtime didn't bind the endpoint (no listener / bind failure — the
// caller degrades loudly). AlwaysLoad forces the server past
// claude-code's tool-search deferral so ask_user surfaces without a
// ToolSearch hit and an unreachable endpoint fails loudly at session
// start — the same rationale as the board HTTP transport (C082).
func askUserSandboxHTTPServer(task Task) *claudesdk.MCPHTTPServer {
	if task.AskUserHTTPEndpoint == "" || task.AskUserRunToken == "" {
		return nil
	}
	return &claudesdk.MCPHTTPServer{
		URL: task.AskUserHTTPEndpoint,
		Headers: map[string]string{
			"X-Iterion-Run": task.AskUserRunToken,
		},
		AlwaysLoad: true,
	}
}

// wireAsyncAskHooks adds the non-blocking question pair (ADR-081) on top
// of the ask_user MCP server wireAskUserHook just registered. Only wired
// when the executor bound the async closures (interaction: async).
//
//   - ask_user_async: the PreToolUse hook persists the pending
//     interaction (Task.PostAsyncQuestion) and ALLOWS the call — the
//     stdio server's canned success text tells the model to keep
//     working. On a post error, deny with the error so nothing is
//     silently lost.
//   - await_answers: nothing pending → deny with DecisionReason carrying
//     the formatted answers (the deny channel is the only hook path that
//     can return content; the reason is phrased as the success payload —
//     permission-gate precedent). Pending → capture + cancel the stream,
//     exactly the blocking ask_user shape; the post-session check turns
//     it into an await-tagged pause.
func (b *ClaudeCodeBackend) wireAsyncAskHooks(task Task, opts []claudesdk.Option, extras *[]string, pendingQuestion *atomic.Value, cancelStream context.CancelFunc) []claudesdk.Option {
	if task.PostAsyncQuestion == nil {
		return opts
	}
	if len(task.AllowedTools) > 0 {
		*extras = append(*extras, askUserAsyncMCPToolName, awaitAnswersMCPToolName)
	}
	noContinue := false
	deny := func(format string, a ...any) (claudesdk.HookOutput, error) {
		return claudesdk.HookOutput{
			Decision:       "deny",
			DecisionReason: fmt.Sprintf(format, a...),
		}, nil
	}

	asyncMatcher := "^" + askUserAsyncMCPToolName + "$"
	opts = append(opts, claudesdk.WithHook(claudesdk.HookPreToolUse, claudesdk.HookMatcher{
		Matcher: &asyncMatcher,
		Handler: func(_ context.Context, in claudesdk.HookCallbackInput) (claudesdk.HookOutput, error) {
			q, _ := in.ToolInput["question"].(string)
			options, allowFree := ParseAskUserToolInput(in.ToolInput)
			id, err := task.PostAsyncQuestion(AsyncQuestion{Question: q, Options: options, AllowFreeText: allowFree})
			if err != nil {
				return deny("ask_user_async failed: %v", err)
			}
			b.Logger.Info("[%s#%d/claude-code] 💬 async question posted (%s)", task.NodeID, task.Iteration, id)
			return claudesdk.HookOutput{}, nil
		},
	}))

	awaitMatcher := "^" + awaitAnswersMCPToolName + "$"
	opts = append(opts, claudesdk.WithHook(claudesdk.HookPreToolUse, claudesdk.HookMatcher{
		Matcher: &awaitMatcher,
		Handler: func(_ context.Context, _ claudesdk.HookCallbackInput) (claudesdk.HookOutput, error) {
			pending, err := task.PendingAsyncQuestions()
			if err != nil {
				return deny("await_answers failed: %v", err)
			}
			if len(pending) == 0 {
				answers, err := task.CollectAsyncAnswers()
				if err != nil {
					return deny("await_answers failed: %v", err)
				}
				return deny("%s\n(No pause was needed — every posted question is already answered. Use these answers and continue.)", answers)
			}
			b.Logger.Info("[%s#%d/claude-code] ⏸ await_answers with %d pending question(s) — escalating to pause", task.NodeID, task.Iteration, len(pending))
			pendingQuestion.Store(pendingAskUser{Question: AwaitPauseQuestion(pending), AwaitPending: pending})
			cancelStream()
			return claudesdk.HookOutput{
				Decision:      "deny",
				Continue:      &noContinue,
				SystemMessage: "await_answers has been escalated to the iterion runtime; stop generating.",
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
			policyToolName := claudePermissionToolName(task, in.ToolName, in.ToolInput)
			dec, rule := policy.Evaluate(policyToolName, in.ToolInput)
			switch dec {
			case permission.Deny:
				return claudesdk.HookOutput{
					Decision:       "deny",
					DecisionReason: permission.DenyMessage(policyToolName, in.ToolInput, rule),
				}, nil
			case permission.Ask:
				// Surface the approval request to the human and stop the
				// stream — the post-session check reuses the ask_user
				// pending path to pause the run. The marker carries the
				// structured request so the runtime can auto-grant on resume.
				pendingQuestion.Store(pendingAskUser{Question: permission.AskPrompt(policyToolName, in.ToolInput, rule)})
				pendingPermission.Store(permission.Marker(policyToolName, in.ToolInput, rule))
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

// claudePermissionToolName is the narrow bridge between Claude Code's native
// Bash spelling and Iterion's diagnostic_shell approval alias. The alias is
// opt-in per declared node, never accepts a multiline command, and never wins
// over a workflow author's explicit Bash deny. That lets a persisted Copi ask
// for one inspectable verification command without exposing raw Bash to the
// fresh Kimi/Grok reviewer routes that cannot pause for it.
func claudePermissionToolName(task Task, toolName string, input map[string]any) string {
	if toolName != "Bash" || !task.DiagnosticShell || task.Permission == nil {
		return toolName
	}
	command, _ := input["command"].(string)
	if strings.TrimSpace(command) == "" || strings.ContainsAny(command, "\r\n") {
		return toolName
	}
	if task.Permission.HasExplicitDeny(toolName, input) {
		return toolName
	}
	return "diagnostic_shell"
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
	opts = append(opts, claudesdk.WithHook(claudesdk.HookPreToolUse, claudesdk.HookMatcher{
		Handler: materializeSecretsHandler(materialize),
	}))
	if task.UnmaterializeSecrets != nil {
		opts = append(opts, claudesdk.WithHook(claudesdk.HookPostToolUse, claudesdk.HookMatcher{
			Handler: unmaterializeOutputHandler(task.MaterializeSecrets, task.UnmaterializeSecrets, task.WorkDir),
		}))
	}
	return opts
}

// rawOutputTools: the tools whose output shows what the workspace holds — a
// file, a command's output, a search over files (2.1.280's names). Their
// output is left as is: an agent editing a line that holds a secret must see
// the value the file holds. Every other tool's output — a stop naming its
// command, a fetch its URL, a task or a message echoing what it was given, a
// foreground subagent's report, an MCP tool whatever it reports — goes back
// to placeholders.
var rawOutputTools = map[string]bool{
	"Read": true, "Edit": true, "Glob": true, "Grep": true, "Bash": true, "PowerShell": true, "LSP": true,
}

// unmaterializeOutputHandler is the PostToolUse handler that turns the known
// secret values a tool's output quotes back into their placeholders. It
// replaces the output only when that changed anything: the CLI applies
// sibling hooks' replacements last-write-wins.
func unmaterializeOutputHandler(materialize, unmaterialize func(string) string, workspace string) func(context.Context, claudesdk.HookCallbackInput) (claudesdk.HookOutput, error) {
	own := workspaceRef(workspace)
	return func(_ context.Context, in claudesdk.HookCallbackInput) (claudesdk.HookOutput, error) {
		// A workspace reader keeps what the workspace holds — unless its call
		// carried a secret (a command using a credential may print it back).
		if rawOutputTools[in.ToolName] && !carriesSecret(in.ToolInput, materialize, unmaterialize) && !namesTaskOutput(in.ToolInput, own) {
			return claudesdk.HookOutput{}, nil
		}
		out, changed := secretguard.MaterializeLeaves(in.ToolResponse, unmaterialize)
		if !changed {
			return claudesdk.HookOutput{}, nil
		}
		// The CLI checks the replacement against the tool's output schema and
		// keeps the ORIGINAL when it does not match: a replacement stays on
		// schema even where the original was not — a notebook's language comes
		// from its own metadata, whatever type that holds.
		if m, ok := out.(map[string]any); ok && in.ToolName == "NotebookEdit" {
			if l, ok := m["language"]; ok {
				if _, isString := l.(string); !isString {
					m["language"] = fmt.Sprint(l)
				}
			}
		}
		return claudesdk.HookOutput{UpdatedToolOutput: out}, nil
	}
}

// carriesSecret reports whether a tool call's input named a secret — a
// placeholder, or the value the PreToolUse hook materialised it into (the
// CLI hands PostToolUse the input the tool ran with).
func carriesSecret(input map[string]any, materialize, unmaterialize func(string) string) bool {
	for _, f := range []func(string) string{materialize, unmaterialize} {
		if f == nil {
			continue
		}
		if _, changed := secretguard.MaterializeLeaves(input, f); changed {
			return true
		}
	}
	return false
}

// taskOutputFile matches the files the CLI writes background tasks' output
// to — <tmp>/claude-<uid>/<project>/<session>/tasks/<id>.output, an agent's
// id running to 128 characters, a glob or a variable standing for it — the
// session's tasks directory (<session> is a UUID) and the CLI's own tmp root:
// a search or a glob over either reads every task's output. A file outside
// the node's workspace named like one matches too: its output returns
// placeholders, the safe side.
var taskOutputFile = regexp.MustCompile(`/tasks/[^/\s"']{1,128}\.output\b|/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/tasks\b|/claude-[0-9]+(?:/|$|[\s"'])`)

// workspaceRef matches the node's workspace where a string names it — the
// directory itself or a path under it, as given or with its symlinks resolved
// (the CLI reports its working directory resolved) — or is nil without one.
func workspaceRef(workspace string) *regexp.Regexp {
	if workspace == "" {
		return nil
	}
	var names []string
	for _, ws := range []string{filepath.Clean(workspace), resolvedPath(workspace)} {
		if ws != "" && ws != "/" && ws != "." && !slices.Contains(names, ws) {
			names = append(names, ws)
		}
	}
	if len(names) == 0 {
		return nil
	}
	// The longest first: one name may extend the other.
	slices.SortFunc(names, func(a, b string) int { return len(b) - len(a) })
	for i, n := range names {
		names[i] = regexp.QuoteMeta(n)
	}
	return regexp.MustCompile(`(?:` + strings.Join(names, "|") + `)(/|$|[\s"';&|)])`)
}

// resolvedPath is path with its symlinks resolved, or "" when it does not
// resolve.
func resolvedPath(path string) string {
	r, err := filepath.EvalSymlinks(path)
	if err != nil {
		return ""
	}
	return r
}

// namesTaskOutput reports whether a call reads a background task's output
// file: a command's output, read after the call that ran it — which may have
// named a secret the command prints back. A path in the node's workspace is
// the workspace's own, whatever its name: a worktree's root is named by its
// run's UUID, like the CLI's session directory.
func namesTaskOutput(input map[string]any, own *regexp.Regexp) bool {
	found := false
	secretguard.MaterializeLeaves(input, func(s string) string {
		t := s
		if own != nil {
			t = own.ReplaceAllString(t, ".$1")
		}
		found = found || taskOutputFile.MatchString(t)
		return s
	})
	return found
}

// materializeSecretsHandler is the PreToolUse handler that swaps secret
// placeholders for their values in a tool's input.
func materializeSecretsHandler(materialize func(string) string) func(context.Context, claudesdk.HookCallbackInput) (claudesdk.HookOutput, error) {
	return func(_ context.Context, in claudesdk.HookCallbackInput) (claudesdk.HookOutput, error) {
		if len(in.ToolInput) == 0 || keepsPlaceholders(in.ToolName) {
			return claudesdk.HookOutput{}, nil
		}
		updated, changed := secretguard.MaterializeLeaves(in.ToolInput, materialize)
		if !changed {
			return claudesdk.HookOutput{}, nil // no placeholder present
		}
		out := updated.(map[string]any)
		if describedByCommand[in.ToolName] {
			// The CLI labels a shell with its description — with its command
			// when there is none — and relays that label to the model when a
			// background shell ends: it stays in placeholder form.
			if d, ok := in.ToolInput["description"].(string); ok && d != "" {
				out["description"] = d
			} else if c, ok := in.ToolInput["command"].(string); ok {
				out["description"] = c
			}
		}
		return claudesdk.HookOutput{Decision: "allow", UpdatedInput: out}, nil
	}
}

// describedByCommand: the tools the CLI labels with their description, or
// their command when there is none (a background shell, a monitor).
var describedByCommand = map[string]bool{"Bash": true, "PowerShell": true, "Monitor": true}

// installRewriteHook adds a PreToolUse hook on the Bash tool that rewrites
// commands to their compressed equivalent (e.g. "git status" → "rtk git
// status"), saving 60–90% of output tokens, when compression is enabled for
// this node and at least one rewriter plugin's binary is present. The rewrite
// decision is delegated to each rewriter's own contract (single source of
// truth); iterion uses rewriters purely as compressors — never a permission
// gate — so it always auto-allows the rewritten command. The rewrite runs
// host-side; the (sandboxed) CLI runs the rewritten command in-container
// against the bind-mounted rewriter binary (the chain's run env rides both
// spawns: rewriterRunEnvOpts).
func installRewriteHook(task Task, opts []claudesdk.Option) []claudesdk.Option {
	mode := rewrite.ParseMode(task.CompressMode)
	chain := rewrite.NewChain(task.Rewriters)
	if !mode.Enabled() || !chain.Available() {
		return opts
	}
	bashMatcher := "^Bash$"
	return append(opts, claudesdk.WithHook(claudesdk.HookPreToolUse, claudesdk.HookMatcher{
		Matcher: &bashMatcher,
		Handler: rewriteCommandHandler(chain, mode, task.MaterializeSecrets),
	}))
}

// rewriterRunEnvOpts sets the rewriter chain's run env on a CLI spawn, whatever
// the compression mode — the agent may run a rewriter itself, or an operator's
// own hook may: it keeps what a command ran and printed out of the rewriter's
// own stores.
func rewriterRunEnvOpts(task Task) []claudesdk.Option {
	var opts []claudesdk.Option
	for _, kv := range rewrite.NewChain(task.Rewriters).RunEnv() {
		k, v, _ := strings.Cut(kv, "=")
		opts = append(opts, claudesdk.WithEnv(k, v))
	}
	return opts
}

// rewriteCommandHandler is the PreToolUse handler that rewrites a Bash
// command to its compressed equivalent. A command naming a secret is not
// compressed: the compressor runs it — rtk records every command it runs in
// its history, value included — and the CLI keeps one PreToolUse hook's
// updatedInput: the materialisation hook's runs it.
func rewriteCommandHandler(chain *rewrite.Chain, mode rewrite.Mode, materialize func(string) string) func(context.Context, claudesdk.HookCallbackInput) (claudesdk.HookOutput, error) {
	return func(hookCtx context.Context, in claudesdk.HookCallbackInput) (claudesdk.HookOutput, error) {
		if materialize != nil {
			if _, named := secretguard.MaterializeLeaves(in.ToolInput, materialize); named {
				return claudesdk.HookOutput{}, nil
			}
		}
		updated, changed := chain.RewriteCommandField(hookCtx, mode, in.ToolInput)
		if !changed {
			return claudesdk.HookOutput{}, nil
		}
		// The CLI labels a shell with its description, with its command when
		// there is none, and relays the label to the agent when a background
		// shell ends: the agent's own command, not the compressed one.
		if d, _ := in.ToolInput["description"].(string); d == "" {
			if c, ok := in.ToolInput["command"].(string); ok {
				updated["description"] = c
			}
		}
		return claudesdk.HookOutput{
			Decision:       "allow",
			DecisionReason: "compress auto-rewrite",
			UpdatedInput:   updated,
		}, nil
	}
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
		if permission.IsReservedMCPServerName(s.Name) {
			b.Logger.Warn("[%s#%d/claude-code] MCP server %q: reserved for internal infrastructure; skipped — rename the custom server", task.NodeID, task.Iteration, s.Name)
			continue
		}
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

// keepsPlaceholders: the tools whose input is kept rather than run — the
// node's report (StructuredOutput: the CLI stores the input it is called
// with), the session's task list, a scheduled prompt (CronCreate writes it to
// .claude/scheduled_tasks.json, ScheduleWakeup sends it back to the model), a
// memory note, a skill's arguments (the CLI injects the skill's body with
// them, where no hook runs), iterion's own MCP tools (a question to the
// operator, a board issue, a run query). The CLI or iterion stores and shows
// it: it stays in placeholder form.
func keepsPlaceholders(name string) bool {
	switch name {
	case structuredOutputToolName, "TodoWrite", "TaskCreate", "TaskUpdate", "CronCreate", "memory_write", "Skill", "ScheduleWakeup":
		return true
	}
	return strings.HasPrefix(name, "mcp__"+askUserMCPServerName+"__") ||
		strings.HasPrefix(name, "mcp__"+boardMCPServerName+"__") ||
		strings.HasPrefix(name, "mcp__"+runsMCPServerName+"__")
}
