package runview

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// teamBlindAssertStore watches the FIRST LoadRun the service makes — the
// launch's taken-id lookup, which is the first store read of a launch — and
// counts the ones that are not team-blind. It counts rather than fails:
// the check reads a lookup error as "id free", so a probe error would be
// swallowed by the very code it probes, and the engine's own later loads
// (unstamped, unfiltered, by design) must pass through untouched.
type teamBlindAssertStore struct {
	store.RunStore
	seen    atomic.Bool
	badCtxs atomic.Int64
}

func (s *teamBlindAssertStore) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	if s.seen.CompareAndSwap(false, true) {
		if _, ok := store.TenantFromContext(ctx); ok {
			s.badCtxs.Add(1)
		}
		if !store.IsWithoutTenantFilter(ctx) {
			s.badCtxs.Add(1)
		}
	}
	return s.RunStore.LoadRun(ctx, id)
}

// The launch's taken-id lookup runs team-blind: it must see every team's
// runs, which is what makes a taken id a conflict whatever team holds it.
// A filesystem store ignores the context entirely, so this counter is the
// only witness the wiring has.
func TestLaunch_TheTakenIDLookupIsTeamBlind(t *testing.T) {
	dir := t.TempDir()
	botPath := filepath.Join(dir, "demo.bot")
	if err := os.WriteFile(botPath, []byte("workflow demo:\n  entry: done\n"), 0o600); err != nil {
		t.Fatalf("write bot: %v", err)
	}
	fs, err := store.New(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	probe := &teamBlindAssertStore{RunStore: fs}
	svc, err := NewService(dir, WithLogger(iterlog.Nop()), WithStore(probe))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	// A STAMPED parent: the lookup must shed the caller's stamp as well as
	// lift the filter. Launched from a bare context, this test cannot see a
	// wiring that keeps the stamp — which is the half that silently
	// re-scopes the check to the caller's team.
	res, err := svc.Launch(store.WithTenant(context.Background(), "team-a"), LaunchSpec{FilePath: botPath, RunID: "team-blind-fresh"})
	if err != nil {
		t.Fatalf("launch with an explicit fresh id: %v", err)
	}
	<-res.Done
	if !probe.seen.Load() {
		t.Fatal("the launch never performed its taken-id lookup")
	}
	if n := probe.badCtxs.Load(); n != 0 {
		t.Fatalf("the taken-id lookup was not team-blind (%d bad context(s))", n)
	}
}

// A caller-supplied run id names a run in ANY team, so it must be refused
// when that id is already taken — before the launch reaches the credential
// pool, which acts on the id. A fresh id still launches.
func TestLaunch_RefusesARunIDAnotherRunAlreadyUses(t *testing.T) {
	dir := t.TempDir()
	botPath := filepath.Join(dir, "demo.bot")
	if err := os.WriteFile(botPath, []byte("workflow demo:\n  entry: done\n"), 0o600); err != nil {
		t.Fatalf("write bot: %v", err)
	}
	svc, err := NewService(dir, WithLogger(iterlog.Nop()))

	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	res, err := svc.Launch(context.Background(), LaunchSpec{FilePath: botPath})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if res.RunID == "" {
		t.Fatal("generated run id is empty")
	}

	_, err = svc.Launch(context.Background(), LaunchSpec{FilePath: botPath, RunID: res.RunID})
	if !errors.Is(err, ErrRunIDTaken) {
		t.Fatalf("relaunch with the taken id %s = %v, want ErrRunIDTaken", res.RunID, err)
	}

	// A deleted run's id stays taken: its tombstone still conflicts at the
	// save, and the refusal says so before the pool acts on the id.
	del, err := store.New(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := del.DeleteRun(context.Background(), res.RunID); err != nil {
		t.Fatalf("delete run: %v", err)
	}
	if _, err := svc.Launch(context.Background(), LaunchSpec{FilePath: botPath, RunID: res.RunID}); !errors.Is(err, ErrRunIDTaken) {
		t.Fatalf("launch with a deleted run's id = %v, want ErrRunIDTaken", err)
	}

	fresh, err := svc.Launch(context.Background(), LaunchSpec{FilePath: botPath, RunID: "launch-runid-fresh"})
	if err != nil {
		t.Fatalf("launch with a fresh id: %v", err)
	}
	if fresh.RunID != "launch-runid-fresh" {
		t.Errorf("run id = %q, want the requested one", fresh.RunID)
	}
	<-res.Done
	<-fresh.Done
}
