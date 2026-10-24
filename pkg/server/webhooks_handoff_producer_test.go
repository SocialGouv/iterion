package server

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

func TestHandoffProducerBinding(t *testing.T) {
	ctx := context.Background()
	s := newBotMetadataTeamServer(t)
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.Store = rs
	s.runs = newTestRunviewService(t, "", runview.WithStore(rs))

	seed := func(t *testing.T, prURL, botID, tenant, title string, readable bool) {
		t.Helper()
		tctx := store.WithTenant(ctx, "t1")
		run, err := rs.CreateRun(tctx, mustRunID(t), "workflow-name-is-not-the-producer", map[string]any{"pr_url": prURL})
		if err != nil {
			t.Fatal(err)
		}
		run.Status = store.RunStatusFinished
		run.BotID = botID
		run.BotSourceTenant = tenant
		run.BotSourceTier = store.BotSourceTierBaked
		if tenant != "" {
			run.BotSourceTier = store.BotSourceTierTeam
		}
		if err := rs.SaveRun(tctx, run); err != nil {
			t.Fatal(err)
		}
		node := "publish_baked"
		if botID == "teamonly-bot" {
			node = "publish_teamonly"
		} else if tenant != "" {
			node = "publish_fork"
		}
		data := map[string]any{
			"total_findings": 1,
			"findings": []any{map[string]any{
				"severity": "high", "category": "correctness", "title": title,
				"file": "a.go", "line": float64(3),
			}},
		}
		if !readable {
			data["findings"] = "not an array"
		}
		if err := rs.WriteArtifact(tctx, &store.Artifact{RunID: run.ID, NodeID: node, Data: data}); err != nil {
			t.Fatal(err)
		}
	}
	lookup := func(prURL, from string) string {
		return s.realWebhookHandoff(ctx, webhooks.Config{ID: "wh-1", TenantID: "t1"},
			bundle.HandoffKindReview, handoffQuery{PRURL: prURL, From: from})
	}

	t.Run("an older custom team producer wins over a newer other producer", func(t *testing.T) {
		const prURL = "https://forge.example/acme/widgets/pull/101"
		seed(t, prURL, "teamonly-bot", "t1", "selected team review", true)
		seed(t, prURL, "reviewer-bot", "", "newer other review", true)
		if got := lookup(prURL, "teamonly-bot"); !strings.Contains(got, "selected team review") {
			t.Errorf("from selected the wrong producer:\n%s", got)
		}
	})

	t.Run("a newer custom team producer wins over an older other producer", func(t *testing.T) {
		const prURL = "https://forge.example/acme/widgets/pull/102"
		seed(t, prURL, "reviewer-bot", "", "older other review", true)
		seed(t, prURL, "teamonly-bot", "t1", "selected team review", true)
		if got := lookup(prURL, "teamonly-bot"); !strings.Contains(got, "selected team review") {
			t.Errorf("from selected the wrong producer:\n%s", got)
		}
	})

	t.Run("an older baked producer wins over a newer other producer", func(t *testing.T) {
		const prURL = "https://forge.example/acme/widgets/pull/106"
		seed(t, prURL, "reviewer-bot", "", "selected baked review", true)
		seed(t, prURL, "teamonly-bot", "t1", "newer other review", true)
		if got := lookup(prURL, "reviewer-bot"); !strings.Contains(got, "selected baked review") {
			t.Errorf("from selected the wrong producer:\n%s", got)
		}
	})

	t.Run("a newer baked producer wins over an older other producer", func(t *testing.T) {
		const prURL = "https://forge.example/acme/widgets/pull/107"
		seed(t, prURL, "teamonly-bot", "t1", "older other review", true)
		seed(t, prURL, "reviewer-bot", "", "selected baked review", true)
		if got := lookup(prURL, "reviewer-bot"); !strings.Contains(got, "selected baked review") {
			t.Errorf("from selected the wrong producer:\n%s", got)
		}
	})

	t.Run("an unreadable selected run continues to an older run of the same bot", func(t *testing.T) {
		const prURL = "https://forge.example/acme/widgets/pull/103"
		seed(t, prURL, "teamonly-bot", "t1", "older selected review", true)
		seed(t, prURL, "reviewer-bot", "", "other producer review", true)
		seed(t, prURL, "teamonly-bot", "t1", "unreadable selected review", false)
		got := lookup(prURL, "teamonly-bot")
		if !strings.Contains(got, "older selected review") {
			t.Errorf("the scan did not continue within the selected producer:\n%s", got)
		}
		if strings.Contains(got, "other producer review") {
			t.Errorf("the scan fell back to another producer:\n%s", got)
		}
	})

	t.Run("no readable selected run yields no other producer", func(t *testing.T) {
		const prURL = "https://forge.example/acme/widgets/pull/104"
		seed(t, prURL, "reviewer-bot", "", "other producer review", true)
		if got := lookup(prURL, "teamonly-bot"); got != "" {
			t.Errorf("from fell back to another producer instead of returning no seed:\n%s", got)
		}
	})

	t.Run("an absent binding preserves the historical latest-of-any-producer behavior", func(t *testing.T) {
		const prURL = "https://forge.example/acme/widgets/pull/105"
		seed(t, prURL, "teamonly-bot", "t1", "older team review", true)
		seed(t, prURL, "reviewer-bot", "", "latest baked review", true)
		if got := lookup(prURL, ""); !strings.Contains(got, "latest baked review") {
			t.Errorf("an absent from changed the historical selection:\n%s", got)
		}
	})
}
