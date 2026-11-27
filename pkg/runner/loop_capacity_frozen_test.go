package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cloud/metrics"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The capacity park of a started run freezes the program its delivery
// carried (ADR-167 T4a, frozen-program re-admission), observed on the park
// the runner asks for: the record rides the park's write. A run that never
// started freezes nothing; a freeze that fails parks the run all the same,
// without a record, and says so.

const frozenRunnerBot = "schema out:\n  ok: bool\n\ntool one:\n  command: `true`\n  output: out\n\nworkflow frozen:\n  entry: one\n  one -> done\n"

// runnerIRBlobs is an in-memory IR blob store that fails its writes on
// demand.
type runnerIRBlobs struct {
	mu     sync.Mutex
	data   map[string][]byte
	putErr error
}

func (b *runnerIRBlobs) PutIRBlob(_ context.Context, id string, body []byte) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.putErr != nil {
		return "", b.putErr
	}
	if b.data == nil {
		b.data = map[string][]byte{}
	}
	key := "ir/" + id + ".json"
	b.data[key] = append([]byte(nil), body...)
	return key, nil
}

func (b *runnerIRBlobs) GetIRBlob(_ context.Context, key string) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	body, ok := b.data[key]
	if !ok {
		return nil, fmt.Errorf("ir blob %s: %w", key, os.ErrNotExist)
	}
	return append([]byte(nil), body...), nil
}

func (b *runnerIRBlobs) IRBlobBackend() string { return "s3" }

// blobParkingStore is the parking store with an IR blob store.
type blobParkingStore struct {
	*parkingStore
	*runnerIRBlobs
}

func (s *blobParkingStore) Unwrap() store.RunStore { return s.parkingStore }

// frozenTransport is the IR a launch of frozenRunnerBot publishes and the
// hash it stamps — checked to derive each other, the freeze's premise.
func frozenTransport(t *testing.T) ([]byte, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "frozen.bot")
	if err := os.WriteFile(path, []byte(frozenRunnerBot), 0o644); err != nil {
		t.Fatal(err)
	}
	wf, hash, err := runview.CompileWorkflowWithHash(path, "", store.RunTrustDefault)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	body, err := runview.MarshalSourceSnapshot(wf.SourceSnapshot)
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	if derived, err := runview.TransportWorkflowHash(body); err != nil || derived != hash {
		t.Fatalf("precondition: the transport derives %s (%v), the launch stamped %s", derived, err, hash)
	}
	return body, hash
}

type startedShape struct {
	name       string
	checkpoint bool
	attempts   bool
}

// newFrozenFixture is the guard fixture on a run shaped as started (or
// not), stamped with the transport's hash, its message carrying that IR,
// its place taken.
func newFrozenFixture(t *testing.T, runID string, shape startedShape) (*guardFixture, *runnerIRBlobs, []byte) {
	t.Helper()
	f := newGuardFixture(t, runID, store.FailureInterrupted, "")
	body, hash := frozenTransport(t)
	r := f.load(t)
	r.WorkflowHash = hash
	r.Checkpoint = nil
	if shape.checkpoint {
		r.Checkpoint = &store.Checkpoint{NodeID: "one"}
	}
	r.Attempts = nil
	if shape.attempts {
		r.Attempts = []store.RunAttempt{{ID: "a1", WorkflowHash: hash}}
	}
	if err := f.fs.SaveRun(idCtx(), r); err != nil {
		t.Fatalf("shape the run: %v", err)
	}
	if got := f.load(t); got.ProgramStarted() != (shape.checkpoint || shape.attempts) {
		t.Fatalf("precondition: the run reads started=%v, want %v", got.ProgramStarted(), shape.checkpoint || shape.attempts)
	}
	blobs := &runnerIRBlobs{}
	f.r.cfg.Store = &blobParkingStore{parkingStore: f.park, runnerIRBlobs: blobs}
	f.msg.WorkflowName, f.msg.WorkflowHash, f.msg.IRCompiled = "frozen", hash, body
	f.evict(t)
	return f, blobs, body
}

func frozenCount(t *testing.T, f *guardFixture, outcome string) float64 {
	t.Helper()
	return counterValue(t, f.reg.CapacityFrozenProgramTotal.WithLabelValues(store.FrozenProgramSourceCapacity, outcome))
}

// F15. Driven through the pickup's lost place: a started run — a checkpoint
// alone, or a recorded attempt alone — is parked with its program frozen,
// the record naming an envelope that holds the delivered IR; a run that never
// started is parked with none, counted not_started. Red when the started
// predicate reads one arm only, or when every park freezes.
func TestCapacityPark_FreezesTheProgramOfAStartedRun(t *testing.T) {
	for _, shape := range []startedShape{
		{"checkpoint only", true, false},
		{"attempts only", false, true},
		{"not started", false, false},
	} {
		t.Run(shape.name, func(t *testing.T) {
			f, blobs, body := newFrozenFixture(t, "cap-frozen", shape)
			_, out, proceed := f.pickup(t, 2)
			if proceed || out.action != actionAck {
				t.Fatalf("pickup: proceed=%v action=%v, want parked and acked", proceed, out.action)
			}
			parks := f.park.recordedParks()
			if len(parks) != 1 {
				t.Fatalf("parks %+v, want one", parks)
			}
			fp := parks[0].Frozen
			if !shape.checkpoint && !shape.attempts {
				if fp != nil {
					t.Fatalf("a run that never started was parked with a frozen program %+v", fp)
				}
				if n := frozenCount(t, f, metrics.FrozenProgramNotStarted); n != 1 {
					t.Fatalf("not_started count %v, want 1", n)
				}
				return
			}
			if fp == nil {
				t.Fatal("a started run was parked without its program")
			}
			env, err := runview.LoadFrozenProgram(context.Background(), blobs, f.msg.RunID, fp)
			if err != nil {
				t.Fatalf("the record names no readable envelope: %v", err)
			}
			if string(env.IR) != string(body) || env.WorkflowHash != f.msg.WorkflowHash || fp.Source != store.FrozenProgramSourceCapacity {
				t.Fatalf("envelope hash %s source %s, want the delivered program", env.WorkflowHash, fp.Source)
			}
			if n := frozenCount(t, f, metrics.FrozenProgramWritten); n != 1 {
				t.Fatalf("written count %v, want 1", n)
			}
		})
	}
}

// F16. A freeze that fails — a legacy bundle reference with no snapshot
// digest, a blob store that refuses the write — never holds the park back:
// the run is parked without a record, the delivery acked (no nak, no longer
// transit), the failure counted. Red when a freeze failure stops the park.
func TestCapacityPark_AFailedFreezeStillParks(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome string
		setup   func(f *guardFixture, blobs *runnerIRBlobs)
	}{
		{"legacy digest-less bundle", metrics.FrozenProgramFreezeFailed, func(f *guardFixture, _ *runnerIRBlobs) {
			f.msg.BotBundle = &queue.BotBundleRef{TenantID: "team-1", Slug: "bot", Version: 2}
		}},
		{"blob write refused", metrics.FrozenProgramFreezeFailed, func(_ *guardFixture, blobs *runnerIRBlobs) {
			blobs.putErr = errors.New("s3: access denied")
		}},
		{"a delivered program that is not the run's", metrics.FrozenProgramHashMismatch, func(f *guardFixture, _ *runnerIRBlobs) {
			r := f.load(t)
			r.WorkflowHash = "sha256:another-revision"
			if err := f.fs.SaveRun(idCtx(), r); err != nil {
				t.Fatalf("restamp: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, blobs, _ := newFrozenFixture(t, "cap-freeze-fails", startedShape{"checkpoint", true, false})
			tc.setup(f, blobs)
			_, out, proceed := f.pickup(t, 2)
			if proceed || out.action != actionAck {
				t.Fatalf("pickup: proceed=%v action=%v, want parked and acked", proceed, out.action)
			}
			parks := f.park.recordedParks()
			if len(parks) != 1 || parks[0].Frozen != nil {
				t.Fatalf("parks %+v, want one park without a frozen program", parks)
			}
			run := f.load(t)
			if run.FailureCode != store.FailureCredSlotWait || run.ContinuationState != store.ContinuationRetryArmed {
				t.Fatalf("run %s continuation %q, want parked CRED_SLOT_WAIT with its retry armed", run.FailureCode, run.ContinuationState)
			}
			if n := frozenCount(t, f, tc.outcome); n != 1 {
				t.Fatalf("%s count %v, want 1", tc.outcome, n)
			}
		})
	}
}
