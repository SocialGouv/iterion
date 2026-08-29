package bundle

import "testing"

func TestDecodeManifestWorkflowSharing(t *testing.T) {
	m, err := DecodeManifest([]byte(`
name: shared-planner
schema_version: 1
exports:
  workflows:
    - id: hierarchy-feature-author
      path: main.bot
dependencies:
  workflows:
    - name: shared-reviewer
`), "test manifest")
	if err != nil {
		t.Fatalf("DecodeManifest: %v", err)
	}
	if got := m.Exports.Workflows[0].ID; got != "hierarchy-feature-author" {
		t.Fatalf("export id = %q", got)
	}
	if got := m.Dependencies.Workflows[0].Name; got != "shared-reviewer" {
		t.Fatalf("dependency name = %q", got)
	}
}

func TestDecodeManifestRejectsUnsafeWorkflowExport(t *testing.T) {
	_, err := DecodeManifest([]byte(`
schema_version: 1
exports:
  workflows:
    - id: escape
      path: ../outside.bot
`), "test manifest")
	if err == nil {
		t.Fatal("expected unsafe export to fail")
	}
}
