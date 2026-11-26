package runtimeseams

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/sweepwalk"
)

// TestEverySubbotRunnerHandsTheChildItsWorktreeFacts: the in-place rule reads
// parentNodeID and the node's `isolated:` assertion, and only the runner that
// turns a SubbotRequest into a child engine can supply them. A runner that
// passes the request's parent RUN id and forgets its NODE id would silently
// hand its local children a worktree of their own again; one that forgets the
// isolation would lend the parent's tree to children the author certified
// write elsewhere, and queue their parallel fan-out on that one tree. The
// class is discovered from the source, not listed, so a new runner joins it
// by existing.
func TestEverySubbotRunnerHandsTheChildItsWorktreeFacts(t *testing.T) {
	const builds = "runtime.WithParentRunID(req.ParentRunID)"
	owed := map[string]string{
		"runtime.WithParentNodeID(req.NodeID)":    "its children would take a worktree of their own",
		"runtime.WithIsolatedChild(req.Isolated)": "an `isolated:` child would work in the parent's tree, and its siblings would queue on that tree's resource gate",
	}
	runners := map[string][]string{}
	// Every SubbotRunner implementation lives under pkg/.
	pkgDir := filepath.Join(sweepwalk.ModuleRoot(t), "pkg")
	err := filepath.WalkDir(pkgDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if !strings.Contains(string(src), builds) {
			return nil
		}
		missing := []string{}
		for marker := range owed {
			if !strings.Contains(string(src), marker) {
				missing = append(missing, marker)
			}
		}
		runners[path] = missing
		return nil
	})
	if err != nil {
		t.Fatalf("walk pkg/: %v", err)
	}
	// Four launchers run a child .bot: the CLI, the dispatcher, the studio
	// and the cloud runner. Fewer means the discovery stopped working, and a
	// test that finds nothing proves nothing.
	if len(runners) < 4 {
		t.Fatalf("found %d subbot runners (%v), want at least the four launchers — the marker %q no longer discovers them", len(runners), runners, builds)
	}
	for path, missing := range runners {
		for _, marker := range missing {
			t.Errorf("%s builds a child engine from a SubbotRequest without %s: %s", path, marker, owed[marker])
		}
	}
}
