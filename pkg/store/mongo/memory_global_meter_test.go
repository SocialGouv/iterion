package mongo

import (
	"bytes"
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	"github.com/SocialGouv/iterion/pkg/knowledge"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestAggregateIDs_GlobalChargesTheInstanceAndTheWriter is D25's shape, host
// only: the instance-wide space had NO aggregate ceiling in this adapter
// (ref.TenantID is empty for `global`, and the old `if tenantID != ""` skipped
// the whole org-aggregate step), while the FS adapter charged its whole-tree
// aggregate for the same write. It now charges the tenant-less instance row —
// the ceiling — and the writing org, so the bytes are attributed.
func TestAggregateIDs_GlobalChargesTheInstanceAndTheWriter(t *testing.T) {
	s := &MongoMemoryStore{}
	globalRef := knowledge.SpaceRef{Visibility: knowledge.VisibilityGlobal, Name: "catalogue"}

	t.Run("a team's run writing global charges both", func(t *testing.T) {
		ctx := store.WithTenant(context.Background(), "acme")
		got := s.aggregateIDs(ctx, globalRef)
		want := []string{knowledge.InstanceUsageID, "acme"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("aggregateIDs = %v, want %v", got, want)
		}
	})

	t.Run("no tenant on the context still charges the instance", func(t *testing.T) {
		got := s.aggregateIDs(context.Background(), globalRef)
		want := []string{knowledge.InstanceUsageID}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("aggregateIDs = %v, want %v — an unattributed global write must still meet the ceiling", got, want)
		}
	})

	t.Run("a tenant spelt like the reserved id is not charged twice", func(t *testing.T) {
		ctx := store.WithTenant(context.Background(), knowledge.InstanceUsageID)
		got := s.aggregateIDs(ctx, globalRef)
		want := []string{knowledge.InstanceUsageID}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("aggregateIDs = %v, want %v — a doubled row would charge the write twice and halve the ceiling", got, want)
		}
	})

	t.Run("a team-scoped space charges its own org only", func(t *testing.T) {
		// The ctx names a DIFFERENT tenant on purpose: the ref's tenant is the
		// one the server stamped from the identity, and it is what must be
		// charged — reading the ctx here would let a re-scoped request bill
		// the wrong org.
		ctx := store.WithTenant(context.Background(), "other")
		ref := knowledge.SpaceRef{Visibility: knowledge.VisibilityOrg, TenantID: "acme", Name: "conventions"}
		got := s.aggregateIDs(ctx, ref)
		if !reflect.DeepEqual(got, []string{"acme"}) {
			t.Fatalf("aggregateIDs = %v, want [acme]", got)
		}
	})
}

// TestSpaceDocFilter_CarriesTheTenantClause: the tenant-isolation docs claimed
// space-wide queries were tenant-stamped and they were not — every filter was
// the composite id alone. The clause cannot widen a result (the id already
// carries the tenant); it refuses a document whose stamped tenant disagrees.
// It must stay OFF for the global space, whose documents carry no tenant_id
// field at all (bson omitempty) — an equality on "" would match nothing and
// silently empty the instance catalogue.
func TestSpaceDocFilter_CarriesTheTenantClause(t *testing.T) {
	org := knowledge.SpaceRef{Visibility: knowledge.VisibilityOrg, TenantID: "acme", Name: "conventions"}
	f := spaceDocFilter(org)
	if f["tenant_id"] != "acme" {
		t.Errorf("team-scoped space-wide filter = %v, want a tenant_id clause", f)
	}
	if f["space_id"] != org.ID() {
		t.Errorf("filter lost the space id: %v", f)
	}

	global := knowledge.SpaceRef{Visibility: knowledge.VisibilityGlobal, Name: "catalogue"}
	if _, present := spaceDocFilter(global)["tenant_id"]; present {
		t.Errorf("the global space's documents carry no tenant_id: a clause here reads an empty catalogue: %v", spaceDocFilter(global))
	}
}

// TestGlobalWriteIsMetered_Mongo is the unmetered half of #2400, measured on
// a real server: a global write of N bytes must move the instance counter AND
// the writing org's, and the instance ceiling must refuse the write that
// would breach it.
//
// Mongo-gated, like the other rows here — skipped unless
// ITERION_TEST_MONGO_URI is set. A standalone mongod is enough:
//
//	ITERION_TEST_MONGO_URI='mongodb://localhost:27017' \
//	    devbox run -- go test ./pkg/store/mongo/ -run GlobalWriteIsMetered
func TestGlobalWriteIsMetered_Mongo(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping the Mongo metering test")
	}
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()

	cli, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	db := cli.Database("iterion_memmeter_" + bsonNonce(t))
	t.Cleanup(func() {
		dctx, dcancel := mongotest.TeardownCtx()
		defer dcancel()
		_ = db.Drop(dctx)
		_ = cli.Disconnect(dctx)
	})
	s := NewMongoMemoryStore(db)
	if err := s.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}

	usage := func(id string) int64 {
		t.Helper()
		var doc struct {
			Used int64 `bson:"used_bytes"`
		}
		if err := db.Collection(colMemoryTenantUsage).FindOne(ctx, bson.M{"_id": id}).Decode(&doc); err != nil {
			if errors.Is(err, mongo.ErrNoDocuments) {
				return 0
			}
			t.Fatalf("read usage %q: %v", id, err)
		}
		return doc.Used
	}

	// The writer is a team's run: platform authority on the ctx (the gate is
	// not what this row measures), tenant "acme" (the org to bill).
	wctx := knowledge.WithPlatformWriteAuthority(store.WithTenant(ctx, "acme"))
	ref := knowledge.SpaceRef{Visibility: knowledge.VisibilityGlobal, Name: "catalogue"}
	const n = 4096
	body := bytes.Repeat([]byte("g"), n)
	if _, err := s.WriteDocument(wctx, ref, knowledge.DocumentInput{Path: "a.md", Content: body}); err != nil {
		t.Fatalf("global write: %v", err)
	}
	if got := usage(knowledge.InstanceUsageID); got != n {
		t.Errorf("instance counter = %d, want %d — a global write met no aggregate ceiling at all before this", got, n)
	}
	if got := usage("acme"); got != n {
		t.Errorf("writing org counter = %d, want %d — the bytes stayed anonymous", got, n)
	}

	// The instance ceiling bites: cap it below the next write and the write is
	// refused with the aggregate error, and neither counter moves.
	if err := s.SetTenantQuota(ctx, knowledge.InstanceUsageID, n+1); err != nil {
		t.Fatalf("SetTenantQuota(instance): %v", err)
	}
	beforeInstance, beforeOrg := usage(knowledge.InstanceUsageID), usage("acme")
	_, err = s.WriteDocument(wctx, ref, knowledge.DocumentInput{Path: "b.md", Content: body})
	if !errors.Is(err, knowledge.ErrOrgQuotaExceeded) {
		t.Fatalf("write over the instance ceiling: err=%v, want knowledge.ErrOrgQuotaExceeded", err)
	}
	if got := usage(knowledge.InstanceUsageID); got != beforeInstance {
		t.Errorf("instance counter moved on a refused write: %d -> %d", beforeInstance, got)
	}
	if got := usage("acme"); got != beforeOrg {
		t.Errorf("the refused write still charged the org: %d -> %d — a denied aggregate must roll back the ones already bumped", beforeOrg, got)
	}

	// A delete credits both counters back.
	if err := s.DeleteDocument(wctx, ref, "a.md"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := usage(knowledge.InstanceUsageID); got != 0 {
		t.Errorf("instance counter after delete = %d, want 0", got)
	}
	if got := usage("acme"); got != 0 {
		t.Errorf("org counter after delete = %d, want 0", got)
	}
}

// TestMongoStore_GlobalMutationsAnswerToPlatformAuthority is the gate on the
// cloud adapter, host only: the refusal happens BEFORE any Mongo I/O, which
// is what makes it reachable from the engine's path (no server, no grant).
//
// The store is real but points at a port nothing listens on, and the second
// arm is what makes the first one mean something: an ungranted mutation must
// return the TYPED refusal immediately, while a GRANTED one must fail with a
// DIFFERENT, I/O-shaped error. A store that simply refused everything would
// pass the first arm and fail the second.
func TestMongoStore_GlobalMutationsAnswerToPlatformAuthority(t *testing.T) {
	cli, err := mongo.Connect(options.Client().
		ApplyURI("mongodb://127.0.0.1:1/").
		SetServerSelectionTimeout(300 * time.Millisecond))
	if err != nil {
		t.Fatalf("build an unconnected client: %v", err)
	}
	t.Cleanup(func() {
		dctx, dcancel := mongotest.TeardownCtx()
		defer dcancel()
		_ = cli.Disconnect(dctx)
	})
	s := NewMongoMemoryStore(cli.Database("iterion_gate_never_dialled"))
	ref := knowledge.SpaceRef{Visibility: knowledge.VisibilityGlobal, Name: "catalogue"}
	in := knowledge.DocumentInput{Path: "a.md", Content: []byte("x")}

	if _, err := s.WriteDocument(context.Background(), ref, in); !errors.Is(err, knowledge.ErrGlobalWriteForbidden) {
		t.Fatalf("ungranted global write: err=%v, want knowledge.ErrGlobalWriteForbidden before any I/O", err)
	}
	if err := s.DeleteDocument(context.Background(), ref, "a.md"); !errors.Is(err, knowledge.ErrGlobalWriteForbidden) {
		t.Fatalf("ungranted global delete: err=%v, want knowledge.ErrGlobalWriteForbidden before any I/O", err)
	}

	granted := knowledge.WithPlatformWriteAuthority(context.Background())
	_, gerr := s.WriteDocument(granted, ref, in)
	if gerr == nil {
		t.Fatal("premise lost: the unreachable store accepted a write, so the arms below prove nothing")
	}
	if errors.Is(gerr, knowledge.ErrGlobalWriteForbidden) {
		t.Fatalf("a granted write was refused by the gate: %v — the capability is gone, not gated", gerr)
	}
	// A team-scoped mutation needs no grant and must reach I/O the same way.
	orgRef := knowledge.SpaceRef{Visibility: knowledge.VisibilityOrg, TenantID: "acme", Name: "conventions"}
	if _, oerr := s.WriteDocument(context.Background(), orgRef, in); errors.Is(oerr, knowledge.ErrGlobalWriteForbidden) {
		t.Fatalf("a team-scoped write was gated: %v", oerr)
	}
}
