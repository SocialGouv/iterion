package server

import (
	"context"
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
