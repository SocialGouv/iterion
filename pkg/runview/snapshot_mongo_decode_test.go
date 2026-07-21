package runview

import (
	"bytes"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/SocialGouv/iterion/pkg/store"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
)

// mongoDecoded round-trips an event through the cloud store's exact
// encode+decode pair: driver marshal (the wire bytes AppendEvent's
// InsertOne sends) and a bson.Decoder with the store client's registry
// (mongostore.Registry) — Cursor.Decode's construction. The reducers
// below therefore see events shaped exactly as BuildSnapshot sees them
// when the server scans a cloud run, not a hand-built map[string]any.
func mongoDecoded(t *testing.T, e *store.Event) *store.Event {
	t.Helper()
	raw, err := bson.Marshal(e)
	if err != nil {
		t.Fatalf("marshal event seq %d: %v", e.Seq, err)
	}
	dec := bson.NewDecoder(bson.NewDocumentReader(bytes.NewReader(raw)))
	dec.SetRegistry(mongostore.Registry())
	var out store.Event
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("decode event seq %d: %v", e.Seq, err)
	}
	return &out
}

// TestSnapshotReducers_CloudDecodeShape drives the snapshot reducers
// with events in the cloud store's decode shape. Every assertion here
// names a reducer that reads structured event data: backends_used and
// the deployment card (Data["output"] map), loop bounds (Data["loops"]
// map + int), iteration resolution and artifact versions (wire-int32
// scalars). All of them must produce identical snapshots for a run
// read from the filesystem store and from Mongo.
func TestSnapshotReducers_CloudDecodeShape(t *testing.T) {
	b := NewSnapshotBuilder(&store.Run{ID: "r-cloud", Status: store.RunStatusRunning})
	events := []*store.Event{
		evt(0, store.EventRunStarted, "", "", map[string]any{
			"loops": map[string]any{"fix": 4},
		}),
		evt(1, store.EventNodeStarted, "", "implement", map[string]any{
			"kind":      "agent",
			"iteration": 2,
		}),
		evt(2, store.EventArtifactWritten, "", "implement", map[string]any{
			"version": 1,
		}),
		evt(3, store.EventNodeFinished, "", "implement", map[string]any{
			"output": map[string]any{
				"_backend": "claude_code",
				"_model":   "claude-opus-4-8",
				"summary":  "done",
			},
		}),
		evt(4, store.EventNodeStarted, "", "deploy", map[string]any{"kind": "tool"}),
		evt(5, store.EventNodeFinished, "", "deploy", map[string]any{
			"output": map[string]any{
				"deployed_url":    "https://app.example.test",
				"deployed":        true,
				"healthy":         true,
				"image_ref":       "ghcr.io/acme/app@sha256:abc",
				"commit":          "deadbeef",
				"verifiable":      true,
				"pushed":          true,
				"image_from_repo": true,
				"built_from_head": true,
			},
		}),
	}
	for _, e := range events {
		b.Apply(mongoDecoded(t, e))
	}
	snap := b.Snapshot()

	if len(snap.Run.BackendsUsed) != 1 {
		t.Fatalf("BackendsUsed = %+v, want exactly one (backend usage reducer dropped the node_finished output)", snap.Run.BackendsUsed)
	}
	bu := snap.Run.BackendsUsed[0]
	if bu.Backend != "claude_code" || bu.Model != "claude-opus-4-8" || bu.NodeCount != 1 {
		t.Errorf("BackendsUsed[0] = %+v, want claude_code/claude-opus-4-8/1", bu)
	}

	dep := snap.Run.Deployment
	if dep == nil {
		t.Fatal("Deployment = nil, want the deploy node's report (deployment reducer dropped the node_finished output)")
	}
	if dep.URL != "https://app.example.test" || !dep.Healthy || !dep.Deployed {
		t.Errorf("Deployment = %+v, want URL/healthy/deployed from the deploy output", dep)
	}
	if dep.ImageRef != "ghcr.io/acme/app@sha256:abc" || dep.Commit != "deadbeef" {
		t.Errorf("Deployment image/commit = %q/%q, want ghcr ref + deadbeef", dep.ImageRef, dep.Commit)
	}
	if dep.Trace == nil {
		t.Fatal("Deployment.Trace = nil, want the traceability verdict")
	}
	if !dep.Trace.Verifiable || !dep.Trace.Pushed || !dep.Trace.ImageFromRepo || !dep.Trace.BuiltFromHead {
		t.Errorf("Deployment.Trace = %+v, want all verdict facts true", dep.Trace)
	}

	if lp, ok := snap.Run.Loops["fix"]; !ok || lp.Max != 4 {
		t.Errorf("Loops[fix] = %+v (present=%v), want Max 4", lp, ok)
	}

	var implement *ExecutionState
	for i := range snap.Executions {
		if snap.Executions[i].IRNodeID == "implement" {
			implement = &snap.Executions[i]
		}
	}
	if implement == nil {
		t.Fatal("no execution recorded for implement")
	}
	if implement.LoopIteration != 2 {
		t.Errorf("implement LoopIteration = %d, want 2 (runtime-supplied iteration field dropped)", implement.LoopIteration)
	}
	if implement.LastArtifactVersion == nil || *implement.LastArtifactVersion != 1 {
		t.Errorf("implement LastArtifactVersion = %v, want 1", implement.LastArtifactVersion)
	}
}
