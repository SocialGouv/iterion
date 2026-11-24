package botregistry

// AnyRootPresent and discovery's silent skip must agree: a caller that reads
// "no entry" as a FACT about what serves (pkg/server's override-shadow check)
// needs the empty walk of a workspace with no bundle told apart from the empty
// walk of roots that are not there at all. Discovery itself cannot say so — it
// skips a missing root on purpose, so optimistic default paths stay free.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAnEmptyWalkAndAnAbsentRootAreDifferentFacts(t *testing.T) {
	root := t.TempDir()
	absent := []string{filepath.Join(root, "absent"), filepath.Join(root, "gone", "deeper")}

	// Absent roots: the walk succeeds with nothing — silently, which is the
	// shape a dropped bots path takes — and AnyRootPresent is the only thing
	// that can say the walk read no root.
	entries, diags, err := ListWithDiagnostics(ListOptions{Paths: absent})
	if err != nil {
		t.Fatalf("premise: discovery skips a missing root silently; got %v", err)
	}
	if len(entries) != 0 || len(diags) != 0 {
		t.Fatalf("premise: absent roots yield nothing; got %d entries %d diagnostics", len(entries), len(diags))
	}
	if AnyRootPresent(absent) {
		t.Error("AnyRootPresent must be false when not one root is on disk — a verdict built on this walk would read 'nothing serves' from a catalog it never opened")
	}

	// A root that IS there and holds no bundle: the same empty walk, the
	// opposite fact. This is the escape hatch for a deployment that serves
	// stored bundles only, so it must stay distinguishable.
	empty := filepath.Join(root, "empty")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	entries, _, err = ListWithDiagnostics(ListOptions{Paths: append([]string{empty}, absent...)})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("premise: an empty root holds no bot; got %d entries", len(entries))
	}
	if !AnyRootPresent(append([]string{empty}, absent...)) {
		t.Error("an existing root beside absent ones IS a catalog, and an empty one is a verdict")
	}

	// And a root with a bot in it: both sides say present.
	writeFile(t, filepath.Join(empty, "a.bot"), "## ---\n## name: alpha\n## ---\nagent y:\n  model: \"test\"\n")
	entries, _, err = ListWithDiagnostics(ListOptions{Paths: []string{empty}})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !AnyRootPresent([]string{empty}) {
		t.Errorf("a populated root must be present on both sides; got %d entries present=%v", len(entries), AnyRootPresent([]string{empty}))
	}

	// No root configured at all is the same unknown, not an empty catalog.
	if AnyRootPresent(nil) {
		t.Error("no configured root is no catalog")
	}
}
