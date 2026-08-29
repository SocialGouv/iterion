package bundle

import (
	"strings"
	"testing"
)

func TestDecodeManifestAuthoringEditableFiles(t *testing.T) {
	m, err := DecodeManifest([]byte(`
schema_version: 1
authoring:
  editable_files:
    - scope: bundle
      path: ./subbots/review.bot
    - scope: WORKSPACE
      path: film_pipeline/matter.py
`), "test manifest")
	if err != nil {
		t.Fatal(err)
	}
	if m.Authoring == nil || len(m.Authoring.EditableFiles) != 2 {
		t.Fatalf("authoring = %#v", m.Authoring)
	}
	if got := m.Authoring.EditableFiles[0]; got.Scope != AuthoringScopeBundle || got.Path != "subbots/review.bot" {
		t.Fatalf("bundle file = %#v", got)
	}
	if got := m.Authoring.EditableFiles[1]; got.Scope != AuthoringScopeWorkspace || got.Path != "film_pipeline/matter.py" {
		t.Fatalf("workspace file = %#v", got)
	}
}

func TestDecodeManifestRejectsUnsafeAuthoringFiles(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"unknown scope", "- {scope: project, path: file.py}", "unknown scope"},
		{"absolute", "- {scope: workspace, path: /etc/passwd}", "must be relative"},
		{"traversal", "- {scope: bundle, path: ../other/main.bot}", "escapes"},
		{"normalized traversal", "- {scope: bundle, path: sub/../main.bot}", "escapes"},
		{"windows traversal", `- {scope: workspace, path: '..\\secret.txt'}`, "escapes"},
		{"empty", "- {scope: bundle, path: ''}", "path is required"},
		{"duplicate", "- {scope: bundle, path: a.py}\n    - {scope: bundle, path: ./a.py}", "duplicate"},
		{"glob", "- {scope: workspace, path: scripts/*.py}", "globs are not allowed"},
		{"secret", "- {scope: workspace, path: .env.local}", "secret-bearing path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DecodeManifest([]byte("schema_version: 1\nauthoring:\n  editable_files:\n    "+tt.body+"\n"), "test manifest")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want containing %q", err, tt.want)
			}
		})
	}
}
