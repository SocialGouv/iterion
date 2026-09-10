package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// R33cf5e — the webhook lane launches a team's fork (#871), so the METADATA
// it reads on the same delivery has to come from the same row. Before the
// team tier reached this lane, launch and metadata both resolved
// platform-over-baked and could not disagree; making the launch tenant-aware
// is what opened the gap, so it closes here rather than in the general
// metadata sweep (#946).
//
// The failure is silent by construction: a fork that renames its `consumes:`
// var gets its seed stamped under the ORIGIN's name — an empty hand-off, not
// an error — and a fork that moves its gate context greens a status nothing
// requires.

const handoffBakedBot = "schema out:\n  ok: bool\n\nvars:\n  gate_context: string = \"iterion/baked\"\n\ntool work:\n  command: `printf '{\"ok\":true}'`\n  output: out\n\nworkflow reviewer_bot:\n  worktree: none\n  entry: work\n  work -> done\n"

const handoffForkBot = "schema out:\n  ok: bool\n\nvars:\n  gate_context: string = \"iterion/fork\"\n\ntool work:\n  command: `printf '{\"ok\":true}'`\n  output: out\n\nworkflow reviewer_bot:\n  worktree: none\n  entry: work\n  work -> done\n"

const handoffBakedManifest = "name: reviewer-bot\nversion: 1.0.0\nretry:\n  usage_window: resume\n  max_attempts: 9\nconsumes:\n  - kind: review\n    var: prior_review\n    scope: pr\n"

const handoffForkManifest = "name: reviewer-bot\nversion: 1.0.0\nretry:\n  usage_window: off\n  max_attempts: 2\nconsumes:\n  - kind: review\n    var: prior_review_fork\n    scope: pr\n"

// newHandoffTeamServer bakes `reviewer-bot` into the catalog and gives team
// t1 a fork of it whose manifest and vars BOTH differ from the origin's.
func newHandoffTeamServer(t *testing.T) *Server {
	t.Helper()
	s := newOrgTestServer(t)
	seedGate(t, s, gateSpec{id: "t1"})
	botsDir := t.TempDir()
	bundleDir := filepath.Join(botsDir, "reviewer-bot")
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "main.bot"), []byte(handoffBakedBot), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "manifest.yaml"), []byte(handoffBakedManifest), 0o600); err != nil {
		t.Fatal(err)
	}
	s.cfg.Bots.Paths = []string{botsDir}
	s.botSources = botsource.NewMemoryStore()
	if _, err := s.botSources.Create(store.WithTenant(context.Background(), "t1"), botsource.BotSource{
		TenantID: "t1", Slug: "reviewer-bot",
		Files: map[string]string{
			botsource.MainBotFile: handoffForkBot,
			"manifest.yaml":       handoffForkManifest,
		},
	}); err != nil {
		t.Fatalf("seed the team's fork: %v", err)
	}
	return s
}

func TestWebhookMetadataReadsTheTeamsFork(t *testing.T) {
	ctx := context.Background()

	t.Run("hand-off seeds come from the fork's manifest", func(t *testing.T) {
		s := newHandoffTeamServer(t)
		got := s.handoffConsumersFor(ctx, "t1", "reviewer-bot")
		if len(got) != 1 || got[0].Var != "prior_review_fork" || got[0].Kind != bundle.HandoffKindReview {
			t.Fatalf("consumes = %+v, want the FORK's `prior_review_fork` — the launch runs the fork, so the seed must be stamped under the var the fork actually declares", got)
		}
	})

	t.Run("gate context comes from the fork's var default", func(t *testing.T) {
		s := newHandoffTeamServer(t)
		got := s.resolveGateContext(ctx, webhooks.Config{ID: "wh-1", TenantID: "t1"}, "reviewer-bot")
		if got != "iterion/fork" {
			t.Fatalf("gate context = %q, want %q — a fork that moves its gate context must not green the origin's status", got, "iterion/fork")
		}
	})

	// A team with no row of its own still reads the baked bot: the team pass
	// must not shadow the tiers below it.
	t.Run("a team with no fork still reads the baked bot", func(t *testing.T) {
		s := newHandoffTeamServer(t)
		if got := s.handoffConsumersFor(ctx, "t2", "reviewer-bot"); len(got) != 1 || got[0].Var != "prior_review" {
			t.Fatalf("consumes = %+v, want the baked `prior_review`", got)
		}
		if got := s.resolveGateContext(ctx, webhooks.Config{ID: "wh-1", TenantID: "t2"}, "reviewer-bot"); got != "iterion/baked" {
			t.Fatalf("gate context = %q, want %q", got, "iterion/baked")
		}
	})

	// The third read on the same delivery, same class as the two Revi named:
	// a fork's manifest is what decides whether ITS run is auto-retried.
	t.Run("retry policy comes from the fork's manifest", func(t *testing.T) {
		s := newHandoffTeamServer(t)
		if got := s.resolveRunRetryPolicy(ctx, "t1", "reviewer-bot"); got == nil || got.MaxAttempts != 2 {
			t.Fatalf("retry = %+v, want the FORK's max_attempts 2 — the fork's run is the one being retried", got)
		}
		if got := s.resolveRunRetryPolicy(ctx, "t2", "reviewer-bot"); got == nil || got.MaxAttempts != 9 {
			t.Fatalf("retry = %+v, want the baked max_attempts 9 for a team with no fork", got)
		}
	})

	// An explicit operator/webhook launch var still outranks BOTH — the fork
	// is a default source, never an override of what the operator pinned.
	t.Run("an operator launch var still wins over the fork", func(t *testing.T) {
		s := newHandoffTeamServer(t)
		cfg := webhooks.Config{ID: "wh-1", TenantID: "t1", OperatorLaunchVars: map[string]string{gateContextVar: "iterion/pinned"}}
		if got := s.resolveGateContext(ctx, cfg, "reviewer-bot"); got != "iterion/pinned" {
			t.Fatalf("gate context = %q, want the operator's pin", got)
		}
	})
}
