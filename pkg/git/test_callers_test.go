package git

import (
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
	// internal/gittest is the chokepoint itself: its Cmd applies the config.
	// Its tests deliberately use bare read-only Git commands to distinguish
	// Cmd's flags from InitRepo's persisted config.
	chokepoint := filepath.Join(root, "internal", "gittest") + string(filepath.Separator)
	files := gitSitesInTree(t, root)

	var offenders []string
	resolved := 0
	for _, path := range sortedGitFiles(files) {
		if !strings.HasSuffix(path, "_test.go") || strings.HasPrefix(path, chokepoint) {
			continue
		}
		f := files[path]
		for i, site := range f.sites {
			if site.resolved {
				resolved++
			}
			// Bounded by the NEXT call site so one site's config cannot vouch
			// for its neighbour's — the failure mode the production sweep
			// measured with a fixed window.
			end := min(site.start+1600, len(f.body))
			if i+1 < len(f.sites) {
				end = min(end, f.sites[i+1].start)
			}
			if strings.Contains(f.body[site.start:end], "NoAutoMaintenance(") {
				continue
			}
			offenders = append(offenders, siteLabel(filepath.ToSlash(path), f.body, site))
		}
	}
	requireResolvedSites(t, resolved, "the ambient, pluginsource and plugin-sources route tests")
	if len(offenders) > 0 {
		t.Errorf("git subprocess(es) built in a test without refusing auto-maintenance — since git 2.48 a writing command detaches `git maintenance run --auto`, which keeps writing under .git/objects after the command returned and races t.TempDir()'s removal.\nRoute them through internal/gittest (gittest.Run / gittest.Try / gittest.Cmd), or pass git.NoAutoMaintenance(args...) when the package cannot import it:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}
