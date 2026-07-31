package server

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
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

// reviewSpec is a FIXTURE producer declaration — deliberately not a copy of any
// shipped bot's. The renderer is handed the node layout by whichever bot
// declared `produces: kind: review`; naming the nodes here proves it reads them
// from the declaration rather than knowing a particular bot's graph.
var reviewSpec = bundle.ProducedArtifact{
	Kind:          bundle.HandoffKindReview,
	Node:          "merged_findings",
	FallbackNodes: []string{"per_family_findings"},
	AnchorNode:    "precheck",
}

// TestRenderPriorReview pins what the hand-off actually carries.
//
// The point of seeding a fixer with a review is to save it a round. A digest
// that drops the ready-made `replacement`, the confidence, or the cross-family
// confirmation costs exactly that round back: the fixer re-derives a patch that
// already exists and re-verifies every finding at equal weight. So the fields
// are asserted individually — "it rendered something" is not the contract.
func TestRenderPriorReview(t *testing.T) {
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const prURL = "https://github.com/acme/widgets/pull/7"

	newRun := func(t *testing.T, pr string) *store.Run {
		t.Helper()
		run, err := rs.CreateRun(ctx, mustRunID(t), "review-pr", map[string]any{"pr_url": pr})
		if err != nil {
			t.Fatal(err)
		}
		return run
	}
	write := func(t *testing.T, runID, node string, data map[string]any) {
		t.Helper()
		if err := rs.WriteArtifact(ctx, &store.Artifact{RunID: runID, NodeID: node, Data: data}); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("carries every field the fixer needs to skip a round", func(t *testing.T) {
		run := newRun(t, prURL)
		write(t, run.ID, reviewSpec.Node, map[string]any{
			"total_findings": 2,
			"questions":      "Does EditorTabHost still open the file?\n",
			"findings": []any{
				map[string]any{
					"severity": "high", "category": "security", "title": "SQL injection",
					"file": "db.go", "line": float64(42), "line_end": float64(44),
					"detail": "user input concatenated into a query", "suggestion": "use a placeholder",
					"replacement": "db.Query(`SELECT * FROM t WHERE id = ?`, id)",
					"confidence":  "high", "reviewers": "both",
				},
				map[string]any{"severity": "low", "category": "style", "title": "unused var"},
			},
		})

		got := renderPriorReview(ctx, rs, run.ID, reviewSpec, priorReviewQuery{PRURL: prURL})
		for name, want := range map[string]string{
			"the anchor span":       "db.go:42-44",
			"the severity/category": "high/security",
			"the detail":            "user input concatenated",
			"the fix sketch":        "use a placeholder",
			"the ready replacement": "db.Query(",
			"the confidence":        "confidence: high",
			"cross-confirmation":    "cross-confirmed by both model families",
			"the second finding":    "unused var",
			"the open questions":    "EditorTabHost",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("digest drops %s (%q):\n%s", name, want, got)
			}
		}
		// The id is the handle the ledger and the operator's arbitration use.
		if id := findingID("db.go", "SQL injection"); !strings.Contains(got, id) {
			t.Errorf("digest missing the stable finding id %s:\n%s", id, got)
		}
	})

	t.Run("empty findings still seed a verdict", func(t *testing.T) {
		run := newRun(t, prURL)
		write(t, run.ID, reviewSpec.Node, map[string]any{"findings": []any{}, "total_findings": 0})
		if e := renderPriorReview(ctx, rs, run.ID, reviewSpec, priorReviewQuery{PRURL: prURL}); !strings.Contains(e, "no findings") {
			t.Errorf("empty-findings render should note no findings, got %q", e)
		}
	})

	// Revi's own publish step recovers from a prose `findings` by unioning the
	// two reviewers' raw arrays. Without the same recovery here, a review that
	// posted N findings onto the PR hands the fixer an empty seed.
	t.Run("recovers the reviewers' raw arrays when the merge degraded", func(t *testing.T) {
		run := newRun(t, prURL)
		write(t, run.ID, reviewSpec.Node, map[string]any{
			"findings": "See structured findings array.", "total_findings": 2,
		})
		write(t, run.ID, reviewSpec.FallbackNodes[0], map[string]any{
			"claude_findings": []any{map[string]any{"title": "race on cache", "file": "c.go", "line": float64(9), "severity": "high"}},
			"gpt_findings":    []any{map[string]any{"title": "race on cache", "file": "c.go", "line": float64(9), "severity": "high"}},
		})
		got := renderPriorReview(ctx, rs, run.ID, reviewSpec, priorReviewQuery{PRURL: prURL})
		if !strings.Contains(got, "race on cache") {
			t.Errorf("degraded merge lost every finding:\n%s", got)
		}
		if strings.Count(got, "race on cache") != 1 {
			t.Errorf("the two families' identical finding must be de-duplicated by anchor:\n%s", got)
		}
		if !strings.Contains(got, "unreadable") {
			t.Errorf("a degraded set must say so — the threshold and cap were not applied:\n%s", got)
		}
	})

	// A review anchors to the tree it read. Handing it over as current makes the
	// fixer "fix" what is already fixed.
	t.Run("labels a review whose head has moved", func(t *testing.T) {
		run := newRun(t, prURL)
		write(t, run.ID, reviewSpec.Node, map[string]any{
			"total_findings": 1,
			"findings":       []any{map[string]any{"title": "t", "file": "a.go", "line": float64(1), "severity": "low"}},
		})
		write(t, run.ID, reviewSpec.AnchorNode, map[string]any{"reviewed_sha": "aaaaaaaaaaaa1111"})

		moved := renderPriorReview(ctx, rs, run.ID, reviewSpec, priorReviewQuery{PRURL: prURL, HeadSHA: "bbbbbbbbbbbb2222"})
		if !strings.Contains(moved, "the branch moved after this review") {
			t.Errorf("a stale review must be labelled stale:\n%s", moved)
		}
		if !strings.Contains(moved, "aaaaaaaaaaaa") || !strings.Contains(moved, "bbbbbbbbbbbb") {
			t.Errorf("the digest must name both revisions so the fixer can diff them:\n%s", moved)
		}
		same := renderPriorReview(ctx, rs, run.ID, reviewSpec, priorReviewQuery{PRURL: prURL, HeadSHA: "aaaaaaaaaaaa1111"})
		if strings.Contains(same, "branch moved") {
			t.Errorf("an up-to-date review must not be labelled stale:\n%s", same)
		}
	})

	// The artifact is the truth, not the run status: a review still in flight has
	// written no findings yet, and short-circuiting on it would strand a complete
	// older review one step down the list.
	t.Run("a review with nothing readable renders empty", func(t *testing.T) {
		run := newRun(t, prURL)
		if got := renderPriorReview(ctx, rs, run.ID, reviewSpec, priorReviewQuery{PRURL: prURL}); got != "" {
			t.Errorf("a run with no artifacts must render empty so the scan continues, got %q", got)
		}
	})
}

// TestFindingIDIsStableAcrossALineShift pins the one property the id exists for.
// Between a review and the fix that answers it, code above a finding moves — so
// an id keyed on the line would change exactly when the round-trip needs it to
// hold, and every ledger entry and pushback would dangle.
func TestFindingIDIsStableAcrossALineShift(t *testing.T) {
	base := findingID("pkg/db.go", "SQL injection in the user lookup")
	for _, variant := range []string{
		"SQL injection in the user lookup",
		"  SQL injection in the user lookup  ",
		"SQL   injection in the\tuser lookup",
		"sql injection in the USER lookup",
	} {
		if got := findingID("pkg/db.go", variant); got != base {
			t.Errorf("findingID(%q) = %s, want %s — normalization must absorb it", variant, got, base)
		}
	}
	if findingID("pkg/other.go", "SQL injection in the user lookup") == base {
		t.Error("a different file must yield a different id")
	}
	if findingID("pkg/db.go", "unused variable") == base {
		t.Error("a different title must yield a different id")
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

// TestRenderReviewLedger pins the REPLY half of the hand-off — the half that
// makes the pair converge.
//
// Without it a reviewer re-raises, every pass, a finding the fixer already
// contested: the oscillating relay ADR-058 removed from the catalog. So a
// refusal must cross WITH its argument and with an explicit bar for re-raising
// — and must not read as permission to drop the finding, which would let a
// fixer clear any review by refusing everything.
func TestRenderReviewLedger(t *testing.T) {
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	spec := bundle.ProducedArtifact{Kind: bundle.HandoffKindReviewLedger, Node: "campaign"}

	run, err := rs.CreateRun(ctx, mustRunID(t), "some-fixer", map[string]any{"pr_url": "https://f/o/r/pull/1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := rs.WriteArtifact(ctx, &store.Artifact{RunID: run.ID, NodeID: "campaign", Data: map[string]any{
		"finding_ledger": []any{
			map[string]any{"id": "R1111", "status": "fixed", "commit": "abcdef1234567890"},
			map[string]any{"id": "R2222", "status": "refused", "note": "--skill on a directory loads only SKILL.md and returns"},
			map[string]any{"id": "R3333", "status": "deferred", "note": "needs a schema migration"},
		},
	}}); err != nil {
		t.Fatal(err)
	}

	got := renderReviewLedger(ctx, rs, run.ID, spec)
	for name, want := range map[string]string{
		"the refused id":            "R2222",
		"the refusal's ARGUMENT":    "loads only SKILL.md",
		"the bar for re-raising":    "NEW evidence",
		"the fixed id":              "R1111",
		"the commit that fixed it":  "abcdef123456",
		"the instruction to verify": "verify against the current diff",
		"the deferred id":           "R3333",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("ledger drops %s (%q):\n%s", name, want, got)
		}
	}
	// A refusal is an argument to answer, never a licence to drop the finding:
	// a fixer could otherwise clear any review by contesting every finding.
	if !strings.Contains(got, "still disagree") {
		t.Errorf("the ledger must leave the reviewer free to re-raise against a wrong argument:\n%s", got)
	}

	t.Run("a ledger with no entries renders nothing", func(t *testing.T) {
		empty, err := rs.CreateRun(ctx, mustRunID(t), "some-fixer", map[string]any{"pr_url": "https://f/o/r/pull/2"})
		if err != nil {
			t.Fatal(err)
		}
		if err := rs.WriteArtifact(ctx, &store.Artifact{RunID: empty.ID, NodeID: "campaign", Data: map[string]any{
			"finding_ledger": []any{}, "summary": "nothing to answer",
		}}); err != nil {
			t.Fatal(err)
		}
		if got := renderReviewLedger(ctx, rs, empty.ID, spec); got != "" {
			t.Errorf("an empty ledger must render nothing rather than an empty preamble, got %q", got)
		}
	})
}

// TestBoardCardCarriesTheDeclaredHandoffVar pins the board lane, which is the
// one a `/command` bot actually takes in cloud.
//
// A board-mode command with a dispatcher active never reaches the launch tail:
// the CARD is the launch, and the cloud coordinator launches from `BotArgs`
// ONLY. So a var stamped anywhere downstream of the card is dropped — with no
// error, and with the bot quietly falling back to its DSL default. That has bit
// this exact path before (`ensureBoardCard` rebuilding BotArgs from scratch,
// fixed in bc2918024), so the carry is asserted here rather than assumed.
func TestBoardCardCarriesTheDeclaredHandoffVar(t *testing.T) {
	s := newWebhookTestServer(t)
	s.cfg.WorkDir = writeConsumerBotFixture(t, "fixer-bot", "prior_review")

	if got := s.handoffConsumersFor("fixer-bot"); len(got) != 1 || got[0].Var != "prior_review" {
		t.Fatalf("the fixture bot's declaration did not load: %+v", got)
	}
	// The carry set is derived from the declaration, so a bot that declares a
	// DIFFERENT var must have that one carried — a hardcoded list would pass
	// the assertion above and still drop this.
	s2 := newWebhookTestServer(t)
	s2.cfg.WorkDir = writeConsumerBotFixture(t, "other-fixer", "upstream_notes")
	got := s2.handoffConsumersFor("other-fixer")
	if len(got) != 1 || got[0].Var != "upstream_notes" {
		t.Fatalf("a bot consuming into its own var name must be honoured, got %+v", got)
	}
}
