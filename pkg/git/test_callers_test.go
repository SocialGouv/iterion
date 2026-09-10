package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEveryTestGitCallerDisablesAutoMaintenance is the _test.go half of
// TestEveryGitCallerSanitizesEnv, which deliberately skips test files.
//
// It has to exist for the same reason that one does: reviewing it by hand does
// not work. The sweep behind issue #828 found 33 test files building a git
// subprocess, four of them with a helper used dozens of times, and every one of
// those helpers is the identical merge-queue ejector on CI's git — a test
// builds its repository under t.TempDir(), runs a writing command
// (commit/merge/rebase/am/fetch), and Go removes the directory the moment the
// test returns, while `git maintenance run --auto` is still detached inside it
// writing under .git/objects.
//
// The chokepoint is internal/gittest: gittest.Run / gittest.Try / gittest.Cmd
// bake the config in, so the next helper cannot forget it. A caller that
// genuinely has to assemble its own argv — pkg/git's own tests, which cannot
// import internal/gittest without an import cycle — satisfies the property by
// passing through NoAutoMaintenance directly.
func TestEveryTestGitCallerDisablesAutoMaintenance(t *testing.T) {
	root := filepath.Join("..", "..")
	skipNames := map[string]bool{"vendor": true, "studio": true, "node_modules": true, "testdata": true}

	var offenders []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// Same exclusions as the production sweep: hidden directories hold
			// scratch clones of other people's code (.local, .works, .repos).
			name := info.Name()
			// internal/gittest is the chokepoint itself: its Cmd applies the
			// config. Its tests deliberately use bare read-only Git commands
			// to distinguish Cmd's flags from InitRepo's persisted config.
			if skipNames[name] || (strings.HasPrefix(name, ".") && path != root) ||
				path == filepath.Join("..", "..", "internal", "gittest") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, rerr := os.ReadFile(path) // #nosec G304 -- walking this repository's own tree
		if rerr != nil {
			return rerr
		}
		body := string(src)
		for _, loc := range gitExec.FindAllStringIndex(body, -1) {
			// A call quoted inside a comment is prose, not a call site.
			lineStart := strings.LastIndex(body[:loc[0]], "\n") + 1
			if strings.Contains(body[lineStart:loc[0]], "//") {
				continue
			}
			// Bounded by the NEXT call site so one site's config cannot vouch
			// for its neighbour's — the failure mode the production sweep
			// measured with a fixed window.
			end := min(loc[0]+1600, len(body))
			if next := gitExec.FindStringIndex(body[loc[1]:]); next != nil {
				end = min(end, loc[1]+next[0])
			}
			if strings.Contains(body[loc[0]:end], "NoAutoMaintenance(") {
				continue
			}
			line := 1 + strings.Count(body[:loc[0]], "\n")
			offenders = append(offenders, filepath.ToSlash(path)+":"+itoa(line)+"  "+strings.TrimSpace(body[loc[0]:min(loc[1]+40, len(body))]))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(offenders) > 0 {
		t.Errorf("git subprocess(es) built in a test without refusing auto-maintenance — since git 2.48 a writing command detaches `git maintenance run --auto`, which keeps writing under .git/objects after the command returned and races t.TempDir()'s removal.\nRoute them through internal/gittest (gittest.Run / gittest.Try / gittest.Cmd), or pass git.NoAutoMaintenance(args...) when the package cannot import it:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}
