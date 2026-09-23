package delegate

import (
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate/claudesdk"
)

// The live half of #1615: a node that declares `tools: []` must lose Claude
// Code's whole native roster, not keep it. Asserted through the two functions
// the option-building site composes — the boundary predicate and the disallow
// list — so a regression in either reddens.
//
// Reddens on the mutation that reads the declaration as `len(allowed) > 0`
// (the forbidden alternative: an empty declaration read as no declaration).
func TestADeclaredEmptyToolListRemovesEveryNativeToolOnClaudeCode(t *testing.T) {
	declaredEmpty := Task{AllowedTools: []string{}, ToolsDeclared: true}
	if !toolBoundaryApplies(declaredEmpty) {
		t.Fatal("a declared-empty tools: list must be read as a boundary")
	}
	// The roster is pinned LITERALLY rather than read from
	// `claudeNativeTools`: that variable is both the output's source and
	// would be its oracle, so a name LEAVING it — the drift this roster is
	// known for — would be invisible. Measured: dropping `Skill`,
	// `ToolSearch` or `TodoWrite` from it left every other test in the
	// package green while `tools: []` silently kept the tool.
	want := []string{
		"Bash", "Read", "Glob", "Grep", "Write", "Edit", "MultiEdit",
		"NotebookEdit", "Task", "WebFetch", "WebSearch", "ToolSearch",
		"TodoWrite", "Skill",
	}
	got := claudeNativeDisallowedTools(declaredEmpty.AllowedTools, true, false)
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Errorf("native tool %q survived a `tools: []` declaration", w)
		}
	}
	for _, g := range got {
		if !slices.Contains(want, g) {
			t.Errorf("%q is disallowed but is not on the pinned roster — update the pin deliberately", g)
		}
	}
}

// The other direction, on the same fixture shape: an UNDECLARED list keeps
// the legacy unrestricted semantics. Without this, "disallow everything"
// could be reached by disallowing everything always.
func TestAnUndeclaredToolListStillCarriesTheFullNativeSurface(t *testing.T) {
	undeclared := Task{}
	if toolBoundaryApplies(undeclared) {
		t.Fatal("an undeclared tools: list must not be read as a boundary")
	}
	// The guarantee is carried by the CALLER: with no declaration the
	// option block is never entered, so nothing is removed. Asserted on the
	// predicate rather than on a `declared=false` argument the production
	// path can no longer pass.
	if toolBoundaryApplies(Task{AllowedTools: nil}) {
		t.Fatal("an undeclared list must not reach the disallow arm")
	}
}

// codex maps the list onto a sandbox mode. A declared-empty list names
// nothing that writes, so the least-privilege mode is read-only; an
// undeclared one keeps workspace-write, which is what "unrestricted native
// toolset" means there.
//
// Reddens on the mutation that drops ToolsDeclared from codexSandboxForTask.
func TestCodexSandboxIsReadOnlyForADeclaredEmptyToolListAndWritableWithout(t *testing.T) {
	for _, tc := range []struct {
		name string
		task Task
		want string
	}{
		{"declared empty", Task{AllowedTools: []string{}, ToolsDeclared: true}, "read-only"},
		{"undeclared", Task{}, "workspace-write"},
		{"declared read-only names", Task{AllowedTools: []string{"read_file"}, ToolsDeclared: true}, "read-only"},
		{"declared writing names", Task{AllowedTools: []string{"bash"}, ToolsDeclared: true}, "workspace-write"},
		{"readonly: wins", Task{Readonly: true}, "read-only"},
		{"full_access: wins", Task{FullAccess: true, AllowedTools: []string{}, ToolsDeclared: true}, "danger-full-access"},
	} {
		if got := codexSandboxForTask(tc.task); got != tc.want {
			t.Errorf("%s: codex sandbox = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The sentences this change writes about `tools: []` are executed here rather
// than read. Three review rounds in a row found a FALSE claim about what a
// backend does with a declared-empty list — "claw resolves zero ToolDefs from
// it", "Workflow survives `--disallowedTools`", "the backend's full native
// toolset becomes available" — each written by the fix for the previous one.
// Prose cannot hold this; a table can.
//
// Each row is a sentence that appears in docs/permissions.md, in
// toolcatalog.ReceivesToolList's godoc, or in C270's message. Reddens when a
// claim stops being true, wherever the claim is written.
func TestTheDocumentedConsequencesOfAnEmptyToolListAreTrue(t *testing.T) {
	declaredEmpty := Task{AllowedTools: []string{}, ToolsDeclared: true}
	disallowed := claudeNativeDisallowedTools(declaredEmpty.AllowedTools, true, false)

	// "claude_code disallows all fourteen names of claudeNativeTools"
	if len(disallowed) != 14 {
		t.Errorf("claude_code disallows %d names, the docs say fourteen: %v", len(disallowed), disallowed)
	}

	// "Agent, TaskOutput and Monitor survive it, and so does every MCP tool"
	// — the claim the whole `tools: []` narrative rests on, and the reason
	// the parallel-branch scheduler does NOT read such a node as read-only.
	for _, survivor := range []string{"Agent", "TaskOutput", "Monitor", "mcp__srv__anything"} {
		if slices.Contains(disallowed, survivor) {
			t.Errorf("%q is disallowed — the docs say it survives `tools: []`, and the scheduler's pessimism is justified by that", survivor)
		}
	}

	// "Workflow is withheld separately, from every NON-ultracode node
	// whatever its list; an ultracode node with `tools: []` keeps it." The
	// roster is the wrong place to look for it, which is exactly what the
	// first two attempts at this sentence got wrong.
	if slices.Contains(disallowed, "Workflow") {
		t.Error("Workflow is on the native disallow list — the docs say it is withheld by a separate, ultracode-keyed rule")
	}

	// "codex drops to the read-only sandbox"
	if got := codexSandboxForTask(declaredEmpty); got != "read-only" {
		t.Errorf("codex sandbox for a declared-empty list = %q, the docs say read-only", got)
	}
}

// The decision executed at the ARGV, not at the helper.
//
// `toolBoundaryApplies` and `claudeNativeDisallowedTools` were each tested
// alone, and their composition — the only place either decides anything — was
// tested nowhere: reverting the caller's predicate to the pre-#1615
// `len(task.AllowedTools) > 0` restored the reported bug (a `tools: []` node
// keeping all fourteen natives) with every test in the repo green. A mutation
// that reddens inside a helper and not at its call site proves the helper,
// not the product.
//
// So this drives the real flag builder: whatever `claudeToolOptions` returns
// is resolved to the command line the CLI would receive.
func TestTheCommandLineCarriesTheBoundaryForADeclaredEmptyToolList(t *testing.T) {
	argv := func(task Task, extra ...string) string {
		_, args := claudesdk.ResolveSpawn(claudeToolOptions(task, extra)...)
		return strings.Join(args, " ")
	}

	declared := argv(Task{AllowedTools: []string{}, ToolsDeclared: true})
	for _, native := range []string{
		"Bash", "Read", "Glob", "Grep", "Write", "Edit", "MultiEdit",
		"NotebookEdit", "Task", "WebFetch", "WebSearch", "ToolSearch",
		"TodoWrite", "Skill",
	} {
		if !strings.Contains(declared, native) {
			t.Errorf("`tools: []` reaches the CLI without removing %q — argv: %s", native, declared)
		}
	}
	if !strings.Contains(declared, "--disallowedTools") {
		t.Errorf("`tools: []` emits no --disallowedTools at all: %q", declared)
	}
	// An empty approval list is a no-op flag, not an empty allowlist.
	if strings.Contains(declared, "--allowedTools") {
		t.Errorf("a declared-empty list must emit no --allowedTools: %q", declared)
	}

	// An UNDECLARED list touches neither flag: that is the legacy
	// unrestricted surface, and it must not move.
	// Parsed, not substring-matched: "allowedTools" is a substring of
	// "--disallowedTools", so the two flags have to be told apart by field.
	for _, flag := range strings.Fields(argv(Task{})) {
		if flag == "--allowedTools" || flag == "--disallowedTools" {
			t.Errorf("an undeclared list emitted %s", flag)
		}
	}

	// A named list still bounds the roster and registers its approval.
	named := argv(Task{AllowedTools: []string{"read_file"}, ToolsDeclared: true}, "mcp__iterion__ask_user")
	if !strings.Contains(named, "--allowedTools read_file,mcp__iterion__ask_user") {
		t.Errorf("the named list and its MCP extras must be registered once: %q", named)
	}
	// Parsed from the FLAG, not matched in the joined argv: `Contains(argv,
	// "Read")` is satisfied by `--allowedTools read_file`, and
	// `"--disallowedTools Read"` can never match because `Bash` precedes
	// `Read` in the roster — an assertion that cannot fire is worse than none.
	namedDisallow := disallowed(named)
	if slices.Contains(namedDisallow, "Read") {
		t.Errorf("a named list removed the one tool it declared: %v", namedDisallow)
	}
	if !slices.Contains(namedDisallow, "Bash") || !slices.Contains(namedDisallow, "Write") {
		t.Errorf("a named list must disallow the natives it does not carry: %v", namedDisallow)
	}
}
