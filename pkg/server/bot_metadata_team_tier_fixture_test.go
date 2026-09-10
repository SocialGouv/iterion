package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/store"
)

// #946 — the metadata reads that DESCRIBE a launch must resolve the tier that
// SERVES it. #871 made every launch surface tenant-aware, so a team's fork now
// runs on its board cards, triggers, schedules and webhooks; a description read
// from the origin's manifest disagrees with the bundle that executes, silently.
//
// One fixture, one lane per subtest: a baked `reviewer-bot` and team `t1`'s
// fork of it, whose manifest differs in every field a lane reads.

const metaBakedBot = "schema out:\n  ok: bool\n\nvars:\n  scope_notes: string = \"\"\n  fork_prompt: string = \"\"\n\ntool work:\n  command: `printf '{\"ok\":true}'`\n  output: out\n\nworkflow reviewer_bot:\n  worktree: none\n  entry: work\n  work -> done\n"

const metaBakedManifest = `name: reviewer-bot
display_name: Baked Revi
version: 1.0.0
invocations:
  - kind: command
    mode: board
    args_var: scope_notes
    command:
      name: revi
      scope: any
      opens_mr: false
  - kind: schedule
    schedule:
      suggested_cron: 0 2 * * 1
produces:
  - kind: review
    node: publish_baked
config_share:
  config_path: baked.yaml
  editable_paths:
    - feeds
forge:
  events:
    - pull_request
`

const metaForkManifest = `name: reviewer-bot
display_name: Fork Revi
version: 2.0.0
invocations:
  - kind: command
    mode: board
    args_var: fork_prompt
    command:
      name: revi
      scope: any
      opens_mr: true
  - kind: schedule
    schedule:
      suggested_cron: 0 5 * * 3
  - kind: command
    args_var: fork_prompt
    command:
      name: revifork
      scope: any
produces:
  - kind: review
    node: publish_fork
config_share:
  config_path: fork.yaml
  editable_paths:
    - entries
  editor_title: Fork editor
forge:
  events:
    - pull_request_comment
`

// A team bot with a slug the catalog never carried — the shape that makes the
// tenant-free reads answer "no such bot" rather than "the wrong bot".
const metaTeamOnlyManifest = `name: teamonly-bot
display_name: Teamonly
version: 1.0.0
invocations:
  - kind: schedule
    schedule:
      suggested_cron: 0 7 * * 5
  - kind: command
    args_var: fork_prompt
    command:
      name: teamonly
      scope: any
produces:
  - kind: review
    node: publish_teamonly
forge:
  events:
    - pull_request_comment
`

// newBotMetadataTeamServer bakes `reviewer-bot` into the catalog, gives team
// t1 a fork of that slug plus a bot of its own (`teamonly-bot`).
func newBotMetadataTeamServer(t *testing.T) *Server {
	t.Helper()
	s := newOrgTestServer(t)
	seedGate(t, s, gateSpec{id: "t1"})
	botsDir := t.TempDir()
	bundleDir := filepath.Join(botsDir, "reviewer-bot")
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "main.bot"), []byte(metaBakedBot), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "manifest.yaml"), []byte(metaBakedManifest), 0o600); err != nil {
		t.Fatal(err)
	}
	s.cfg.Bots.Paths = []string{botsDir}
	s.botSources = botsource.NewMemoryStore()
	ctx := store.WithTenant(context.Background(), "t1")
	if _, err := s.botSources.Create(ctx, botsource.BotSource{
		TenantID: "t1", Slug: "reviewer-bot",
		Files: map[string]string{botsource.MainBotFile: metaBakedBot, "manifest.yaml": metaForkManifest},
	}); err != nil {
		t.Fatalf("seed the team's fork: %v", err)
	}
	if _, err := s.botSources.Create(ctx, botsource.BotSource{
		TenantID: "t1", Slug: "teamonly-bot",
		Files: map[string]string{botsource.MainBotFile: metaBakedBot, "manifest.yaml": metaTeamOnlyManifest},
	}); err != nil {
		t.Fatalf("seed the team's own bot: %v", err)
	}
	return s
}
