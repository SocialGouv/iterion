package bundle

import (
	"os"
	"path/filepath"
	"testing"
)

// DirClaim is the ONE claim verdict (ADR-140): the shape of the directory
// decides — an iterion manifest by EITHER spelling or a skills/ dir claims
// it, main.bot beside the claim opens it, and another tool's manifest
// claims nothing. The matrix pins the shapes every reader of the verdict
// (the discovery walk, the localize path) agrees on.

func writeDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestDirClaimMatrix(t *testing.T) {
	ours := "name: planner\nschema_version: 1\n"
	foreign := "services:\n  web:\n    image: nginx\n"
	generic := "name: thing\nversion: 2\n"
	for _, tc := range []struct {
		name         string
		files        map[string]string
		wantClaimed  bool
		wantOpenable bool
	}{
		{"manifest.yaml and main.bot", map[string]string{"manifest.yaml": ours, "main.bot": "workflow w {}\n"}, true, true},
		{"manifest.yml and main.bot", map[string]string{"manifest.yml": ours, "main.bot": "workflow w {}\n"}, true, true},
		{"manifest.yaml without main.bot", map[string]string{"manifest.yaml": ours}, true, false},
		{"manifest.yml without main.bot", map[string]string{"manifest.yml": ours}, true, false},
		{"skills directory only", map[string]string{"skills/x/SKILL.md": "# x\n"}, true, false},
		{"generic-keys manifest and main.bot", map[string]string{"manifest.yaml": generic, "main.bot": "workflow w {}\n"}, true, true},
		{"foreign manifest and main.bot", map[string]string{"manifest.yaml": foreign, "main.bot": "workflow w {}\n"}, false, false},
		{"foreign manifest.yml and main.bot", map[string]string{"manifest.yml": foreign, "main.bot": "workflow w {}\n"}, false, false},
		{"empty directory", map[string]string{}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeDir(t, tc.files)
			claimed, openable, _ := DirClaim(dir)
			if claimed != tc.wantClaimed || openable != tc.wantOpenable {
				t.Fatalf("DirClaim = (%v, %v), want (%v, %v)", claimed, openable, tc.wantClaimed, tc.wantOpenable)
			}
		})
	}
}
