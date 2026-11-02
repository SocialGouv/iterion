package sweepwalk_test

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/SocialGouv/iterion/internal/sweepwalk"
)

// The walk is the shared ruleset of every blocking sweep gate, so its rules
// owe proof that they prune what they claim and reach what they claim: the
// gates above it stay green while judging nothing if the walk goes quiet.
// A change to the rules reddens HERE, on a planted tree, instead of
// silently changing what every sweep in the repository judges.
func TestTheWalkReachesOnlyWhatTheRulesJudge(t *testing.T) {
	root := t.TempDir()
	plant := func(rel, src string) {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}

	// The repository's own shape at the root: a .git DIRECTORY directly in
	// the root, pruned by name without swallowing the root itself.
	plant(".git/HEAD", "ref: refs/heads/main\n")
	// Reached: this repository's own sources, test files included — whether
	// a sweep judges a test file is the sweep's policy, not the walk's.
	plant("root.go", "package main\n")
	plant("pkg/inner/inner.go", "package inner\n")
	plant("pkg/inner/inner_test.go", "package inner\n")
	// A plain subdirectory beside everything below is still walked.
	plant("downstream/plain.go", "package downstream\n")
	// The skip list, one canary each.
	plant("vendor/vendor.go", "package vendor\n")
	plant("node_modules/node_modules.go", "package node_modules\n")
	plant("studio/studio.go", "package studio\n")
	plant("testdata/broken.go", "package testdata\n")
	plant(".iterion/runstate.go", "package iterion\n")
	// The nested-checkout rule, both markers: a worktree's .git FILE and a
	// clone's .git DIRECTORY. Where an operator parks them is the rule's
	// business, not a list of homes.
	plant("parked-worktree/.git", "gitdir: /somewhere/else\n")
	plant("parked-worktree/old.go", "package parked\n")
	plant("parked-clone/.git/HEAD", "ref: refs/heads/main\n")
	plant("parked-clone/old.go", "package cloned\n")
	// Not a Go file: never reaches visit.
	plant("notes.md", "# notes\n")

	var seen []string
	sweepwalk.GoFiles(t, root, func(rel, _ string) { seen = append(seen, rel) })
	sort.Strings(seen)

	want := []string{"downstream/plain.go", "pkg/inner/inner.go", "pkg/inner/inner_test.go", "root.go"}
	if len(seen) != len(want) {
		t.Fatalf("the walk reached %v, want exactly %v — a skipped tree leaked in or a judged file went unseen", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("the walk reached %q, want %q", seen[i], want[i])
		}
	}
}

// Skip is the question the walk asks; the name set it answers from is the
// one contract other readers (a sweep naming its exemptions, a guard
// enumerating the set) can hold it to. It must agree with the walk: a name
// Skip accepts is pruned, and the walk test above proves it.
func TestSkipAnswersFromOneSet(t *testing.T) {
	for _, name := range []string{"vendor", "node_modules", ".git", ".iterion", "studio", "testdata"} {
		if !sweepwalk.Skip(name) {
			t.Errorf("Skip(%q) is false, and no sweep judges that tree", name)
		}
	}
	for _, name := range []string{"pkg", "docs", "cmd", "internal"} {
		if sweepwalk.Skip(name) {
			t.Errorf("Skip(%q) is true, and that tree is this repository's own source", name)
		}
	}
}
