package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
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
		context.Background(), outsidePath, "", persistedSource,
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
		context.Background(), outsidePath, explicitSource, persistedSource,
	)
	if err != nil {
		t.Fatalf("resolveResumeSource() error = %v", err)
	}
	if resolvedSource != explicitSource {
		t.Errorf("resolved source = %q, want explicit source %q", resolvedSource, explicitSource)
	}
}
