package delegate

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/cost"
	"github.com/SocialGouv/iterion/pkg/backend/delegate/claudesdk"
	"github.com/SocialGouv/iterion/pkg/backend/permission"
	"github.com/SocialGouv/iterion/pkg/backend/rewrite"
	"github.com/SocialGouv/iterion/pkg/backend/toolcatalog"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/usagecap"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// claudeCodeModelID strips the provider prefix a model spec carries when
// that provider rides the SAME Anthropic-compatible wire this backend
// drives — derived from secrets.AnthropicWireSlotOrder (whose docs
// mandate adding a provider HERE, once): the claude CLI forwards the
// value verbatim, and a prefixed code ("zai/glm-5.3") reaches z.ai whole
// and dies with "[1211] Unknown Model". A genuinely foreign prefix
// ("openai/…") stays and fails fast as the non-Anthropic model it is.
func claudeCodeModelID(spec string) string {
	for _, slot := range secrets.AnthropicWireSlotOrder {
		if slot == string(secrets.OAuthKindClaudeCode) {
			// An OAuth kind is a credential slot, not a model prefix.
			continue
		}
		if after, ok := strings.CutPrefix(spec, slot+"/"); ok {
			return after
		}
	}
	return spec
}

// defaultClaudeCodeModel is the model iterion forces on the claude_code
// backend when the workflow doesn't specify one. Mirrors the official
// Claude Code CLI default — Opus 5 (1M context window). Workflows can
// always override via the node's `model:` field — including the
// env-driven form `model: "${ITERION_CLAUDE_CODE_MODEL:-claude-opus-5-5}"`
// which the IR expander in pkg/backend/model/executor.go resolves
// before this backend ever sees the task. Operators who want to pin
// every claude_code node to a single gateway-side alias (e.g. GLM 5.1
// on z.ai) should put the env var in their .env and use the DSL form
// above in the bots that opt in.
const defaultClaudeCodeModel = "claude-opus-5-5"

// defaultClaudeCodeEffort is the reasoning effort iterion forces on the
// claude_code backend when the workflow doesn't specify one. The bare API
// default on the opus tier is "high", but the claude_code backend runs
// implementers/fixers — coding and agentic work — for which Anthropic
// recommends starting at "xhigh" (platform.claude.com/docs/en/build-with-claude/effort).
// Workflows can always override via `reasoning_effort:`.
const defaultClaudeCodeEffort = "xhigh"

// claudeNativeTools is the built-in Claude Code surface which remains present
// unless explicitly removed with --disallowedTools. The DSL's tools: list is
// expressed in Iterion aliases (read_file, glob, ...), while
// --allowedTools only auto-approves matching calls; it does not hide the
// other native tools. Keep this closed list deliberately small and explicit:
// MCP tools are supplied separately and must not be guessed here.
var claudeNativeTools = []string{
	"Bash", "Read", "Glob", "Grep", "Write", "Edit", "MultiEdit",
	"NotebookEdit", "Task", "WebFetch", "WebSearch", "ToolSearch",
	"TodoWrite", "Skill",
}

// claudeNativeToolsForAllowed maps Iterion declarations to the corresponding
// Claude Code tool names.
//
// It is a PROJECTION of the shared spelling table (toolcatalog.CanonicalToolName)
// onto this backend's roster, never a second table: a synonym added there
// grants here and bounds in the permission gate at once, which is what stops
// `tools:` and `allow:/ask:/deny:` from disagreeing about one word (#1579).
//
// A declaration whose canonical key names no native tool is intentionally not
// treated as a native permission: it may be an Iterion wrapper or an MCP
// tool, and must never widen the ambient native surface by accident.
//
// Keying on the canonical rather than on a spelling list of its own DOES
// widen what some declarations keep, deliberately: `shell`/`sh`/
// `run_terminal_command` now keep `Bash`, `multiedit`/`str_replace`/
// `search_replace`/`edit_mode`/`apply_patch` keep `Edit`+`MultiEdit`,
// `agent`/`spawn_subagent` keep `Task`, `web`/`fetchurl` keep `WebFetch`, and
// the case-insensitive forms of all of them do too. Each was a node declaring
// a tool and losing it — the same disagreement between `tools:` and the gate
// that #1579 is about, seen from the grant side. Measured over the repo's
// whole `.bot` corpus: of its 85 distinct declared tool names, exactly one
// changes what it grants — `agent`, on two e2e fixtures, and on no shipped
// bot.
func claudeNativeToolsForAllowed(allowed []string, diagnosticShell bool) map[string]bool {
	native := make(map[string]bool)
	for _, declared := range allowed {
		for _, name := range claudeNativeForCanonical[toolcatalog.CanonicalToolName(declared)] {
			native[name] = true
		}
	}
	if diagnosticShell {
		native["Bash"] = true
	}
	return native
}

// claudeNativeForCanonical is the projection itself: one row per concept the
// native roster has a tool for. `edit` is one-to-many on purpose — no
// declaration grants MultiEdit on its own, so an edit-capable node that lost
// it could not apply a multi-hunk change.
var claudeNativeForCanonical = map[string][]string{
	"bash":         {"Bash"},
	"read":         {"Read"},
	"glob":         {"Glob"},
	"grep":         {"Grep"},
	"write":        {"Write"},
	"edit":         {"Edit", "MultiEdit"},
	"notebookedit": {"NotebookEdit"},
	"agent":        {"Task"},
	"webfetch":     {"WebFetch"},
	"websearch":    {"WebSearch"},
	"toolsearch":   {"ToolSearch"},
	"todowrite":    {"TodoWrite"},
	"skill":        {"Skill"},
}

// claudeSpawnBounds are the bounds that hold for a task whatever the spawn
// is — the ones that do NOT come from the node's `tools:` declaration.
//
// They live in a function, beside claudeToolOptions, for one measured reason:
// a claude_code task with an output schema spawns the CLI TWICE on the same
// session under the same always-on bypassPermissions, and a bound carried on
// one spawn and not the other is no bound at all.
//
// The bounds that travel on ARGV follow the task this way. The HOOK-borne
// ones cannot: claudesdk.Prompt has no hook channel and drops them, which is
// why the permission gate is answered on the spawn that cannot carry it
// (formatOutput) by withholding the native surface there, and never on the
// one that can — joining the gate to the declaration would delete a gated
// node's own tools. Capabilities that are not bounds
// (secrets materialisation, the rtk rewriter, inbox drain, edit-miss
// resilience, ask_user) are absent from the second spawn on purpose: their
// absence narrows it.
//
//   - --strict-mcp-config makes the node's own MCP declaration authoritative:
//     the operator's personal ~/.claude.json servers don't boot inside bot
//     nodes (undeclared tools, per-visit npx/chromium boots on loop-heavy
//     bots, API keys on the argv — issue #506).
//     ITERION_CLAUDE_CODE_STRICT_MCP=0 restores host inheritance.
//   - The tools that hand work to a later turn (headlessWithheldTools:
//     Workflow, the wake-up and cron schedulers, RemoteTrigger) are withheld
//     from every node, ultracode included. The session is one-shot, so none
//     of them can deliver. The single-subagent surface (Agent/Task/
//     TaskOutput/Monitor) stays by default: that adaptivity is the point of
//     the backend, and backgroundTasksOffEnv makes it run in the foreground.
//     It goes with the opt-in knob for a deployment whose served model family
//     hallucinates task ids and deadlocks on TaskOutput. An ultracode node
//     keeps it on the spawn that can be gated. The structured-output spawn of
//     a GATED node keeps none of it, because nothing there can run the policy
//     (see formatOutput).
func claudeSpawnBounds(task Task) []claudesdk.Option {
	var opts []claudesdk.Option
	if strictMCPFromEnv() {
		opts = append(opts, claudesdk.WithStrictMCPConfig(true))
	}
	opts = append(opts, claudesdk.WithDisallowedTools(claudeSpawnBoundWithheld(task)...))
	// tool_max_steps caps agentic tool-use iterations. The field was defined
	// in delegate.Task but never wired into the CLI, so an author who set
	// `tool_max_steps: 25` got silent infinity — observed with GLM running
	// discover_outdated through 60+ tool calls instead of stopping at 25.
	// Mapped to claude's --max-turns (the closest semantic: one turn = one
	// assistant message exchange, usually one tool call + response). A cap
	// carried on one spawn and not the other is not a cap.

	if task.ToolMaxSteps > 0 {
		opts = append(opts, claudesdk.WithMaxTurns(task.ToolMaxSteps))
	}
	return opts
}

// claudeSpawnBoundWithheld is the half of a spawn's --disallowedTools that
// claudeSpawnBounds owns: the tools no headless session can use, plus the
// single-subagent surface when the opt-in knob removes it from a node that is
// not ultracode.
func claudeSpawnBoundWithheld(task Task) []string {
	withheld := append([]string(nil), headlessWithheldTools...)
	if !task.Ultracode && disallowOrchestrationToolsFromEnv() {
		withheld = append(withheld, orchestrationTools...)
	}
	return withheld
}

// claudeDeclarationWithheld is the half of a spawn's --disallowedTools that the
// node's `tools:` declaration owns. It is nil for an undeclared surface.
func claudeDeclarationWithheld(task Task) []string {
	if !toolBoundaryApplies(task) {
		return nil
	}
	return claudeNativeDisallowedTools(task.AllowedTools, true, task.DiagnosticShell)
}

// claudeKeepsSubagents reports whether the spawn Execute makes still offers
// the model a subagent tool. It reads the same two lists that spawn puts on
// --disallowedTools, so the answer cannot drift from the argv. `Task` counts
// as the tool itself: it is the tool on older CLIs, and the current CLI maps
// the legacy name onto `Agent` (its alias table) before it applies a deny
// rule.
func claudeKeepsSubagents(task Task) bool {
	withheld := append(claudeSpawnBoundWithheld(task), claudeDeclarationWithheld(task)...)
	return !slices.Contains(withheld, "Agent") && !slices.Contains(withheld, "Task")
}

// headlessSubagentRule describes, in one place, how subagents behave in a
// claude_code session. Execute appends it once to the system prompt of every
// spawn that keeps the subagent tool, ultracode or not. It holds because
// backgroundTasksOffEnv makes it hold: the text describes the mechanism, it is
// not the mechanism. The ultracode section grants the orchestration and says
// nothing about how a subagent returns. A backend-neutral section cannot name
// this CLI's mechanics, and two statements of one rule drift apart.
const headlessSubagentRule = "\n\n## Subagents in this session\n\n" +
	"This session is not interactive. It ends with your final output, and nothing " +
	"reaches you after that: no completion notification, no scheduled wake-up. " +
	"Subagents therefore run in the foreground here: an Agent call returns that " +
	"agent's report as its tool result. To run several at once, put several Agent " +
	"calls in ONE message. They run concurrently, and every report comes back as " +
	"a tool result before your next step. Never end your turn to wait for work to " +
	"finish: anything still running when you produce your final output is lost. " +
	"A Bash command that outlives its timeout is killed: give it a timeout that " +
	"fits, and start anything longer with `nohup … &` and poll it."

// headlessBackgroundRule replaces headlessSubagentRule when background work
// is on (backgroundTasksOnFromEnv). The CLI's own guidance then says every
// background task notifies the agent when it completes; in this session the
// lifecycle waits for a background subagent or workflow only (holdsSession),
// and a shell or a monitor dies with the session.
const headlessBackgroundRule = "\n\n## Background work in this session\n\n" +
	"This session is not interactive: it ends with your final output. A background " +
	"subagent is waited for: its report reaches you before the session ends — " +
	"unless you launch it with `isolation: \"remote\"`, which is never waited for. A " +
	"background command (`run_in_background`), or a monitor you arm, is not: " +
	"anything still running when you give your final output is killed, its completion " +
	"notification with it. So, unlike what the Bash tool says, do not end your turn " +
	"to wait for a background command whose result you need: read the output file " +
	"its result names until the command has finished, or run it in the foreground " +
	"with a timeout that fits."

// headlessBackgroundUnheldRule is headlessBackgroundRule with the background
// lifecycle off (ITERION_CLAUDE_CODE_BACKGROUND_LIFECYCLE=off): nothing that
// runs in the background is waited for.
const headlessBackgroundUnheldRule = "\n\n## Background work in this session\n\n" +
	"This session is not interactive: it ends with your final output, and nothing " +
	"that runs in the background is waited for. A background subagent, command " +
	"(`run_in_background`) or monitor still running when you give your final output is " +
	"killed, its completion notification with it. The Agent tool launches in the " +
	"background by DEFAULT here, so pass `run_in_background: false` on every Agent " +
	"call — its report is then the tool result — and give a command a timeout that " +
	"fits instead of backgrounding it. Never end your turn to wait for background " +
	"work: no turn will come to deliver it."

// claudeCodeSystemPrompt is the text Execute's spawn appends to the CLI's
// native system prompt: the task's own sections, then what the session does
// with the work the agent hands off — the subagent rule when the spawn keeps
// a subagent tool, or, with background work on, the background rule on every
// spawn (a background shell needs no subagent tool).
func claudeCodeSystemPrompt(task Task) string {
	prompt := task.BuildSystemPrompt()
	rule := headlessSubagentRule
	switch {
	case backgroundTasksOnFromEnv():
		rule = headlessBackgroundRule
		if !resolveBackgroundLifecycleConfig().enabled {
			rule = headlessBackgroundUnheldRule
		}
	case !claudeKeepsSubagents(task):
		return prompt
	}
	if prompt == "" {
		return strings.TrimLeft(rule, "\n")
	}
	return prompt + rule
}

// claudeToolOptions turns a node's `tools:` declaration into the two CLI
// flags that carry it, for ONE spawn. It is a pure function so the decision
// can be executed rather than reasoned about, and so that every spawn of a
// task can take the same one: Execute's main pass and formatOutput's
// structured-output pass both call it, and a boundary appended to one and not
// the other is no boundary at all. The one exception is a GATED task, whose
// formatting spawn withholds the whole native surface instead of carrying the
// declaration — the two lists stay disjoint there (see formatOutput).
//
// It is not the node's whole tool surface: the bounds that do not come from
// the declaration live in claudeSpawnBounds (headlessWithheldTools, the
// orchestration knob, --strict-mcp-config, --max-turns); the gated-task
// withholding is NOT one of them — it belongs to formatOutput alone, the
// spawn that cannot carry the hook, and putting it in the shared helper
// deleted a gated node's own declared tools. `extraAllowedTools` is accumulated by the ask_user /
// board / runs / user-MCP wiring before it arrives here. What this owns is
// the declaration.
//
// The base list plus those extras are registered once, so no tool is listed
// twice (WithAllowedTools appends). An UNDECLARED surface means "no
// restriction" and registers nothing. A declared-empty list still registers:
// WithAllowedTools with no name is a no-op (the CLI flag is emitted only for
// a non-empty list), and the disallow list is the boundary.
func claudeToolOptions(task Task, extraAllowedTools []string) []claudesdk.Option {
	if !toolBoundaryApplies(task) {
		return nil
	}
	combined := append([]string(nil), task.AllowedTools...)
	combined = append(combined, extraAllowedTools...)
	return []claudesdk.Option{
		claudesdk.WithAllowedTools(combined...),
		// WithAllowedTools is an approval list, not an availability boundary.
		// Remove every undeclared built-in tool as well so a restricted judge
		// with Read/Glob cannot silently fall back to Bash or a write surface.
		claudesdk.WithDisallowedTools(claudeDeclarationWithheld(task)...),
	}
}

// toolBoundaryApplies reports whether this task's tool list is a visibility
// BOUNDARY for the CLI — the question `tools:` answers, as opposed to how
// many names it holds.
//
// A DECLARED list is one, empty included: `tools: []` is the author saying
// the node has no tools. An UNDECLARED list is not — the node keeps the
// ambient native surface — except when the runtime itself put something in
// AllowedTools (the image-attachment read_image append), which is a boundary
// the same way a declared list is.
func toolBoundaryApplies(task Task) bool {
	return task.ToolsDeclared || len(task.AllowedTools) > 0
}

// claudeNativeDisallowedTools turns a DSL tools: declaration into an actual
// Claude Code visibility boundary: every native tool the declaration does not
// name is removed. A DECLARED but empty list therefore removes all fourteen —
// that is the whole point of `tools: []`, and reading it as "no declaration"
// is what let a node that asked for no tools keep the full native roster.
//
// Whether a declaration exists at all is the caller's question, answered once
// by toolBoundaryApplies; `declared` keeps this function total for anyone who
// calls it without asking first — with no declaration the legacy unrestricted
// native-tool semantics are preserved.
func claudeNativeDisallowedTools(allowed []string, declared, diagnosticShell bool) []string {
	if !declared {
		return nil
	}
	nativeAllowed := claudeNativeToolsForAllowed(allowed, diagnosticShell)
	disallowed := make([]string, 0, len(claudeNativeTools))
	for _, tool := range claudeNativeTools {
		if !nativeAllowed[tool] {
			disallowed = append(disallowed, tool)
		}
	}
	return disallowed
}

// gatedFormattingWithheld is everything the structured-output spawn of a
// GATED task withholds: the whole native roster and the whole orchestration
// surface this package enumerates.
//
// The roster half is the declaration's stand-in — a declaration has no role on
// a spawn where nothing may run, and emitting it would name a tool on
// --allowedTools and on --disallowedTools at once. The orchestration half is
// the part the roster misses: `claudeNativeTools` happens to carry `Task`, so
// `Task` was withheld by accident while `Agent` — the spelling the current CLI
// uses — `TaskOutput` and `Monitor` were not. claudeSpawnBounds removes those
// only under an opt-in knob that is off by default and never for an ultracode
// node; neither condition has anything to do with the gate, and this spawn has
// no gate at all.
//
// It is NOT in claudeSpawnBounds: that helper runs on both spawns, and a
// withholding that belongs to one of them deleted a gated node's own declared
// tools when it was put there.
//
// What this costs, named rather than assumed: a node whose first pass was cut
// off mid-orchestration (the `--max-turns` case this pass exists for) cannot
// dispatch a subagent here to finish that work. `bots/whats-next` is the
// shipped example — ultracode, `permission: deny`, an output schema. The
// exchange is deliberate: finishing it meant running tools the operator asked
// to approve, on the one spawn where no approval can be asked.
//
// This withholding is the ENFORCED half, and it is incomplete: it names a
// roster this project does not own, so the tools outside all three lists
// survive it — #1651 tracks the roster itself. The sentence the gated pass
// also carries ("call no tool other than StructuredOutput") covers those names
// whatever they are spelled, but only while the model cooperates, and the
// threat here is data in the resumed transcript. It is defence in depth beside
// the flag, never in place of it.
func gatedFormattingWithheld() []string {
	withheld := claudeNativeDisallowedTools(nil, true, false)
	withheld = append(withheld, orchestrationTools...)
	withheld = append(withheld, headlessWithheldTools...)
	// What the live CLI STILL registered once the lists above were withheld,
	// read from its own `system/init` roster on 2.1.220 rather than guessed:
	// EnterWorktree, ExitWorktree, ReportFindings, SendMessage,
	// StructuredOutput, TaskStop. All but StructuredOutput reach another
	// session or MOVE THE WORKTREE the session acts in — which is the very
	// thing the engine's parallel-branch guard protects. StructuredOutput is
	// the one tool this pass needs and is deliberately kept.
	//
	// BashOutput and KillShell are withheld too: they register in other
	// configurations of the same CLI, and withholding a name this pass has no
	// use for costs nothing.
	//
	// Listed here, in the gated arm alone, so neither `orchestrationTools` nor
	// the env knob that reads it changes meaning. And the list remains an
	// enumeration of a roster this project does not own (#1651) — it is read
	// from one CLI version and will go stale; the pass's own instruction is
	// what covers whatever the next version adds.
	withheld = append(withheld,
		"EnterWorktree", "ExitWorktree",
		"SendMessage", "ReportFindings",
		"BashOutput", "KillShell", "TaskStop")
	// `Task` is on the native roster AND in orchestrationTools, so the union
	// repeats it. The argv join dedupes for every caller, but a list that
	// names a tool twice is a poor witness for the tests that read it.
	seen := make(map[string]bool, len(withheld))
	out := withheld[:0]
	for _, name := range withheld {
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// ClaudeCodeBackend delegates work to the `claude` CLI (claude-code)
// via the Claude Agent SDK.
type ClaudeCodeBackend struct {
	// Command overrides the CLI binary path (default: "claude").
	Command string
	// Logger is the leveled logger for diagnostic output.
	Logger *iterlog.Logger
	// renewalWait bounds how long an auth render from a forfait spawn waits
	// for a rotation to reach the forfait file (forfaitRenewalWait); zero is
	// the default.
	renewalWait time.Duration

	// formatOutputFn replaces the CLI-spawning formatting pass in tests: the
	// loop around it — retry, terminal verdict, usage, cost, lost background
	// work — is where the accounting defects lived, and it had no seam to be
	// exercised through.
	formatOutputFn func(ctx context.Context, task Task, sessionID string) (*claudesdk.ResultMessage, []string, error)
	// formatRetryDelay overrides the pause before a repeated formatting
	// attempt: zero means the default, negative means none (tests). Per
	// backend, not package-wide, so parallel tests cannot race on it.
	formatRetryDelay time.Duration
}

// defaultFormatRetryDelay is the pause before a formatting attempt is
// repeated after a retryable render: a throttle answered immediately is a
// throttle again.
const defaultFormatRetryDelay = 2 * time.Second

// retryDelay is the pause this backend takes before repeating a formatting
// attempt.
func (b *ClaudeCodeBackend) retryDelay() time.Duration {
	switch {
	case b.formatRetryDelay < 0:
		return 0
	case b.formatRetryDelay == 0:
		return defaultFormatRetryDelay
	}
	return b.formatRetryDelay
}

// formatPass runs one formatting pass: the CLI, or the test seam. It also
// returns the background work the pass's process lost (see formatOutput).
func (b *ClaudeCodeBackend) formatPass(ctx context.Context, task Task, sessionID string) (*claudesdk.ResultMessage, forfaitSpawn, []string, error) {
	if b.formatOutputFn != nil {
		rm, lost, err := b.formatOutputFn(ctx, task, sessionID)
		return rm, forfaitSpawn{}, lost, err
	}
	return b.formatOutput(ctx, task, sessionID)
}

// Execute runs the claude CLI with the given task using the Claude Agent SDK.
// buildTransportOptions assembles the base claudesdk options for a claude_code
// run — system prompt, setting sources, cwd, CLI path, permission mode, model,
// sandbox command builder, reasoning effort, and max-turns. Split out of the
// long Execute method; this prefix carries no post-session state (the
// closure-capturing hooks — stderr/ask_user/secret/board/inbox — stay in
// Execute).
//
// The second return value is the spawn's cleanup, never nil: it removes the
// flag settings file the spawn reads (claudeSettingsFiles on the host, its
// in-container copy in a sandbox) and, for a sandboxed task, terminates the
// in-container claude process recorded by the command wrapper
// (native:221edac8). Execute must defer it so aborted sessions leak neither.
func (b *ClaudeCodeBackend) buildTransportOptions(task Task) ([]claudesdk.Option, func()) {
	var opts []claudesdk.Option
	var cleanup func()
	// The flag settings object perTaskSpawnOpts hands the spawn below. The
	// builder finds it on argv and moves it, routing pin added, into a file.
	_, flagSettings := claudeSpawnPins(task)

	// Route the SDK's internal error diagnostics (control-protocol
	// delivery failures and the like) to the backend logger.
	opts = append(opts, claudesdk.WithLogf(func(format string, args ...any) {
		b.Logger.Error(format, args...)
	}))

	// APPEND, do not REPLACE. --system-prompt would discard Claude Code's
	// native agentic system prompt (tool-use discipline, plan-before-act,
	// read-before-edit, parallel-tool reflex, file:line conventions, refusal
	// posture) and leave the model with only the recipe's task text — the root
	// cause of iterion-via-Claude-Code being less adaptive than native Claude
	// Code. --append-system-prompt keeps the native prompt as the base and adds
	// the workflow's instructions on top. Task.SystemPromptMode is
	// SystemPromptAppendToNative for this backend, so BuildSystemPrompt emits
	// author + suffixes only (no iterion-authored base — the native prompt is it);
	// claudeCodeSystemPrompt adds the subagent rule this backend owns.
	systemPrompt := claudeCodeSystemPrompt(task)
	if systemPrompt != "" {
		opts = append(opts, claudesdk.WithAppendSystemPrompt(systemPrompt))
	}
	// Load the operator's settings sources so the agent behaves like native
	// Claude Code in the target repo: user-level (~/.claude/CLAUDE.md +
	// settings.json) and project-level (the repo's CLAUDE.md + .claude/
	// settings.json). --append-system-prompt alone does not re-enable settings
	// discovery in --print mode; --setting-sources does. Honours the same paths
	// in a sandbox (the workspace and ~/.claude are bind-mounted at their host
	// absolute paths). Which scopes, and which memory files they may not read,
	// is the node's ambient-context policy (ADR-119, claudeAmbient); the
	// exclusions ride the flag settings layer (claudeSpawnPins).
	amb := claudeAmbient(task)
	reportLegacySources(b.Logger, amb)
	opts = append(opts, amb.settingSourcesOption())
	// Setting sources are inherited (above); MCP servers are NOT. The node's
	// resolved MCP set — .bot `mcp_server:`/`mcp:` blocks, the repo's
	// .mcp.json via autoload_project, iterion's ask_user/board servers —
	// travels via --mcp-config, and --strict-mcp-config makes that set
	// authoritative: the operator's personal ~/.claude.json servers don't
	// boot inside bot nodes (undeclared tools, per-visit npx/chromium boots
	// on loop-heavy bots, API keys on the argv — issue #506).
	// ITERION_CLAUDE_CODE_STRICT_MCP=0 restores host inheritance.
	opts = append(opts, claudeSpawnBounds(task)...)
	// Cwd handling differs by sandbox state. On the host (no sandbox)
	// we pass the workdir straight through to claudesdk → cmd.Dir.
	// In the sandbox it's the host worktree path that doesn't exist
	// inside the container — the docker driver's Command falls back
	// to the spec's WorkspaceFolder (the bind-mount target) when
	// Cwd is empty, which is the path we actually want.
	if task.WorkDir != "" && task.Sandbox == nil {
		opts = append(opts, claudesdk.WithCwd(task.WorkDir))
	}
	// Same lifetime trade-off for the CLI binary path: the SDK's
	// default exec.LookPath("claude") runs on the host and returns
	// the operator's host path (e.g. /home/jo/.local/bin/claude).
	// Forwarded into a `docker exec` invocation that path doesn't
	// exist inside the container, and claude exits silently with
	// "session ended without result message" upstream. Pin to the
	// bare name so the in-container PATH lookup wins.
	if task.Sandbox != nil {
		opts = append(opts, claudesdk.WithCLIPath("claude"))
	}
	// Bypass interactive permission prompts: the runtime enforces safety via
	// workspace isolation and allowed-tool lists, so the delegate subprocess
	// does not need its own permission gate.
	opts = append(opts, claudesdk.WithPermissionMode("bypassPermissions"))

	// The CLI requires --verbose when using --output-format=stream-json in
	// --print mode. The SDK always uses stream-json, so we must enable verbose.
	opts = append(opts, claudesdk.WithVerbose(true))

	// Stderr forwarding is registered once, further down (the
	// stderrBuf-capturing callback): WithStderrCallback assigns (not
	// appends) the SDK's single callback slot, so a logger-only
	// registration here would simply be overwritten by that later,
	// richer one (live Info logging + buffered capture for diagnostics).

	model := task.Model
	if model == "" {
		model = defaultClaudeCodeModel
	}
	model = claudeCodeModelID(model)
	opts = append(opts, claudesdk.WithModel(model))

	// CLI binary path: the per-node task override (DSL `command:`, an
	// alternate claude-code-compatible CLI) wins over the backend-level
	// default; the shared backend is
	// left unmutated so it can serve other nodes with their own override.
	cliPath := b.Command
	if task.Command != "" {
		cliPath = task.Command
	}
	if cliPath != "" {
		opts = append(opts, claudesdk.WithCLIPath(cliPath))
	}

	// When the run is sandboxed, route the claude CLI subprocess
	// through the sandbox driver so the agent's bash/edit tools
	// execute inside the container, not on the host. Cwd/Env are
	// passed via the runtime-native channels (e.g. `docker exec
	// --workdir / --env`); the SDK disables its own cmd.Dir / cmd.Env
	// application when a builder is set.
	if task.Sandbox != nil {
		run := task.Sandbox
		// Record the in-container PID so the session end can actually
		// terminate claude: killing the host-side `docker exec` client
		// leaks the in-container process (native:221edac8 — leaked
		// claudes stack across retries and starve the forfait). The
		// wrapper writes its PID to a pidfile then exec's claude (same
		// PID, same fds); Execute defers killSandboxDelegate.
		mark := sandboxDelegateMark(task)
		cleanup = killSandboxDelegate(run, mark, b.Logger, sandboxSettingsFile(mark))
		opts = append(opts, claudesdk.WithCommandBuilder(func(ctx context.Context, path string, args []string, cwd string, env map[string]string, openStdin bool) *exec.Cmd {
			env = claudeModelDefaultEnv(env)
			// Surface the resolved CLI invocation so failures like
			// "session ended without result" can be traced back to a
			// concrete `docker exec` command. Without this every silent
			// claude exit is opaque even with stderr capture. (Logger
			// methods are nil-safe — no guard needed.)
			b.Logger.Info("claude-code: exec %v (cwd=%s, env_keys=%d, stdin=%v)",
				redactedArgvPreview(path, args), cwd, len(env), openStdin)
			// The routing pin is computed inside the container, from the
			// environment the CLI runs with (claudeSandboxArgv).
			argv, err := claudeSandboxArgv(mark, path, args, flagSettings)
			if err != nil {
				return claudeFailedCmd(ctx, path, err)
			}
			// KeepStdinOpen mirrors the SDK's OpenStdin flag so the docker
			// driver adds `--interactive` to docker exec. Without this,
			// Session-mode (NDJSON over stdin) silently fails: the SDK
			// later wires cmd.StdinPipe() but docker has already closed
			// stdin on the child, claude reads EOF, and exits 0 with no
			// output — matching the cli_exit_code=0 silent-failure path.
			return run.Command(ctx, wrapSandboxDelegateArgv(mark, argv), sandbox.ExecOpts{
				WorkDir:       cwd,
				Env:           env,
				KeepStdinOpen: openStdin,
			})
		}))
	} else {
		// Host path: install a builder to (a) surface the resolved claude
		// invocation — the default spawn is opaque, so a silent
		// "0 tokens / formatting-pass-fallback" structured-output failure can't
		// be traced to the concrete command + per-task env overrides — (b) keep
		// the env identical to the SDK default (os.Environ() + the per-task
		// entries via hostSpawnEnv), and (c) pin the routing variables at the
		// values that env holds (claudeSettingsFiles.pinHost).
		files := &claudeSettingsFiles{}
		cleanup = func() { files.remove(b.Logger) }
		opts = append(opts, claudesdk.WithCommandBuilder(func(ctx context.Context, path string, args []string, cwd string, env map[string]string, openStdin bool) *exec.Cmd {
			env = claudeModelDefaultEnv(env)
			keys := make([]string, 0, len(env))
			for k := range env {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			cmd := exec.CommandContext(ctx, path, args...)
			if cwd != "" {
				cmd.Dir = cwd
			}
			cmd.Env = hostSpawnEnv(env)
			files.pinHost(cmd, flagSettings)
			b.Logger.Info("claude-code: host exec %v (cwd=%s, stdin=%v, task_env_keys=%v)",
				redactedArgvPreview(path, cmd.Args[1:]), cwd, openStdin, keys)
			return cmd
		}))
	}

	opts = append(opts, perTaskSpawnOpts(task)...)
	// An ExtraEnv entry for a pinned key is dropped on every spawn. Said once
	// per delegation, here: the formatting pass drops the same entries.
	_, overridden := claudeExtraEnvEntries(task)
	for _, key := range overridden {
		b.Logger.Warn("[%s#%d/claude-code] ExtraEnv sets %s, which every claude_code spawn pins: the entry is ignored and the pinned value applies",
			task.NodeID, task.Iteration, key)
	}
	// Same for a rewriter plugin's run_env: a pin outranks it, said out loud.
	if _, dropped := claudeEnvPinsAndDropped(task); len(dropped) > 0 {
		for _, key := range dropped {
			b.Logger.Warn("[%s#%d/claude-code] a rewriter's run_env sets %s, which every claude_code spawn pins: the entry is ignored and the pinned value applies",
				task.NodeID, task.Iteration, key)
		}
	}

	return opts, cleanup
}

// claudeCodeThinkingDisplay resolves the --thinking-display value passed
// to the CLI. In headless (--print) mode the CLI defaults thinking
// display to omitted on Opus 4.8+ — thinking blocks stream with empty
// text and only the encrypted signature — so iterion requests the
// readable summary by default. ITERION_CLAUDE_CODE_THINKING_DISPLAY
// overrides: "omitted" restores the latency-optimised default, "off"
// stops passing the flag entirely (required for claude CLIs older than
// the flag, which reject unknown options).
func claudeCodeThinkingDisplay() string {
	switch v := os.Getenv("ITERION_CLAUDE_CODE_THINKING_DISPLAY"); v {
	case "":
		return "summarized"
	case "off":
		return ""
	default:
		return v
	}
}

func (b *ClaudeCodeBackend) Execute(ctx context.Context, task Task) (result Result, err error) {
	if task.WorkDir != "" {
		if err := validateWorkDir(task.WorkDir, task.BaseDir); err != nil {
			return Result{}, err
		}
	}
	// cliTurnCompleted records that the CLI carried this call to its own
	// ResultMessage. It is the discriminator turnFinished needs and the
	// defer below cannot read: `rm` is declared further down, so the
	// closure registered here cannot close over it.
	var cliTurnCompleted bool
	// Fire OnTurnFinished once on the way out, when the runtime wired
	// the hook and the delegate produced a SessionID. Wrapped in a
	// defer so every return path that HAS a turn (Pass 1, recovery,
	// two-pass, ask_user escalation, and a result the guards below then
	// type as a failure) flows through the same notification — avoiding
	// the maintenance trap of remembering to call it before every
	// `return result, ...`. Skipped when no session was ever opened, and
	// when the stream died before the CLI produced a result.
	defer func() {
		if task.Hooks.OnTurnFinished == nil {
			return
		}
		if !turnFinished(err, cliTurnCompleted, result) {
			return
		}
		text := ""
		if s := result.Output["_assistant_text"]; s != nil {
			text, _ = s.(string)
		}
		task.Hooks.OnTurnFinished(TurnFinishedInfo{
			SessionID:    result.SessionID,
			FinishReason: "", // claude_code SDK doesn't surface a granular reason at Result level
			Text:         text,
			// Result.Tokens is in+out with no split available here, so it
			// travels as the aggregate rather than being filed under a
			// direction it was never measured in (#992).
			AggregateTokens:           result.Tokens,
			TerminatedBackgroundTasks: result.TerminatedBackgroundTasks,
		})
	}()

	opts, spawnCleanup := b.buildTransportOptions(task)
	// Remove the flag settings file and terminate the in-container claude on
	// every exit path — clean, aborted, or panicking. Idempotent: after a
	// clean CLI exit the recorded PID is gone and the kill script no-ops
	// (native:221edac8).
	defer spawnCleanup()
	// Allowed-tools registration is deferred to a single call near the end
	// of this function. WithAllowedTools APPENDS to the SDK's slice, so
	// registering the base set here and again below (combined with MCP
	// extras) would list every base tool twice. We accumulate the MCP
	// extras (ask_user, board.*) into extraAllowedTools and emit one call.
	var extraAllowedTools []string

	// Inject Anthropic-flavoured credentials and resolve session resume/fork
	// (see helper). The returned fingerprint is recorded on the Result so a
	// later resume can detect a credential change.
	opts, currentFingerprint, spawn, credErr := b.setupCredsAndSession(ctx, task, opts)
	if credErr != nil {
		return Result{BackendName: BackendClaudeCode, ExitCode: -1}, credErr
	}

	// Stamp every usage reading with the provider-routing label of THIS
	// session. One wrap here covers all three detection sites (the
	// rate_limit_event stream and both text-relayed refusal paths): the
	// consumer keys the reading under the credential the session actually
	// ran on, not the bundle's default precedence.
	task.Hooks.OnUsageWindow = stampUsageSource(task.Hooks.OnUsageWindow, currentFingerprint)

	// Structured output handling. claude CLI >= 2.1 accepts --json-schema
	// (WithOutputFormat) TOGETHER with --allowedTools in a single pass: the
	// agent does its tool work and then calls the native StructuredOutput
	// tool, which populates result.structured_output. So we always set
	// WithOutputFormat when a schema is present, even WITH tools. The
	// `needsTwoPass` flag no longer gates whether structured output is
	// requested — it gates only the Pass-2 FALLBACK (resume with no tools to
	// extract the schema) used when Pass 1 returns no structured output
	// (e.g. the agent hit --max-turns before calling StructuredOutput, or a
	// sandbox edge case). Setting the schema in Pass 1 also stops the agent
	// from reaching for an unregistered StructuredOutput tool and logging a
	// spurious "No such tool available: StructuredOutput" error. Empirically
	// the agent still completes its tool work BEFORE finalizing (verified
	// against claude 2.1.177), so this does not make it rush its output.
	told := ledgerTerminated(task.SessionLedger, resumedSessionID(task, currentFingerprint))
	prompt := terminatedBackgroundNote(told, task.UserPrompt)
	needsTwoPass := len(task.OutputSchema) > 0 && len(task.AllowedTools) > 0
	if len(task.OutputSchema) > 0 {
		var schema map[string]any
		if json.Unmarshal(task.OutputSchema, &schema) == nil {
			opts = append(opts, claudesdk.WithOutputFormat(schema))
		}
	}

	// Capture stderr for post-session diagnostics AND surface every
	// line live so the user can see what the CLI is doing during long
	// reasoning intervals. Without live stderr, the SDK is a black box
	// while it streams thinking tokens or reads files: the runtime
	// emits nothing between "Delegation started" and the final
	// AssistantMessage, which can be many minutes for Opus xhigh/max.
	// The SDK invokes WithStderrCallback from its own drainStderr goroutine,
	// which cmd.Wait() does not synchronise with — so it can still be writing
	// when we read stderrBuf.String() after runSession returns. Guard both
	// sides with a mutex (strings.Builder is not concurrency-safe).
	var (
		stderrMu  sync.Mutex
		stderrBuf strings.Builder
	)
	readStderr := func() string {
		stderrMu.Lock()
		defer stderrMu.Unlock()
		return stderrBuf.String()
	}
	opts = append(opts, claudesdk.WithStderrCallback(func(line string) {
		stderrMu.Lock()
		stderrBuf.WriteString(line)
		stderrBuf.WriteString("\n")
		stderrMu.Unlock()
		if line != "" {
			b.Logger.Info("[%s#%d/claude-code:err] %s", task.NodeID, task.Iteration, line)
		}
	}))

	// Native ask_user interception (see wireAskUserHook). streamCtx is the
	// session context; cancelStream short-circuits the stream when the LLM
	// calls ask_user, and pendingQuestion carries the captured question to
	// the post-session escalation check below. The system prompt's
	// [INTERACTION PROTOCOL] suffix is preserved so the JSON-output fallback
	// still works (and is the only path when sandboxed).
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()
	var pendingQuestion atomic.Value   // pendingAskUser
	var pendingPermission atomic.Value // map[string]any (permission marker)
	opts = b.wireAskUserHook(task, opts, &extraAllowedTools, &pendingQuestion, cancelStream)

	// Tool-permission gate (anti-prompt-injection boundary). Shares the
	// pendingQuestion/cancelStream pause path with ask_user so an Ask
	// decision surfaces to the human exactly like a clarifying question;
	// pendingPermission additionally carries the structured marker so the
	// pause becomes an approval card and the runtime can auto-grant on
	// resume.
	opts = b.wirePermissionHook(task, opts, &pendingQuestion, &pendingPermission, cancelStream)

	// Secret materialisation, rtk command compression, and board MCP wiring —
	// each a self-contained set of opts hooks/servers (see helpers). Board
	// and ask_user extend extraAllowedTools; the single registration below
	// emits one WithAllowedTools call.
	opts = installMaterializeSecretsHook(task, opts)
	opts = installRewriteHook(task, opts)
	opts = b.wireBoardMCP(task, opts, &extraAllowedTools)
	opts = b.wireRunsMCP(task, opts, &extraAllowedTools)
	opts = b.wireUserMCP(task, opts, &extraAllowedTools)

	// Watch capabilities (watch.subscribe / watch.unsubscribe) are wired for
	// the claw backend only so far — the claude_code stdio (__mcp-watch) and
	// sandbox HTTP transports are not built yet (board's own rollout was
	// stdio-then-HTTP across phases; watch is at the claw-only phase). Warn
	// so the gap is visible instead of the bot calling a tool that isn't
	// there mid-loop.
	if HasWatchCapability(task.Capabilities) {
		b.Logger.Warn("[%s#%d/claude-code] watch.* capabilities are not yet supported on the claude_code backend (claw only); ignoring for this node", task.NodeID, task.Iteration)
	}

	opts = append(opts, claudeToolOptions(task, extraAllowedTools)...)

	// Operator-chatbox mid-session inbox delivery (see helper) and
	// Edit-miss resilience (PostToolUse) — breaks the Edit/MultiEdit
	// blind-retry wedge; see installEditMissResilience for the rationale.
	opts = b.installInboxDrainHooks(task, opts)
	opts = b.installEditMissResilience(opts, task)

	startTime := time.Now()
	rm, sessMeta, streamErr := b.runSession(streamCtx, prompt, task, opts)
	duration := time.Since(startTime)
	// What the process left behind is recorded under the session it actually
	// ran (a fork's own new id). What it was told is settled only if it took
	// the prompt that told it — it answered, or the CLI announced the turn
	// (init): a process that died on a hook event before that never read the
	// note, and the next one must still be told.
	tookPrompt := told
	if rm == nil && !sessMeta.sessionIDFromInit {
		tookPrompt = nil
	}
	settleLedger(task.SessionLedger, claudeSessionID(rm, sessMeta), tookPrompt, sessMeta.terminatedBackground)

	// Native ask_user capture takes precedence over any error: if the hook
	// fired, the resulting context cancellation surfaces here as ctx.Err(),
	// which we must not treat as a failure.
	if p, ok := pendingQuestion.Load().(pendingAskUser); ok && p.Question != "" {
		marker, _ := pendingPermission.Load().(map[string]any)
		return b.buildAskUserPendingResult(task, p, marker, rm, sessMeta, currentFingerprint, duration, readStderr()), nil
	}

	if streamErr != nil {
		res, err := b.buildStreamErrorResult(rm, sessMeta, streamErr, readStderr(), duration, task)
		res.SessionFingerprint = currentFingerprint
		return res, err
	}
	// The CLI carried the call to its own ResultMessage: the turn ran to
	// its end. Everything below judges that result's CONTENT — a rendered
	// API error, an error subtype, a recovery pass that could not extract
	// structured output — and typing the content a failure does not unmake
	// the turn, or the session an operator may want to fork from.
	cliTurnCompleted = true

	result = Result{
		Duration:           duration,
		ExitCode:           0,
		Stderr:             readStderr(),
		BackendName:        BackendClaudeCode,
		SessionFingerprint: currentFingerprint,
	}
	// The id is the helper's to decide, here as on the pause and failure
	// paths: rm's when there is one — which on this path there always is
	// — and the streamed one otherwise. Naming rm.SessionID here too
	// would spell that precedence a second time.
	applyClaudeCodeSessionMeta(&result, rm, sessMeta)

	var totalIn, totalOut int
	if rm.Usage != nil {
		totalIn += rm.Usage.InputTokens
		totalOut += rm.Usage.OutputTokens
	}
	result.Tokens = totalIn + totalOut

	if rm.IsError && rm.Subtype != claudesdk.ResultSuccess {
		if errResult, errOut, fatal := b.handleCLIErrorSubtype(rm, task, result, totalIn, totalOut); fatal {
			return errResult, errOut
		}
	}

	// Every guard on the result TEXT lives in renderedFailure, shared with
	// the formatting passes: a render is never an answer, on any pass. The
	// session was billed all the same: its cost goes out with the verdict.
	if err := b.renderedFailure(ctx, rm, task, "pass 1", spawn); err != nil {
		typed := typedFailure(&result, task, totalIn, totalOut, err, rm)
		return result, typed
	}

	if needsTwoPass && rm.SessionID != "" {
		if handled, twoPassResult, twoPassErr := b.runTwoPassFormatting(ctx, task, rm, result, &totalIn, &totalOut); handled {
			return twoPassResult, twoPassErr
		}
	}

	// Single-pass path: parse Pass 1 directly.
	output, rawLen, fallback := parseSDKOutput(rm.Result, rm.StructuredOutput, task.OutputSchema)
	result.Output = output
	result.RawOutputLen = rawLen
	result.ParseFallback = fallback

	// Safety net: schema declared but Pass 1 gave empty/fallback output —
	// try one recovery formatting pass via session resume (see helper).
	var recoveryRM *claudesdk.ResultMessage
	if (len(output) == 0 || fallback) && len(task.OutputSchema) > 0 && rm.SessionID != "" {
		var rerr error
		if recoveryRM, rerr = b.runRecoveryFormatterPass(ctx, task, rm.SessionID, &result, &totalIn, &totalOut); rerr != nil {
			typed := typedFailure(&result, task, totalIn, totalOut, rerr, rm, recoveryRM)
			return result, typed
		}
	}

	annotateCost(&result, task, totalIn, totalOut, rm, recoveryRM)
	return result, nil
}

// renderedFailure re-types a result whose TEXT is the CLI's render of an
// upstream failure — a quota window, a rejected credential, an unavailable
// model, a transient API error — into the typed error the executor knows how
// to route. One predicate for every result message the delegation reads:
// pass 1, each formatting pass, the recovery pass. A render that reaches
// parseSDKOutput becomes the node's answer, and the graph continues on it
// (measured: a campaign node "rendered" an upstream 500 and the next node
// spent 283 minutes on it). Order is the most specific verdict first: a
// window notice carries evidence the generic retry would lose, and a dead
// credential must not be retried at all.
//
// spawn is the forfait token the spawn that produced rm was handed. A CLI
// keeps that token for its whole life, and the store's refresh worker
// revokes it when it rotates the record; the runner then writes the
// rotation into the forfait file. An auth render on a token the file no
// longer carries is that rotation, not a dead credential (see the auth
// guard).
func (b *ClaudeCodeBackend) renderedFailure(ctx context.Context, rm *claudesdk.ResultMessage, task Task, pass string, spawn forfaitSpawn) error {
	if rm == nil || rm.Result == nil {
		return nil
	}
	// An object the TEXT itself is (direct or fenced) is the answer, whatever
	// words it contains: a short JSON answer about quotas must not read as a
	// quota notice — structuredObject is the one definition, the one
	// parseSDKOutput ships. Two vetoes: the CLI's render form ("API Error:
	// …") is never an answer, whatever it carries after the prefix; an
	// error envelope ({"error": …}, {"type":"error", …}) is not one either.
	// An object the SDK carried BESIDE a text earns no exemption: the text
	// goes through the guards below like any other — a resumed pass could
	// echo a prior turn's object next to a refusal, and a window verdict
	// shipped as an answer is the class this predicate closes; beside plain
	// prose no guard matches and the answer ships anyway. No evidence is
	// filed from a shipped answer: a false bench costs more than a reading
	// the next pass files.
	// Read from the text alone: with the SDK object passed too, a populated
	// structured_output — the normal shape on a schema pass — answers first
	// and hides that the text is that very object.
	if obj, _, found, fromText := structuredObject(rm.Result, nil); found && fromText && len(obj) > 0 &&
		!errorBodyObject(obj) && !hasRenderPrefix(*rm.Result) {
		return nil
	}
	// Quota / usage-window guard on the RESULT. The forfait's weekly / session /
	// 5h caps can come back as the result text (subtype=success, IsError=true)
	// with no assistant text block for the stream classifier to catch — re-check
	// here so the notice becomes a typed, resumable rate-limit error instead of
	// flowing into structured-output validation as a misleading "missing
	// required field". Usage-window → resumable after reset; a plain throttle →
	// the executor's transient retry.
	if rm.Result != nil && isRateLimitMessage(*rm.Result) {
		detail := strings.TrimSpace(*rm.Result)
		kind, window, resetAt := classifyRateLimit(detail, time.Now())
		b.Logger.Warn("[%s#%d/claude-code %s] provider quota/rate-limit result (%s) — failing: %.120s",
			task.NodeID, task.Iteration, pass, kind, detail)
		// Same evidence duty as the stream path: a text-relayed refusal
		// that names a meter window must reach the store, or the
		// credential-tier skip stays blind to it.
		if window != "" && task.Hooks.OnUsageWindow != nil {
			_ = task.Hooks.OnUsageWindow(usagecap.Reading{
				Window:     window,
				Status:     usagecap.StatusRejected,
				ObservedAt: time.Now().UTC(),
				ResetsAt:   resetAt,
			})
		}
		return &ErrRateLimited{Provider: BackendClaudeCode, Detail: detail, Kind: kind, ResetAt: resetAt}
	}

	// Auth-failure guard. A dead/expired forfait token (or a rejected API key)
	// does NOT fail the stream (subtype=success, IsError=true): the claude CLI
	// renders the auth error AS the result text (e.g. "Failed to authenticate.
	// API Error: 401 Invalid bearer token"). Left untouched it flows into the
	// formatting passes and finally surfaces as an opaque "missing required
	// field" schema error — the exact masking that turns a dead credential into
	// a wild goose chase through the structured-output machinery. Fail fast with
	// a legible auth error. Non-transient (a retry can't revive a dead token).
	//
	// A render on a forfait token the store rotated under the running CLI is
	// typed transient instead, and files no evidence, which would bench a
	// healthy forfait: the executor retries on a spawn that reads the new
	// token, resuming the session the dead attempt opened. The provider
	// refuses the rotated token at once while the runner writes it within
	// its follow interval, so the file is watched for a bounded while before
	// the credential is called dead.
	if rm.Result != nil && isAuthErrorResult(*rm.Result) && spawn.renewedWithin(ctx, b.forfaitRenewalWait()) {
		detail := redactAuthRender(strings.TrimSpace(*rm.Result))
		b.Logger.Warn("[%s#%d/claude-code %s] the forfait token was renewed under the running CLI — retrying on the new token, no auth evidence filed: %.160s",
			task.NodeID, task.Iteration, pass, detail)
		return &ErrTransient{Provider: BackendClaudeCode, Reason: "forfait token renewed under the running CLI", Detail: detail}
	}
	if authErr := authFailureFast(rm.Result, task); authErr != nil {
		b.Logger.Error("[%s#%d/claude-code %s] authentication failed — failing fast: %.160s",
			task.NodeID, task.Iteration, pass, redactAuthRender(strings.TrimSpace(*rm.Result)))
		return authErr
	}

	// Model-unavailable guard. An invalid/unauthorized `--model` does NOT fail
	// the stream (subtype=success, IsError=false): the claude CLI renders its
	// model-error sentence AS the result text (e.g. "There's an issue with the
	// selected model (openai/gpt-5.5). It may not exist or you may not have
	// access to it."). Left untouched that prose flows into the formatting
	// passes and finally surfaces as an opaque "missing required field" schema
	// error, masking the real cause. Fail fast with a legible error naming the
	// offending model. Non-transient (unlike the API-error guard above) — a
	// retry can't fix a bad/unauthorized model; the usual cause is a
	// claude_code node pinned to a non-Anthropic model (e.g. the shared
	// ITERION_SEC_AUDIT_BACKEND/MODEL override dragging detect_tech onto
	// openai/gpt-5.5).
	if rm.Result != nil && isModelUnavailableResult(*rm.Result) {
		detail := strings.TrimSpace(*rm.Result)
		b.Logger.Error("[%s#%d/claude-code %s] model %q unavailable to the CLI — failing fast: %.160s",
			task.NodeID, task.Iteration, pass, task.Model, detail)
		return fmt.Errorf("claude-code: model %q is unavailable or unauthorized (check the node's backend/model — a claude_code node cannot run a non-Anthropic model): %s", task.Model, detail)
	}

	// Overload/5xx guard. The claude CLI sometimes completes the stream
	// "successfully" (subtype=success, IsError=false) but renders an
	// unrecoverable upstream API failure AS the result text — e.g.
	// "API Error: 529 Overloaded". Left untouched, that string becomes the
	// node's output AND poisons any downstream session that inherits this
	// one (observed in a test-coverage dogfood: a 529 on the `plan` node
	// flowed a non-plan into `act`). Re-type it as ErrTransient so the
	// executor's retry loop rides the outage out — exactly as it does for a
	// connectivity drop surfaced on stderr (retypeNetworkError). Only
	// transient classes (429/5xx/overload/connectivity) retry; a 4xx
	// client/auth error falls through as the visible node output.
	if rm.Result != nil && isTransientAPIErrorResult(*rm.Result) {
		detail := strings.TrimSpace(*rm.Result)
		b.Logger.Warn("[%s#%d/claude-code %s] upstream API-error result text detected — flagging for retry: %.120s",
			task.NodeID, task.Iteration, pass, detail)
		return &ErrTransient{Provider: BackendClaudeCode, Reason: "api_error_result", Detail: detail}
	}

	return nil
}

// hasRenderPrefix reports the CLI's own render form of an upstream failure:
// "API Error: …", whatever follows — a relayed body, fenced or not, is not an
// answer.
func hasRenderPrefix(text string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(text)), "api error")
}

// errorBodyObject reports an object that is an error envelope — a bare
// {"error": …} body, or the provider's own {"type":"error","error":{…}}
// with whatever else it carries (a request id) — relayed verbatim as the
// result text: not an answer even though it parses as one. A legitimate
// answer that carries an `error` field beside real data, without the
// provider's type marker, stays an answer.
func errorBodyObject(obj map[string]any) bool {
	if _, ok := obj["error"]; !ok {
		return false
	}
	return len(obj) == 1 || obj["type"] == "error"
}

// turnFinished reports whether OnTurnFinished has a finished turn to
// announce.
//
// It used to be spelled inline as "the result carries a session id", which
// was a PROXY for "the CLI got far enough to have a turn": true only while
// a failure could not carry one. A stream that DIES now names the session
// it opened — that is the point of capturing it — so the proxy no longer
// holds and the condition has to say what it meant.
//
// What it meant is not "the delegation succeeded". The hook's one consumer
// writes the store.TurnCheckpoint that anchors a FORK (`claude --resume
// <id> --fork-session`), and forking the session of a node that ended on a
// rendered API error is exactly the recovery an operator reaches for — it
// ran a whole session before the failure. So a turn the CLI carried to its
// own ResultMessage is announced whatever verdict iterion then puts on its
// content; only a delegation that died before producing one has no turn to
// announce.
func turnFinished(err error, cliTurnCompleted bool, result Result) bool {
	return result.SessionID != "" && (err == nil || cliTurnCompleted)
}

// typedFailure returns err with the delegation's spend stamped on the result
// first: a typed failure still spent Pass 1 and whatever passes ran, and the
// caps, the fallback chain's carried spend and a donor's ledger read the
// cost from the output map — an unallocated map records nothing. Callers
// hoist the call into its own statement before returning `result`: Go leaves
// the order between a plain operand and a call in one return list
// unspecified, and the stamp must land before the copy is taken.
func typedFailure(result *Result, task Task, totalIn, totalOut int, err error, rms ...*claudesdk.ResultMessage) error {
	if result.Output == nil {
		result.Output = map[string]any{}
	}
	annotateCost(result, task, totalIn, totalOut, rms...)
	return err
}

// renderRetryable reports whether a rendered failure is one a repeat of the
// same pass can recover from: the transient class (5xx, overload,
// connectivity) and a bare throttle. The throttle is safe by construction —
// classifyRateLimit returns RateLimitKindTransient only together with an
// empty window, and the usagecap write is gated on a named window, so a
// repeat cannot re-file evidence. A credential, model or usage-window verdict
// is terminal for this delegation — retrying it re-spends the pass against a
// provider that just refused and re-files the same usage evidence.
func renderRetryable(err error) bool {
	var tr *ErrTransient
	if errors.As(err, &tr) {
		return true
	}
	var rl *ErrRateLimited
	return errors.As(err, &rl) && rl.Kind == RateLimitKindTransient
}

// annotateCost stamps `_tokens` / `_model` / `_cost_usd` on the delegation
// output. The pricing model is the workflow-declared task.Model when set,
// else the CLI-resolved effective model captured from system/init — a node
// that omits `model:` (backend auto-detection) otherwise annotates with an
// empty model id, prices to zero, and the whole run reports tokens but no
// cost (the studio report then claims "no LLM cost recorded" forever). A
// cost the CLI itself computed (ResultMessage.TotalCostUSD, metered API
// runs) wins over the static estimate; sessions that report none (OAuth
// forfait) fall back to the token estimate. Across multiple result
// messages (Pass 1 + a formatting pass) the MAX is used, never the sum:
// per the CLI's session-cumulative accounting the later message subsumes
// the earlier, and max degrades to a small under-count rather than a
// double-count if that accounting is ever per-invocation.
func annotateCost(result *Result, task Task, totalIn, totalOut int, rms ...*claudesdk.ResultMessage) {
	model := task.Model
	if model == "" {
		model = result.EffectiveModel
	}
	var cliCost float64
	for _, rm := range rms {
		if rm != nil && rm.TotalCostUSD != nil && *rm.TotalCostUSD > cliCost {
			cliCost = *rm.TotalCostUSD
		}
	}
	// The same session-cumulative property the MAX above rests on, carried
	// out of the backend: a caller folding two CALLS of one session must
	// MAX this figure too, and must not do that to the token estimate the
	// forfait falls back to. AnnotateWithUSD degrades to Annotate when
	// cliCost is zero, so the flag tracks the value it describes.
	result.CostIsSessionTotal = cliCost > 0
	cost.AnnotateWithUSD(result.Output, model, totalIn, totalOut, cliCost)
}

// buildAskUserPendingResult packages the Result returned when the native
// ask_user MCP hook fired mid-session: it short-circuits the stream and
// surfaces the captured question to the runtime via the
// `_needs_interaction` / `_interaction_questions` envelope so the engine
// can pause the run and elicit the operator.
//
// `rm` is nil here as the RULE, not as an edge case: in Execute the
// pendingQuestion branch returns ahead of the `streamErr != nil` test
// precisely because the hook firing is what cancels the stream, so no
// ResultMessage ever arrives. The session id therefore comes from the
// STREAM — without it this path published an anonymous session, and it is
// the one path that persists a session across a pause (ADR-089:
// ErrNeedsInteraction.SessionID → the checkpoint's BackendSessionID, and
// packLiveSession, which is gated on a non-empty id and so never ran).
func (b *ClaudeCodeBackend) buildAskUserPendingResult(task Task, p pendingAskUser, marker map[string]any, rm *claudesdk.ResultMessage, sessMeta sessionMeta, currentFingerprint string, duration time.Duration, stderr string) Result {
	if marker != nil {
		b.Logger.Info("[%s#%d/claude-code] 🔐 tool-permission approval escalated to the runtime", task.NodeID, task.Iteration)
	} else {
		b.Logger.Info("[%s#%d/claude-code] 🛑 ask_user escalated via native MCP tool", task.NodeID, task.Iteration)
	}
	questions := map[string]any{AskUserQuestionKey: p.Question}
	AddAskUserOptionKeys(questions, p.Options, p.AllowFreeText)
	if marker != nil {
		questions[permission.InteractionMarkerKey] = marker
	}
	if len(p.AwaitPending) > 0 {
		questions[AwaitPendingInteractionsKey] = AwaitPendingToQuestions(p.AwaitPending)
	}
	askResult := Result{
		Output: map[string]any{
			"_needs_interaction":     true,
			"_interaction_questions": questions,
		},
		Duration:    duration,
		ExitCode:    0,
		Stderr:      stderr,
		BackendName: BackendClaudeCode,
		// SessionFingerprint is NOT redundant with the line below the way
		// a hand-set SessionID would be: nothing else supplies it, and the
		// checkpoint needs it to be allowed to reuse the session this
		// pause records (shouldDropSessionFork drops a fork of unknown
		// provenance).
		SessionFingerprint: currentFingerprint,
	}
	// One rule for the id, not two: rm's when there is one, the streamed
	// one otherwise. Setting it from rm here as well would spell the same
	// precedence a second time, in a function whose whole premise is that
	// rm is nil.
	applyClaudeCodeSessionMeta(&askResult, rm, sessMeta)
	return askResult
}

// buildStreamErrorResult packages the Result + wrapped error returned when
// the claude session streaming failed. Network-shaped errors are re-typed
// to ErrTransient so the executor's retry loop rides connectivity blips
// out instead of failing the whole node. Extracted from Execute; behavior
// (ExitCode=-1, Stderr, BackendName, session-meta application, "delegate:
// claude-code failed: %w" wrapping) matches the original inline path.
func (b *ClaudeCodeBackend) buildStreamErrorResult(rm *claudesdk.ResultMessage, sessMeta sessionMeta, streamErr error, stderr string, duration time.Duration, task Task) (Result, error) {
	errResult := Result{
		Duration:    duration,
		ExitCode:    -1,
		Stderr:      stderr,
		BackendName: BackendClaudeCode,
	}
	applyClaudeCodeSessionMeta(&errResult, rm, sessMeta)
	// A connectivity drop during the API call surfaces as an opaque
	// "session ended without result" — the CLI exits non-zero and the
	// only network evidence (fetch failed / ECONNRESET / overloaded …)
	// lands on stderr. Re-type it as ErrTransient so the executor's
	// retry loop rides the blip out instead of failing the whole node.
	streamErr = b.retypeNetworkError(streamErr, stderr, task)
	// The session was billed for whatever it streamed before the drop, and
	// this return is TERMINAL for the delegation. The caps, the fallback
	// chain's carried spend and a donor's ledger all read the cost from the
	// output map, so a return that skips the stamp records nothing — the
	// spend is real either way, only the accounting disappears. Same choke
	// point as every other typed failure.
	in, out := usageOf(rm)
	errResult.Tokens = in + out
	return errResult, typedFailure(&errResult, task, in, out,
		fmt.Errorf("delegate: claude-code failed: %w", streamErr), rm)
}

// usageOf reads a result message's token usage, tolerating the nils a
// broken stream leaves behind: the message may never have arrived, or have
// arrived without usage, and neither is a reason to bill zero silently
// when the other half is there.
func usageOf(rm *claudesdk.ResultMessage) (int, int) {
	if rm == nil || rm.Usage == nil {
		return 0, 0
	}
	return rm.Usage.InputTokens, rm.Usage.OutputTokens
}

// handleCLIErrorSubtype branches on the CLI's error subtype after a stream
// completed with rm.IsError=true. error_max_turns is a SOFT stop (the agent
// hit its tool_max_steps cap) — for an implementer (act/fix, no output
// schema) the work it did is already in the worktree, so we return the
// partial result and let the workflow continue; for a node with structured
// output (a judge), the partial result lacks the required fields and
// upstream schema validation fails it, which is the correct outcome. Other
// error subtypes (error_during_execution, error_max_budget_usd) remain
// hard failures. Returns `fatal=true` when the caller must return the
// (result, err) pair immediately; `fatal=false` lets Execute fall through.
func (b *ClaudeCodeBackend) handleCLIErrorSubtype(rm *claudesdk.ResultMessage, task Task, result Result, totalIn, totalOut int) (Result, error, bool) {
	// error_max_turns is a SOFT stop, not a failure: the agent hit its
	// tool_max_steps cap (claude --max-turns). For an implementer
	// (act/fix, no output schema) the work it did is already in the
	// worktree, so return the partial result and let the workflow
	// continue — the review/fix loop completes any gaps. For a node
	// with structured output (a judge), the partial result lacks the
	// required fields and upstream schema validation fails it, which is
	// the correct outcome. Other error subtypes (error_during_execution,
	// error_max_budget_usd) remain hard failures.
	if rm.Subtype == claudesdk.ResultErrorMaxTurns {
		b.Logger.Warn("[%s#%d/claude-code] hit max turns (tool_max_steps) — returning partial result; downstream review/fix completes any gaps", task.NodeID, task.Iteration)
		return result, nil, false
	}
	// The stamp lives HERE, with the decision, not at the call site: a
	// session that reached a hard subtype was billed exactly like one that
	// rendered a refusal — which stamps — and a caller is free to forget.
	typed := typedFailure(&result, task, totalIn, totalOut,
		fmt.Errorf("delegate: claude-code error: subtype=%s", rm.Subtype), rm)
	return result, typed, true
}

// runTwoPassFormatting runs the Pass-2 structured-output extraction loop
// when tools + schema are both present and Pass 1 produced a SessionID.
// Returns `handled=true` when an explicit return path was hit (and the
// caller must propagate result,err); `handled=false` lets Execute fall
// through to the single-pass parsing path. totalIn / totalOut are
// updated in place across formatting attempts so cost annotation at the
// caller sees the cumulative usage.
//
// Two-pass execution: when tools + schema are both present, Pass 1 now
// carries --json-schema (set above), so a well-behaved agent finishes its
// tool work and calls the native StructuredOutput tool, populating
// rm.StructuredOutput. The formatting pass below is therefore a FALLBACK,
// not the default: it runs only when Pass 1 returned no usable structured
// output. Both passes route through the sandbox command builder when
// sandboxed, so the resumed session is found inside the container where
// Pass 1 created it.
func (b *ClaudeCodeBackend) runTwoPassFormatting(ctx context.Context, task Task, rm *claudesdk.ResultMessage, result Result, totalIn, totalOut *int) (bool, Result, error) {
	// Fast path: Pass 1 already produced valid structured output. The
	// empty-map guard in parseSDKOutput rejects the `structured_output: {}`
	// a tool session emits when the agent never called StructuredOutput
	// (e.g. --max-turns), so a non-empty, non-fallback result here means
	// the schema was genuinely satisfied in one pass — skip Pass 2.
	if output, rawLen, fallback := parseSDKOutput(rm.Result, rm.StructuredOutput, task.OutputSchema); len(output) > 0 && !fallback {
		result.Output = output
		result.RawOutputLen = rawLen
		result.ParseFallback = false
		annotateCost(&result, task, *totalIn, *totalOut, rm)
		return true, result, nil
	}
	const maxFmtAttempts = 2
	var lastFmtErr error
	// Every attempt that produced a message: each was billed, and the
	// pricing takes the highest CLI figure among them — an earlier attempt
	// must count when a later one could not spawn, or answered.
	var ranRMs []*claudesdk.ResultMessage
	for attempt := 1; attempt <= maxFmtAttempts; attempt++ {
		b.Logger.Debug("claude-code [formatting pass %d/%d] starting structured output extraction (session=%s)", attempt, maxFmtAttempts, rm.SessionID)
		fmtRM, fmtSpawn, fmtLost, fmtErr := b.formatPass(ctx, task, rm.SessionID)
		result.TerminatedBackgroundTasks = appendLabels(result.TerminatedBackgroundTasks, fmtLost...)
		if fmtErr == nil {
			ranRMs = append(ranRMs, fmtRM)
			// The pass ran and was billed, whatever its result says: its
			// usage counts on every path out of here, the typed ones too.
			if fmtRM.Usage != nil {
				*totalIn += fmtRM.Usage.InputTokens
				*totalOut += fmtRM.Usage.OutputTokens
				result.Tokens = *totalIn + *totalOut
			}
			result.FormattingPassUsed = true
			// The formatter's result is read through the same predicate as
			// pass 1: a render here would otherwise be parsed as the output.
			if rerr := b.renderedFailure(ctx, fmtRM, task, fmt.Sprintf("formatting pass %d/%d", attempt, maxFmtAttempts), fmtSpawn); rerr != nil {
				if !renderRetryable(rerr) {
					// A credential, model or window verdict is terminal: a
					// second attempt re-spends the pass against a provider
					// that just refused and re-files the same evidence.
					typed := typedFailure(&result, task, *totalIn, *totalOut,
						fmt.Errorf("delegate: claude-code formatting pass failed: %w", rerr), append([]*claudesdk.ResultMessage{rm}, ranRMs...)...)
					return true, result, typed
				}
				fmtErr = rerr
			}
		}
		if fmtErr != nil {
			lastFmtErr = fmtErr
			if attempt < maxFmtAttempts {
				b.Logger.Warn("claude-code [formatting pass %d/%d] failed, retrying: %v", attempt, maxFmtAttempts, fmtErr)
				// A throttle or an overload answered at once is the same
				// answer again: a short pause before the repeat, bounded by
				// the run's context.
				if d := b.retryDelay(); d > 0 {
					select {
					case <-ctx.Done():
						// Cancellation wins over the typed cause: a run being
						// cancelled must not read as rate-limited downstream.
						typed := typedFailure(&result, task, *totalIn, *totalOut, ctx.Err(), append([]*claudesdk.ResultMessage{rm}, ranRMs...)...)
						return true, result, typed
					case <-time.After(d):
					}
				}
				continue
			}
			// Both attempts exhausted. Pass 1's own output was already tried
			// by the fast path above with the same arguments, so there is
			// nothing left to recover from it: the delegation fails typed —
			// priced from Pass 1 and from the last attempt that produced a
			// message, whether or not the final one did (annotateCost takes
			// the highest CLI figure and skips a nil message).
			typed := typedFailure(&result, task, *totalIn, *totalOut,
				fmt.Errorf("delegate: claude-code formatting pass failed: %w", fmtErr), append([]*claudesdk.ResultMessage{rm}, ranRMs...)...)
			return true, result, typed
		}

		output, rawLen, fallback := parseSDKOutput(fmtRM.Result, fmtRM.StructuredOutput, task.OutputSchema)
		if fallback && attempt < maxFmtAttempts {
			b.Logger.Warn("claude-code [formatting pass %d/%d] produced fallback text, retrying", attempt, maxFmtAttempts)
			continue
		}
		result.Output = output
		result.RawOutputLen = rawLen
		result.ParseFallback = fallback
		// Priced from every attempt that ran, the one that answered included.
		annotateCost(&result, task, *totalIn, *totalOut, append([]*claudesdk.ResultMessage{rm}, ranRMs...)...)
		return true, result, nil
	}
	// Defensive: loop fell through without returning. Shouldn't happen
	// (every iteration either returns or continues), but if it did,
	// surface the last formatting error rather than a generic one.
	if lastFmtErr != nil {
		return true, result, fmt.Errorf("delegate: claude-code formatting pass failed: %w", lastFmtErr)
	}
	return false, result, nil
}

// resumedSessionID is the transcript this call continues — resumed or forked —
// or "" when it runs a fresh one (no session, or a fork the fingerprint guard
// drops).
func resumedSessionID(task Task, currentFingerprint string) string {
	if task.SessionID == "" {
		return ""
	}
	if drop, _ := shouldDropSessionFork(task, currentFingerprint); drop {
		return ""
	}
	return task.SessionID
}

// ledgerTerminated is what a process continuing sessionID must be told (see
// SessionLedger); nothing without a ledger or a session.
func ledgerTerminated(l SessionLedger, sessionID string) []string {
	if l == nil || sessionID == "" {
		return nil
	}
	return l.Terminated(sessionID)
}

// settleLedger records the end of a process that ran sessionID. A process
// that never reported a session never took its prompt: nothing is settled.
func settleLedger(l SessionLedger, sessionID string, told, terminated []string) {
	if l == nil || sessionID == "" {
		return
	}
	l.Settle(sessionID, told, terminated)
}

// setupCredsAndSession injects Anthropic-flavoured credentials into the CLI
// subprocess (single helper so Pass 1 and Pass 2 stay symmetric) and, when
// the task carries a SessionID, decides whether to resume/fork that session
// or drop it on a provider-fingerprint mismatch. Returns the extended opts,
// the current provider fingerprint and the forfait token the spawn is
// handed.
//
// A node pinned to a facade provider with no key reachable is REFUSED here,
// by name. The env it would otherwise spawn with has every Anthropic-flavoured
// channel suppressed on purpose, so the CLI can only die on "Not logged in" —
// a message naming neither the provider the operator pinned nor the credential
// that was missing, and only after paying for the spawn.
func (b *ClaudeCodeBackend) setupCredsAndSession(ctx context.Context, task Task, opts []claudesdk.Option) ([]claudesdk.Option, string, forfaitSpawn, error) {
	credEnv, selected := anthropicCredRouteForTask(ctx, task)
	if err := facadeHintRefusal(task.ProviderHint, credEnv); err != nil {
		return opts, "", forfaitSpawn{}, err
	}
	opts = append(opts, credEnvToOpts(credEnv)...)
	currentFingerprint := providerFingerprint(anthropicFingerprintEnvForTask(task, credEnv, selected))

	if task.SessionID != "" {
		drop, reason := shouldDropSessionFork(task, currentFingerprint)
		if drop {
			b.Logger.Warn("[%s#%d/claude-code] dropping session fork: %s",
				task.NodeID, task.Iteration, reason)
		} else {
			opts = append(opts, claudesdk.WithResume(task.SessionID))
			if task.ForkSession {
				opts = append(opts, claudesdk.WithForkSession(true))
			}
		}
	}
	return opts, currentFingerprint, forfaitSpawnOf(ctx, credEnv), nil
}

// runRecoveryFormatterPass is the single-pass safety net: when a schema is
// declared but Pass 1 produced empty/nil output or only a fallback text
// wrapper, resume the session for one formatting pass to extract structured
// output. Catches agents that did real work (tools, code changes) but whose
// structured output the SDK didn't capture (e.g. backends where tools are
// implicit). Mutates result and the running token totals in place. A pass
// that fails to run is logged and left non-fatal (the caller keeps Pass 1's
// output, which the schema then judges); a pass that RENDERS an upstream
// failure is returned typed — the executor routes it, instead of shipping
// Pass 1's fallback text to an opaque schema failure. Returns the pass's own
// ResultMessage whenever it ran (with the verdict too — a billed pass is a
// billed pass), nil when it did not, so the caller's cost annotation sees
// its CLI-reported cost, not just Pass 1's.
func (b *ClaudeCodeBackend) runRecoveryFormatterPass(ctx context.Context, task Task, sessionID string, result *Result, totalIn, totalOut *int) (*claudesdk.ResultMessage, error) {
	b.Logger.Debug("claude-code: empty output with schema — attempting recovery formatting pass (session=%s)", sessionID)
	fmtRM, fmtSpawn, fmtLost, fmtErr := b.formatPass(ctx, task, sessionID)
	result.TerminatedBackgroundTasks = appendLabels(result.TerminatedBackgroundTasks, fmtLost...)
	if fmtErr != nil {
		b.Logger.Warn("claude-code: recovery formatting pass failed: %v", fmtErr)
		return nil, nil
	}
	// The pass ran and was billed, whatever its result says.
	if fmtRM.Usage != nil {
		*totalIn += fmtRM.Usage.InputTokens
		*totalOut += fmtRM.Usage.OutputTokens
		result.Tokens = *totalIn + *totalOut
	}
	result.FormattingPassUsed = true
	// A render on the recovery pass is typed and returned, never parsed as
	// the output nor swallowed into an opaque schema failure — with its
	// message, so the caller's cost annotation sees the billed pass.
	if rerr := b.renderedFailure(ctx, fmtRM, task, "recovery formatting pass", fmtSpawn); rerr != nil {
		return fmtRM, rerr
	}
	fmtOutput, fmtRawLen, fmtFallback := parseSDKOutput(fmtRM.Result, fmtRM.StructuredOutput, task.OutputSchema)
	if len(fmtOutput) > 0 {
		result.Output = fmtOutput
		result.RawOutputLen = fmtRawLen
		result.ParseFallback = fmtFallback
	} else {
		b.Logger.Warn("claude-code: recovery formatting pass also produced empty output")
	}
	return fmtRM, nil
}

// hostSpawnEnv returns the process environment with the per-task env entries
// appended (last-wins), matching the SDK's default host spawn
// (claudesdk/process.go: cmd.Env = os.Environ() then append) and Pass 1. The
// host-side CommandBuilder installed by formatOutput to capture the spawned
// cmd would otherwise set cmd.Env to ONLY the per-task entries, stripping
// PATH/HOME and any ambient credential env from the structured-output format
// pass. Appending the per-task entries last preserves their precedence over
// inherited values (os/exec keeps the last occurrence of a duplicate key).
func hostSpawnEnv(extra map[string]string) []string {
	base := os.Environ()
	for k, v := range extra {
		base = append(base, k+"="+v)
	}
	return base
}

// perTaskSpawnOpts are the per-task knobs BOTH claude spawns must carry: the
// main pass and the structured-output formatting pass that resumes the same
// session.
//
// It is one function rather than two copies because the failure mode of two
// copies is silent and asymmetric — a knob wired into the main pass only lets
// the CLI change its behaviour halfway through a node, with nothing in the
// output to show for it.
//
// It also carries what every spawn pins whatever the task (claudeSpawnPins):
// each pinned variable in the process environment here and in the flag
// settings layer through the one `--settings` object. ExtraEnv never carries a
// pinned key (claudeExtraEnvEntries), so no provisioning layer can move one.
func perTaskSpawnOpts(task Task) []claudesdk.Option {
	effort := task.ReasoningEffort
	if effort == "" {
		effort = defaultClaudeCodeEffort
	}
	effort = claudeCodeEffort(effort)
	opts := []claudesdk.Option{claudesdk.WithEnv("CLAUDE_CODE_EFFORT_LEVEL", effort)}
	env, settings := claudeSpawnPins(task)
	opts = append(opts, claudesdk.WithSettingsJSON(settings))
	opts = append(opts, taskExtraEnvOpts(task)...)
	if d := claudeCodeThinkingDisplay(); d != "" {
		opts = append(opts, claudesdk.WithThinkingDisplay(d))
	}
	for _, key := range slices.Sorted(maps.Keys(env)) {
		opts = append(opts, claudesdk.WithEnv(key, env[key]))
	}
	return opts
}

// claudeCodeEffort coerces an iterion effort level to one Claude Code
// accepts. Anthropic's effort dial starts at "low" — no Claude model (Opus
// 5.5 included) carries "none" — so a DSL `reasoning_effort: none` clamps
// to "low", the same floor the claw route applies through
// model.coerceEffort for these models (claw ↔ claude_code parity). The
// coercion is documented, not silent-by-accident: better a degraded run
// than a node refused for a level only OpenAI's GPT-6 Sol/Luna expose.
func claudeCodeEffort(effort string) string {
	if effort == "none" {
		return "low"
	}
	return effort
}

// claudeEnvPins is the environment every claude_code spawn pins, whatever the
// node declares:
//
//   - backgroundTasksOffEnv = "1" (see its doc);
//   - CLAUDE_CODE_DISABLE_AUTO_MEMORY, the node's auto-memory decision
//     (autoMemorySpawn). It is emitted in the off case too, because the CLI's
//     own default is ON: left alone, a bot run reads and writes the
//     operator's personal `~/.claude/projects/<cwd>/memory/`. "0" is not a
//     no-op: it force-ENABLES over a settings file that turned auto-memory
//     off;
//   - BASH_DEFAULT_TIMEOUT_MS / BASH_MAX_TIMEOUT_MS (claudeBashTimeouts);
//   - CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS, the bound of the CLI's own
//     wind-down wait for background work (printBgWaitCeilingEnv);
//   - the background lifecycle's signals (bgLifecycleEnv), while the
//     lifecycle is on: a settings `env` that turned them off would leave it
//     waiting for an idle the CLI never reports;
//   - the rewriter chain's run env (rtk's stores off), when a rewriter is
//     available — under the pins above: it moves none of them.
//
// Every one of them rides the process environment AND the flag settings
// layer (claudeSpawnPins). The CLI does not resolve these variables before
// its settings: at startup it copies the `env` block of every settings source
// it loads into its own environment, in the order user, project, local, flag,
// policy, and reads the variables live afterwards. A target repository's
// committed .claude/settings.json (loaded under --setting-sources project) or
// the operator's user settings would otherwise override the process
// environment: switch background work back on, or turn auto-memory back on
// against the operator's personal memory. The flag layer comes after them;
// only managed policy settings come later.
func claudeEnvPins(task Task) map[string]string {
	pins, _ := claudeEnvPinsAndDropped(task)
	return pins
}

// claudeEnvPinsAndDropped is claudeEnvPins plus the rewriter run_env names it
// dropped: a plugin's explicit choice that a pin outranks is said out loud,
// like an ExtraEnv entry for a pinned key (claudeExtraEnvEntries).
func claudeEnvPinsAndDropped(task Task) (map[string]string, []string) {
	disable, _ := autoMemorySpawn(task)
	defaultMs, maxMs := claudeBashTimeouts()
	background := "1"
	if backgroundTasksOnFromEnv() {
		// Empty: the CLI reads the switch as set only for 1, true, yes or on.
		background = ""
	}
	pins := map[string]string{}
	// The rewriter chain's run env, whatever the compression mode: the agent
	// may run a rewriter itself, or an operator's own hook may. A settings
	// `env` would otherwise replace it (rtk's history back on, a secret in
	// it). It goes in first: a run_env naming a variable pinned below does
	// not move that pin.
	for _, kv := range rewrite.NewChain(task.Rewriters).RunEnv() {
		k, v, _ := strings.Cut(kv, "=")
		pins[k] = v
	}
	var dropped []string
	pin := func(k, v string) {
		if _, fromRunEnv := pins[k]; fromRunEnv {
			dropped = append(dropped, k)
		}
		pins[k] = v
	}
	pin(backgroundTasksOffEnv, background)
	pin(autoMemoryDisableEnv, disable)
	pin(bashDefaultTimeoutEnv, strconv.FormatInt(defaultMs, 10))
	pin(bashMaxTimeoutEnv, strconv.FormatInt(maxMs, 10))
	// The CLI's own wind-down ceiling, pinned at the CLI's own default and at
	// nothing else. Past it the CLI KILLS the background work it still holds,
	// where iterion's wave budget only asks for a report — so this is never
	// derived from that budget: the point is that the repository under review
	// cannot MOVE it, `0` ("wait indefinitely") included, not to shorten a
	// deadline that kills. It arms only where the CLI's stdin is closed,
	// which is the formatting pass: the one spawn with no lifecycle to catch
	// what a kill loses.
	pin(printBgWaitCeilingEnv, strconv.FormatInt(defaultPrintBgWaitCeiling.Milliseconds(), 10))
	cfg := resolveBackgroundLifecycleConfig()
	if cfg.enabled {
		for _, k := range slices.Sorted(maps.Keys(bgLifecycleEnv)) {
			pin(k, bgLifecycleEnv[k])
		}
	}
	slices.Sort(dropped)
	return pins, dropped
}

// claudeSpawnPins returns the pinned environment and the one `--settings`
// object that carries it into the CLI's flag settings layer, merged with the
// auto-memory keys when the node's memory is on. WithSettingsJSON replaces
// rather than merges, so everything the engine puts in that layer is composed
// here, once. `autoMemoryDirectory` is one key the CLI refuses to read from a
// checked-in `.claude/settings.json` at all, so the target repository cannot
// redirect where the memory is written.
func claudeSpawnPins(task Task) (env map[string]string, settings []byte) {
	env = claudeEnvPins(task)
	_, memory := autoMemorySpawn(task)
	excludes := claudeAmbient(task).excludes
	settings, err := claudeFlagSettings(env, memory, excludes)
	if err != nil {
		// Strings and a bool cannot fail to marshal. If they somehow did,
		// enabling auto-memory without pinning the directory would send the
		// agent's notes to the operator's personal memory instead of the
		// run's space: stay off rather than write to the wrong place.
		env[autoMemoryDisableEnv] = "1"
		settings, _ = claudeFlagSettings(env, nil, excludes)
	}
	return env, settings
}

// claudeFlagSettings is the `--settings` object: the pinned environment as an
// `env` block, the memory keys when there are any, and the memory files the
// node's ambient-context policy keeps out (`claudeMdExcludes`).
func claudeFlagSettings(env map[string]string, memory map[string]any, excludes []string) ([]byte, error) {
	settings := map[string]any{"env": env}
	for key, value := range memory {
		settings[key] = value
	}
	if len(excludes) > 0 {
		settings["claudeMdExcludes"] = excludes
	}
	return json.Marshal(settings)
}

// autoMemoryDisableEnv is the CLI's own auto-memory switch.
const autoMemoryDisableEnv = "CLAUDE_CODE_DISABLE_AUTO_MEMORY"

// autoMemorySpawn is the auto-memory decision, split out so the mapping is
// testable without reaching into the SDK's unexported config. It returns the
// value for CLAUDE_CODE_DISABLE_AUTO_MEMORY and, when memory is on, the
// settings keys pinning the directory (nil otherwise).
func autoMemorySpawn(task Task) (disable string, settings map[string]any) {
	if task.AutoMemoryDir == "" {
		return "1", nil
	}
	return "0", map[string]any{
		"autoMemoryEnabled":   true,
		"autoMemoryDirectory": task.AutoMemoryDir,
	}
}

// The Bash timeout variables the CLI reads live from its environment.
const (
	bashDefaultTimeoutEnv = "BASH_DEFAULT_TIMEOUT_MS"
	bashMaxTimeoutEnv     = "BASH_MAX_TIMEOUT_MS"

	// unboundedBashTimeout is both Bash timeouts when no watchdog is enabled.
	// Nothing can abort the session over a silent command then, so nothing
	// kills a command early either: without a timeout of its own, a command
	// runs as long as the longest one allowed.
	unboundedBashTimeout = time.Hour

	// bashWatchdogMargin is the least a Bash command's longest timeout stays
	// under the session's silence watchdog: the CLI still has to kill the
	// command and report it before the watchdog fires.
	bashWatchdogMargin = 30 * time.Second
)

// claudeBashTimeouts returns the default and the maximum Bash timeout, in
// milliseconds, that every spawn pins.
//
// With background work off, a Bash command that outlives its timeout is
// killed, not moved to the background: the CLI's own default (2 min) kills a
// long build the model ran without a timeout, and its maximum (10 min) caps
// what can finish at all. The limit is this backend's own watchdogs. While a
// foreground command runs, the session is silent to them, because the SDK
// drops the CLI's tool_progress lines. A command that outlives the hot idle
// tier or the no-progress tier therefore aborts the whole session.
//
// The maximum sits under the tighter of the two enabled watchdogs, by a
// margin: a tenth of it, at least bashWatchdogMargin, at most half of it. The
// default is half the maximum. With both watchdogs disabled, both are
// unboundedBashTimeout: the CLI's own 2 minutes would kill a long command
// that no watchdog threatens.
func claudeBashTimeouts() (defaultMs, maxMs int64) {
	var bound time.Duration
	for _, watchdog := range []time.Duration{resolveStreamHotTimeout(), resolveNoProgressTimeout()} {
		if watchdog > 0 && (bound == 0 || watchdog < bound) {
			bound = watchdog
		}
	}
	if bound == 0 {
		return unboundedBashTimeout.Milliseconds(), unboundedBashTimeout.Milliseconds()
	}
	margin := min(max(bound/10, bashWatchdogMargin), bound/2)
	maxTimeout := bound - margin
	// The CLI ignores a value that is not a positive integer of milliseconds.
	return max((maxTimeout / 2).Milliseconds(), 1), max(maxTimeout.Milliseconds(), 1)
}

// taskExtraEnvOpts converts Task.ExtraEnv (KEY=value entries — run-level
// provisioning such as the devbox profile PATH) into per-spawn env
// options.
func taskExtraEnvOpts(task Task) []claudesdk.Option {
	entries, _ := claudeExtraEnvEntries(task)
	opts := make([]claudesdk.Option, 0, len(entries))
	for _, kv := range entries {
		opts = append(opts, claudesdk.WithEnv(kv[0], kv[1]))
	}
	return opts
}

// claudeExtraEnvEntries is Task.ExtraEnv as a claude_code spawn applies it:
// key/value pairs in order, without entries that have no '=' (they cannot form
// an assignment) and without the keys every spawn pins (claudeEnvPins), which
// provisioning does not get to move. Those keys come back as `overridden`, in
// order, so the Session spawn can say it ignored them. Every reader of
// ExtraEnv on this backend goes through it: the entries are applied twice per
// spawn (here and in the credential environment, which comes last), and a key
// filtered in one of the two comes back through the other.
func claudeExtraEnvEntries(task Task) (entries [][2]string, overridden []string) {
	pinned := claudeEnvPins(task)
	entries = make([][2]string, 0, len(task.ExtraEnv))
	for _, kv := range task.ExtraEnv {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		if _, isPinned := pinned[k]; isPinned {
			overridden = append(overridden, k)
			continue
		}
		entries = append(entries, [2]string{k, v})
	}
	return entries, overridden
}

// formatOutput performs the second pass of two-pass execution: resumes the
// Pass 1 session with WithOutputFormat to guarantee structured JSON output
// conforming to the schema. The model already has full context from the
// session, so only a short formatting instruction is needed.
//
// It is a full CLI spawn, not a tool-less one: it carries every bound this
// task has that can travel on argv — claudeSpawnBounds always, and
// claudeToolOptions unless the task is GATED, where the native surface is
// withheld instead of the declaration being restated.
// The hook-borne ones cannot travel — Prompt has no hook channel — so a gated
// task has its native surface withheld here instead. The sentence that used
// to sit here — "(no tools)" — is what kept that gap invisible for five
// rounds; what it names is the instruction, not the toolset.
func (b *ClaudeCodeBackend) formatOutput(ctx context.Context, task Task, sessionID string) (*claudesdk.ResultMessage, forfaitSpawn, []string, error) {
	// Use the parent context directly — the runtime already enforces budget
	// timeouts. Adding a short artificial timeout here risks cancelling the
	// formatting pass while the CLI is still loading the resumed session.
	fmtCtx := ctx

	var schema map[string]any
	if err := json.Unmarshal(task.OutputSchema, &schema); err != nil {
		return nil, forfaitSpawn{}, nil, fmt.Errorf("invalid output schema: %w", err)
	}

	opts := []claudesdk.Option{
		claudesdk.WithResume(sessionID),
		claudesdk.WithOutputFormat(schema),
		claudesdk.WithPermissionMode("bypassPermissions"),
		claudesdk.WithVerbose(true),
		claudesdk.WithStderrCallback(func(line string) {
			if line != "" {
				b.Logger.Info("[%s#%d/fmt] %s", task.NodeID, task.Iteration, line)
			}
		}),
	}
	// Every bound this task carries, on THIS spawn too. It is a second CLI
	// process resuming the same session under the same always-on
	// bypassPermissions, so a bound appended to one spawn and not the other
	// is no bound at all: without it a node that declared `tools: []` gets
	// the whole native roster here, and `Workflow` — the one tool the DATA in
	// the resumed transcript can arm — comes back. No MCP extras:
	// this pass passes no --mcp-config and reformats text the first pass
	// already produced.
	// The permission gate cannot travel to THIS spawn: it is a PreToolUse
	// hook, and hooks exist only on the Session path — claudesdk.Prompt has
	// no hook channel and drops them silently (#1672). Execute's spawn
	// carries the hook and therefore keeps everything the node DECLARED —
	// `tools:` bounds what exists, the policy bounds what runs, and joining
	// them would delete a gated node's own tools. Here, with no gate to run,
	// the native surface is withheld instead, and the DECLARATION is not
	// emitted at all: naming a tool on `--allowedTools` and on
	// `--disallowedTools` in one argv would make the withholding rest on the
	// CLI resolving deny over allow — stated in WithDisallowedTools' godoc,
	// never executed here. Disjoint lists hold whatever that precedence is.
	// Nothing is lost: a declaration has no role on a spawn where nothing may
	// run, and its own disallow half is a subset of the whole roster.
	// StructuredOutput, the only tool this pass needs, is not on the roster.
	// Permission.Enabled() is the predicate on purpose, and it is the same
	// expression wirePermissionHook gates on: spawn #1 carries the hook
	// exactly when this arm withholds, so the two cannot disagree about which
	// nodes are gated. A narrower neighbour (CanAsk) would leave a
	// `permission: deny` node here and a mode-only one would leave `ask`.
	if task.Permission.Enabled() {
		opts = append(opts, claudesdk.WithDisallowedTools(gatedFormattingWithheld()...))
	} else {
		opts = append(opts, claudeToolOptions(task, nil)...)
	}
	opts = append(opts, claudeSpawnBounds(task)...)

	// Cwd / CLI path handling mirrors Execute(): on the host, pass workdir
	// through; in the sandbox, leave cwd unset (the docker driver picks the
	// spec's WorkspaceFolder) and pin the CLI to the bare in-container name.
	if task.WorkDir != "" && task.Sandbox == nil {
		opts = append(opts, claudesdk.WithCwd(task.WorkDir))
	}
	if task.Sandbox != nil {
		opts = append(opts, claudesdk.WithCLIPath("claude"))
	}

	model := task.Model
	if model == "" {
		model = defaultClaudeCodeModel
	}
	model = claudeCodeModelID(model)
	opts = append(opts, claudesdk.WithModel(model))
	// CLI binary path: the per-node task override (DSL `command:`, an
	// alternate claude-code-compatible CLI) wins over the backend-level
	// default; the shared backend is
	// left unmutated so it can serve other nodes with their own override.
	cliPath := b.Command
	if task.Command != "" {
		cliPath = task.Command
	}
	if cliPath != "" {
		opts = append(opts, claudesdk.WithCLIPath(cliPath))
	}

	// Capture every spawned subprocess so promptWithTimeout can SIGKILL
	// them if the SDK's read loop gets stuck and ctx cancellation alone
	// fails to wake it. The CommandBuilder we install wraps either the
	// sandbox-routing path or the default exec.CommandContext path; both
	// arms collect the returned cmd into killables.
	var killMu sync.Mutex
	var killables []*exec.Cmd
	captureCmd := func(cmd *exec.Cmd) {
		if cmd == nil {
			return
		}
		killMu.Lock()
		killables = append(killables, cmd)
		killMu.Unlock()
	}
	killAll := func() {
		killMu.Lock()
		defer killMu.Unlock()
		for _, cmd := range killables {
			if cmd == nil || cmd.Process == nil {
				continue
			}
			_ = cmd.Process.Kill()
		}
	}

	// The flag settings object perTaskSpawnOpts hands the spawn below. The
	// builder finds it on argv and moves it, routing pin added, into a file.
	_, flagSettings := claudeSpawnPins(task)
	if task.Sandbox != nil {
		// When sandboxed, route the CLI subprocess through the sandbox driver so
		// it resumes the session inside the container (where the session file
		// lives) rather than spawning a host claude that can't see it.
		run := task.Sandbox
		// The settings file this spawn writes in the container stays until
		// the run's sandbox is torn down: it holds the static pins and values
		// of the container's own environment, nothing a process there cannot
		// already read, and removing it would cost an exec per pass.
		mark := sandboxDelegateMark(task) + "-fmt"
		opts = append(opts, claudesdk.WithCommandBuilder(func(ctx context.Context, path string, args []string, cwd string, env map[string]string, openStdin bool) *exec.Cmd {
			env = claudeModelDefaultEnv(env)
			b.Logger.Info("claude-code [fmt]: exec %v (cwd=%s, env_keys=%d, stdin=%v)",
				redactedArgvPreview(path, args), cwd, len(env), openStdin)
			// The routing pin is computed inside the container, from the
			// environment the CLI runs with (claudeSandboxArgv).
			argv, err := claudeSandboxArgv(mark, path, args, flagSettings)
			if err != nil {
				return claudeFailedCmd(ctx, path, err)
			}
			cmd := run.Command(ctx, argv, sandbox.ExecOpts{
				WorkDir:       cwd,
				Env:           env,
				KeepStdinOpen: openStdin,
			})
			captureCmd(cmd)
			return cmd
		}))
	} else {
		// Host-side fallback: the SDK normally constructs its own
		// exec.CommandContext, so we install a builder to capture the cmd
		// reference and to pin the routing variables at the values its env
		// holds (claudeSettingsFiles.pinHost). exec.CommandContext kills the
		// subprocess when ctx fires; the explicit Kill() in killAll is the
		// belt-and-braces hedge for the case where ctx propagation is
		// what's stuck.
		files := &claudeSettingsFiles{}
		defer files.remove(b.Logger)
		opts = append(opts, claudesdk.WithCommandBuilder(func(ctx context.Context, path string, args []string, cwd string, env map[string]string, openStdin bool) *exec.Cmd {
			env = claudeModelDefaultEnv(env)
			cmd := exec.CommandContext(ctx, path, args...)
			cmd.Dir = cwd
			// Seed os.Environ() before the per-task entries — matching the
			// SDK's default host spawn (claudesdk/process.go) and Pass 1 — so
			// the format pass inherits PATH/HOME and any ambient credential
			// env instead of running with a stripped environment. The prior
			// code set cmd.Env to ONLY the per-task entries, dropping the
			// inherited env (CLAUDE_CODE_EFFORT_LEVEL is always present, so the
			// strip always fired). Per-task entries stay last so they still win.
			cmd.Env = hostSpawnEnv(env)
			files.pinHost(cmd, flagSettings)
			captureCmd(cmd)
			return cmd
		}))
	}

	// Forward BYOK credentials and effort level into the formatting pass so
	// the resumed session uses the same auth path as Pass 1.
	opts = append(opts, perTaskSpawnOpts(task)...)
	// The structured-output pass loads the same scopes as the session: an
	// omitted --setting-sources would load every scope, `local` included.
	opts = append(opts, claudeAmbient(task).settingSourcesOption())
	credEnv := anthropicCredEnvForTask(ctx, task)
	if err := facadeHintRefusal(task.ProviderHint, credEnv); err != nil {
		return nil, forfaitSpawn{}, nil, err
	}
	opts = append(opts, credEnvToOpts(credEnv)...)

	// A GATED node gets one extra sentence — advisory defence in depth beside
	// the withholding, for the names the roster misses. It is scoped to the
	// gated arm because an UNGATED structured-output pass legitimately does
	// tool work: it is the `--max-turns` fallback for a first pass cut off
	// mid-run, and 54 of the catalog's 57 two-pass nodes are ungated. And it
	// exempts StructuredOutput by name: that tool is how the agent RETURNS
	// its result (the permission gate exempts it for the same reason), so a
	// blanket "call no tools" would push every gated schema'd node onto the
	// text-parsing fallback.
	prompt := "Format your complete findings as JSON matching the required output schema."
	if task.Permission.Enabled() {
		prompt += " Do not call any tool other than StructuredOutput; just return the JSON."
	}

	// This pass resumes pass 1's transcript in a process of its own, with the
	// node's tools: it is told what pass 1 lost like any other resume, and
	// what it launches and never sees report back is lost with its own
	// process — recorded like pass 1's.
	tracker := newBackgroundTracker()
	tracker.redact = labelRedactor(task)
	opts = append(opts, claudesdk.WithMessageObserver(tracker.observe))
	told := ledgerTerminated(task.SessionLedger, sessionID)
	rm, err := promptWithTimeout(fmtCtx, formattingPassNote(told, prompt), killAll, opts...)
	lost := bgTaskLabels(tracker.view().lost)
	if len(lost) > 0 {
		reason := "the formatting pass's process ended"
		b.Logger.Warn("[%s#%d/claude-code] ⏳ %s: %d background task(s) that never reported back to the agent are lost with it: %s",
			task.NodeID, task.Iteration, reason, len(lost), strings.Join(lost, "; "))
		if fn := task.Hooks.OnBackgroundWork; fn != nil {
			fn(BackgroundWork{Backend: BackendClaudeCode, Phase: BackgroundAbandoned, Running: len(lost), Tasks: lost, Reason: reason})
		}
	}
	// A process that answered — or launched work — took its prompt.
	if rm != nil || len(lost) > 0 {
		sid := sessionID
		if rm != nil {
			sid = cmp.Or(rm.SessionID, sessionID)
		}
		settleLedger(task.SessionLedger, sid, told, lost)
	}
	return rm, forfaitSpawnOf(ctx, credEnv), lost, err
}

// promptWithTimeout wraps claudesdk.Prompt in a goroutine with
// context-aware cancellation AND a hard subprocess kill on ctx cancel.
//
// The Claude Agent SDK's Prompt() function does not always check
// ctx.Done() in its internal ReadLine() loop — a stuck stream that
// stops emitting bytes will block the goroutine indefinitely, leaking
// the subprocess and pinning the host slot. The killCmd callback,
// when non-nil, is invoked on ctx cancellation to SIGKILL whatever
// subprocesses the SDK spawned via the caller's CommandBuilder. See
// formatOutput for an example of how to wire the cmd capture.
func promptWithTimeout(ctx context.Context, prompt string, killCmd func(), opts ...claudesdk.Option) (*claudesdk.ResultMessage, error) {
	type result struct {
		rm  *claudesdk.ResultMessage
		err error
	}
	ch := make(chan result, 1)
	go func() {
		rm, err := claudesdk.Prompt(ctx, prompt, opts...)
		ch <- result{rm, err}
	}()

	select {
	case res := <-ch:
		return res.rm, res.err
	case <-ctx.Done():
		if killCmd != nil {
			killCmd()
		}
		// Drain in the background so the Prompt goroutine doesn't
		// leak — Prompt() will return now that the subprocess is dead.
		go func() { <-ch }()
		return nil, fmt.Errorf("claude prompt cancelled: %w", ctx.Err())
	}
}
