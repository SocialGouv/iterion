package parser

import (
	"strings"
	"testing"
)

// A prompt, schema, cursor, mcp_server, supervisor or group header with
// no indented body — followed by another declaration, or by the end of the
// file — declares an EMPTY one. The studio saves a declaration the moment
// it is created; the bare header is its written form.
func TestEmptyDeclarationsParse(t *testing.T) {
	src := "schema s:\n\nprompt p:\ncursor c:\n## a comment between two empty headers\nmcp_server m:\nsupervisor v:\ngroup g:\n\nworkflow w:\n  entry: done\n"
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

// A body at the wrong indentation is still the indentation error with its
// hint, not an empty declaration followed by a stray line.
func TestUnindentedBodyIsStillTheIndentError(t *testing.T) {
	pr := Parse("bad.bot", "schema s:\ncode: string\n")
	var indentErr bool
	for _, d := range pr.Diagnostics {
		if strings.Contains(d.Message, "INDENT") {
			indentErr = true
		}
	}
	if !indentErr {
		t.Fatalf("want the E002 indentation error, got %v", pr.Diagnostics)
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
