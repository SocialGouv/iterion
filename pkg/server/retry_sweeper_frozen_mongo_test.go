package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/cloud/metrics"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/server/cloudpublisher"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/blob"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
)

// heroIRBlob keeps IR blobs in memory, keyed as S3 keys them; every other
// object kind is the no-op backend's.
type heroIRBlob struct {
	wsOriginExactBlob
	mu   sync.Mutex
	data map[string][]byte
}

func (b *heroIRBlob) PutIRBlob(_ context.Context, id string, body []byte) error {
	key, err := blob.IRBlobKey(id)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.data == nil {
		b.data = map[string][]byte{}
	}
	b.data[key] = append([]byte(nil), body...)
	return nil
}

func (b *heroIRBlob) GetIRBlob(_ context.Context, key string) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	body, ok := b.data[key]
	if !ok {
		return nil, blob.ErrArtifactNotFound
	}
	return append([]byte(nil), body...), nil
}

const (
	heroV1 = "schema out:\n  ok: bool\n\ntool one:\n  command: `true`\n  output: out\n\nworkflow hero:\n  entry: one\n  one -> done\n"
	heroV2 = "schema out:\n  ok: bool\n\ntool one:\n  command: `true`\n  output: out\n\ntool two:\n  command: `true`\n  output: out\n\nworkflow hero:\n  entry: one\n  one -> two\n  two -> done\n"
	// heroBroken is v1 with an edge to a node it does not declare: it does
	// not compile.
	heroBroken = "schema out:\n  ok: bool\n\ntool one:\n  command: `true`\n  output: out\n\nworkflow hero:\n  entry: one\n  one -> missing\n"
)

// F21. The hero, on a real replica set: a run launched on v1 through the real
// publisher (the queue captured), started, parked on a capacity wait by the
// runner's own sequence — the program frozen from the CAPTURED v1 message,
// the park's one write carrying its record — then its bot pushed to v2, then
// one retry-sweeper pass through the real run service and publisher. Exactly
// one publication follows: v1's program (its derived hash; v2's node absent),
// unforced, the run's attempts untouched. Skipped without
// ITERION_TEST_MONGO_URI. Red when the sweeper ignores the record (v2 is
// compiled, refused for its hash, re-armed against the budget, nothing
// published).
func TestSweepFrozen_HeroMongo(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; the frozen re-admission is unverified on Mongo")
	}
	ctx := store.WithIdentity(t.Context(), "team-1", "u1")
	ms, err := mongostore.New(ctx, mongostore.Config{URI: uri, Database: fmt.Sprintf("frozen_hero_%d", time.Now().UnixNano()), Blob: &heroIRBlob{}, RunFilesScratchDir: filepath.Join(t.TempDir(), "scratch")})
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

	// The runner: claims, runs node one (a checkpoint), is interrupted, the
	// queue hands it back (a transit), the redelivery finds its place gone
	// and parks it with its program frozen.
	launched, err := ms.LoadRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if launched.WorkflowHash != v1Hash {
		t.Fatalf("precondition: the run reads %s, the launch's IR derives %s", launched.WorkflowHash, v1Hash)
	}
	launched.Status = store.RunStatusRunning
	if err := ms.SaveRun(ctx, launched); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := ms.FailRunResumable(ctx, runID, &store.Checkpoint{NodeID: "one"}, "drained", store.FailureInterrupted); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	until := store.TransitInstant(time.Now().Add(5 * time.Minute))
	if start, err := ms.BeginCapacityTransit(ctx, runID, store.CapacityTransit{Until: until}); err != nil || !start.Started {
		t.Fatalf("transit: %+v %v", start, err)
	}
	inTransit, err := ms.LoadRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	fp, err := runview.FreezeProgram(ctx, ms, v1Msg, inTransit, store.FrozenProgramSourceCapacity)
	if err != nil {
		t.Fatalf("FreezeProgram: %v", err)
	}
	now := time.Now().UTC()
	parked, _, err := ms.ParkRunOnCapacity(ctx, runID, store.CapacityPark{
		From: store.RunStatusFailedResumable, TransitUntil: until, Code: store.FailureCredSlotWait,
		Error: "parked: place lost", Reason: store.RetryReasonCredSlot, RetryAfter: now.Add(store.CredSlotProbeInterval),
		MaxAttempts: 5, Frozen: fp,
	})
	if err != nil || !parked {
		t.Fatalf("park: %v %v", parked, err)
	}

	// The bot moves on while the run waits.
	if err := os.WriteFile(botPath, []byte(heroV2), 0o644); err != nil {
		t.Fatal(err)
	}

	s := &Server{cfg: Config{Store: ms, WorkDir: workDir, Metrics: metrics.New()}}
	due := now.Add(store.CredSlotProbeInterval + time.Minute)
	s.retryClock = func() time.Time { return due }
	s.sweepDueRetries(ctx, ms, svc, due)

	mu.Lock()
	defer mu.Unlock()
	if len(published) != 2 {
		after, _ := ms.LoadRun(ctx, runID)
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
	after, err := ms.LoadRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if after.RetryState != nil && after.RetryState.Attempts != 0 {
		t.Fatalf("attempts %d, want untouched by the re-admission", after.RetryState.Attempts)
	}
	if n := counterValue(t, s.cfg.Metrics.RunsRetryResumed.WithLabelValues(retryResultFrozenReadmitted)); n != 1 {
		t.Fatalf("frozen_readmitted %v, want 1", n)
	}
}

// F-A on a real replica set: the stored bots live in the Mongo bot-source
// store, whose rows keep created_at to the millisecond. The parked attempt's
// marker against the row the sweeper reads: a slug deleted and re-created
// after the attempt is another bot (the record counted stale, the current
// program resumed, nothing abandoned); a bot updated in place, or one the
// parked attempt ran after its re-creation, keeps its record. Skipped without
// ITERION_TEST_MONGO_URI. Red when the existence check reads the slug alone.
func TestSweepFrozen_ARecreatedSlugIsAnotherBotMongo(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; the re-created slug's frozen record is unverified on Mongo")
	}
	for _, tc := range []struct {
		shape  string
		frozen bool
	}{
		{"re-created after the attempt", false},
		{"updated in place", true},
		{"re-created then resumed on", true},
	} {
		t.Run(tc.shape, func(t *testing.T) {
			ctx := store.WithIdentity(t.Context(), "team-1", "u1")
			ms, err := mongostore.New(ctx, mongostore.Config{URI: uri, Database: fmt.Sprintf("frozen_recreated_%d", time.Now().UnixNano()), Blob: &heroIRBlob{}, RunFilesScratchDir: filepath.Join(t.TempDir(), "scratch")})
			if err != nil {
				t.Fatalf("mongostore.New: %v", err)
			}
			t.Cleanup(func() {
				c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
				defer cancel()
				_ = ms.DB().Drop(c)
				_ = ms.Close(c)
			})
			bots := botsource.NewMongoStore(ms.DB())
			if err := bots.EnsureSchema(ctx); err != nil {
				t.Fatalf("EnsureSchema: %v", err)
			}
			q := incarnations(t, bots, tc.shape)
			s, st := newFrozenSweeper(t)
			s.botSources = bots
			run, ref := frozenDue(t, st, "run-frozen", "team-1")
			bindAttempt(run, q)
			resumer := &fakeResumer{}
			sweepOne(s, resumer, ref)
			if reason := st.abandoned["run-frozen"]; reason != "" {
				t.Fatalf("abandoned %q, want resumed", reason)
			}
			if len(resumer.calls) != 1 || (resumer.calls[0].Frozen != nil) != tc.frozen {
				t.Fatalf("resumes %d (first frozen: %v), want one, frozen %v", len(resumer.calls), len(resumer.calls) == 1 && resumer.calls[0].Frozen != nil, tc.frozen)
			}
		})
	}
}
