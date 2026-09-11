//go:build unix

package tool

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A FIFO committed to a repository is the case that turns a missing
// regular-file check into a hung agent turn: os.Open blocks until a writer
// appears, and nothing in the tool has a timeout.
func TestWorkspaceReadAndGrep_DoNotBlockOnAFIFO(t *testing.T) {
	workspace := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(workspace, "pipe"), 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "real.txt"), []byte("needle\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		call func() (string, error)
	}{
		{"read_file", func() (string, error) {
			return executeWorkspaceReadFile(map[string]any{"path": "pipe"}, workspace)
		}},
		// grep walks the whole tree, so it reaches the FIFO with no help
		// from the model at all.
		{"workspace_grep", func() (string, error) {
			return executeWorkspaceGrep(map[string]any{"pattern": "needle"}, workspace)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			done := make(chan error, 1)
			go func() {
				_, err := tc.call()
				done <- err
			}()
			select {
			case err := <-done:
				if tc.name == "read_file" && (err == nil || !strings.Contains(err.Error(), "not a regular file")) {
					t.Fatalf("error = %v, want the regular-file refusal", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("blocked on a FIFO — the regular-file check is missing")
			}
		})
	}
}

// filepath.Walk lstats, so a symlink to a file outside the workspace is
// reported as a plain non-dir and opened through — with the credential
// check evaluated on the LINK name, not on the target it resolves to.
func TestWorkspaceGrep_DoesNotFollowSymlinksOutOfTheWorkspace(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "id_rsa")
	if err := os.WriteFile(secret, []byte("needle-private-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// An innocuous name inside the workspace: sensitiveWorkspacePath sees
	// "notes.txt" and waves it through.
	if err := os.Symlink(secret, filepath.Join(workspace, "notes.txt")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "real.txt"), []byte("needle-in-tree\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := executeWorkspaceGrep(map[string]any{"pattern": "needle"}, workspace)
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	if strings.Contains(out, "needle-private-key") {
		t.Fatalf("grep followed a symlink out of the workspace: %q", out)
	}
	if !strings.Contains(out, "needle-in-tree") {
		t.Fatalf("grep stopped matching real files: %q", out)
	}
}
