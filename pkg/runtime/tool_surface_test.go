package runtime

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// authoredDefaults reads no environment: `${VAR:-x}` resolves to x and a
// bare `${VAR}` to "", which is how a catalog guard must read a backend so
// its verdict does not depend on the host it runs on.
func authoredDefaults(string) string { return "" }

// A node that holds a shell is a writer whatever `readonly:` says: the codex
// and pi delegates enforce readonly as a sandbox mode, claude_code never reads
// it. Parallel-branch admission (isMutatingNode) keeps honouring the flag —
// the shared classifier must not have leaked readonly into either direction.
func TestToolSurfaceCanWrite_IgnoresReadonlyWhereAdmissionHonoursIt(t *testing.T) {
	shell := &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: "a"},
		LLMFields: ir.LLMFields{Readonly: true, Backend: "claude_code"},
		Tools:     []string{"bash", "read_file"},
	}
	if !ToolSurfaceCanWrite(shell, "", authoredDefaults) {
		t.Error("a readonly agent holding bash can still write: readonly is not enforced by claude_code")
	}
	if isMutatingNode(shell) {
		t.Error("parallel-branch admission must keep honouring readonly")
	}
	bare := &ir.JudgeNode{
		BaseNode:  ir.BaseNode{ID: "j"},
		LLMFields: ir.LLMFields{Readonly: true, Backend: "claude_code"},
	}
	if !ToolSurfaceCanWrite(bare, "", authoredDefaults) {
		t.Error("a readonly judge with no tools on claude_code runs the full native toolset")
	}
	if isMutatingNode(bare) {
		t.Error("parallel-branch admission must keep honouring readonly on judges")
	}
}

// An omitted tools: list is the full native toolset on a CLI delegate, zero
// tools on claw and on a direct model call; the workflow default backend
// fills in when the node declares none.
func TestToolSurfaceCanWrite_OmittedToolsFollowTheEffectiveBackend(t *testing.T) {
	for _, tc := range []struct {
		backend, dflt string
		want          bool
	}{
		{"claude_code", "", true},
		{"codex", "", true},
		{"kimi", "", true},
		{"", "", false},
		{"auto", "", false},
		{"claw", "", false},
		{"", "claude_code", true},
		{"", "claw", false},
		{"claw", "claude_code", false},
	} {
		n := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}, LLMFields: ir.LLMFields{Backend: tc.backend}}
		if got := ToolSurfaceCanWrite(n, tc.dflt, authoredDefaults); got != tc.want {
			t.Errorf("backend=%q default=%q: got %v, want %v", tc.backend, tc.dflt, got, tc.want)
		}
	}
}

// A tools-less claw node that declares a fallbacks: route onto a CLI backend
// would run that route with the full native toolset, so it is a writer; a
// route that inherits the node's backend, or names claw, is not.
func TestToolSurfaceCanWrite_ClawFallbackReachingACLIBackend(t *testing.T) {
	for _, tc := range []struct {
		route string
		want  bool
	}{
		{"claude_code", true},
		{"${ITERION_TEST_ROUTE_BACKEND:-codex}", true},
		{"", false},
		{"auto", false},
		{"claw", false},
	} {
		n := &ir.AgentNode{
			BaseNode:  ir.BaseNode{ID: "a"},
			LLMFields: ir.LLMFields{Backend: "claw"},
			Fallbacks: []ir.Fallback{{Name: "r", Backend: tc.route}},
		}
		if got := ToolSurfaceCanWrite(n, "", authoredDefaults); got != tc.want {
			t.Errorf("claw with fallback backend %q: got %v, want %v", tc.route, got, tc.want)
		}
	}
}

// A declared list bounds a CLI backend: only the read-only vocabulary keeps a
// node out of the class, and a name the engine does not know as read-only
// (a shell, a writer, an MCP or wrapper tool) puts it in.
func TestToolSurfaceCanWrite_DeclaredTools(t *testing.T) {
	readOnly := []string{"read_file", "glob", "grep", "web_fetch", "git_diff", "git_status", "list_files", "search_codebase", "tree"}
	n := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}, LLMFields: ir.LLMFields{Backend: "claude_code"}, Tools: readOnly}
	if ToolSurfaceCanWrite(n, "", authoredDefaults) {
		t.Error("a claude_code node whose declared list is read-only cannot write")
	}
	for _, name := range readOnly {
		if !IsReadOnlyTool(name) {
			t.Errorf("%s is read-only", name)
		}
	}
	for _, name := range []string{"bash", "diagnostic_shell", "write_file", "file_edit", "edit_file", "workspace_grep", "ask_user"} {
		if IsReadOnlyTool(name) {
			t.Errorf("%s must not be read-only", name)
		}
		n := &ir.JudgeNode{BaseNode: ir.BaseNode{ID: "j"}, Tools: append([]string{"read_file"}, name)}
		if !ToolSurfaceCanWrite(n, "", authoredDefaults) {
			t.Errorf("a judge holding %s can act", name)
		}
	}
}

// The lookup decides a templated backend: the authored default alone with
// the defaults-only lookup, the host environment with a nil lookup. A guard
// that read the environment would be green on the one host whose dial
// happens to be set.
func TestToolSurfaceCanWrite_LookupDecidesATemplatedBackend(t *testing.T) {
	const dial = "ITERION_TEST_SURFACE_BACKEND"
	n := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}, LLMFields: ir.LLMFields{Backend: "${" + dial + ":-claude_code}"}}
	if !ToolSurfaceCanWrite(n, "", authoredDefaults) {
		t.Error("the authored default is claude_code: a writer")
	}
	clawOnHost := func(name string) string {
		if name == dial {
			return "claw"
		}
		return ""
	}
	if ToolSurfaceCanWrite(n, "", clawOnHost) {
		t.Error("the lookup must be honoured: resolved to claw, the node holds zero tools")
	}
	t.Setenv(dial, "claw")
	if ToolSurfaceCanWrite(n, "", nil) {
		t.Error("a nil lookup reads the process environment, where the dial says claw")
	}
	if !ToolSurfaceCanWrite(n, "", authoredDefaults) {
		t.Error("the defaults-only lookup must not read the process environment")
	}
}

func TestToolSurfaceCanWrite_FullAccessAndNonLLMNodes(t *testing.T) {
	full := &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: "a"},
		LLMFields: ir.LLMFields{Backend: "codex", FullAccess: true},
		Tools:     []string{"read_file"},
	}
	if !ToolSurfaceCanWrite(full, "", authoredDefaults) {
		t.Error("full_access lifts the sandbox whatever the declared list says")
	}
	for _, n := range []ir.Node{
		&ir.ToolNode{BaseNode: ir.BaseNode{ID: "t"}},
		&ir.SubbotNode{BaseNode: ir.BaseNode{ID: "s"}},
		&ir.ComputeNode{BaseNode: ir.BaseNode{ID: "c"}},
	} {
		if ToolSurfaceCanWrite(n, "claude_code", authoredDefaults) {
			t.Errorf("%T is not an LLM node and has no prompt to bound", n)
		}
	}
}
