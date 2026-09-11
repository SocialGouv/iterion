package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspacePathContract_DefaultsRegisteredToolsToActiveRoot(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "root-marker.txt"), []byte("root-needle\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workspace, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "nested", "nested-marker.txt"), []byte("nested-needle\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := NewRegistry()
	if err := RegisterClawBuiltins(r, workspace); err != nil {
		t.Fatal(err)
	}
	if err := RegisterClawWorkspaceDiagnostics(r, workspace, nil); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"glob", "workspace_grep"} {
		td, err := r.Resolve(name)
		if err != nil {
			t.Fatal(err)
		}
		var schema struct {
			Required []string `json:"required"`
		}
		if err := json.Unmarshal(td.InputSchema, &schema); err != nil {
			t.Fatalf("%s schema: %v", name, err)
		}
		for _, required := range schema.Required {
			if required == "path" {
				t.Fatalf("%s still requires optional path: %s", name, td.InputSchema)
			}
		}
	}

	checks := []struct {
		name    string
		tool    string
		payload string
		want    string
	}{
		{"glob omitted", "glob", `{"pattern":"root-marker.txt"}`, "root-marker.txt"},
		{"glob empty", "glob", `{"pattern":"root-marker.txt","path":""}`, "root-marker.txt"},
		{"glob dot", "glob", `{"pattern":"root-marker.txt","path":"."}`, "root-marker.txt"},
		{"grep omitted", "workspace_grep", `{"pattern":"root-needle"}`, "root-marker.txt:1:root-needle"},
		{"grep empty", "workspace_grep", `{"pattern":"root-needle","path":""}`, "root-marker.txt:1:root-needle"},
		{"grep dot", "workspace_grep", `{"pattern":"root-needle","path":"."}`, "root-marker.txt:1:root-needle"},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			td, err := r.Resolve(check.tool)
			if err != nil {
				t.Fatal(err)
			}
			out, err := td.Execute(context.Background(), json.RawMessage(check.payload))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out, check.want) {
				t.Fatalf("%s output = %q, want %q", check.tool, out, check.want)
			}
		})
	}

	for _, check := range []struct {
		name    string
		tool    string
		payload string
		want    string
	}{
		{"glob subdirectory", "glob", `{"pattern":"nested-marker.txt","path":"nested"}`, "nested/nested-marker.txt"},
		{"grep subdirectory", "workspace_grep", `{"pattern":"nested-needle","path":"nested"}`, "nested-marker.txt:1:nested-needle"},
	} {
		t.Run(check.name, func(t *testing.T) {
			td, err := r.Resolve(check.tool)
			if err != nil {
				t.Fatal(err)
			}
			out, err := td.Execute(context.Background(), json.RawMessage(check.payload))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out, check.want) {
				t.Fatalf("%s output = %q, want %q", check.tool, out, check.want)
			}
		})
	}
}

func TestWorkspacePathContract_RejectsInvalidPathsAndMissingContext(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "marker.txt"), []byte("needle\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "outside.txt"), []byte("needle\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := NewRegistry()
	if err := RegisterClawBuiltins(r, workspace); err != nil {
		t.Fatal(err)
	}
	if err := RegisterClawWorkspaceDiagnostics(r, workspace, nil); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		tool    string
		payload string
		want    string
	}{
		{"grep invalid type", "workspace_grep", `{"pattern":"needle","path":7}`, "path' must be a string"},
		{"grep whitespace", "workspace_grep", `{"pattern":"needle","path":" "}`, "path' must be a non-empty string"},
		{"grep outside", "workspace_grep", `{"pattern":"needle","path":"OUTSIDE"}`, "outside workspace"},
		{"grep missing", "workspace_grep", `{"pattern":"needle","path":"missing"}`, "no such file or directory"},
		{"glob invalid type", "glob", `{"pattern":"*.txt","path":7}`, "path' must be a string"},
		{"glob whitespace", "glob", `{"pattern":"*.txt","path":" "}`, "path' must be a non-empty string"},
		{"glob outside", "glob", `{"pattern":"*.txt","path":"OUTSIDE"}`, "outside workspace"},
		{"glob missing", "glob", `{"pattern":"*.txt","path":"missing"}`, "no such file or directory"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			payload := strings.ReplaceAll(testCase.payload, "OUTSIDE", outside)
			td, err := r.Resolve(testCase.tool)
			if err != nil {
				t.Fatal(err)
			}
			_, err = td.Execute(context.Background(), json.RawMessage(payload))
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("%s error = %v, want substring %q", testCase.tool, err, testCase.want)
			}
		})
	}

	for _, toolName := range []string{"workspace_grep", "glob"} {
		t.Run(toolName+" missing active workspace", func(t *testing.T) {
			var err error
			if toolName == "workspace_grep" {
				_, err = executeWorkspaceGrep(map[string]any{"pattern": "needle"}, "")
			} else {
				_, err = executeWorkspaceGlob(context.Background(), map[string]any{"pattern": "*.txt"}, "")
			}
			if err == nil || !strings.Contains(err.Error(), "active workspace is required") {
				t.Fatalf("error = %v, want missing active-workspace error", err)
			}
		})
	}

	if err := os.Symlink(outside, filepath.Join(workspace, "escape")); err == nil {
		for _, toolName := range []string{"workspace_grep", "glob"} {
			t.Run(toolName+" escaping symlink", func(t *testing.T) {
				td, err := r.Resolve(toolName)
				if err != nil {
					t.Fatal(err)
				}
				payload := `{"pattern":"*.txt","path":"escape"}`
				if toolName == "workspace_grep" {
					payload = `{"pattern":"needle","path":"escape"}`
				}
				_, err = td.Execute(context.Background(), json.RawMessage(payload))
				if err == nil || !strings.Contains(err.Error(), "outside workspace") {
					t.Fatalf("error = %v, want symlink confinement error", err)
				}
			})
		}
	}
}
