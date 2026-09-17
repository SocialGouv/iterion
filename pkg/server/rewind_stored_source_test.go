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

// Two abstentions, both deliberate: a run on a tier that IS on this
// filesystem resolves through the ordinary path, and a caller that named a
// source itself has overridden this resolution outright. The answer is the
// empty path, which leaves every other part of the rewind as it was.
//
// Note which case is NOT here. A `--node` rewind names its own pivot but
// still derives its blast radius — the dropped outputs, the tombstoned
// artifacts — from the graph this source compiles to. Gating on Auto is
// exactly what left it computing that from the baked twin.
func TestCurrentStoredBotSource_AnswersNothingWhenItIsNotItsJob(t *testing.T) {
	s, _, _ := newBotSourceTestServer(t)
	s.cfg.Mode = "cloud"
	ctx := context.Background()

	stored := &store.Run{ID: "r", FilePath: "bots/shared/main.bot", BotSourceTier: store.BotSourceTierTeam, BotSourceTenant: "t1"}
	cases := []struct {
		name   string
		run    *store.Run
		wanted bool
	}{
		{"a baked-catalog run", &store.Run{ID: "r", FilePath: "bots/shared/main.bot", BotSourceTier: store.BotSourceTierBaked}, true},
		{"a local run with no tier", &store.Run{ID: "r", FilePath: "main.bot"}, true},
		{"the caller named a source itself", stored, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path, release, err := s.currentStoredBotSource(ctx, tc.run, tc.wanted)
			if err != nil {
				t.Fatalf("currentStoredBotSource: %v", err)
			}
			defer release()
			if path != "" {
				t.Errorf("resolved %q — a bundle was materialized where the rewind does not use one", path)
			}
		})
	}
}

// A run launched BEFORE the tier stamp existed carries only BotSourceTenant,
// and it is still a stored-bot run: its source is a row, not a file. A guard
// on the tier alone passed it straight through to the baked twin — measured
// at a pivot on the ENTRY node with all four executed nodes dropped and their
// artifacts tombstoned, reported auto_targeted.
func TestCurrentStoredBotSource_ARunPredatingTheTierStampIsStillStored(t *testing.T) {
	s, _, _ := newBotSourceTestServer(t)
	s.cfg.Mode = "cloud"
	ctx := context.Background()
	if _, err := s.botSources.Create(store.WithTenant(ctx, "t1"), botsource.BotSource{
		TenantID: "t1", Slug: "shared",
		Files: map[string]string{botsource.MainBotFile: testBotMain},
	}); err != nil {
		t.Fatal(err)
	}
	// No BotSourceTier: the launch predates the stamp.
	run := &store.Run{ID: "r1", FilePath: "bots/shared/main.bot", BotSourceTenant: "t1"}
	path, release, err := s.currentStoredBotSource(ctx, run, true)
	if err != nil {
		t.Fatalf("currentStoredBotSource: %v", err)
	}
	defer release()
	if path == "" {
		t.Fatal("no current source resolved — the run's bundle is a stored row, so the rewind would compute " +
			"its graph from the baked catalog twin: a different program")
	}
}
