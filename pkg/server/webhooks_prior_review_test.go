package server

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// TestRealIterionBotAuthor pins the iterion-forge-bot detection (req 5): it
// matches ONLY the bot identity derived from the tenant's provisioned forge
// connection — a GitHub App's `<app_slug>[bot]` or (GitLab only) the connected
// account — never a generic `[bot]` that would catch Dependabot/Renovate.
func TestRealIterionBotAuthor(t *testing.T) {
	conns := forge.NewMemoryConnectionStore()
	// GitHub App connection: bot login = iterion-forge-1234[bot].
	if err := conns.Create(context.Background(), forge.Connection{
		ID: "c-gh", TenantID: "t1", Provider: forge.ProviderGitHub, AppSlug: "iterion-forge-1234",
	}); err != nil {
		t.Fatal(err)
	}
	// GitLab PAT connection: iterion authors MRs as this bot account.
	if err := conns.Create(context.Background(), forge.Connection{
		ID: "c-gl", TenantID: "t1", Provider: forge.ProviderGitLab, AccountLogin: "iterion-bot",
	}); err != nil {
		t.Fatal(err)
	}
	s := &Server{forgeConnections: conns}

	ghCfg := webhooks.Config{TenantID: "t1", Provider: webhooks.ProviderGitHub, ProvisionedBy: "forge:c-gh"}
	glCfg := webhooks.Config{TenantID: "t1", Provider: webhooks.ProviderGitLab, ProvisionedBy: "forge:c-gl"}
	handCfg := webhooks.Config{TenantID: "t1", Provider: webhooks.ProviderGitHub} // operator-created, no provisioning

	cases := []struct {
		name  string
		cfg   webhooks.Config
		login string
		want  bool
	}{
		{"github app bot matches", ghCfg, "iterion-forge-1234[bot]", true},
		{"github app bot case-insensitive", ghCfg, "Iterion-Forge-1234[bot]", true},
		{"github dependabot NOT matched", ghCfg, "dependabot[bot]", false},
		{"github human NOT matched", ghCfg, "alice", false},
		{"gitlab bot account matches", glCfg, "iterion-bot", true},
		{"gitlab other account NOT matched", glCfg, "alice", false},
		{"hand-created webhook never matches (no connection)", handCfg, "iterion-forge-1234[bot]", false},
		{"empty login never matches", ghCfg, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := s.realIterionBotAuthor(context.Background(), c.cfg, c.login); got != c.want {
				t.Errorf("realIterionBotAuthor(%q) = %v, want %v", c.login, got, c.want)
			}
		})
	}
}

// TestRenderPriorReview checks that Revi's merged findings render into a bounded
// markdown digest Billy can consume (req 4), including the empty-findings case.
func TestRenderPriorReview(t *testing.T) {
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// A review run with two findings.
	run, err := rs.CreateRun(ctx, mustRunID(t), "review-pr", map[string]any{"pr_url": "https://github.com/acme/widgets/pull/7"})
	if err != nil {
		t.Fatal(err)
	}
	if err := rs.WriteArtifact(ctx, &store.Artifact{
		RunID:  run.ID,
		NodeID: reviewConvergeNode,
		Data: map[string]any{
			"total_findings": 2,
			"findings": []any{
				map[string]any{"severity": "high", "category": "security", "title": "SQL injection", "file": "db.go", "line": float64(42), "detail": "user input concatenated into a query"},
				map[string]any{"severity": "low", "category": "style", "title": "unused var"},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	got := renderPriorReview(ctx, rs, run.ID, "https://github.com/acme/widgets/pull/7")
	for _, want := range []string{"Prior review", "SQL injection", "db.go:42", "high/security", "unused var"} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered prior review missing %q:\n%s", want, got)
		}
	}

	// Empty findings → a "no findings" seed, not an empty string.
	empty, err := rs.CreateRun(ctx, mustRunID(t), "review-pr", map[string]any{"pr_url": "https://github.com/acme/widgets/pull/8"})
	if err != nil {
		t.Fatal(err)
	}
	if err := rs.WriteArtifact(ctx, &store.Artifact{
		RunID: empty.ID, NodeID: reviewConvergeNode, Data: map[string]any{"findings": []any{}},
	}); err != nil {
		t.Fatal(err)
	}
	if e := renderPriorReview(ctx, rs, empty.ID, "https://github.com/acme/widgets/pull/8"); !strings.Contains(e, "no findings") {
		t.Errorf("empty-findings render should note no findings, got %q", e)
	}
}

// TestRunTargetsPR pins the PR-match predicate used to select the review run.
func TestRunTargetsPR(t *testing.T) {
	run := &store.Run{Inputs: map[string]any{"pr_url": "https://github.com/acme/widgets/pull/7"}}
	if !runTargetsPR(run, "https://github.com/acme/widgets/pull/7") {
		t.Error("should match the exact pr_url")
	}
	if runTargetsPR(run, "https://github.com/acme/widgets/pull/9") {
		t.Error("must not match a different PR")
	}
	if runTargetsPR(&store.Run{Inputs: map[string]any{}}, "x") {
		t.Error("no pr_url input → no match")
	}
}

func mustRunID(t *testing.T) string {
	t.Helper()
	id, err := store.GenerateRunID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
