package server

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/store"
)

// currentStoredBotSource answers `rewind --auto` with the CURRENT version of
// the bot a stored-tier run was served by — the side of the diff that does not
// live on this filesystem.
//
// It re-resolves the SAME botsource row the launch used, never a path
// re-derived from the tier: two rows can share a slug, and resolving by path
// once swapped a team bot's resume onto a same-slug platform override. The
// materialization it hands back must therefore carry the TEAM row's text even
// when a platform twin exists.
func TestCurrentStoredBotSource_ResolvesTheSameRowAtItsCurrentVersion(t *testing.T) {
	s, _, _ := newBotSourceTestServer(t)
	s.cfg.Mode = "cloud"
	ctx := context.Background()

	for tenant, marker := range map[string]string{
		"t1":                       "printf team",
		botsource.PlatformTenantID: "printf platform",
	} {
		if _, err := s.botSources.Create(store.WithTenant(ctx, tenant), botsource.BotSource{
			TenantID: tenant, Slug: "shared",
			Files: map[string]string{botsource.MainBotFile: strings.Replace(testBotMain, "printf ok", marker, 1)},
		}); err != nil {
			t.Fatal(err)
		}
	}

	run := &store.Run{
		ID: "r1", FilePath: "bots/shared/main.bot",
		BotSourceTier: store.BotSourceTierTeam, BotSourceTenant: "t1",
	}
	path, release, err := s.currentStoredBotSource(ctx, run, true)
	if err != nil {
		t.Fatalf("currentStoredBotSource: %v", err)
	}
	defer release()
	if path == "" {
		t.Fatal("no current source resolved for a team-tier run — --auto keeps refusing and the operator " +
			"must name the node by hand on every rewind")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the materialized current source: %v", err)
	}
	if !strings.Contains(string(body), "printf team") {
		t.Errorf("materialized source is not the TEAM row — a same-slug platform override was resolved instead, "+
			"so --auto would diff two unrelated programs:\n%s", body)
	}
}

// Two refusals, both deliberate, because materializing a bundle is real work
// and a wrong answer here is worse than none.
//
// A run on a tier that IS on this filesystem resolves through the ordinary
// path; a rewind that names its node needs no diff at all. In both cases the
// answer is the empty path, which leaves every other part of the rewind
// exactly as it was.
func TestCurrentStoredBotSource_AnswersNothingWhenItIsNotItsJob(t *testing.T) {
	s, _, _ := newBotSourceTestServer(t)
	s.cfg.Mode = "cloud"
	ctx := context.Background()

	cases := []struct {
		name   string
		run    *store.Run
		wanted bool
	}{
		{"a baked-catalog run", &store.Run{ID: "r", FilePath: "bots/shared/main.bot", BotSourceTier: store.BotSourceTierBaked}, true},
		{"a local run with no tier", &store.Run{ID: "r", FilePath: "main.bot"}, true},
		{"a --node rewind on a stored tier", &store.Run{ID: "r", FilePath: "bots/shared/main.bot", BotSourceTier: store.BotSourceTierTeam, BotSourceTenant: "t1"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path, release, err := s.currentStoredBotSource(ctx, tc.run, tc.wanted)
			if err != nil {
				t.Fatalf("currentStoredBotSource: %v", err)
			}
			defer release()
			if path != "" {
				t.Errorf("resolved %q — a bundle was materialized for a rewind that never diffs it", path)
			}
		})
	}
}
