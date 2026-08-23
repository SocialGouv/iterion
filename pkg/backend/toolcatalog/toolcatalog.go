// Package toolcatalog is the compile-time view of a node's `tools:` list:
// which backends the list actually CONSTRAINS, and which bare tool names the
// run-time registry can resolve there.
//
// It exists because the two facts live in different layers and a copy of
// either one drifts silently. The engine learns a name is unresolvable at the
// moment it dispatches the node — after the run started, after the workspace
// was prepared, and (for a fallback route) at the exact moment the run was
// already under stress. The name is right there in the .bot source, so the
// answer was knowable before a single token was spent.
//
// The package is deliberately a LEAF — stdlib only, literals only. pkg/dsl/ir
// may not depend on the execution stack (see the note on KnownFallbackTriggers
// in pkg/dsl/ir/validate_fallbacks.go), and the compiler is the main consumer.
// The literals are guarded against drift from the other side instead:
// pkg/backend/tool's conformance test builds the real registry and fails if
// the two sets disagree, so a claw bump that adds, renames or drops a tool
// breaks CI here rather than in someone's run.
package toolcatalog

import "strings"

// ClawBackend is the in-process backend's literal name — the one backend
// whose tool list is a real constraint. Hardcoded rather than imported from
// pkg/backend/delegate for the layering reason above.
const ClawBackend = "claw"

// ConstrainsTools reports whether a node's `tools:` list actually restricts
// what the backend can call — the precondition for saying anything about the
// names in it.
//
// Only claw does. It resolves every declared name against the in-process
// registry and hard-fails on one it does not know. Every CLI backend runs its
// own native toolset:
//
//   - claude_code ignores the lowercase list entirely under the always-on
//     `--permission-mode bypassPermissions` (the real hard-restrict flag,
//     `--tools`, is deliberately unused to preserve adaptivity);
//   - codex cannot narrow its built-in shell at all;
//   - pi, kimi and grok are driven through the CLI-agent seam, which never
//     passes the list to the agent.
//
// So a name that is meaningless on those backends is dead config, not a
// failure — flagging it would train authors to ignore the diagnostic.
func ConstrainsTools(backend string) bool {
	return strings.TrimSpace(backend) == ClawBackend
}

// IsBuiltin reports whether name is a bare built-in tool the registry can
// resolve on a constraining backend.
//
// The set is the UNION of everything RegisterClawAll can register, including
// the families behind a runtime flag (web_search, the computer-use pair, plan
// mode, the privacy pair). A compile-time check cannot know whether the host
// enabled them, and refusing a name that the operator's own environment
// supplies would be a false positive — the expensive kind, since it blocks a
// run that works.
func IsBuiltin(name string) bool {
	return clawBuiltins[strings.TrimSpace(name)]
}

// Builtins returns the catalog as a sorted slice — for diagnostics, docs and
// the conformance test. The returned slice is a copy.
func Builtins() []string {
	out := make([]string, 0, len(clawBuiltins))
	for name := range clawBuiltins {
		out = append(out, name)
	}
	sortStrings(out)
	return out
}

// IsStaticBuiltinRef reports whether an entry in a `tools:` list is a bare
// built-in reference this package can decide — i.e. one the registry resolves
// by exact name against its built-in namespace.
//
// Everything else is deliberately outside the catalog's authority and must be
// left alone:
//
//   - a dotted name (`mcp.github.create_issue`) or a wildcard
//     (`mcp.github.*`) names an MCP server's tools, discovered when the
//     server connects — they exist nowhere at compile time;
//   - the claude_code FQN alias form (`mcp__iterion_board__create`) is the
//     same thing spelled for a CLI;
//   - a `${VAR}` reference is resolved from the environment at run time.
func IsStaticBuiltinRef(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	if strings.Contains(name, ".") || strings.Contains(name, "__") || strings.Contains(name, "${") {
		return false
	}
	return true
}

// Suggest returns the built-in that most plausibly replaces an unresolvable
// name, or "" when nothing is close enough to be worth printing.
//
// Two sources, in order. First a curated map of names that read like tools but
// have never been registered: they came from older docs and examples and are
// copied from bot to bot, so they are by far the likeliest thing an author
// actually typed — and an edit-distance search answers them badly
// (`search_codebase` is nowhere near `grep`). Then a plain nearest-name search
// for ordinary typos, capped tight enough that it never invents a suggestion
// for a name that simply does not exist here.
func Suggest(name string) string {
	name = strings.TrimSpace(name)
	if alias, ok := legacyNames[name]; ok {
		return alias
	}
	best, bestDist := "", 0
	// A third of the name's length, so `read_fil` finds `read_file` while a
	// short name cannot match half the catalog. Never more than 2 edits.
	budget := len(name) / 3
	if budget > 2 {
		budget = 2
	}
	if budget < 1 {
		return ""
	}
	for candidate := range clawBuiltins {
		d := editDistance(name, candidate)
		if d > budget {
			continue
		}
		if best == "" || d < bestDist || (d == bestDist && candidate < best) {
			best, bestDist = candidate, d
		}
	}
	return best
}

// legacyNames maps tool names that appear in workflows but have never been
// registered onto the built-in that does the job. Keeping the mapping here
// rather than in the diagnostic keeps the advice in one place for every
// consumer (the compiler, the launch-time fallback screen, and any future
// editor surface).
var legacyNames = map[string]string{
	"apply_patch":     "file_edit",
	"edit_file":       "file_edit",
	"git_diff":        "bash",
	"git_log":         "bash",
	"git_status":      "bash",
	"list_files":      "glob",
	"patch":           "file_edit",
	"run_command":     "bash",
	"search_codebase": "grep",
	"shell":           "bash",
	"tree":            "glob",
	"web_search_tool": "web_search",
}

// clawBuiltins is the catalog: every bare name RegisterClawAll (plus the
// iterion-side registrations that ride with it) can put in the registry.
//
// Grouped by registrar so a diff against pkg/backend/tool/claw_builtins.go is
// readable. The board and watch tools are absent on purpose: they register
// under the MCP namespace (`mcp.iterion_board.*`), which IsStaticBuiltinRef
// hands back to the run time.
var clawBuiltins = map[string]bool{
	// RegisterClawBuiltinsWithEnv — file IO, shell, search, fetch.
	"read_file":  true,
	"write_file": true,
	"glob":       true,
	"grep":       true,
	"file_edit":  true,
	"web_fetch":  true,
	"bash":       true,

	// RegisterClawSimple — process-level utilities.
	"send_user_message": true,
	"remote_trigger":    true,
	"sleep":             true,
	"notebook_edit":     true,
	"repl":              true,
	"structured_output": true,

	// RegisterClawTodo / RegisterClawSkill / RegisterClawToolSearch /
	// RegisterClawSubagents / RegisterClawConfig / RegisterClawLSP.
	"todo_write":  true,
	"skill":       true,
	"tool_search": true,
	"agent":       true,
	"config":      true,
	"lsp":         true,

	// RegisterAskUser + RegisterAsyncAsk (ADR-081).
	"ask_user":       true,
	"ask_user_async": true,
	"await_answers":  true,

	// RegisterClawMCPResources — the MCP *resource* tools, which are
	// ordinary built-ins (they are how an agent reads a server's
	// resources; the server's own tools live under `mcp.<server>.*`).
	"list_mcp_resources": true,
	"read_mcp_resource":  true,
	"mcp_auth":           true,

	// RegisterClawTasks.
	"task_create":     true,
	"task_get":        true,
	"task_list":       true,
	"task_output":     true,
	"task_stop":       true,
	"task_update":     true,
	"run_task_packet": true,

	// RegisterClawWorkers.
	"worker_create":             true,
	"worker_get":                true,
	"worker_observe":            true,
	"worker_observe_completion": true,
	"worker_resolve_trust":      true,
	"worker_await_ready":        true,
	"worker_send_prompt":        true,
	"worker_restart":            true,
	"worker_terminate":          true,

	// RegisterClawTeams / RegisterClawCron.
	"team_create": true,
	"team_get":    true,
	"team_list":   true,
	"team_delete": true,
	"cron_create": true,
	"cron_get":    true,
	"cron_list":   true,
	"cron_delete": true,

	// Conditionally registered — accepted unconditionally, see IsBuiltin.
	"read_image":       true, // always on: no display needed
	"web_search":       true, // ClawDefaults.IncludeWebSearch
	"screenshot":       true, // ClawDefaults.IncludeComputerUse
	"computer_use":     true, // ClawDefaults.IncludeComputerUse
	"enter_plan_mode":  true, // ClawDefaults.PlanMode
	"exit_plan_mode":   true, // ClawDefaults.PlanMode
	"privacy_filter":   true, // ClawDefaults.Privacy
	"privacy_unfilter": true, // ClawDefaults.Privacy
}

// editDistance is the ordinary Levenshtein distance, two rows deep. Tool names
// are short and the catalog is small, so the simple form is the right one.
func editDistance(a, b string) int {
	if a == b {
		return 0
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min3(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

func min3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}

// sortStrings is an insertion sort over the catalog — kept local so the
// package stays import-free (see the package comment).
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
