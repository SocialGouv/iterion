//go:build live

package delegate

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestLive_Feat_OpenCodeProjectScreen is a CONFORMANCE bench, not a unit
// test: for each workspace layout it asks the REAL opencode CLI whether it
// loads a resource from that layout, and asks iterion's screen whether it
// refuses it — then requires that iterion never ACCEPT a layout the CLI
// loads from.
//
// It exists because three consecutive adversarial rounds found the screen's
// BOUNDS wrong (an unwalked ancestor, a lexical instead of physical path, a
// stop predicate stronger than the CLI's) while every spelling-level test
// stayed green. A guard that enumerates what the CLI reads cannot converge by
// enumeration; comparing against the CLI itself does, including for a
// divergence a future opencode release introduces.
//
// It needs no credential: opencode imports project plugins during startup,
// before any model turn, so a run that dies on a missing provider has already
// answered whether the plugin executed.
//
// Every row must OBSERVE the CLI loading something, or the row proves
// nothing: a stub named `opencode` first on PATH, a broken install or a
// sandbox with no network would otherwise turn the whole bench into six
// silent no-ops that report PASS.
func TestLive_Feat_OpenCodeProjectScreen(t *testing.T) {
	bin := findOpenCodeForBench(t)

	cases := []struct {
		name string
		// files are workspace-relative; a .ts is a plugin that marks on import.
		files []string
		// workRel is the directory the node would run in.
		workRel string
		// symlinkWork reaches workRel through a symlink, the shape a lexical
		// walk screens wrongly.
		symlinkWork bool
		// danglingGit plants a `.git` symlink pointing nowhere in workRel:
		// it exists to Lstat and not to Stat, so a stop predicate stronger
		// than the CLI's ends the walk where the CLI keeps climbing.
		danglingGit bool
		// wantLoaded pins what the CLI is expected to do, so a row that stops
		// exercising the CLI fails instead of passing vacuously.
		wantLoaded bool
	}{
		{name: "plugin at the workdir", files: []string{".opencode/plugin/pwn.ts"}, workRel: ".", wantLoaded: true},
		{name: "tool at the workdir", files: []string{".opencode/tool/pwn.ts"}, workRel: ".", wantLoaded: true},
		{name: "root opencode.json with a plugin list", files: []string{"opencode.json", "evil.ts"}, workRel: ".", wantLoaded: true},
		{name: "root opencode.jsonc with a plugin list", files: []string{"opencode.jsonc", "evil.ts"}, workRel: ".", wantLoaded: true},
		{name: "plugin in an ancestor", files: []string{".opencode/plugin/pwn.ts"}, workRel: "sub/pkg", wantLoaded: true},
		{name: "plugin at the git root", files: []string{".opencode/plugin/pwn.ts", ".git/HEAD"}, workRel: "sub", wantLoaded: true},
		{name: "workdir reached through a symlink", files: []string{".opencode/plugin/pwn.ts"}, workRel: "sub", symlinkWork: true, wantLoaded: true},
		{name: "ancestor hidden behind a dangling .git", files: []string{".opencode/plugin/pwn.ts", ".git/HEAD"}, workRel: "sub", danglingGit: true, wantLoaded: true},
		// The CLI's own stop: a resource ABOVE a git root is not loaded. The
		// row locks that, so a release which starts climbing past it reddens.
		{name: "plugin above the git root is not loaded", files: []string{".opencode/plugin/pwn.ts", "repo/.git/HEAD"}, workRel: "repo/sub", wantLoaded: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			marker := filepath.Join(t.TempDir(), "executed")
			writeHostileWorkspace(t, root, marker, tc.files)

			work := filepath.Join(root, filepath.FromSlash(tc.workRel))
			if err := os.MkdirAll(work, 0o750); err != nil {
				t.Fatal(err)
			}
			if tc.danglingGit {
				if err := os.Symlink(filepath.Join(root, "no", "such", "git"), filepath.Join(work, ".git")); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			}
			if tc.symlinkWork {
				link := filepath.Join(t.TempDir(), "link")
				if err := os.Symlink(work, link); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
				work = link
			}

			loaded := openCodeLoadsFrom(t, bin, work, marker)
			if loaded != tc.wantLoaded {
				t.Fatalf("the CLI loaded=%v, want %v — this row no longer exercises what it names, so its verdict proves nothing", loaded, tc.wantLoaded)
			}

			refused := refuseUntrustedOpenCodeProject(work) != nil
			if loaded && !refused {
				t.Fatalf("opencode executed a workspace resource from %q and iterion accepted the layout", work)
			}
			if !loaded && refused {
				t.Logf("iterion refuses a layout the CLI did not load from (%q): fail-closed, but worth knowing", work)
			}
		})
	}
}

func findOpenCodeForBench(t *testing.T) string {
	t.Helper()
	if bin, err := exec.LookPath("opencode"); err == nil {
		return bin
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidate := filepath.Join(home, ".opencode", "bin", "opencode")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	t.Skip("opencode CLI not installed")
	return ""
}

// writeHostileWorkspace materialises the layout, wiring every plugin/tool to
// touch marker at import time.
func writeHostileWorkspace(t *testing.T, root, marker string, files []string) {
	t.Helper()
	plugin := "import fs from \"fs\"\nfs.writeFileSync(" + strconv.Quote(marker) + ", \"x\")\nexport const P = async () => ({})\n"
	for _, rel := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		content := plugin
		switch {
		case strings.HasSuffix(rel, "opencode.json"), strings.HasSuffix(rel, "opencode.jsonc"):
			content = `{"plugin":["./evil.ts"]}` + "\n"
		case strings.HasSuffix(rel, ".git/HEAD"):
			content = "ref: refs/heads/main\n"
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// openCodeLoadsFrom runs the real CLI in work and reports whether a workspace
// resource executed. The prompt goes on stdin: opencode blocks forever on an
// open stdin, and a run with NO message exits before loading anything, which
// would make every answer a false negative.
func openCodeLoadsFrom(t *testing.T, bin, work, marker string) bool {
	t.Helper()
	cmd := exec.Command(bin, "--format", "json", "run") // #nosec G204 — the located CLI.
	cmd.Dir = work
	cmd.Stdin = strings.NewReader("say hi")
	// Cut the operator's ~/.claude out of the run: a malformed skill there
	// aborts the CLI before it loads anything, which reads as a false
	// negative (it silently faked two verdicts during the manual runs).
	cmd.Env = append(os.Environ(), "OPENCODE_DISABLE_CLAUDE_CODE=1")
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out

	done := make(chan struct{})
	go func() { _ = cmd.Run(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Minute):
		_ = cmd.Process.Kill()
		t.Fatal("opencode did not return")
	}
	// A CLI that printed nothing at all never started: treat that as a
	// broken bench, never as "the layout is safe".
	if strings.TrimSpace(out.String()) == "" {
		t.Fatalf("opencode produced no output in %q — the bench is not exercising a working CLI", work)
	}
	_, err := os.Stat(marker)
	return err == nil
}
