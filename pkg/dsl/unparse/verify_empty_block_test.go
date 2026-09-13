package unparse

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// Every block the AST can hold empty — the `{}` a canvas document carries
// for a block it created and did not fill in — has a written form (the
// bare header) and reads back as the same empty block, so the document
// stays saveable. Presence is kept, not normalised away: the text carries
// the declaration as the author wrote it, and whether an empty block means
// anything is the compiler's call (a `recovery:` block's presence is read
// by the verified-action checks; the others compile as their absence does).
func TestVerifyAcceptsEmptyBlocks(t *testing.T) {
	for name, f := range emptyBlockCases() {
		t.Run(name, func(t *testing.T) {
			text := Unparse(f)
			// The header must be IN the text: with a workflow present the
			// guard compares compiled programs, and an empty block compiles
			// like its absence — so a writer that omitted the block would
			// pass the guard and still delete the header on save.
			header, ok := emptyBlockHeaders[name]
			if !ok {
				t.Fatalf("no expected header for case %q", name)
			}
			if !strings.Contains(text, header) {
				t.Fatalf("the empty %s block was omitted (no %q in the text):\n%s", name, header, text)
			}
			if err := Verify(f, text); err != nil {
				t.Fatalf("an empty %s block does not round-trip: %v\n%s", name, err, text)
			}
			if again := Unparse(parser.Parse("", text).File); again != text {
				t.Fatalf("the round-trip is not stable:\n%s\n---\n%s", text, again)
			}
		})
	}
}

// emptyBlockHeaders is the header each empty block must leave in the text.
var emptyBlockHeaders = map[string]string{
	"vars":                  "vars:",
	"presets":               "presets:",
	"attachments":           "attachments:",
	"secrets":               "secrets:",
	"mcp_server auth":       "auth:",
	"agent mcp":             "mcp:",
	"agent compaction":      "compaction:",
	"agent memory":          "memory:",
	"agent cursors":         "cursors:",
	"agent sandbox":         "sandbox: inline",
	"agent sandbox build":   "build:",
	"agent sandbox network": "network:",
	"tool recovery":         "recovery:",
	"workflow vars":         "vars:",
	"workflow attachments":  "attachments:",
	"workflow mcp":          "mcp:",
	"workflow budget":       "budget:",
	"workflow resources":    "resources:",
	"workflow compaction":   "compaction:",
}

// emptyBlockCases is one document per block the AST can hold empty, each
// on a document that compiles (so the guard compares programs).
func emptyBlockCases() map[string]*ast.File {
	doc := func(mut func(f *ast.File)) *ast.File {
		f := promptDoc("x")
		mut(f)
		return f
	}
	return map[string]*ast.File{
		"vars":        doc(func(f *ast.File) { f.Vars = &ast.VarsBlock{} }),
		"presets":     doc(func(f *ast.File) { f.Presets = &ast.PresetsBlock{} }),
		"attachments": doc(func(f *ast.File) { f.Attachments = &ast.AttachmentsBlock{} }),
		"secrets":     doc(func(f *ast.File) { f.Secrets = &ast.SecretsBlock{} }),
		"mcp_server auth": doc(func(f *ast.File) {
			f.MCPServers = []*ast.MCPServerDecl{{Name: "m", Transport: ast.MCPTransportStdio, Command: "x", Auth: &ast.MCPAuthDecl{}}}
		}),
		"agent mcp":        doc(func(f *ast.File) { f.Agents[0].MCP = &ast.MCPConfigDecl{} }),
		"agent compaction": doc(func(f *ast.File) { f.Agents[0].Compaction = &ast.CompactionBlock{} }),
		"agent memory":     doc(func(f *ast.File) { f.Agents[0].Memory = &ast.MemoryBlock{} }),
		"agent cursors":    doc(func(f *ast.File) { f.Agents[0].Cursors = &ast.CursorBlock{Enabled: true} }),
		"agent sandbox":    doc(func(f *ast.File) { f.Agents[0].Sandbox = &ast.SandboxBlock{Mode: "inline"} }),
		"agent sandbox build": doc(func(f *ast.File) {
			f.Agents[0].Sandbox = &ast.SandboxBlock{Mode: "inline", Build: &ast.SandboxBuildBlock{}}
		}),
		"agent sandbox network": doc(func(f *ast.File) {
			f.Agents[0].Sandbox = &ast.SandboxBlock{Mode: "inline", Image: "img", Network: &ast.SandboxNetworkBlock{}}
		}),
		"tool recovery": doc(func(f *ast.File) {
			f.Tools = []*ast.ToolNodeDecl{{Name: "t", Command: "true", Recovery: &ast.RecoveryBlock{}}}
		}),
		"workflow vars":        doc(func(f *ast.File) { f.Workflows[0].Vars = &ast.VarsBlock{} }),
		"workflow attachments": doc(func(f *ast.File) { f.Workflows[0].Attachments = &ast.AttachmentsBlock{} }),
		"workflow mcp":         doc(func(f *ast.File) { f.Workflows[0].MCP = &ast.MCPConfigDecl{} }),
		"workflow budget":      doc(func(f *ast.File) { f.Workflows[0].Budget = &ast.BudgetBlock{} }),
		"workflow resources":   doc(func(f *ast.File) { f.Workflows[0].Resources = &ast.ResourcesBlock{} }),
		"workflow compaction":  doc(func(f *ast.File) { f.Workflows[0].Compaction = &ast.CompactionBlock{} }),
	}
}

// The JSON transport carries every empty block the text carries: a studio
// open → save of an unrelated field must not delete a bare `resources:`
// (or any other empty header) from the file, so what the transport hands
// back writes exactly what the document wrote.
func TestEmptyBlocksSurviveTheJSONTransport(t *testing.T) {
	for name, f := range emptyBlockCases() {
		t.Run(name, func(t *testing.T) {
			want := Unparse(f)
			data, err := ast.MarshalFile(f)
			if err != nil {
				t.Fatal(err)
			}
			back, err := ast.UnmarshalFile(data)
			if err != nil {
				t.Fatal(err)
			}
			if got := Unparse(back); got != want {
				t.Fatalf("the transport dropped or changed the empty %s block:\n%s\n--- after the transport ---\n%s", name, want, got)
			}
		})
	}
}

// A fallback route is written under its name; a route with no name — the
// canvas's route before it is named — has no written form, and a chain
// whose routes are all nameless would serialise to the bare `fallbacks:`
// header the parser refuses. The guard says which node's route is
// nameless before anything is written, and the writer emits no header it
// has no route to put under.
func TestVerifyRefusesANamelessFallbackRouteByName(t *testing.T) {
	f := promptDoc("x")
	f.Agents[0].Fallbacks = []*ast.FallbackDecl{{Name: "", Backend: "claw", Model: "m2"}}
	text := Unparse(f)
	if strings.Contains(text, "fallbacks:") {
		t.Fatalf("a chain with no writable route was written as a bare header:\n%s", text)
	}
	err := Verify(f, text)
	if err == nil {
		t.Fatal("a nameless fallback route was accepted — and dropped from the saved text")
	}
	if !strings.Contains(err.Error(), `agent "a"`) || !strings.Contains(err.Error(), "no name") {
		t.Fatalf("the refusal does not name the node and the cause: %v", err)
	}
}

// The canvas carries a sandbox it created and did not fill in as `{}` — a
// block with no mode, which no `.bot` text can express (the block form is
// inline, the short form names a mode) and which means what its absence
// means: inherit. The transport reads it as absent, so the document saves
// and reads back the same.
func TestVerifyAcceptsAnEmptySandboxFromTheTransport(t *testing.T) {
	for name, doc := range map[string]string{
		"agent":    `{"agents":[{"name":"a","model":"m","sandbox":{}}],"workflows":[{"name":"w","entry":"a","edges":[{"from":"a","to":"done"}]}]}`,
		"workflow": `{"agents":[{"name":"a","model":"m"}],"workflows":[{"name":"w","entry":"a","sandbox":{},"edges":[{"from":"a","to":"done"}]}]}`,
	} {
		f, err := ast.UnmarshalFile([]byte(doc))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		text := Unparse(f)
		if err := Verify(f, text); err != nil {
			t.Fatalf("%s: a `sandbox: {}` from the canvas cannot be saved: %v\n%s", name, err, text)
		}
	}
}

// A group's agents and judges are written by the same writers as the
// top-level ones, so a nameless route on one of them is refused by name
// too — named by group and node — instead of vanishing on save.
func TestVerifyRefusesANamelessFallbackRouteInAGroup(t *testing.T) {
	f := &ast.File{
		Groups: []*ast.GroupDecl{{
			Name:   "g",
			Agents: []*ast.AgentDecl{{Name: "a", LLMDecl: ast.LLMDecl{Model: "m", Fallbacks: []*ast.FallbackDecl{{Name: "", Backend: "claw"}}}}},
		}},
	}
	err := Verify(f, Unparse(f))
	if err == nil {
		t.Fatal("a nameless fallback route on a group's agent was accepted — and dropped from the saved text")
	}
	if !strings.Contains(err.Error(), `group "g"`) || !strings.Contains(err.Error(), `"a"`) || !strings.Contains(err.Error(), "no name") {
		t.Fatalf("the refusal does not name the group, the node and the cause: %v", err)
	}
}

// A sandbox whose only field beside its mode is host_state: is not the
// short form: written as `sandbox: auto` alone, the host_state would be
// dropped on save. The block form carries it, and a transported block
// with only host_state: (no mode) reads back as it was written.
func TestSandboxHostStateSurvivesTheWriter(t *testing.T) {
	f := promptDoc("x")
	f.Agents[0].Sandbox = &ast.SandboxBlock{Mode: "auto", HostState: "none"}
	text := Unparse(f)
	if !strings.Contains(text, "host_state: none") {
		t.Fatalf("host_state was dropped by the short form:\n%s", text)
	}
	if err := Verify(f, text); err != nil {
		t.Fatalf("a sandbox with a host_state does not round-trip: %v\n%s", err, text)
	}
	doc := `{"agents":[{"name":"a","model":"m","sandbox":{"host_state":"none"}}],"workflows":[{"name":"w","entry":"a","edges":[{"from":"a","to":"done"}]}]}`
	g, err := ast.UnmarshalFile([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(g, Unparse(g)); err != nil {
		t.Fatalf("a transported sandbox with only host_state cannot be saved: %v\n%s", err, Unparse(g))
	}
}

// A sandbox block the transport carries with fields but NO mode — the shape
// a canvas editor would emit before it learns about modes — is the block
// form, which the parser reads as inline; the transport reads it the same
// way, so the document and its re-parse agree.
func TestVerifyAcceptsAModelessSandboxFromTheTransport(t *testing.T) {
	for name, doc := range map[string]string{
		"agent, no workflow": `{"agents":[{"name":"a","model":"m","sandbox":{"image":"img"}}]}`,
		"agent":              `{"agents":[{"name":"a","model":"m","sandbox":{"image":"img"}}],"workflows":[{"name":"w","entry":"a","edges":[{"from":"a","to":"done"}]}]}`,
		"workflow":           `{"agents":[{"name":"a","model":"m"}],"workflows":[{"name":"w","entry":"a","sandbox":{"build":{}},"edges":[{"from":"a","to":"done"}]}]}`,
	} {
		f, err := ast.UnmarshalFile([]byte(doc))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		text := Unparse(f)
		if err := Verify(f, text); err != nil {
			t.Fatalf("%s: a mode-less sandbox block from the canvas cannot be saved: %v\n%s", name, err, text)
		}
	}
}

// A plain file whose sandbox build block carries only an empty map parses
// to an empty block, which the writer can only render as the bare header;
// that header has to read back as the same block.
func TestZeroValuedSandboxBuildRoundTrips(t *testing.T) {
	src := "agent a:\n  model: \"m\"\n  sandbox:\n    build:\n      args: {}\n\nworkflow w:\n  entry: a\n  a -> done\n"
	pr := parser.Parse("zero.bot", src)
	for _, d := range pr.Diagnostics {
		t.Fatalf("parse: %s", d.Error())
	}
	text := Unparse(pr.File)
	if err := Verify(pr.File, text); err != nil {
		t.Fatalf("a build block with only an empty map does not round-trip: %v\n%s", err, text)
	}
}

// A plain file whose budget carries only a zero-valued property parses to
// an all-zero block, which the writer can only render as the bare header;
// that header has to read back as the same block.
func TestZeroValuedBudgetRoundTrips(t *testing.T) {
	src := "agent a:\n  model: \"m\"\n\nworkflow w:\n  entry: a\n  budget:\n    max_cost_usd: 0\n  a -> done\n"
	pr := parser.Parse("zero.bot", src)
	for _, d := range pr.Diagnostics {
		t.Fatalf("parse: %s", d.Error())
	}
	text := Unparse(pr.File)
	if !strings.Contains(text, "  budget:\n\n") {
		t.Fatalf("the all-zero budget is not written as a bare header:\n%s", text)
	}
	if err := Verify(pr.File, text); err != nil {
		t.Fatalf("a zero-valued budget does not round-trip: %v\n%s", err, text)
	}
}
