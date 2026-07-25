package cloudpublisher

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
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

// A team-authored bot's bundle skills ride the Contributions channel so they
// reach a runner pod (which can't resolve a tenant bundle off its baked
// BotsPaths). Flat skills/<name>.md become library skills; main.bot, manifest
// and nested skill dirs are excluded.
func TestAppendTenantBotSkills(t *testing.T) {
	mem := botsource.NewMemoryStore()
	ctx := store.WithTenant(context.Background(), "team-1")
	if _, err := mem.Create(ctx, botsource.BotSource{
		TenantID: "team-1",
		Slug:     "reviewer",
		Files: map[string]string{
			botsource.MainBotFile: "workflow main:\n  a -> done\n",
			"manifest.yaml":       "name: reviewer\n",
			"skills/deep.md":      "# deep skill",
			"skills/wide.md":      "# wide skill",
			"skills/nested/x.md":  "# nested — excluded",
		},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	p := &Publisher{botSources: mem}

	got := p.appendTenantBotSkills(context.Background(), nil, "team-1", "reviewer")
	if got == nil {
		t.Fatal("expected contributions with library skills")
	}
	names := map[string]string{}
	for _, s := range got.Library {
		names[s.Name] = string(s.Content)
	}
	if len(names) != 2 || names["deep"] != "# deep skill" || names["wide"] != "# wide skill" {
		t.Fatalf("wrong library skills: %v", names)
	}

	// A catalog/unknown bot (no store entry) adds nothing.
	if p.appendTenantBotSkills(context.Background(), nil, "team-1", "not-a-tenant-bot") != nil {
		t.Error("unknown bot must not synthesize contributions")
	}
	// No store → passthrough.
	if (&Publisher{}).appendTenantBotSkills(context.Background(), nil, "team-1", "reviewer") != nil {
		t.Error("nil store must be a passthrough")
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
