package unparse

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

// A group the canvas created and did not fill has no written form (a group
// header needs an indented body, and a comment does not open one). The
// guard refuses the save by name rather than with a parse error about an
// INDENT, so the operator knows which group to fill or remove.
func TestVerifyNamesAnEmptyGroup(t *testing.T) {
	f := &ast.File{
		Groups:    []*ast.GroupDecl{{Name: "later"}},
		Workflows: []*ast.WorkflowDecl{{Name: "w", Entry: "done"}},
	}
	err := Verify(f, Unparse(f))
	if err == nil || !strings.Contains(err.Error(), `group "later" is empty`) {
		t.Fatalf("want a refusal naming the empty group, got %v", err)
	}
}
