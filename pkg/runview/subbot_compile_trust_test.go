package runview

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// A sub-bot child is compiled for its parent's trust, read off the parent's
// document (SubbotParentTrust); a parent that cannot be read is refused,
// typed, the rule CreateChildRun applies.
func TestSubbotParentTrust_ReadsTheParentsDocument(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	seedSourceRun(t, st, "parent-trusted", store.RunTrustDefault)
	seedSourceRun(t, st, "parent-fork", store.RunTrustFork)
	for id, want := range map[string]store.RunTrust{"parent-trusted": store.RunTrustDefault, "parent-fork": store.RunTrustFork} {
		got, err := SubbotParentTrust(context.Background(), st, id)
		if err != nil || got != want {
			t.Errorf("SubbotParentTrust(%s) = (%q, %v), want %q", id, got, err, want)
		}
	}
	if _, err := SubbotParentTrust(context.Background(), st, "no-such-parent"); !errors.Is(err, store.ErrChildParentUnreadable) || !errors.Is(err, store.ErrRunNotFound) {
		t.Errorf("SubbotParentTrust(missing) = %v, want %v wrapping %v", err, store.ErrChildParentUnreadable, store.ErrRunNotFound)
	}
}

func TestCompileSubbotWorkflow_ResolvesTheCatalogForTheTrustItIsGiven(t *testing.T) {
	path := filepath.Join(t.TempDir(), "child.bot")
	if err := os.WriteFile(path, []byte("\nworkflow child:\n  entry: done\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, trust := range []store.RunTrust{store.RunTrustDefault, store.RunTrustFork} {
		wf, _, _, err := CompileSubbotWorkflow(path, nil, t.TempDir(), trust)
		if err != nil {
			t.Fatalf("CompileSubbotWorkflow(%q): %v", trust, err)
		}
		if got, ok := preparedTrust(wf); !ok || store.RunTrust(got) != trust {
			t.Errorf("CompileSubbotWorkflow(%q) resolved the catalog for %q (recorded=%v)", trust, got, ok)
		}
	}
}
