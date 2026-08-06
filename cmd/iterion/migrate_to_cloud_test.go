package main

import (
	"context"
	"os"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// `iterion migrate to-cloud` is a one-way door: after it runs, the cloud
// store is what the studio, `iterion resume` and the retention sweepers
// read, and the local .iterion/ tree is expected to be disposable. So
// the promise is completeness — every run, every event, EVERY artifact
// version (not just the latest the index caches), every interaction —
// plus the two properties an operator relies on to run it safely: a
// --dry-run that writes nothing, and idempotency so an interrupted
// migration can simply be re-run.
//
// The walker (migrateRun) is exercised here between two real stores.
// The S3 upload it drives on the far side has its own coverage in
// pkg/store/blob/s3_roundtrip_test.go (canonical key layout, byte
// round-trip, prefix sweep) against an in-process S3 gateway.

func quietLogger() *iterlog.Logger { return iterlog.New(iterlog.LevelError, os.Stderr) }

// seedMigrationSource builds a store shaped like a real one: a run with
// a checkpointed status, several events, two nodes whose artifacts were
// re-written across versions, and a human interaction with answers.
func seedMigrationSource(t *testing.T, dir string) *store.FilesystemRunStore {
	t.Helper()
	src, err := store.New(dir)
	if err != nil {
		t.Fatalf("open source store: %v", err)
	}
	ctx := context.Background()
	r, err := src.CreateRun(ctx, "run-alpha", "review-pr", map[string]any{"pr_url": "https://example/1"})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := src.UpdateRunStatus(ctx, r.ID, store.RunStatusFinished, ""); err != nil {
		t.Fatalf("UpdateRunStatus: %v", err)
	}
	for _, e := range []store.Event{
		{Type: store.EventRunStarted, RunID: r.ID},
		{Type: store.EventNodeStarted, RunID: r.ID, NodeID: "review"},
		{Type: store.EventArtifactWritten, RunID: r.ID, NodeID: "review", Data: map[string]any{"version": 0}},
		{Type: store.EventRunFinished, RunID: r.ID},
	} {
		if _, err := src.AppendEvent(ctx, r.ID, e); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}
	// Two nodes; `review` was re-run in a loop, so v0 is history the
	// index no longer points at — precisely what a latest-only migration
	// would silently drop.
	for _, a := range []store.Artifact{
		{RunID: r.ID, NodeID: "review", Version: 0, Data: map[string]any{"verdict": "changes_requested"}, WrittenAt: time.Unix(1700000000, 0).UTC()},
		{RunID: r.ID, NodeID: "review", Version: 1, Data: map[string]any{"verdict": "approved"}, Labels: []string{"verdict"}, WrittenAt: time.Unix(1700000060, 0).UTC()},
		{RunID: r.ID, NodeID: "plan", Version: 0, Data: map[string]any{"steps": []any{"a", "b"}}, WrittenAt: time.Unix(1699999000, 0).UTC()},
	} {
		a := a
		if err := src.WriteArtifact(ctx, &a); err != nil {
			t.Fatalf("WriteArtifact %s/v%d: %v", a.NodeID, a.Version, err)
		}
	}
	answered := time.Unix(1700000120, 0).UTC()
	if err := src.WriteInteraction(ctx, &store.Interaction{
		ID: "int-1", RunID: r.ID, NodeID: "gate",
		RequestedAt: time.Unix(1700000100, 0).UTC(), AnsweredAt: &answered,
		Questions: map[string]any{"ship": "Ship it?"},
		Answers:   map[string]any{"ship": "yes"},
	}); err != nil {
		t.Fatalf("WriteInteraction: %v", err)
	}
	return src
}

func artifactVersions(t *testing.T, s store.RunStore, runID, nodeID string) []int {
	t.Helper()
	infos, err := s.ListArtifactVersions(context.Background(), runID, nodeID)
	if err != nil {
		t.Fatalf("ListArtifactVersions %s/%s: %v", runID, nodeID, err)
	}
	out := make([]int, 0, len(infos))
	for _, i := range infos {
		out = append(out, i.Version)
	}
	return out
}

// The whole run — metadata, events, every artifact version, the
// interaction — has to land on the far side, and a second pass must not
// duplicate any of it.
func TestMigrateToCloud_TransfersEveryVersionAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	src := seedMigrationSource(t, t.TempDir())
	dst, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("open destination store: %v", err)
	}

	if err := migrateRun(ctx, src, dst, "run-alpha", false, quietLogger()); err != nil {
		t.Fatalf("migrateRun: %v", err)
	}

	got, err := dst.LoadRun(ctx, "run-alpha")
	if err != nil {
		t.Fatalf("run did not land in the destination: %v", err)
	}
	if got.WorkflowName != "review-pr" || got.Status != store.RunStatusFinished {
		t.Fatalf("migrated run = %s/%s, want review-pr/finished", got.WorkflowName, got.Status)
	}
	if got.Inputs["pr_url"] != "https://example/1" {
		t.Errorf("migrated inputs = %v, want the source's", got.Inputs)
	}

	events, err := dst.LoadEvents(ctx, "run-alpha")
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	if len(events) != 4 {
		t.Fatalf("migrated %d events, want 4", len(events))
	}
	if events[0].Type != store.EventRunStarted || events[3].Type != store.EventRunFinished {
		t.Errorf("event order was not preserved: %s … %s", events[0].Type, events[3].Type)
	}

	// The history, not just the latest version.
	if vs := artifactVersions(t, dst, "run-alpha", "review"); len(vs) != 2 {
		t.Fatalf("migrated review versions = %v, want both v0 and v1 (a latest-only walk loses the loop's history)", vs)
	}
	if vs := artifactVersions(t, dst, "run-alpha", "plan"); len(vs) != 1 {
		t.Fatalf("migrated plan versions = %v, want v0", vs)
	}
	v0, err := dst.LoadArtifact(ctx, "run-alpha", "review", 0)
	if err != nil {
		t.Fatalf("LoadArtifact v0: %v", err)
	}
	if v0.Data["verdict"] != "changes_requested" {
		t.Errorf("v0 payload = %v, want the superseded verdict verbatim", v0.Data)
	}
	v1, err := dst.LoadArtifact(ctx, "run-alpha", "review", 1)
	if err != nil {
		t.Fatalf("LoadArtifact v1: %v", err)
	}
	if v1.Data["verdict"] != "approved" || len(v1.Labels) != 1 || v1.Labels[0] != "verdict" {
		t.Errorf("v1 = %v labels=%v, want the payload and labels carried over", v1.Data, v1.Labels)
	}

	ints, err := dst.ListInteractions(ctx, "run-alpha")
	if err != nil {
		t.Fatalf("ListInteractions: %v", err)
	}
	if len(ints) != 1 {
		t.Fatalf("migrated %d interactions, want 1 (the operator's answers are part of the run's record)", len(ints))
	}
	in, err := dst.LoadInteraction(ctx, "run-alpha", "int-1")
	if err != nil {
		t.Fatalf("LoadInteraction: %v", err)
	}
	if in.Answers["ship"] != "yes" || in.AnsweredAt == nil {
		t.Errorf("migrated interaction lost its answer: %+v", in)
	}

	// Second pass: idempotent (the doc is upserted, artifact versions are
	// overwritten in place — an interrupted migration is simply re-run).
	if err := migrateRun(ctx, src, dst, "run-alpha", false, quietLogger()); err != nil {
		t.Fatalf("second migrateRun: %v", err)
	}
	if vs := artifactVersions(t, dst, "run-alpha", "review"); len(vs) != 2 {
		t.Fatalf("a second pass duplicated artifact versions: %v", vs)
	}
	if vs := artifactVersions(t, dst, "run-alpha", "plan"); len(vs) != 1 {
		t.Fatalf("a second pass duplicated plan versions: %v", vs)
	}
	if ints, _ := dst.ListInteractions(ctx, "run-alpha"); len(ints) != 1 {
		t.Fatalf("a second pass duplicated interactions: %v", ints)
	}
}

// --dry-run is the rehearsal an operator runs first: it must read the
// source and write nothing at all.
func TestMigrateToCloud_DryRunWritesNothing(t *testing.T) {
	ctx := context.Background()
	src := seedMigrationSource(t, t.TempDir())
	dstDir := t.TempDir()
	dst, err := store.New(dstDir)
	if err != nil {
		t.Fatalf("open destination store: %v", err)
	}

	if err := migrateRun(ctx, src, dst, "run-alpha", true, quietLogger()); err != nil {
		t.Fatalf("dry-run migrateRun: %v", err)
	}
	if ids, err := dst.ListRuns(ctx); err != nil || len(ids) != 0 {
		t.Fatalf("dry run wrote %v to the destination (err=%v)", ids, err)
	}
	if _, err := dst.LoadRun(ctx, "run-alpha"); err == nil {
		t.Fatal("dry run persisted the run document")
	}
	// And the source is untouched — the migration never mutates it.
	if vs := artifactVersions(t, src, "run-alpha", "review"); len(vs) != 2 {
		t.Fatalf("source artifacts changed under a dry run: %v", vs)
	}
}

// A run whose id is absent must fail loudly: a silent skip would leave a
// hole in the migrated store nobody notices until the run is opened.
func TestMigrateToCloud_MissingRunIsAnError(t *testing.T) {
	ctx := context.Background()
	src := seedMigrationSource(t, t.TempDir())
	dst, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("open destination store: %v", err)
	}
	if err := migrateRun(ctx, src, dst, "run-nope", false, quietLogger()); err == nil {
		t.Fatal("migrating an unknown run reported success")
	}
}
