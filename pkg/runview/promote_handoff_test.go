package runview

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestPromoteForHandoff(t *testing.T) {
	if err := PromoteForHandoff(context.Background(), "run-1", nil); err != nil {
		t.Fatalf("a launch naming no upload errs: %v", err)
	}
	called := ""
	if err := PromoteForHandoff(context.Background(), "run-1", func(_ context.Context, runID string) error { called = runID; return nil }); err != nil || called != "run-1" {
		t.Fatalf("err=%v called=%q", err, called)
	}
	err := PromoteForHandoff(context.Background(), "run-1", func(context.Context, string) error { return errors.New("upload \"x\" not found") })
	if err == nil || !strings.Contains(err.Error(), "run-1") || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v, want the cause wrapped with the run id", err)
	}
}

// A detached launch promotes before it spawns: a promotion that fails
// spawns nothing and parks the row LAUNCH_FAILED; one that succeeds runs
// before the runner starts.
func TestLaunchDetachedPromotesBeforeTheSpawn(t *testing.T) {
	bin := t.TempDir()
	marker := filepath.Join(t.TempDir(), "spawned")
	script := "#!/bin/sh\ntouch \"" + marker + "\"\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "iterion"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// The locator prefers a sibling of the executable, then ITERION_BIN,
	// then the install paths — and the operator's own install would run for
	// real here. ITERION_BIN pins the shim.
	t.Setenv("ITERION_BIN", filepath.Join(bin, "iterion"))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	dir := t.TempDir()
	botPath := writeBudgetBot(t, dir)
	svc, err := NewService(dir, WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		svc.Stop(stopCtx)
	})
	ctx := context.Background()

	_, err = svc.launchDetached(ctx, "run-detached-fail", LaunchSpec{FilePath: botPath, AttachmentPromote: func(context.Context, string) error {
		return errors.New("upload \"x\" not found")
	}})
	if err == nil || !strings.Contains(err.Error(), "promote the launch's attachments") {
		t.Fatalf("err = %v, want the promotion failure", err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("the runner was spawned although the promotion failed")
	}
	r, err := svc.store.LoadRun(ctx, "run-detached-fail")
	if err != nil || r.Status != store.RunStatusFailed || r.FailureCode != store.FailureLaunchFailed {
		t.Fatalf("run = %+v (%v), want failed LAUNCH_FAILED", r, err)
	}

	if resolved, _ := runnerBinary(); resolved != filepath.Join(bin, "iterion") {
		t.Fatalf("the runner binary resolved to %q, not the shim — the witness would run a real runner", resolved)
	}
	res, err := svc.launchDetached(ctx, "run-detached-ok", LaunchSpec{FilePath: botPath, AttachmentPromote: func(context.Context, string) error {
		if _, statErr := os.Stat(marker); statErr == nil {
			return errors.New("the runner was spawned before the promotion")
		}
		return nil
	}})
	if err != nil {
		t.Fatalf("launchDetached: %v", err)
	}
	<-res.Done
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Fatal("the runner never ran after the promotion")
	}
}
