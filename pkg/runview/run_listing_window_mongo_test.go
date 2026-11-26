package runview

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
)

// listingFutureSchemaID is a Mongo-only fixture run whose document carries a
// schema version this binary does not know: LoadRunForListing refuses it, so
// the window must report it, never list it.
const listingFutureSchemaID = "r13"

// scanOnlyStore hides the Mongo store's indexed window, so a Service over it
// takes the id-by-id scan — today's path, on the very same documents. It keeps
// the projected per-id load, which that path uses.
type scanOnlyStore struct {
	store.RunStore
	listing store.RunListingStore
}

func (s scanOnlyStore) LoadRunForListing(ctx context.Context, id string) (*store.Run, error) {
	return s.listing.LoadRunForListing(ctx, id)
}

// TestRunListingWindowNativeMongo is the Mongo row of the listing
// equivalence: the scan path and the window path over one Mongo database
// holding runs of both value formats, two teams and a tenant-less run, a
// three-way created_at tie, a deleted run and two undecodable documents.
// CI's mongo-conformance job supplies the URI.
func TestRunListingWindowNativeMongo(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set")
	}
	ctx := t.Context()
	s, err := mongostore.New(ctx, mongostore.Config{URI: uri, Database: fmt.Sprintf("listing_window_%d", time.Now().UnixNano()), Blob: snapshotExactBlob{}, RunFilesScratchDir: filepath.Join(t.TempDir(), "scratch")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = s.DB().Drop(c)
		_ = s.Close(c)
	})
	seedListingMongo(t, ctx, s)

	scan, err := NewService(t.TempDir(), WithStore(scanOnlyStore{RunStore: s, listing: s}), WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("NewService(scan): %v", err)
	}
	if store.AsRunWindowListingStore(scan.store) != nil {
		t.Fatal("the scan service still sees the window — the comparison would compare the window with itself")
	}
	window, err := NewService(t.TempDir(), WithStore(s), WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("NewService(window): %v", err)
	}
	if rows := assertListingsAgree(t, scan, window, store.WithIdentity(ctx, "team-a", "owner")); rows == 0 {
		t.Fatal("the window path listed no row at all — the fixture is not exercised")
	}

	// The store's own answer: the deleted run is absent (not skipped), each
	// undecodable document is reported by id, and a read whose limit the
	// skipped documents ate continues until it holds Limit records.
	blind := store.TeamBlind(ctx)
	for _, tc := range []struct {
		limit       int
		wantIDs     []string
		wantSkipped []string
	}{
		{0, []string{"r12", "r09", "r08", "r07", "r06", "r05", "r04", "r03", "r02", "r01"}, []string{listingFutureSchemaID, listingCorruptID}},
		{2, []string{"r12", "r09"}, []string{listingFutureSchemaID, listingCorruptID}},
		{1, []string{"r12"}, []string{listingFutureSchemaID}},
	} {
		runs, skipped, err := s.ListRunWindow(blind, store.RunWindow{Limit: tc.limit})
		if err != nil {
			t.Fatalf("ListRunWindow(limit %d): %v", tc.limit, err)
		}
		var skippedIDs []string
		for _, sk := range skipped {
			skippedIDs = append(skippedIDs, sk.ID)
			if sk.Err == nil {
				t.Errorf("ListRunWindow(limit %d): skipped %s carries no error", tc.limit, sk.ID)
			}
		}
		if !slices.Equal(runIDs(runs), tc.wantIDs) || !slices.Equal(skippedIDs, tc.wantSkipped) {
			t.Errorf("ListRunWindow(limit %d) = %v skipped %v, want %v skipped %v", tc.limit, runIDs(runs), skippedIDs, tc.wantIDs, tc.wantSkipped)
		}
	}
}

// seedListingMongo writes the shared fixture through the store's own create
// path — each run in its fixture value format, stamped with its team — then
// sets the listing fields on the document, deletes the deleted run and
// corrupts the undecodable ones.
func seedListingMongo(t *testing.T, ctx context.Context, s *mongostore.Store) {
	t.Helper()
	teamCtx := func(team string) context.Context {
		if team == "" {
			return store.TeamBlind(ctx)
		}
		return store.WithIdentity(ctx, team, "owner")
	}
	seeds := append(listingSeeds(), listingSeed{listingFutureSchemaID, "team-b", "finished", "wf-x", "", "", 9*time.Second + 500*time.Millisecond, 9 * time.Second, 1})
	for _, sd := range seeds {
		if _, err := s.CreateRunWithValueFormat(teamCtx(sd.team), store.RunCreateSpec{ID: sd.id, WorkflowName: sd.workflow, Format: sd.format}); err != nil {
			t.Fatalf("create %s: %v", sd.id, err)
		}
		set := bson.M{
			"status":     sd.status,
			"created_at": listingBase.Add(sd.created),
			"updated_at": listingBase.Add(sd.updated),
		}
		if sd.repo != "" {
			set["project_path"] = sd.repo
		}
		if sd.bundle != "" {
			set["bundle_name"] = sd.bundle
		}
		if sd.id == listingCorruptID {
			set["name"] = bson.A{"not", "a", "string"}
		}
		if sd.id == listingFutureSchemaID {
			set["v"] = mongostore.SchemaVersion + 1
		}
		res, err := s.RunsCollection().UpdateOne(ctx, bson.M{"_id": sd.id}, bson.M{"$set": set})
		if err != nil || res.MatchedCount != 1 {
			t.Fatalf("set listing fields on %s: matched %v, %v", sd.id, res, err)
		}
	}
	seedListingNodeEvents(t, teamCtx("team-a"), s)
	if err := s.DeleteRun(teamCtx("team-a"), listingDeletedID); err != nil {
		t.Fatalf("delete %s: %v", listingDeletedID, err)
	}
}
