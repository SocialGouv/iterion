package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/botsource"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestResolveResumeSourceFallsBackToPersistedLaunchSource(t *testing.T) {
	workDir := t.TempDir()
	storeDir := filepath.Join(t.TempDir(), ".iterion")
	srv := New(Config{
		WorkDir:                 workDir,
		StoreDir:                storeDir,
		SkipProjectRegistration: true,
	}, iterlog.New(iterlog.LevelError, os.Stderr))

	outsidePath := filepath.Join(t.TempDir(), "dispatcher", "worktree", "child.bot")
	persistedSource := "workflow child:\n  entry: done\n"

	if _, err := srv.safePath(outsidePath); err == nil {
		t.Fatalf("test setup invalid: %q should escape WorkDir", outsidePath)
	}

	resolvedPath, resolvedSource, _, err := srv.resolveResumeSource(
		context.Background(), "", outsidePath, "", persistedSource,
	)
	if err != nil {
		t.Fatalf("resolveResumeSource() error = %v", err)
	}
	if resolvedSource != persistedSource {
		t.Errorf("resolved source = %q, want persisted launch source", resolvedSource)
	}
	if !pathContains(srv.inlineSourceCacheDir(), resolvedPath) {
		t.Errorf("resolved path = %q, want server-owned inline cache", resolvedPath)
	}
	got, err := os.ReadFile(resolvedPath)
	if err != nil {
		t.Fatalf("read materialised source: %v", err)
	}
	if string(got) != persistedSource {
		t.Errorf("materialised source = %q, want %q", got, persistedSource)
	}
}

func TestResolveResumeSourceExplicitSourceWinsOverPersistedFallback(t *testing.T) {
	workDir := t.TempDir()
	storeDir := filepath.Join(t.TempDir(), ".iterion")
	srv := New(Config{
		WorkDir:                 workDir,
		StoreDir:                storeDir,
		SkipProjectRegistration: true,
	}, iterlog.New(iterlog.LevelError, os.Stderr))

	outsidePath := filepath.Join(t.TempDir(), "dispatcher", "worktree", "child.bot")
	explicitSource := "workflow edited:\n  entry: done\n"
	persistedSource := "workflow original:\n  entry: done\n"

	_, resolvedSource, _, err := srv.resolveResumeSource(
		context.Background(), "", outsidePath, explicitSource, persistedSource,
	)
	if err != nil {
		t.Fatalf("resolveResumeSource() error = %v", err)
	}
	if resolvedSource != explicitSource {
		t.Errorf("resolved source = %q, want explicit source %q", resolvedSource, explicitSource)
	}
}

// A resume re-resolves the SAME stored-bot tier the launch used
// (Run.BotSourceTenant), fresh version — never re-derives the tier from
// the path, which silently swapped a team bot's resume onto a same-slug
// platform override (and made a unique-slug team bot unresumable).
func TestResolveResumeSource_HonorsPersistedOrigin(t *testing.T) {
	s, _, _ := newBotSourceTestServer(t)
	s.cfg.Mode = "cloud"
	ctx := context.Background()

	// Same slug in BOTH tiers, different content.
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

	// Team-origin run: resume must load the TEAM row despite the platform twin.
	_, source, lb, err := s.resolveResumeSource(ctx, "t1", "bots/shared/main.bot", "", "")
	if err != nil {
		t.Fatalf("team-origin resume: %v", err)
	}
	defer lb.Cleanup()
	if lb == nil || lb.Origin != "team" || !strings.Contains(source, "printf team") {
		t.Fatalf("team-origin resume resolved %+v (source %q) — tier swapped", lb, source)
	}
	if lb.Ref == nil || lb.Ref.TenantID != "t1" {
		t.Fatalf("ref = %+v, want the team row", lb.Ref)
	}

	// Platform-origin run resolves the platform row.
	_, source, plb, err := s.resolveResumeSource(ctx, botsource.PlatformTenantID, "bots/shared/main.bot", "", "")
	if err != nil {
		t.Fatalf("platform-origin resume: %v", err)
	}
	defer plb.Cleanup()
	if plb == nil || plb.Origin != "platform" || !strings.Contains(source, "printf platform") {
		t.Fatalf("platform-origin resume resolved %+v", plb)
	}

	// Inline source: the caller's source wins for the compile, but the
	// bundle ref must STILL be stamped — dropping it hands the runner the
	// stale baked bundle.
	edited := strings.Replace(testBotMain, "printf ok", "printf edited", 1)
	_, source, ilb, err := s.resolveResumeSource(ctx, "t1", "bots/shared/main.bot", edited, "")
	if err != nil {
		t.Fatalf("inline-source resume: %v", err)
	}
	defer ilb.Cleanup()
	if source != edited {
		t.Fatalf("inline source must win for the compile, got %q", source)
	}
	if ilb == nil || ilb.Ref == nil || ilb.Ref.TenantID != "t1" {
		t.Fatalf("inline-source resume dropped the bundle ref: %+v", ilb)
	}

	// A deleted row is an EXPLICIT error naming the remedy — never a silent
	// fall-through to another tier's content.
	tctx := store.WithTenant(ctx, "t1")
	row, _ := s.botSources.GetBySlug(tctx, "t1", "shared")
	if err := s.botSources.Delete(tctx, row.ID); err != nil {
		t.Fatal(err)
	}
	_, _, _, err = s.resolveResumeSource(ctx, "t1", "bots/shared/main.bot", "", "")
	if err == nil || !strings.Contains(err.Error(), "no longer exists") {
		t.Fatalf("deleted-row resume: err = %v, want the explicit remedy", err)
	}
}

// Team > platform must hold even across tolerated slug spellings: the
// normalized lookup feeds ONLY the platform tier. (Round-1's version
// rewrote the slug itself, letting a platform override named
// "feature-dev" hijack a team's own "feature_dev" bot.)
func TestResolveBotTiered_NormalizationNeverHijacksTeamTier(t *testing.T) {
	s, _, _ := newBotSourceTestServer(t)
	ctx := context.Background()
	mk := func(tenant, slug, marker string) {
		t.Helper()
		if _, err := s.botSources.Create(store.WithTenant(ctx, tenant), botsource.BotSource{
			TenantID: tenant, Slug: slug,
			Files: map[string]string{botsource.MainBotFile: strings.Replace(testBotMain, "printf ok", marker, 1)},
		}); err != nil {
			t.Fatal(err)
		}
	}
	mk("t1", "feature_dev", "printf TEAM")
	mk(botsource.PlatformTenantID, "feature-dev", "printf PLATFORM")
	s.invalidatePlatformBots()

	lb, err := s.resolveBotTiered(ctx, "t1", "feature_dev", "")
	if err != nil || lb == nil {
		t.Fatalf("resolve: %+v, %v", lb, err)
	}
	defer lb.Cleanup()
	if lb.Origin != "team" || !strings.Contains(lb.Source, "printf TEAM") {
		t.Fatalf("HIJACK: team's own bot resolved to origin=%q — the platform override won a tier it must never win", lb.Origin)
	}

	// And the normalization still serves its purpose: with NO team row, a
	// tolerated spelling reaches the platform override.
	plb, err := s.resolveBotTiered(ctx, "", "feature_dev", "")
	if err != nil || plb == nil || plb.Origin != "platform" || !strings.Contains(plb.Source, "printf PLATFORM") {
		t.Fatalf("normalized platform lookup broken: %+v, %v", plb, err)
	}
	plb.Cleanup()
}

// A resume that CARRIES INLINE SOURCE must survive the stored row's
// deletion (WARN + proceed — the delete-reverts-to-baked semantics), and a
// TRANSIENT store failure must be typed so the sweeper re-arms instead of
// permanently abandoning a paid usage-window retry.
func TestResolveResumeSource_InlineSourceDegradation(t *testing.T) {
	s, _, _ := newBotSourceTestServer(t)
	s.cfg.Mode = "cloud"
	ctx := context.Background()

	if _, err := s.botSources.Create(store.WithTenant(ctx, "t1"), botsource.BotSource{
		TenantID: "t1", Slug: "shared",
		Files: map[string]string{botsource.MainBotFile: testBotMain},
	}); err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(testBotMain, "printf ok", "printf edited", 1)

	// Row deleted: the caller's source still runs (no bundle ref — baked
	// fallback), loudly, not a 400 telling them to do what they just did.
	tctx := store.WithTenant(ctx, "t1")
	row, _ := s.botSources.GetBySlug(tctx, "t1", "shared")
	if err := s.botSources.Delete(tctx, row.ID); err != nil {
		t.Fatal(err)
	}
	_, src, lb, err := s.resolveResumeSource(ctx, "t1", "bots/shared/main.bot", edited, "")
	if err != nil {
		t.Fatalf("inline-source resume after delete must proceed, got %v", err)
	}
	lb.Cleanup()
	if src != edited || lb != nil {
		t.Fatalf("degraded resume: src==edited=%v lb=%+v (want caller source, no stale ref)", src == edited, lb)
	}

	// Transient store failure: typed, so the sweeper can re-arm.
	s.botSources = resumeBlipStore{Store: s.botSources}
	_, _, _, err = s.resolveResumeSource(ctx, "t1", "bots/shared/main.bot", edited, "")
	if !errors.Is(err, errResumeResolveTransient) {
		t.Fatalf("transient failure must be typed errResumeResolveTransient, got %v", err)
	}
}

// resumeBlipStore fails every slug read with a non-NotFound error.
type resumeBlipStore struct{ botsource.Store }

func (resumeBlipStore) GetBySlug(context.Context, string, string) (botsource.BotSource, error) {
	return botsource.BotSource{}, context.DeadlineExceeded
}

// The transient typing must cover BOTH resolveResumeBot branches: a run with
// NO persisted origin (every baked-catalog and pre-change run) still touches
// the platform tier via resolveBotTiered, and a store blip there must re-arm
// the sweeper, not permanently abandon a paid usage-window retry.
func TestResolveResumeBot_NoOriginTransientIsTyped(t *testing.T) {
	s, _, _ := newBotSourceTestServer(t)
	s.cfg.Mode = "cloud"
	s.botSources = resumeBlipStore{Store: s.botSources}

	_, err := s.resolveResumeBot(context.Background(), "", "bots/shared/main.bot")
	if !errors.Is(err, errResumeResolveTransient) {
		t.Fatalf("no-origin store blip must be typed errResumeResolveTransient, got %v", err)
	}
}
