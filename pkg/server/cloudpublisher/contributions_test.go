package cloudpublisher

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
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

// queueBotBundleRef is the wire conversion of a launch-resolved stored-bot
// ref — the full-bundle successor of the old appendTenantBotSkills partial
// transport.
func TestQueueBotBundleRef(t *testing.T) {
	if queueBotBundleRef(nil) != nil {
		t.Error("nil ref must stay nil on the wire")
	}
	got := queueBotBundleRef(&runview.BotBundleRef{TenantID: "platform:", Slug: "review-pr", Version: 7})
	if got == nil || got.TenantID != "platform:" || got.Slug != "review-pr" || got.Version != 7 {
		t.Fatalf("wire ref = %+v", got)
	}
	ref := &runview.BotBundleRef{Slug: "catalog", Snapshot: []byte(`{"root":"catalog"}`), SnapshotDigest: "digest"}
	got = queueBotBundleRef(ref)
	if string(got.Snapshot) != string(ref.Snapshot) || got.SnapshotDigest != ref.SnapshotDigest {
		t.Fatal("snapshot lost in wire conversion")
	}
	ref.Snapshot[0] = 'x'
	if got.Snapshot[0] != '{' {
		t.Fatal("wire snapshot aliases mutable launch bytes")
	}
}

// Without a resolver the publisher keeps its previous local-only behaviour, so
// non-cloud and un-migrated deployments are unaffected.
func TestResolveContributionsFor_NilResolverIsLocalOnly(t *testing.T) {
	got, err := resolveContributionsFor(context.Background(), nil, t.TempDir(), "team-1", "run-x", nil, nil)
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

// #1500 R6: an empty resolution is a STATEMENT ("nothing enabled on the
// launching instance"), never a lost field. The payload ships non-nil-empty
// so the runner can tell it from an ABSENT one — nil on the wire is reserved
// for "the field did not arrive", which the runner treats as an unverifiable
// declaration and skips the orphan pruner for that pass. Shipping nil here
// would overload the two and make every empty cloud resume a silent
// prune-veto (or worse, after a future regression, an unverified prune).
//
// Mutation: restore the `return nil, nil` early return for an empty
// resolution and this test reddens.
func TestResolveContributionsFor_EmptyResolutionShipsNonNil(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir()) // nothing installed, nothing enabled
	got, err := resolveContributionsFor(context.Background(), nil, t.TempDir(), "", "run-empty", nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("an empty resolution must ship a non-nil empty payload — nil is reserved for a lost field")
	}
	if len(got.Plugin) != 0 || len(got.Library) != 0 {
		t.Errorf("expected an empty payload, got %d plugin file(s), %d library skill(s)", len(got.Plugin), len(got.Library))
	}
}

// #1500 R6 follow-up: a broken plugin.yaml makes the registry skip the plugin
// SILENTLY — it never enters Enabled(), so its files never reach the payload
// while it is still enabled. The publisher must confess the amputation on the
// wire (Degraded) or the pod reads the truncated payload as the whole
// declaration and prunes the broken plugin's launch-pass mirrors on the first
// resume — the cloud twin of the local path's LoadSkips veto.
//
// Mutation: drop the LoadSkips→degraded assignment (or the regErr one) in
// resolveContributionsFor and this test reddens.
func TestResolveContributionsFor_BrokenPluginYamlFlagsDegraded(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)
	broken := filepath.Join(home, "plugins", "broken-yaml")
	if err := os.MkdirAll(broken, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, "plugin.yaml"),
		[]byte("name: broken-yaml\nversion: 1.0.0\ndescription: unquoted: colon breaks yaml\n"),
		0o644); err != nil {
		t.Fatal(err)
	}
	healthy := filepath.Join(home, "plugins", "ok-pack")
	if err := os.MkdirAll(filepath.Join(healthy, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(healthy, "plugin.yaml"),
		[]byte("name: ok-pack\nversion: 1.0.0\ndescription: fine\ndefault_enabled: true\ncontributes:\n  skills:\n    - skills/ok-skill.md\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(healthy, "skills", "ok-skill.md"), []byte("OK\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := resolveContributionsFor(context.Background(), nil, t.TempDir(), "", "run-degraded", nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.Degraded {
		t.Fatal("a payload whose enumeration skipped a plugin must ship Degraded=true")
	}
	if len(got.Plugin) != 1 || got.Plugin[0].Name != "ok-skill.md" {
		t.Errorf("the healthy plugin's files must still ride the payload, got %+v", got.Plugin)
	}
}
