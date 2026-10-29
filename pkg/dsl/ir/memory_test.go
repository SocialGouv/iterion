package ir

import (
	"fmt"
	"testing"
)

// memorySrc is one bundle whose single agent carries the `memory:` block
// under test, compiled by the REAL compiler: the defaults a declaration does
// not state (read and write both true) are compileMemory's, and a predicate
// judged against a hand-built struct would miss them.
func memorySrc(block string) string {
	return fmt.Sprintf(`
schema empty:
  ok: bool

prompt sys:
  hi

prompt usr:
  hi

agent start:
  backend: "claw"
  model: "test-model"
  input: empty
  output: empty
  system: sys
  user: usr
  memory:
%s

workflow minimal:
  entry: start
  start -> done
`, block)
}

// TestCompileMemoryWriteToolInstallation holds Memory.InstallsWriteTool to
// what a run actually gets (model.installWorkspaceMemory installs
// memory_write on `spec.Write && AuthorizeSpaceWrite(...) == nil`), measured
// over the declarations the compiler produces:
//
//   - the default — a block that says nothing about `write:` gets the tool,
//     which is why the read-only shape is always an explicit `write: false`;
//   - `write: false`, the remedy C319 prescribes;
//   - `visibility: "global"`, the instance-wide catalogue no run may write;
//   - `enabled: false`, which compiles to no block at all.
//
// Every reader of the capability (pkg/bundlelint's C274 and its corpus twin
// under bots/) asks this predicate, so a case here is the agreement between
// them.
func TestCompileMemoryWriteToolInstallation(t *testing.T) {
	cases := []struct {
		what  string
		block string
		want  bool
	}{
		{"the default block", "    enabled: true\n    scope: \"vision\"\n", true},
		{"read: true alone keeps the write default", "    enabled: true\n    scope: \"vision\"\n    read: true\n", true},
		{"write: false", "    enabled: true\n    scope: \"vision\"\n    write: false\n", false},
		{"write: true on a bot-scoped space", "    enabled: true\n    scope: \"vision\"\n    write: true\n    visibility: \"bot\"\n", true},
		{`visibility: "global"`, "    enabled: true\n    scope: \"catalogue\"\n    visibility: \"global\"\n", false},
		{`visibility: "global" with write: false`, "    enabled: true\n    scope: \"catalogue\"\n    write: false\n    visibility: \"global\"\n", false},
		{`visibility: "org" is tenant-scoped`, "    enabled: true\n    scope: \"shared\"\n    visibility: \"org\"\n", true},
	}
	for _, c := range cases {
		w := mustCompile(t, memorySrc(c.block))
		a, ok := w.Nodes["start"].(*AgentNode)
		if !ok {
			t.Fatalf("%s: start node = %T, want *AgentNode", c.what, w.Nodes["start"])
		}
		if a.Memory == nil {
			t.Fatalf("%s: the compiler dropped the block — this case asserts nothing", c.what)
		}
		if got := a.Memory.InstallsWriteTool(); got != c.want {
			t.Errorf("%s: InstallsWriteTool() = %v, want %v (write=%v visibility=%q)", c.what, got, c.want, a.Memory.Write, a.Memory.Visibility)
		}
	}

	// `enabled: false` compiles to no block, and a nil block installs
	// nothing: the predicate is nil-safe so every caller reads it the same
	// way, with no presence test of its own to get wrong.
	w := mustCompile(t, memorySrc("    enabled: false\n    scope: \"vision\"\n"))
	a, ok := w.Nodes["start"].(*AgentNode)
	if !ok {
		t.Fatalf("start node = %T, want *AgentNode", w.Nodes["start"])
	}
	if a.Memory != nil {
		t.Fatalf("a disabled block reached the IR: %+v", a.Memory)
	}
	if a.Memory.InstallsWriteTool() {
		t.Error("a nil `memory:` block was read as installing the write tool")
	}
}
