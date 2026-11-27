package runner

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/cloud/metrics"
	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
)

// F31, the runner's half, on a real replica set with two clients — the
// runner's and the server's. A started run meets its provider's usage window:
// the runner's park records the delivered program on the attempt (q1) and
// arms the retry. The sweeper's probe pulls it forward and re-admits it, which
// moves the attempt (q2) and leaves the record stale. The usage preflight
// refuses that re-admission before anything spends and the runner parks it
// again, free of charge: the park records the program on q2, so the record
// still binds the run at the next sweep (the sweeper half, a bound usage
// record resumed frozen when the bot changed, is F30 in pkg/server). Skipped
// without ITERION_TEST_MONGO_URI. Red when the free re-arm after a probe
// records nothing (the record stays on q1, stale).
func TestUsageParkFrozenProgramFollowsTheProbedAttemptMongo(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping the runner's Mongo usage-park frozen-program test")
	}
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	dbName := "iterion_runner_usage_frozen_" + strings.ReplaceAll(time.Now().UTC().Format("20060102150405.000000000"), ".", "_")
	blobs := &irRunnerBlob{}
	runnerStore, err := mongostore.New(ctx, mongostore.Config{URI: uri, Database: dbName, Blob: blobs})
	if err != nil {
		t.Fatalf("mongostore.New (runner): %v", err)
	}
	server, err := mongostore.New(ctx, mongostore.Config{URI: uri, Database: dbName, Blob: blobs})
	if err != nil {
		t.Fatalf("mongostore.New (server): %v", err)
	}
	t.Cleanup(func() {
		dctx, dcancel := mongotest.TeardownCtx()
		defer dcancel()
		_ = server.DB().Drop(dctx)
		_ = server.Close(dctx)
		_ = runnerStore.Close(dctx)
	})
	rctx := idCtx()
	const runID = "run-usage-frozen"
	body, hash := frozenTransport(t)
	load := func(t *testing.T) *store.Run {
		t.Helper()
		r, err := server.LoadRun(rctx, runID)
		if err != nil {
			t.Fatalf("LoadRun: %v", err)
		}
		return r
	}

	// Launched, started (a checkpoint), failed on the usage window at q1.
	if _, err := server.CreateRun(rctx, runID, "frozen", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	seed := load(t)
	seed.Status = store.RunStatusRunning
	seed.RetryPolicy = &store.RunRetryPolicy{UsageWindow: "resume", MaxAttempts: 5, MaxWait: "192h", Jitter: "0s"}
	if err := server.SaveRun(rctx, seed); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	if err := server.FailRunResumable(rctx, runID, &store.Checkpoint{NodeID: "one"}, "usage window", store.FailureUsageLimitBlocked); err != nil {
		t.Fatalf("usage window: %v", err)
	}
	q1 := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	if _, err := server.DB().Collection("runs").UpdateOne(ctx, map[string]any{"_id": runID},
		map[string]any{"$set": map[string]any{"queued_at": q1, "workflow_hash": hash}}); err != nil {
		t.Fatalf("seed the attempt marker and the hash: %v", err)
	}

	reg := metrics.New()
	r := &Runner{cfg: Config{Store: runnerStore, Logger: iterlog.Nop(), Metrics: reg}}
	logger := iterlog.New(iterlog.LevelError, io.Discard)
	wall := time.Now().UTC().Add(4 * 24 * time.Hour)
	delivery := func(publishedAt time.Time) *queue.RunMessage {
		return &queue.RunMessage{RunID: runID, TenantID: "team-1", OwnerID: "u1",
			WorkflowName: "frozen", WorkflowHash: hash, IRCompiled: body,
			PublishedAtRFC: publishedAt.UTC().Format(time.RFC3339Nano)}
	}
	park := func(t *testing.T, msg *queue.RunMessage) {
		t.Helper()
		d := &fakeDelivery{}
		usage := newMetricsEmitter(&recordingEmitter{}, metrics.New(), nil, nil)
		if handled, status := r.parkUsageLimitRetry(rctx, weeklyWindowErr(wall), d, msg, usage, logger); !handled || status != "usage_limit_retry" || d.acks != 1 {
			t.Fatalf("park: handled=%v status=%q acks=%d, want an acked usage-window park", handled, status, d.acks)
		}
	}
	bound := func(t *testing.T, want time.Time) *store.FrozenProgram {
		t.Helper()
		run := load(t)
		fp, verdict := run.ReadmitFrozen()
		if verdict != store.FrozenBound || fp == nil || fp.Source != store.FrozenProgramSourceUsageWindow || !fp.QueuedAt.Equal(want) {
			t.Fatalf("the run reads %s (%+v), want its usage record bound to the attempt %v", verdict, fp, want)
		}
		return fp
	}

	park(t, delivery(q1.Add(time.Second)))
	bound(t, q1)
	if run := load(t); run.RetryState == nil || run.RetryState.Attempts != 1 || run.RetryState.RetryAfter == nil {
		t.Fatalf("after the first park the retry reads %+v, want armed on its first attempt", run.RetryState)
	}

	// The sweeper's probe pulls the wait forward, claims it and re-admits the
	// run: the flip stamps a new attempt, and the record no longer binds.
	armed := load(t).RetryState.RetryAfter.UTC()
	probeAt := time.Now().UTC().Truncate(time.Millisecond)
	if ok, err := server.PullRunRetry(rctx, runID, armed, probeAt); err != nil || !ok {
		t.Fatalf("PullRunRetry: %v %v", ok, err)
	}
	if ok, err := server.ClaimRunRetry(rctx, runID, probeAt); err != nil || !ok {
		t.Fatalf("ClaimRunRetry: %v %v", ok, err)
	}
	flip, ok, err := server.FlipToQueued(rctx, runID, store.RunStatusFailedResumable, time.Now())
	if err != nil || !ok {
		t.Fatalf("FlipToQueued: %v %v", ok, err)
	}
	if err := server.ClearRunRetry(rctx, runID); err != nil {
		t.Fatalf("ClearRunRetry: %v", err)
	}
	if _, v := load(t).ReadmitFrozen(); v != store.FrozenStale {
		t.Fatalf("after the re-admission's flip the run reads %s, want its q1 record stale", v)
	}

	// The re-admission's delivery: the usage preflight refuses before
	// anything spends, the run is failed_resumable on the attempt q2, and the
	// runner parks it again.
	if ok, err := server.UpdateRunOutcome(rctx, runID, store.RunStatusFailedResumable, "usage window",
		store.RunOutcomeMeta{Code: store.FailureUsageLimitBlocked}, []store.RunStatus{store.RunStatusQueued}); err != nil || !ok {
		t.Fatalf("preflight refusal: %v %v", ok, err)
	}
	if run := load(t); run.QueuedAt == nil || !run.QueuedAt.Equal(flip.At) || !run.RetryState.ProbeInFlight(time.Now().UTC()) {
		t.Fatalf("precondition: queued_at %v (flip %v), retry %+v — want the re-admission's attempt with its probe in flight", run.QueuedAt, flip.At, run.RetryState)
	}
	park(t, delivery(flip.At.Add(time.Millisecond)))

	fp := bound(t, flip.At)
	after := load(t)
	if after.RetryState == nil || after.RetryState.Attempts != 1 || after.RetryState.ProbeFrom != nil {
		t.Fatalf("after the probed park the retry reads %+v, want re-armed free of charge, its marker consumed", after.RetryState)
	}
	env, err := runview.LoadFrozenProgram(rctx, server, runID, fp)
	if err != nil {
		t.Fatalf("LoadFrozenProgram: %v", err)
	}
	if string(env.IR) != string(body) || env.WorkflowHash != hash {
		t.Fatal("the envelope is not the delivered program")
	}
	if n := counterValue(t, reg.CapacityFrozenProgramTotal.WithLabelValues(store.FrozenProgramSourceUsageWindow, metrics.FrozenProgramWritten)); n != 2 {
		t.Fatalf("written count %v, want one per park", n)
	}
}
