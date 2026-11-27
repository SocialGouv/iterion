package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/cloud/metrics"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/server/cloudpublisher"
	"github.com/SocialGouv/iterion/pkg/store"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
)

// F30 on a real replica set, through the real run service and publisher: a
// run launched on v1 (the queue captured), started, parked on a usage window
// with its program recorded the way the runner records it — frozen from the
// CAPTURED v1 message, written by the usage park's own conditional write
// (SetRunFrozenProgram) — and its retry armed; then one retry-sweeper pass.
// Unchanged bot: one publication, the bot's current program, nothing frozen
// counted. Bot pushed to v2: the current program is refused by the real
// source check, and the same pass publishes v1's program instead — one
// publication, unforced, v2's node absent, the run's attempts untouched.
// Skipped without ITERION_TEST_MONGO_URI. Red when the sweeper never falls
// back to the frozen program (v2 refused, re-armed, nothing published).
func TestSweepFrozen_UsageHeroMongo(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; the usage park's frozen re-admission is unverified on Mongo")
	}
	for _, tc := range []struct {
		name    string
		changed bool
	}{
		{"unchanged", false},
		{"changed", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := store.WithIdentity(t.Context(), "team-1", "u1")
			ms, err := mongostore.New(ctx, mongostore.Config{URI: uri, Database: fmt.Sprintf("frozen_usage_hero_%d", time.Now().UnixNano()), Blob: &heroIRBlob{}, RunFilesScratchDir: filepath.Join(t.TempDir(), "scratch")})
			if err != nil {
				t.Fatalf("mongostore.New: %v", err)
			}
			t.Cleanup(func() {
				c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
				defer cancel()
				_ = ms.DB().Drop(c)
				_ = ms.Close(c)
			})
			workDir := t.TempDir()
			botPath := filepath.Join(workDir, "bots", "hero", "main.bot")
			if err := os.MkdirAll(filepath.Dir(botPath), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(botPath, []byte(heroV1), 0o644); err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			var published []*queue.RunMessage
			pub := cloudpublisher.NewForTesting(cloudpublisher.Config{Store: ms}, func(_ context.Context, m *queue.RunMessage) error {
				mu.Lock()
				defer mu.Unlock()
				published = append(published, m)
				return nil
			})
			svc := newTestRunviewService(t, t.TempDir(), runview.WithRecoveryPassive(), runview.WithStore(ms), runview.WithLaunchPublisher(pub))
			res, err := svc.Launch(ctx, runview.LaunchSpec{FilePath: botPath})
			if err != nil {
				t.Fatalf("launch v1: %v", err)
			}
			runID := res.RunID
			mu.Lock()
			if len(published) != 1 {
				mu.Unlock()
				t.Fatalf("the launch published %d messages, want 1", len(published))
			}
			v1Msg := published[0]
			mu.Unlock()
			v1Hash, err := runview.TransportWorkflowHash(v1Msg.IRCompiled)
			if err != nil {
				t.Fatalf("the launch's IR derives no hash: %v", err)
			}

			// The runner: claims, runs node one (a checkpoint), meets the
			// provider's usage window; the park records the delivered program
			// on the attempt it belongs to, then arms the retry.
			launched, err := ms.LoadRun(ctx, runID)
			if err != nil {
				t.Fatal(err)
			}
			launched.Status = store.RunStatusRunning
			if err := ms.SaveRun(ctx, launched); err != nil {
				t.Fatalf("claim: %v", err)
			}
			if err := ms.FailRunResumable(ctx, runID, &store.Checkpoint{NodeID: "one"}, "usage window", store.FailureUsageLimitBlocked); err != nil {
				t.Fatalf("usage window: %v", err)
			}
			parked, err := ms.LoadRun(ctx, runID)
			if err != nil {
				t.Fatal(err)
			}
			if parked.QueuedAt == nil || parked.WorkflowHash != v1Hash {
				t.Fatalf("precondition: queued_at %v hash %s, want the launch's attempt and v1's hash %s", parked.QueuedAt, parked.WorkflowHash, v1Hash)
			}
			fp, err := runview.FreezeProgram(ctx, ms, v1Msg, parked, store.FrozenProgramSourceUsageWindow)
			if err != nil {
				t.Fatalf("FreezeProgram: %v", err)
			}
			if set, err := ms.SetRunFrozenProgram(ctx, runID, *parked.QueuedAt, fp); err != nil || !set {
				t.Fatalf("SetRunFrozenProgram: set=%v err=%v", set, err)
			}
			now := time.Now().UTC()
			if armed, attempt, err := ms.ScheduleRunRetry(ctx, runID, now.Add(time.Minute), time.Time{}, store.RetryReasonUsageWindow, string(store.FailureUsageLimitBlocked), 5); err != nil || !armed || attempt != 1 {
				t.Fatalf("arm: armed=%v attempt=%d err=%v", armed, attempt, err)
			}

			if tc.changed {
				if err := os.WriteFile(botPath, []byte(heroV2), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			s := &Server{cfg: Config{Store: ms, WorkDir: workDir, Metrics: metrics.New()}}
			due := now.Add(2 * time.Minute)
			s.retryClock = func() time.Time { return due }
			s.sweepDueRetries(ctx, ms, svc, due)

			mu.Lock()
			defer mu.Unlock()
			after, err := ms.LoadRun(ctx, runID)
			if err != nil {
				t.Fatal(err)
			}
			if len(published) != 2 {
				t.Fatalf("published %d messages after the sweep, want the launch and one re-admission (run %s/%s, retry %+v)", len(published), after.Status, after.Error, after.RetryState)
			}
			m := published[1]
			if m.Resume == nil || m.Resume.Force {
				t.Fatalf("re-admission resume %+v, want an unforced resume", m.Resume)
			}
			got, err := runview.TransportWorkflowHash(m.IRCompiled)
			if err != nil || got != v1Hash {
				t.Fatalf("the re-admission derives %s (%v), want v1's %s", got, err, v1Hash)
			}
			wf, err := runview.DecodeTransportWorkflow(m.IRCompiled)
			if err != nil {
				t.Fatalf("decode the re-admission: %v", err)
			}
			if _, ok := wf.Nodes["two"]; ok {
				t.Fatal("the re-admission carries v2's node")
			}
			if after.RetryState == nil || after.RetryState.Attempts != 1 {
				t.Fatalf("retry state %+v, want the park's one attempt, untouched by the re-admission", after.RetryState)
			}
			want := 0.0
			if tc.changed {
				want = 1
			}
			if n := counterValue(t, s.cfg.Metrics.RunsRetryResumed.WithLabelValues(retryResultFrozenReadmitted)); n != want {
				t.Fatalf("frozen_readmitted %v, want %v", n, want)
			}
		})
	}
}
