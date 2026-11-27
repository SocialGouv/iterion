package mongo

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// TestEnsureSchema_OneStaleIndexDoesNotDropTheOthers pins ensureIndexes on a
// real mongod: createIndexes refuses a whole batch when ONE of its indexes
// conflicts with an existing one, and a conflict tolerated on the batch used
// to leave every other index of that batch uncreated, silently — any index
// added after a stale spec appeared would then never land, and the query it
// serves would fall back to a collection scan with nothing logged (found
// while fixing #2636). Seeds the runs collection with an index that has
// updated_desc's keys under another name, opens the store, and expects every
// other runs index present and the conflict named in the log.
func TestEnsureSchema_OneStaleIndexDoesNotDropTheOthers(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping the index-conflict test")
	}
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	dbName := "iterion_idxconflict_" + bsonNonce(t)

	// Pre-seed through a bare client: the store under test must find the
	// stale index already there.
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })
	stale := client.Database(dbName).Collection(colRuns)
	if _, err := stale.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "updated_at", Value: -1}},
		Options: options.Index().SetName("updated_at_-1"),
	}); err != nil {
		t.Fatalf("seed stale index: %v", err)
	}

	var log bytes.Buffer
	s, err := New(ctx, Config{URI: uri, Database: dbName, Blob: newInMemoryBlob(), Logger: iterlog.New(iterlog.LevelInfo, &log)})
	if err != nil {
		t.Fatalf("New with a stale index present: %v", err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })

	cur, err := s.runs.Indexes().List(ctx)
	if err != nil {
		t.Fatalf("list indexes: %v", err)
	}
	var specs []bson.M
	if err := cur.All(ctx, &specs); err != nil {
		t.Fatalf("read indexes: %v", err)
	}
	names := map[string]bool{}
	for _, spec := range specs {
		if n, ok := spec["name"].(string); ok {
			names[n] = true
		}
	}
	for _, want := range []string{"status_created", "tenant_status_created_id_desc", "tenant_workflow_created_id_desc", "runner_id_partial", "tenant_source_issue_created", "tenant_parent_run_created"} {
		if !names[want] {
			t.Errorf("index %s is missing after EnsureSchema met one stale index — the batch's conflict dropped it", want)
		}
	}
	if names["updated_desc"] {
		t.Errorf("updated_desc was created over the stale updated_at_-1 — mongod refuses that; the test premise is wrong")
	}
	if out := log.String(); !strings.Contains(out, "updated_desc") || !strings.Contains(out, "conflicts with an existing index") {
		t.Errorf("the conflict was not named in the log: %q", out)
	}
}

// TestEnsureSchema_DropsTheSupersededStatusIndex opens the store on a runs
// collection that still carries tenant_status_created, as a deployment that
// predates tenant_status_created_id_desc does: the schema ensure drops it
// once its successor exists, and a second ensure (nothing left to drop) is a
// no-op.
func TestEnsureSchema_DropsTheSupersededStatusIndex(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping the superseded-index test")
	}
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	dbName := "iterion_idxsuperseded_" + bsonNonce(t)
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })
	if _, err := client.Database(dbName).Collection(colRuns).Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "tenant_id", Value: 1}, {Key: "status", Value: 1}, {Key: "created_at", Value: -1}},
		Options: options.Index().SetName("tenant_status_created").SetPartialFilterExpression(bson.M{"tenant_id": bson.M{"$exists": true}}),
	}); err != nil {
		t.Fatalf("seed the superseded index: %v", err)
	}
	s, err := New(ctx, Config{URI: uri, Database: dbName, Blob: newInMemoryBlob()})
	if err != nil {
		t.Fatalf("New with the superseded index present: %v", err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	names := func() map[string]bool {
		t.Helper()
		specs, err := s.runs.Indexes().ListSpecifications(ctx)
		if err != nil {
			t.Fatalf("list indexes: %v", err)
		}
		out := map[string]bool{}
		for _, spec := range specs {
			out[spec.Name] = true
		}
		return out
	}
	if got := names(); got["tenant_status_created"] || !got["tenant_status_created_id_desc"] {
		t.Fatalf("after EnsureSchema: tenant_status_created present=%v, tenant_status_created_id_desc present=%v — want the superseded index gone and its successor there", got["tenant_status_created"], got["tenant_status_created_id_desc"])
	}
	if err := s.EnsureSchema(ctx, 0); err != nil {
		t.Fatalf("a second EnsureSchema, with nothing left to drop: %v", err)
	}
	if got := names(); got["tenant_status_created"] || !got["tenant_status_created_id_desc"] {
		t.Fatalf("after a second EnsureSchema: the index set changed: %v", got)
	}
}
