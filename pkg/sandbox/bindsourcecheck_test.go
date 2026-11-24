package sandbox

// The shape contract of the border question (#2069): one scenario per
// guard, isolated. The docker witnesses (pkg/sandbox/docker) hold the same
// question through Start; this table holds the answer itself, where it
// lives.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/safepath"
)

func TestAnswerBindSourceCheckAnswersByTheSourceShape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()

	dir := filepath.Join(root, "store", "scratch")
	if err := safepath.EnsureRealBindSourceDir(root, dir, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "plugins.yaml")
	if err := os.WriteFile(file, []byte("enable: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	engineLaid := filepath.Join(root, "projects", "k", "worktrees", "run")
	if err := os.MkdirAll(engineLaid, 0o755); err != nil {
		t.Fatal(err)
	}

	// A swap, played where the scenario names one: the verified leaf
	// replaced by a link pointing outside the store.
	dirSwap := filepath.Join(root, "store", "swapped-dir")
	if err := safepath.EnsureRealBindSourceDir(root, dirSwap, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(dirSwap); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, dirSwap); err != nil {
		t.Fatal(err)
	}
	fileSwap := filepath.Join(root, "swapped-file.yaml")
	if err := os.WriteFile(fileSwap, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(fileSwap); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, fileSwap); err != nil {
		t.Fatal(err)
	}
	workspaceSwap := filepath.Join(root, "projects", "k", "worktrees", "swapped")
	if err := os.MkdirAll(workspaceSwap, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(workspaceSwap); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, workspaceSwap); err != nil {
		t.Fatal(err)
	}

	// Gone sources: removed after "the plan" verified them, never laid
	// back. The parent chain stays, the leaf does not.
	goneDir := filepath.Join(root, "store", "gone-dir")
	if err := safepath.EnsureRealBindSourceDir(root, goneDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(goneDir); err != nil {
		t.Fatal(err)
	}
	goneFile := filepath.Join(root, "gone.yaml")
	if err := os.WriteFile(goneFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(goneFile); err != nil {
		t.Fatal(err)
	}
	goneWorkspace := filepath.Join(root, "projects", "k", "worktrees", "gone")
	if err := os.MkdirAll(goneWorkspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(goneWorkspace); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		check   BindSourceCheck
		wantErr string // "" = the check must answer nil
		keep    string // a path that must still be absent when wantErr = "is gone"
	}{
		{
			name:  "a store directory still real answers nil",
			check: BindSourceCheck{Root: root, Path: dir},
		},
		{
			name:    "a store directory swapped for a symlink refuses, naming the swap",
			check:   BindSourceCheck{Root: root, Path: dirSwap},
			wantErr: "swapped between plan and creation",
		},
		{
			name:    "a store directory gone since the plan refuses without recreating",
			check:   BindSourceCheck{Root: root, Path: goneDir},
			wantErr: "is gone",
			keep:    goneDir,
		},
		{
			name:  "a verified file answers nil",
			check: BindSourceCheck{Root: root, Path: file},
		},
		{
			name:    "a file swapped for a symlink refuses, naming the swap",
			check:   BindSourceCheck{Root: root, Path: fileSwap},
			wantErr: "swapped between plan and creation",
		},
		{
			name:    "a file gone since the plan refuses without recreating",
			check:   BindSourceCheck{Root: root, Path: goneFile},
			wantErr: "is gone",
			keep:    goneFile,
		},
		{
			name:  "the engine-laid workspace still real answers nil",
			check: BindSourceCheck{Root: root, Path: engineLaid, MustExist: true},
		},
		{
			name:    "the engine-laid workspace swapped for a symlink refuses, naming the swap",
			check:   BindSourceCheck{Root: root, Path: workspaceSwap, MustExist: true},
			wantErr: "swapped between plan and creation",
		},
		{
			name:    "the engine-laid workspace gone since the plan refuses without recreating",
			check:   BindSourceCheck{Root: root, Path: goneWorkspace, MustExist: true},
			wantErr: "is gone",
			keep:    goneWorkspace,
		},
		{
			name:    "a source outside its root refuses",
			check:   BindSourceCheck{Root: dir, Path: file},
			wantErr: "is not under",
		},
		{
			name: "a path that cannot be asked refuses",
			check: func() BindSourceCheck {
				// A leaf under a FILE: Lstat fails with ENOTDIR, not an
				// absence — the ask refuses rather than guessing a shape.
				return BindSourceCheck{Root: root, Path: filepath.Join(file, "child")}
			}(),
			wantErr: "cannot be asked",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := AnswerBindSourceCheck(tc.check)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("the border refused a source that is still what the plan verified: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("the border accepted a source the plan would not recognize (want %q in the refusal)", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("the refusal must name %q, got: %v", tc.wantErr, err)
			}
			if tc.keep != "" {
				if _, statErr := os.Lstat(tc.keep); !os.IsNotExist(statErr) {
					t.Fatalf("the border recreated the gone source (%v) — it recreates nothing the window removed", statErr)
				}
			}
		})
	}
}
