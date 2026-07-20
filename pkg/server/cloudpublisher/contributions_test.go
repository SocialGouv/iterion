package cloudpublisher

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/queue"
)

// A locally installed plugin file must deterministically shadow a same-named
// git-hosted one: one (kind, name) resolves to exactly one payload entry, or
// the runner's mirror order would silently decide the winner.
func TestReplaceContribution_ShadowsInPlace(t *testing.T) {
	files := []queue.ContributionFile{
		{Kind: "skills", Name: "deploy-target.md", Content: []byte("from git source")},
		{Kind: "skills", Name: "other.md", Content: []byte("x")},
	}
	if !replaceContribution(files, "skills", "deploy-target.md", []byte("from local plugin")) {
		t.Fatal("expected an in-place replacement")
	}
	if string(files[0].Content) != "from local plugin" {
		t.Errorf("not shadowed: %q", files[0].Content)
	}
	if len(files) != 2 {
		t.Errorf("replacement must not grow the payload: %d", len(files))
	}
	// A different kind with the same name is a distinct target.
	if replaceContribution(files, "commands", "deploy-target.md", []byte("y")) {
		t.Error("must not match across kinds")
	}
}

// Without a resolver the publisher keeps its previous local-only behaviour, so
// non-cloud and un-migrated deployments are unaffected.
func TestResolveContributionsFor_NilResolverIsLocalOnly(t *testing.T) {
	got, err := resolveContributionsFor(context.Background(), nil, t.TempDir(), "team-1", nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Whatever the host's plugin registry holds, a nil resolver must not fail
	// and must not invent team-scoped entries.
	if got != nil {
		for _, f := range got.Plugin {
			if f.Kind == "" || f.Name == "" {
				t.Errorf("malformed entry: %+v", f)
			}
		}
	}
}
