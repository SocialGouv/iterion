package unparse

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

// A declaration the canvas created and did not fill in yet — a schema
// with no field, a prompt with no line, an empty cursor, mcp_server,
// supervisor or group — has a written form (the bare header) and reads
// back as the same empty declaration, so the whole document stays
// saveable while it is being authored.
func TestVerifyAcceptsEmptyDeclarations(t *testing.T) {
	cases := map[string]*ast.File{
		"schema":     {Schemas: []*ast.SchemaDecl{{Name: "s"}}},
		"prompt":     {Prompts: []*ast.PromptDecl{{Name: "p"}}},
		"cursor":     {Cursors: []*ast.CursorDecl{{Name: "c"}}},
		"mcp_server": {MCPServers: []*ast.MCPServerDecl{{Name: "m"}}},
		"supervisor": {Supervisors: []*ast.SupervisorDecl{{Name: "v"}}},
		"group":      {Groups: []*ast.GroupDecl{{Name: "g"}}},
		"all, with a workflow": {
			Schemas:     []*ast.SchemaDecl{{Name: "s"}},
			Prompts:     []*ast.PromptDecl{{Name: "p"}},
			Cursors:     []*ast.CursorDecl{{Name: "c"}},
			MCPServers:  []*ast.MCPServerDecl{{Name: "m"}},
			Supervisors: []*ast.SupervisorDecl{{Name: "v"}},
			Groups:      []*ast.GroupDecl{{Name: "g"}},
			Workflows:   []*ast.WorkflowDecl{{Name: "w", Entry: "done"}},
		},
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			text := Unparse(f)
			if err := Verify(f, text); err != nil {
				t.Fatalf("an empty %s does not round-trip: %v\n%s", name, err, text)
			}
		})
	}
}
