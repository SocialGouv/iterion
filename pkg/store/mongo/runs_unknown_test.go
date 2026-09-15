package mongo

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestSaveRunPreservesFutureStateAcrossOldReaderRename(t *testing.T) {
	s := retryTestStore(t)
	ctx := retryCtx()
	const id = "future-state"
	if _, err := s.CreateRun(ctx, id, "future-workflow", nil); err != nil {
		t.Fatal(err)
	}
	// A newer replica writes fields this reader's Run/Checkpoint/branch
	// structs do not know, without changing the additive schema version.
	filter := withTenantFilter(ctx, bson.M{"_id": id})
	if _, err := s.runs.UpdateOne(ctx, filter, bson.M{"$set": bson.M{
		"future_run_state": bson.D{{Key: "$expression", Value: "$must-stay-literal"}, {Key: "dotted.key", Value: bson.Binary{Subtype: 0, Data: []byte{0, 1, 255}}}},
		"future_counter":   int32(7),
		"error":            "clear me deliberately",
		"checkpoint": bson.M{
			"node_id":                 "a",
			"future_checkpoint_state": "$future.checkpoint",
			"outputs":                 bson.M{"obsolete": bson.M{"value": "remove me"}},
			"selected_incoming": bson.M{"join": bson.A{
				bson.M{"from": "a", "to": "join", "future_edge_state": "state-a"},
				bson.M{"from": "b", "to": "join", "future_edge_state": "state-b"},
			}},
			"parallel": bson.M{
				"router_node_id": "fan",
				"branches": bson.M{
					"branch-a": bson.M{"branch_id": "branch-a", "future_branch_state": "keep branch extension"},
					"removed":  bson.M{"branch_id": "removed", "future_branch_state": "do not resurrect branch"},
				},
			},
		},
	}, "$inc": bson.M{"version": 1}}); err != nil {
		t.Fatal(err)
	}
	before, err := s.runs.FindOne(ctx, filter).Raw()
	if err != nil {
		t.Fatal(err)
	}
	oldReader, err := s.LoadRun(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	oldReader.Name = "renamed by the old control plane"
	oldReader.Error = ""
	delete(oldReader.Checkpoint.Outputs, "obsolete")
	delete(oldReader.Checkpoint.Parallel.Branches, "removed")
	edges := oldReader.Checkpoint.SelectedIncoming["join"]
	edges[0], edges[1] = edges[1], edges[0]
	if err := s.SaveRun(ctx, oldReader); err != nil {
		t.Fatal(err)
	}
	after, err := s.runs.FindOne(ctx, filter).Raw()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range [][]string{{"future_run_state"}, {"future_counter"}, {"checkpoint", "future_checkpoint_state"}, {"checkpoint", "parallel", "branches", "branch-a", "future_branch_state"}} {
		want, got := before.Lookup(path...), after.Lookup(path...)
		if got.Type != want.Type || !bytes.Equal(got.Value, want.Value) {
			t.Errorf("future state %v lost or changed: got %v, want %v", path, got, want)
		}
	}
	for i, want := range []string{"state-b", "state-a"} {
		got := after.Lookup("checkpoint", "selected_incoming", "join").Array().Index(uint(i)).Document().Lookup("future_edge_state")
		if got.Type != bson.TypeString || got.StringValue() != want {
			t.Errorf("edge %d after reorder has %v, want %s", i, got, want)
		}
	}
	for _, path := range [][]string{{"error"}, {"checkpoint", "outputs", "obsolete"}, {"checkpoint", "parallel", "branches", "removed"}} {
		if got := after.Lookup(path...); got.Type != 0 {
			t.Errorf("deliberately removed field %v was resurrected: %v", path, got)
		}
	}
	if got := after.Lookup("name").StringValue(); got != oldReader.Name {
		t.Fatalf("rename did not land: %s", got)
	}
	// Clearing the checkpoint is a deliberate structural change, not an old
	// reader's omission of one of its fields: the whole subtree must go.
	oldReader.Checkpoint = nil
	if err := s.SaveRun(ctx, oldReader); err != nil {
		t.Fatal(err)
	}
	after, err = s.runs.FindOne(ctx, filter).Raw()
	if err != nil {
		t.Fatal(err)
	}
	if got := after.Lookup("checkpoint"); got.Type != 0 {
		t.Fatalf("cleared checkpoint resurrected: %v", got)
	}
	if _, err := s.LoadRun(ctx, id); err != nil {
		t.Fatal(err)
	}
}

func TestSaveRunRefusesStaleCopyWithFutureState(t *testing.T) {
	s := retryTestStore(t)
	ctx := retryCtx()
	run, err := s.CreateRun(ctx, "stale-future-state", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	filter := withTenantFilter(ctx, bson.M{"_id": run.ID})
	if _, err := s.runs.UpdateOne(ctx, filter, bson.M{
		"$set": bson.M{"name": "newer writer", "future_state": "newer state"},
		"$inc": bson.M{"version": 1},
	}); err != nil {
		t.Fatal(err)
	}
	run.Name = "stale rename"
	if err := s.SaveRun(ctx, run); !errors.Is(err, store.ErrRunConflict) {
		t.Fatalf("stale SaveRun = %v, want conflict", err)
	}
	after, err := s.runs.FindOne(ctx, filter).Raw()
	if err != nil {
		t.Fatal(err)
	}
	if after.Lookup("name").StringValue() != "newer writer" || after.Lookup("future_state").StringValue() != "newer state" {
		t.Fatalf("stale save changed the newer document: %v", after)
	}
}

func TestSaveRunRefusesFutureSchema(t *testing.T) {
	s := retryTestStore(t)
	ctx := retryCtx()
	run, err := s.CreateRun(ctx, "future-schema", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	filter := withTenantFilter(ctx, bson.M{"_id": run.ID})
	// A migration changes the schema after this replica loaded the run.
	// Even without a CAS-version bump it must not downgrade the document.
	if _, err := s.runs.UpdateOne(ctx, filter, bson.M{"$set": bson.M{"v": SchemaVersion + 1}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveRun(ctx, run); err == nil || !strings.Contains(err.Error(), "upgrade required") {
		t.Fatalf("SaveRun against a future schema = %v", err)
	}
	after, err := s.runs.FindOne(ctx, filter).Raw()
	if err != nil {
		t.Fatal(err)
	}
	if got := after.Lookup("v").AsInt64(); got != SchemaVersion+1 {
		t.Fatalf("schema downgraded to %d", got)
	}
}
