package runview

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
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
	svc, err := NewService(dir, WithLogger(iterlog.Nop()), WithStore(probe), WithWorkDir(gittest.SourceRepo(t)))
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
	svc, err := NewService(dir, WithLogger(iterlog.Nop()), WithWorkDir(gittest.SourceRepo(t)))

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

// irBlobSeamStore is a filesystem run store that also offers the IR blob
// seam (store.AsIRBlobStore), the capability that gives a store an IR blob
// key space, as the cloud store has.
type irBlobSeamStore struct {
	store.RunStore
}

func (irBlobSeamStore) PutIRBlob(context.Context, string, []byte) (string, error) {
	return "", errors.New("irBlobSeamStore: no IR blob is stored in this test")
}

func (irBlobSeamStore) GetIRBlob(context.Context, string) ([]byte, error) {
	return nil, os.ErrNotExist
}

func (irBlobSeamStore) IRBlobBackend() string { return "s3" }

// On a store with an IR blob key space, a caller-chosen run id carrying the
// IR blob infix would name another run's IR object — its compiled IR's
// content id, its bundle snapshot — as its own legacy key: its tenant could
// read the object and its deletion would delete it. One ending with
// "-bundle" would have its IR objects swept by the deletion of the run named
// before that ending. The launch refuses both as reserved, before any run
// exists under them.
func TestLaunch_RefusesARunIDCarryingTheIRBlobInfix(t *testing.T) {
	dir := t.TempDir()
	botPath := filepath.Join(dir, "demo.bot")
	if err := os.WriteFile(botPath, []byte("workflow demo:\n  entry: done\n"), 0o600); err != nil {
		t.Fatalf("write bot: %v", err)
	}
	fs, err := store.New(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	seam := irBlobSeamStore{RunStore: fs}
	if store.AsIRBlobStore(seam) == nil {
		t.Fatal("the test store offers no IR blob seam")
	}
	svc, err := NewService(dir, WithLogger(iterlog.Nop()), WithStore(seam), WithWorkDir(gittest.SourceRepo(t)))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	const victim = "019ac3d2-7f1e-7c4b-9a0e-2b5c8d1e4f60"
	digest := strings.Repeat("ab", 32)
	contentID, err := store.IRContentLogicalID(victim, digest, rand.Reader)
	if err != nil {
		t.Fatalf("IRContentLogicalID: %v", err)
	}
	for _, id := range []string{contentID, victim + "-bundle-" + digest, victim + "-bundle", "x-bundle"} {
		res, err := svc.Launch(context.Background(), LaunchSpec{FilePath: botPath, RunID: id})
		if res != nil && res.Done != nil {
			<-res.Done
		}
		if !errors.Is(err, ErrRunIDReserved) {
			t.Fatalf("launch with the run id %q = %v, want ErrRunIDReserved", id, err)
		}
		if _, err := fs.LoadRun(context.Background(), id); !errors.Is(err, store.ErrRunNotFound) {
			t.Fatalf("the refused launch left a run under %q (LoadRun = %v)", id, err)
		}
	}
}

// A local store has no IR blob key space, so no run id of it can name
// another run's IR object: a local launch takes a run id that carries the
// IR blob infix or ends with "-bundle", as `iterion run --run-id` does.
func TestLaunch_LocalStoreTakesARunIDCarryingTheIRBlobInfix(t *testing.T) {
	dir := t.TempDir()
	botPath := filepath.Join(dir, "demo.bot")
	if err := os.WriteFile(botPath, []byte("workflow demo:\n  entry: done\n"), 0o600); err != nil {
		t.Fatalf("write bot: %v", err)
	}
	fs, err := store.New(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if store.AsIRBlobStore(fs) != nil {
		t.Fatal("the filesystem store offers an IR blob seam")
	}
	svc, err := NewService(dir, WithLogger(iterlog.Nop()), WithStore(fs), WithWorkDir(gittest.SourceRepo(t)))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	for _, id := range []string{"nightly-bundle-a", "nightly-bundle"} {
		res, err := svc.Launch(context.Background(), LaunchSpec{FilePath: botPath, RunID: id})
		if err != nil {
			t.Fatalf("local launch with the run id %q = %v, want it launched", id, err)
		}
		<-res.Done
		if res.RunID != id {
			t.Fatalf("run id = %q, want the requested %q", res.RunID, id)
		}
		if _, err := fs.LoadRun(context.Background(), id); err != nil {
			t.Fatalf("the local launch left no run under %q: %v", id, err)
		}
	}
}
