package mongo

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestCreateChildRun_ReadsTheParentUnderTheCallersTenant_Mongo: the cloud
// twin reads the parent tenant-scoped like every read here, so a parent in
// another tenant is not found and the child is refused by the rule a missing
// parent meets (store.ErrChildParentUnreadable) — and a caller that forgot
// the tenant does not get a softer answer: the filter panics, which is why
// every launch path hands this method the launch's own identity. Gated on
// ITERION_TEST_MONGO_URI like the conformance suite.
func TestCreateChildRun_ReadsTheParentUnderTheCallersTenant_Mongo(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping the Mongo child-parent tenant rows")
	}
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	s, err := New(ctx, Config{URI: uri, Database: "iterion_child_parent_" + bsonNonce(t), Blob: newInMemoryBlob()})
	if err != nil {
		t.Fatalf("mongo New: %v", err)
	}
	t.Cleanup(func() {
		drop, dropCancel := mongotest.TeardownCtx()
		defer dropCancel()
		_ = s.db.Drop(drop)
		_ = s.Close(drop)
	})
	ta := store.WithIdentity(ctx, "tenant-a", "owner-a")
	tb := store.WithIdentity(ctx, "tenant-b", "owner-b")
	parent, err := s.CreateRun(ta, "run-parent", "parent", nil)
	if err != nil {
		t.Fatalf("CreateRun(parent): %v", err)
	}
	parent.Trust = store.RunTrustFork
	parent.RepoSHAExpected = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	if err := s.SaveRun(ta, parent); err != nil {
		t.Fatalf("SaveRun(parent): %v", err)
	}

	// The control: the parent's own tenant creates the child, which carries
	// the parent's provenance and the tenant it was created under.
	if _, err := s.CreateChildRun(ta, "run-child-a", "child", "run-parent", nil); err != nil {
		t.Fatalf("CreateChildRun under the parent's tenant: %v", err)
	}
	child, err := s.LoadRun(ta, "run-child-a")
	if err != nil {
		t.Fatalf("LoadRun(child): %v", err)
	}
	if child.Trust != store.RunTrustFork || child.TenantID != "tenant-a" {
		t.Fatalf("child Trust=%q TenantID=%q, want the parent's fork trust in tenant-a", child.Trust, child.TenantID)
	}

	// Another tenant cannot read the parent, so it cannot parent a child on
	// it: refused, typed, and nothing is written.
	_, err = s.CreateChildRun(tb, "run-child-b", "child", "run-parent", nil)
	if !errors.Is(err, store.ErrChildParentUnreadable) || !errors.Is(err, store.ErrRunNotFound) {
		t.Fatalf("CreateChildRun under another tenant = %v, want %v wrapping %v", err, store.ErrChildParentUnreadable, store.ErrRunNotFound)
	}
	if _, lerr := s.LoadRun(store.TeamBlind(ctx), "run-child-b"); !errors.Is(lerr, store.ErrRunNotFound) {
		t.Fatalf("LoadRun(refused child) = %v, want %v — the refusal must write nothing", lerr, store.ErrRunNotFound)
	}

	// A context with no tenant at all reaches the filter's panic: the cloud
	// twin never reads a parent on an identity it was not handed.
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("CreateChildRun on a tenantless context did not panic — a launch path that dropped the identity would read another tenant's parent")
			}
		}()
		_, _ = s.CreateChildRun(context.Background(), "run-child-none", "child", "run-parent", nil)
	}()
}
