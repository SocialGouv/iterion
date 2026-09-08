package runview

import (
	"context"
	"errors"
	"fmt"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestListAllArtifacts seeds two nodes' artifacts (one with two versions)
// and verifies the aggregate returns the latest version per node with its
// labels and a derived title.
func TestListAllArtifacts(t *testing.T) {
	dir := t.TempDir()
	logger := iterlog.Nop()
	seed, err := store.New(dir, store.WithLogger(logger))
	if err != nil {
		t.Fatalf("seed store: %v", err)
	}
	ctx := context.Background()
	if _, err := seed.CreateRun(ctx, "run1", "wf", nil); err != nil {
		t.Fatalf("create run: %v", err)
	}
	write := func(node string, v int, labels []string, data map[string]any) {
		if err := seed.WriteArtifact(ctx, &store.Artifact{
			RunID: "run1", NodeID: node, Version: v, Labels: labels, Data: data,
		}); err != nil {
			t.Fatalf("write artifact %s v%d: %v", node, v, err)
		}
	}
	// planner: two versions; latest carries the plan label + a title.
	write("planner", 0, []string{"plan"}, map[string]any{"plan": "draft"})
	write("planner", 1, []string{"plan"}, map[string]any{"plan": "final", "title": "Migration plan"})
	// reviewer: one version, verdict label, no title.
	write("reviewer", 0, []string{"verdict"}, map[string]any{"approved": true})

	svc, err := NewService(dir, WithLogger(logger))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	got, err := svc.ListAllArtifacts("run1")
	if err != nil {
		t.Fatalf("ListAllArtifacts: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d artifacts, want 2: %+v", len(got), got)
	}
	// Sorted by node id: planner, reviewer.
	if got[0].NodeID != "planner" || got[0].Version != 1 {
		t.Errorf("planner: got node=%s v=%d, want planner v1", got[0].NodeID, got[0].Version)
	}
	if got[0].Title != "Migration plan" {
		t.Errorf("planner title = %q, want %q", got[0].Title, "Migration plan")
	}
	if len(got[0].Labels) != 1 || got[0].Labels[0] != "plan" {
		t.Errorf("planner labels = %v, want [plan]", got[0].Labels)
	}
	if got[1].NodeID != "reviewer" || len(got[1].Labels) != 1 || got[1].Labels[0] != "verdict" {
		t.Errorf("reviewer: got %+v, want verdict label", got[1])
	}

	// Unknown run → empty, no error.
	empty, err := svc.ListAllArtifacts("nope")
	if err != nil {
		t.Fatalf("ListAllArtifacts(nope): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("unknown run returned %d artifacts, want 0", len(empty))
	}
}

// TestListAllArtifacts_FromIndexWhenNoDirectory is the cloud shape: the
// server pod has no runs/<id>/artifacts directory (the runner that wrote
// the artifacts has it), but the run document carries artifact_index and
// the store can load every version. The listing must come from there
// instead of reading as an empty run.
func TestListAllArtifacts_FromIndexWhenNoDirectory(t *testing.T) {
	storeRoot := t.TempDir()
	logger := iterlog.Nop()
	seed, err := store.New(storeRoot, store.WithLogger(logger))
	if err != nil {
		t.Fatalf("seed store: %v", err)
	}
	ctx := context.Background()
	if _, err := seed.CreateRun(ctx, "run2", "wf", nil); err != nil {
		t.Fatalf("create run: %v", err)
	}
	for _, v := range []int{0, 1} {
		if err := seed.WriteArtifact(ctx, &store.Artifact{RunID: "run2", NodeID: "report", Version: v, Labels: []string{"report"}, Data: map[string]any{"title": "Report v" + string(rune('0'+v))}}); err != nil {
			t.Fatalf("write artifact v%d: %v", v, err)
		}
	}
	if err := seed.WriteArtifact(ctx, &store.Artifact{RunID: "run2", NodeID: "verdict", Version: 0, Data: map[string]any{"ok": true}}); err != nil {
		t.Fatalf("write verdict: %v", err)
	}

	// A service whose storeDir holds NO artifact directory for the run,
	// backed by the store that does know the run.
	svc, err := NewService(t.TempDir(), WithLogger(logger), WithStore(seed))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	got, err := svc.ListAllArtifacts("run2")
	if err != nil {
		t.Fatalf("ListAllArtifacts: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d artifacts, want 2 (from artifact_index): %+v", len(got), got)
	}
	if got[0].NodeID != "report" || got[0].Version != 1 || got[0].Title != "Report v1" || len(got[0].Labels) != 1 {
		t.Errorf("report: %+v, want the latest version with its label and title", got[0])
	}
	if got[1].NodeID != "verdict" || got[1].Version != 0 {
		t.Errorf("verdict: %+v", got[1])
	}
	empty, err := svc.ListAllArtifacts("unknown")
	if err != nil || len(empty) != 0 {
		t.Errorf("unknown run: got %v, %v; want empty, nil", empty, err)
	}
}

// A store failure that is not "unknown run" must surface as an error: an
// empty listing would read as "nothing published" on exactly the host the
// index fallback exists for.
func TestListAllArtifacts_FromIndexStoreFailureIsAnError(t *testing.T) {
	real, err := store.New(t.TempDir(), store.WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	svc, err := NewService(t.TempDir(), WithLogger(iterlog.Nop()), WithStore(failingRunStore{RunStore: real}))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if _, err := svc.ListAllArtifacts("run-any"); err == nil {
		t.Fatal("a store outage must not be reported as an empty listing")
	}
}

// failingRunStore is a real store whose LoadRun fails with a non-not-found
// error (an outage), the shape the index fallback must not swallow.
type failingRunStore struct{ store.RunStore }

func (failingRunStore) LoadRun(context.Context, string) (*store.Run, error) {
	return nil, errors.New("mongo: connection reset")
}

// tenantGuardedRunStore reproduces the mongo store's fail-closed tenant
// guard (withTenantFilter, pkg/store/mongo/tenant.go): a LoadRun whose ctx
// carries neither a tenant nor the explicit bypass marker PANICS. That is
// the only faithful stand-in for the cloud server pod the index fallback
// exists for — the fs-backed tests above cannot see the defect, because
// the filesystem store ignores the context entirely.
type tenantGuardedRunStore struct {
	store.RunStore
	wantTenant string
}

func (t tenantGuardedRunStore) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	if tenant, ok := store.TenantFromContext(ctx); ok && tenant != "" {
		if tenant != t.wantTenant {
			// What mongo answers a cross-tenant read: not-found, never a leak.
			return nil, fmt.Errorf("run %s not found: %w", id, store.ErrRunNotFound)
		}
	} else if !store.IsWithoutTenantFilter(ctx) {
		panic("store: tenant-scoped query without tenant in ctx (use store.WithoutTenantFilter to bypass)")
	}
	return t.RunStore.LoadRun(ctx, id)
}

// TestListAllArtifacts_FromIndexCarriesTenantContext pins the two halves of
// the context contract on the index path: the exported context-free
// ListAllArtifacts must reach LoadRun with the bypass marker (a bare
// context.Background panics the mongo store — a 500 on the very host this
// fallback targets), and ListAllArtifactsCtx must forward the caller's
// tenant so a cross-tenant read gets nothing.
func TestListAllArtifacts_FromIndexCarriesTenantContext(t *testing.T) {
	logger := iterlog.Nop()
	seed, err := store.New(t.TempDir(), store.WithLogger(logger))
	if err != nil {
		t.Fatalf("seed store: %v", err)
	}
	ctx := context.Background()
	if _, err := seed.CreateRun(ctx, "run3", "wf", nil); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := seed.WriteArtifact(ctx, &store.Artifact{RunID: "run3", NodeID: "report", Version: 0, Data: map[string]any{"title": "R"}}); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	// storeDir holds no artifact directory for the run → the index path.
	svc, err := NewService(t.TempDir(), WithLogger(logger),
		WithStore(tenantGuardedRunStore{RunStore: seed, wantTenant: "team-a"}))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	// The context-free wrapper: must not panic, and must serve the listing.
	got, err := svc.ListAllArtifacts("run3")
	if err != nil {
		t.Fatalf("ListAllArtifacts: %v", err)
	}
	if len(got) != 1 || got[0].NodeID != "report" {
		t.Errorf("context-free listing = %+v, want the report node", got)
	}

	// The tenant-aware variant with the owning tenant: same listing.
	got, err = svc.ListAllArtifactsCtx(store.WithTenant(ctx, "team-a"), "run3")
	if err != nil {
		t.Fatalf("ListAllArtifactsCtx(team-a): %v", err)
	}
	if len(got) != 1 || got[0].NodeID != "report" {
		t.Errorf("tenant listing = %+v, want the report node", got)
	}

	// Another tenant: the store answers not-found, which is an empty
	// listing, not an error.
	other, err := svc.ListAllArtifactsCtx(store.WithTenant(ctx, "team-b"), "run3")
	if err != nil || len(other) != 0 {
		t.Errorf("cross-tenant listing = %+v, %v; want empty, nil", other, err)
	}
}

// deletedRunStore returns the tombstone error mongo answers for a run that
// was deleted — distinct from ErrRunNotFound, and just as much an absence.
type deletedRunStore struct{ store.RunStore }

func (deletedRunStore) LoadRun(_ context.Context, id string) (*store.Run, error) {
	return nil, fmt.Errorf("run %s: %w", id, store.ErrRunDeleted)
}

// A tombstoned run is absent, not an outage: it must list empty rather
// than 500. errors.Is(err, ErrRunNotFound) alone misses this — hence
// store.RunAbsent.
func TestListAllArtifacts_FromIndexDeletedRunIsEmpty(t *testing.T) {
	real, err := store.New(t.TempDir(), store.WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	svc, err := NewService(t.TempDir(), WithLogger(iterlog.Nop()), WithStore(deletedRunStore{RunStore: real}))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	got, err := svc.ListAllArtifacts("gone")
	if err != nil || len(got) != 0 {
		t.Errorf("deleted run: got %+v, %v; want empty, nil", got, err)
	}
}
