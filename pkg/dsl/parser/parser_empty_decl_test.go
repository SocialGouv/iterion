package parser

import (
	"strings"
	"testing"
)

// A prompt, schema, cursor, mcp_server, supervisor or group header with
// no indented body — followed by a blank line and another declaration, or
// by the end of the file — declares an EMPTY one. The studio saves a
// declaration the moment it is created; the bare header is its written
// form, and the unparser writes a blank line between declarations.
func TestEmptyDeclarationsParse(t *testing.T) {
	src := "schema s:\n\nprompt p:\n\ncursor c:\n\n## a comment after the blank line is fine\nmcp_server m:\n\nsupervisor v:\n\ngroup g:\n\nworkflow w:\n  entry: done\n"
	pr := Parse("empty.bot", src)
	for _, d := range pr.Diagnostics {
		t.Errorf("unexpected diagnostic: %s", d.Error())
	}
	f := pr.File
	if len(f.Schemas) != 1 || f.Schemas[0].Name != "s" || len(f.Schemas[0].Fields) != 0 {
		t.Errorf("empty schema not kept: %+v", f.Schemas)
	}
	if len(f.Prompts) != 1 || f.Prompts[0].Name != "p" || f.Prompts[0].Body != "" {
		t.Errorf("empty prompt not kept: %+v", f.Prompts)
	}
	if len(f.Cursors) != 1 || f.Cursors[0].Name != "c" {
		t.Errorf("empty cursor not kept: %+v", f.Cursors)
	}
	if len(f.MCPServers) != 1 || f.MCPServers[0].Name != "m" {
		t.Errorf("empty mcp_server not kept: %+v", f.MCPServers)
	}
	if len(f.Supervisors) != 1 || f.Supervisors[0].Name != "v" {
		t.Errorf("empty supervisor not kept: %+v", f.Supervisors)
	}
	if len(f.Groups) != 1 || f.Groups[0].Name != "g" {
		t.Errorf("empty group not kept: %+v", f.Groups)
	}
	if len(f.Workflows) != 1 {
		t.Errorf("the workflow after the empty headers was lost: %+v", f.Workflows)
	}
}

// An empty declaration may also be the last thing in the file.
func TestEmptyDeclarationAtEndOfFile(t *testing.T) {
	pr := Parse("empty.bot", "workflow w:\n  entry: done\n\nschema s:\n")
	for _, d := range pr.Diagnostics {
		t.Errorf("unexpected diagnostic: %s", d.Error())
	}
	if len(pr.File.Schemas) != 1 {
		t.Fatalf("empty schema at EOF not kept: %+v", pr.File.Schemas)
	}
}

func indentError(pr *ParseResult) bool {
	for _, d := range pr.Diagnostics {
		if strings.Contains(d.Message, "INDENT") {
			return true
		}
	}
	return false
}

// A body at the wrong indentation is still the indentation error with its
// hint, not an empty declaration followed by a stray line.
func TestUnindentedBodyIsStillTheIndentError(t *testing.T) {
	pr := Parse("bad.bot", "schema s:\ncode: string\n")
	if !indentError(pr) {
		t.Fatalf("want the E002 indentation error, got %v", pr.Diagnostics)
	}
}

// A group's members are top-level keywords themselves: without the blank
// line rule an unindented group body would parse as an empty group plus
// top-level nodes, with no diagnostic anywhere.
func TestUnindentedGroupBodyIsStillTheIndentError(t *testing.T) {
	src := "group review:\nagent linter:\n  description: \"lint\"\nagent tester:\n  description: \"test\"\n\nworkflow w:\n  entry: linter\n  linter -> tester\n  tester -> done\n"
	pr := Parse("bad.bot", src)
	if !indentError(pr) {
		t.Fatalf("an unindented group body parsed without the indentation error: %v", pr.Diagnostics)
	}
}

// An indented comment alone is not a body, and not a blank line either.
func TestCommentOnlyBodyIsStillTheIndentError(t *testing.T) {
	pr := Parse("bad.bot", "schema verdict:\n  ## nothing yet\nagent a:\n  output: verdict\n")
	if !indentError(pr) {
		t.Fatalf("a comment-only body parsed without the indentation error: %v", pr.Diagnostics)
	}
}

// A node declaration keeps needing a body: an `agent a:` with nothing
// under it is a mistake, and the unparser writes `description: ""` for a
// node that has nothing else.
func TestNodeHeaderWithoutBodyStillFails(t *testing.T) {
	pr := Parse("bad.bot", "agent a:\n\nworkflow w:\n  entry: a\n")
	if len(pr.Diagnostics) == 0 {
		t.Fatal("a bodyless agent header parsed without a diagnostic")
	}
}
