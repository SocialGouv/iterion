package tool

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// read_file is registered for every claw agent node (RegisterClawBuiltins),
// not just the assistant's. Its two siblings in this file — workspace_grep
// and glob — both contain the path and refuse credential files; read_file
// shipped without either, so the model could name an absolute path and walk
// straight out of the workspace.

func TestWorkspaceReadFile_ContainsPathInsideWorkspace(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "id_rsa")
	if err := os.WriteFile(secret, []byte("PRIVATE KEY\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "inside.txt"), []byte("inside\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The absolute form is the one that used to skip the workspace join
	// entirely and reach the host filesystem verbatim.
	for _, rawPath := range []string{
		secret,
		filepath.Join("..", filepath.Base(outside), "id_rsa"),
	} {
		out, err := executeWorkspaceReadFile(map[string]any{"path": rawPath}, workspace)
		if err == nil {
			t.Fatalf("read_file(%q) returned %q, want a containment error", rawPath, out)
		}
		if strings.Contains(out, "PRIVATE KEY") {
			t.Fatalf("read_file(%q) leaked the file body", rawPath)
		}
	}

	out, err := executeWorkspaceReadFile(map[string]any{"path": "inside.txt"}, workspace)
	if err != nil {
		t.Fatalf("read_file on a workspace file: %v", err)
	}
	if out != "inside\n" {
		t.Fatalf("read_file output = %q, want %q", out, "inside\n")
	}
}

func TestWorkspaceReadFile_RefusesCredentialFilesInsideWorkspace(t *testing.T) {
	workspace := t.TempDir()
	for _, name := range []string{".env", "deploy.pem", "id_ed25519"} {
		if err := os.WriteFile(filepath.Join(workspace, name), []byte("SECRET=1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := executeWorkspaceReadFile(map[string]any{"path": name}, workspace)
		if err == nil || !strings.Contains(err.Error(), "credential or secret file") {
			t.Fatalf("read_file(%q) error = %v (out %q), want the credential exclusion", name, err, out)
		}
	}
}

func TestWorkspaceReadFile_RequiresActiveWorkspace(t *testing.T) {
	// Matches workspace_grep and glob: with no workspace there is no
	// containment boundary at all, so the tool refuses rather than reading
	// whatever absolute path the model supplies.
	if _, err := executeWorkspaceReadFile(map[string]any{"path": "/etc/passwd"}, ""); err == nil ||
		!strings.Contains(err.Error(), "active workspace is required") {
		t.Fatalf("error = %v, want the missing active-workspace error", err)
	}
}

func TestWorkspaceReadFile_RefusesNonRegularFiles(t *testing.T) {
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := executeWorkspaceReadFile(map[string]any{"path": "sub"}, workspace); err == nil ||
		!strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("read_file on a directory: error = %v, want the regular-file refusal", err)
	}
}

// The 240 KiB cap has to bound the READ, not merely the output: os.ReadFile
// pulled the whole file in first, so the retained bytes scaled with the file.
func TestWorkspaceReadFile_RetainsOnlyOneChunkOfALargeFile(t *testing.T) {
	workspace := t.TempDir()
	line := strings.Repeat("x", 1023) + "\n"
	total := (workspaceReadMaxBytes / len(line)) * 4
	var body strings.Builder
	for i := 0; i < total; i++ {
		body.WriteString(line)
	}
	if err := os.WriteFile(filepath.Join(workspace, "big.txt"), []byte(body.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := executeWorkspaceReadFile(map[string]any{"path": "big.txt"}, workspace)
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	if len(out) > workspaceReadMaxBytes+512 {
		t.Fatalf("read_file returned %d bytes, want at most one %d-byte chunk", len(out), workspaceReadMaxBytes)
	}
	if !strings.Contains(out, "byte cap reached") {
		t.Fatalf("read_file output missing the partial marker: %q", tail(out))
	}
	if !strings.Contains(out, fmt.Sprintf("of %d;", total)) {
		t.Fatalf("read_file partial marker lost the total line count: %q", tail(out))
	}

	// The continuation contract still holds: start_line from the marker
	// picks up exactly where the previous chunk stopped.
	window, count, err := workspaceFileWindow(workspace, filepath.Join(workspace, "big.txt"), total, workspaceReadMaxBytes)
	if err != nil {
		t.Fatalf("workspaceFileWindow: %v", err)
	}
	if count != total || len(window) != 1 {
		t.Fatalf("window at the last line = %d lines of %d, want 1 of %d", len(window), count, total)
	}
}

func TestWorkspaceFileWindow_CountsLinesLikeTheWholeFileRead(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		body  string
		lines int
	}{
		{"", 0},
		{"\n", 1},
		{"a\nb\n", 2},
		{"a\nb", 2},
		{"a\n\nb\n", 3},
	}
	for i, tc := range cases {
		path := filepath.Join(dir, fmt.Sprintf("f%d.txt", i))
		if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		window, total, err := workspaceFileWindow(dir, path, 1, workspaceReadMaxBytes)
		if err != nil {
			t.Fatalf("%q: %v", tc.body, err)
		}
		if total != tc.lines {
			t.Errorf("%q: total = %d, want %d", tc.body, total, tc.lines)
		}
		if got := strings.Join(window, ""); got != tc.body {
			t.Errorf("%q: window rejoined to %q", tc.body, got)
		}
	}
}

// A single line longer than the chunk cap must not be retained whole — that
// is the other half of "the cap bounds the read".
func TestWorkspaceReadFile_CapsAnOverlongSingleLine(t *testing.T) {
	workspace := t.TempDir()
	body := strings.Repeat("y", workspaceReadMaxBytes*3)
	if err := os.WriteFile(filepath.Join(workspace, "one-line.txt"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := executeWorkspaceReadFile(map[string]any{"path": "one-line.txt"}, workspace)
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	if len(out) > workspaceReadMaxBytes+512 {
		t.Fatalf("read_file returned %d bytes for one long line, want at most the %d-byte chunk", len(out), workspaceReadMaxBytes)
	}
	if !strings.Contains(out, "exceeds the") {
		t.Fatalf("missing the over-long-line marker: %q", tail(out))
	}
}

func tail(s string) string {
	if len(s) <= 200 {
		return s
	}
	return "..." + s[len(s)-200:]
}

func TestWorkspaceReadFileChecksBothSymlinkNamesAndTargets(t *testing.T) {
	root := t.TempDir()
	for name, body := range map[string]string{"ordinary.txt": "public", ".env": "secret"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for alias, target := range map[string]string{"readable.txt": "ordinary.txt", "hidden.txt": ".env", ".env.alias": "ordinary.txt"} {
		if err := os.Symlink(target, filepath.Join(root, alias)); err != nil {
			t.Skipf("symlinks: %v", err)
		}
	}
	for _, path := range []string{"hidden.txt", ".env.alias"} {
		if out, err := executeWorkspaceReadFile(map[string]any{"path": path}, root); err == nil || out != "" {
			t.Fatalf("%s: %q, %v", path, out, err)
		}
		if out, err := executeWorkspaceGrep(map[string]any{"path": path, "pattern": "."}, root); err == nil || out != "" {
			t.Fatalf("grep %s: %q, %v", path, out, err)
		}
	}
	aliasRoot := filepath.Join(t.TempDir(), "workspace")
	if err := os.Symlink(root, aliasRoot); err != nil {
		t.Fatal(err)
	}
	if out, err := executeWorkspaceReadFile(map[string]any{"path": "readable.txt"}, aliasRoot); err != nil || out != "public" {
		t.Fatalf("internal symlink: %q, %v", out, err)
	}
}

func TestWorkspaceReadFileRejectsOversizedSparseFile(t *testing.T) {
	root := t.TempDir()
	f, err := os.Create(filepath.Join(root, "large.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(workspaceReadMaxFileBytes + 1); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()
	for _, start := range []int{1, 1000000000} {
		out, err := executeWorkspaceReadFile(map[string]any{"path": "large.txt", "start_line": start}, root)
		if err == nil || !strings.Contains(err.Error(), "read ceiling") || out != "" {
			t.Fatalf("start=%d: %q, %v", start, out, err)
		}
	}
}
