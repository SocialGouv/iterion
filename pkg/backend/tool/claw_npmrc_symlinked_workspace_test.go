package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// .npmrc refuses at a project position — the root of the workspace or of a
// read root (#2868). The gate judges the RESOLVED target, so the workspace
// must anchor in its canonical spelling too: under a workspace reached
// through a symlink, the relative spelling agents use and a grep over the
// tree both resolve to the canonical directory, which the as-held spelling
// never matches. Control: the same tree scoped on its real path refuses.
func TestNpmrcRefusesUnderAWorkspaceSpelledThroughASymlink(t *testing.T) {
	t.Parallel()
	real := t.TempDir()
	if err := os.WriteFile(filepath.Join(real, ".npmrc"), []byte("//registry.npmjs.org/:_authToken=npm_SECRET\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "workspace-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	for _, workspace := range []string{real, link} {
		reg := NewRegistry()
		if err := RegisterClawBuiltinsWithScope(reg, NewReadScope(workspace), nil); err != nil {
			t.Fatal(err)
		}
		call := func(name string, input map[string]any) (string, error) {
			td, err := reg.Resolve(name)
			if err != nil {
				t.Fatalf("resolve %s: %v", name, err)
			}
			raw, _ := json.Marshal(input)
			return td.Execute(context.Background(), raw)
		}
		if out, err := call("read_file", map[string]any{"path": ".npmrc"}); err == nil {
			t.Errorf("workspace %s: read_file(.npmrc) = %q, want a credential refusal", workspace, out)
		}
		if out, err := call("grep", map[string]any{"pattern": "npm_SECRET", "path": "."}); err != nil || strings.Contains(out, "_authToken") {
			t.Errorf("workspace %s: grep over the tree = %q, %v — the project-root .npmrc must not be searched", workspace, out, err)
		}
	}
}
