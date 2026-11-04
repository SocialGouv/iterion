package mongo

import (
	"context"
	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/storetest"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/event"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"os"
	"strings"
	"testing"
)

func aliveTestStore(t *testing.T) *Store {
	t.Helper()
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; team cutover Mongo conformance unverified")
	}
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	s, err := New(ctx, Config{URI: uri, Database: "iterion_alive_" + bsonNonce(t), Blob: newInMemoryBlob()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ctx, cancel := mongotest.TeardownCtx(); defer cancel(); _ = s.db.Drop(ctx); _ = s.Close(ctx) })
	return s
}
func TestTeamAliveRunsMongo(t *testing.T) { storetest.TeamAliveRuns(t, aliveTestStore(t)) }

func TestTeamAliveRunsMongoProjectionAndIndex(t *testing.T) {
	s := aliveTestStore(t)
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	// A malformed payload must not matter to a projected metadata query. Tombstones
	// and another tenant must still be excluded before projection.
	docs := []any{
		bson.M{"_id": "alive", "tenant_id": "team", "status": "running", "inputs": 42},
		bson.M{"_id": "deleted", "tenant_id": "team", "status": "running", "deleted_at": true},
	}
	for i := 0; i < 500; i++ {
		docs = append(docs, bson.M{"_id": bson.NewObjectID().Hex(), "tenant_id": "foreign", "status": "running"})
	}
	if _, err := s.runs.InsertMany(ctx, docs); err != nil {
		t.Fatal(err)
	}
	commands := make(chan bson.Raw, 1)
	client, err := mongo.Connect(options.Client().ApplyURI(os.Getenv("ITERION_TEST_MONGO_URI")).SetMonitor(&event.CommandMonitor{Started: func(_ context.Context, e *event.CommandStartedEvent) {
		if e.CommandName == "find" && e.DatabaseName == s.db.Name() {
			select {
			case commands <- append(bson.Raw(nil), e.Command...):
			default:
			}
		}
	}}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })
	s.runs = client.Database(s.db.Name()).Collection(colRuns)
	got, err := s.ListAliveRunsForTeam(store.WithTenant(ctx, "team"), "team")
	if err != nil || len(got) != 1 || got[0] != "alive" {
		t.Fatalf("projected alive query = %v,%v", got, err)
	}
	var command bson.Raw
	select {
	case command = <-commands:
	default:
		t.Fatal("no real find command observed")
	}
	var projection bson.M
	if err := bson.Unmarshal(command.Lookup("projection").Document(), &projection); err != nil {
		t.Fatal(err)
	}
	if len(projection) != 2 || projection["_id"] != int32(1) || projection["status"] != int32(1) {
		t.Fatalf("query loads payload: %v", projection)
	}
	// Explain the actual emitted filter and projection, with no hint: an
	// existing tenant/status index must support the team's bounded question.
	find := bson.D{{Key: "find", Value: colRuns}, {Key: "filter", Value: command.Lookup("filter").Document()}, {Key: "projection", Value: command.Lookup("projection").Document()}}
	explained, err := s.db.RunCommand(ctx, bson.D{{Key: "explain", Value: find}, {Key: "verbosity", Value: "executionStats"}}).Raw()
	if err != nil {
		t.Fatal(err)
	}
	plan := explained.Lookup("queryPlanner", "winningPlan").String()
	if strings.Contains(plan, "COLLSCAN") || !strings.Contains(plan, "tenant_status_created") {
		t.Fatalf("unbounded team query: %s", plan)
	}
	if examined := explained.Lookup("executionStats", "totalDocsExamined").AsInt64(); examined > 2 {
		t.Fatalf("team query examined %d docs", examined)
	}
}

func TestTeamAliveRunsMongoUnknownMetadataRefuses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  any
		unknown bool
	}{{"missing", nil, true}, {"typo", "runing", true}, {"number", 42, true}, {"null", nil, true}, {"object", bson.M{"value": "finished"}, true}, {"mixed-array", bson.A{"running", "finished"}, true}, {"parked-array", bson.A{"finished"}, true}, {"finished", "finished", false}} {
		t.Run(tc.name, func(t *testing.T) {
			s := aliveTestStore(t)
			ctx := store.WithTenant(context.Background(), "team")
			doc := bson.M{"_id": "unknown", "tenant_id": "team"}
			if tc.status != nil || tc.name == "null" {
				doc["status"] = tc.status
			}
			if _, e := s.runs.InsertOne(ctx, doc); e != nil {
				t.Fatal(e)
			}
			ids, e := s.ListAliveRunsForTeam(ctx, "team")
			t.Logf("doc=%v ids=%q err=%v", doc, ids, e)
			if tc.unknown && e == nil {
				t.Fatalf("unknown persistent metadata became drained")
			}
			if !tc.unknown && e != nil {
				t.Fatal(e)
			}
		})
	}
}
