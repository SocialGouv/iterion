package unparse

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

// An artifact label that is not an identifier — what the studio canvas can
// hold — is written quoted, read back as the literal name, and the guard
// accepts the document; it used to be dropped by the re-parse and the
// document could never be saved.
func TestVerifyAcceptsANonIdentifierArtifactLabel(t *testing.T) {
	f := &ast.File{
		Schemas:   []*ast.SchemaDecl{{Name: "out", Fields: []*ast.SchemaField{{Name: "ok", Type: ast.FieldTypeBool}}}},
		Agents:    []*ast.AgentDecl{{Name: "a", LLMDecl: ast.LLMDecl{Model: "m", Output: "out", ArtifactLabels: []string{"review-ledger", "plan"}}}},
		Workflows: []*ast.WorkflowDecl{{Name: "w", Entry: "a", Edges: []*ast.Edge{{From: "a", To: "done"}}}},
	}
	text := Unparse(f)
	if err := Verify(f, text); err != nil {
		t.Fatalf("a document with a kebab-case artifact label does not round-trip: %v\n%s", err, text)
	}
}
