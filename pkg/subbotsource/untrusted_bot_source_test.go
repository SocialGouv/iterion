package subbotsource

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestResolve_RefusesABotSourceOnAnUntrustedWorkspace.
//
// `bot://` resolves a workflow out of `<workspace>/.botz/` and verifies it
// against `<workspace>/bots.lock`. Both files come from the checkout, so on a
// workspace the tenant did not write the integrity check compares the fork's
// bundle against the fork's own lock — and agrees. A `subbot:` node would then
// execute an arbitrary workflow the fork shipped, with the parent's
// credentials, under a manifest dependency name the consumer declared once.
//
// Refused with a typed reason rather than left to fail as "not materialized":
// an author who needs a dependency bot on a fork lane has to see WHY.
func TestResolve_RefusesABotSourceOnAnUntrustedWorkspace(t *testing.T) {
	t.Parallel()
	root, consumerPath, installed := botResolutionFixture(t)
	hash, err := bundle.ContentHashDir(installed)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(root, "bots.lock"), fmt.Sprintf(`version: 1
dependencies:
  shared-planner:
    source: https://example.test/shared-planner.git
    ref: v1.0.0
    bundle_sha256: %s
`, hash))

	const ref = "bot://shared-planner/hierarchy-feature-author"

	// The witness: the SAME fixture resolves for a workspace the tenant wrote,
	// so the refusal below is about provenance and not about a broken lock.
	if _, err := NewResolver(ResolverOptions{WorkDir: root}).Resolve(context.Background(), consumerPath, ref); err != nil {
		t.Fatalf("a trusted workspace failed to resolve %s (%v) — the fixture cannot witness a refusal that never happens", ref, err)
	}

	for _, trust := range []store.RunTrust{store.RunTrustFork, store.RunTrust("vendored")} {
		_, err := NewResolver(ResolverOptions{WorkDir: root, Trust: trust}).Resolve(context.Background(), consumerPath, ref)
		if err == nil {
			t.Errorf("trust=%q resolved %s out of the checkout's own .botz/ and bots.lock", trust, ref)
			continue
		}
		if !strings.Contains(err.Error(), "untrusted") {
			t.Errorf("trust=%q refused with %q, which does not name the reason", trust, err)
		}
	}
}
