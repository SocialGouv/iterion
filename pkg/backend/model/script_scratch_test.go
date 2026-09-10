package model

import (
	"os"
	"path/filepath"
	"testing"
)

// A tool node's script is written out of the judged tree wherever both
// sides can reach it: a gate that judges the tree's cleanliness must never
// read the engine's own instrument as uncommitted work of the run.
func TestScriptScratchDir(t *testing.T) {
	cases := []struct {
		name           string
		sharedStateDir string
		sandboxed      bool
		copyBased      bool
		wantDir        string
		wantInTree     bool
	}{
		{"unsandboxed: the host temp dir", "", false, false, os.TempDir(), false},
		{"bind-mount sandbox with a shared dir: its scripts scratch", "/shared", true, false, filepath.Join("/shared", "scripts"), false},
		{"bind-mount sandbox without a shared dir: the workspace", "", true, false, "/ws", true},
		{"copy-based sandbox: the workspace, pushed through the seam", "/shared", true, true, "/ws", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, inTree := scriptScratchDir("/ws", tc.sharedStateDir, tc.sandboxed, tc.copyBased)
			if dir != tc.wantDir || inTree != tc.wantInTree {
				t.Fatalf("scriptScratchDir = (%q, %v), want (%q, %v)", dir, inTree, tc.wantDir, tc.wantInTree)
			}
		})
	}
}
