package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/cloud/metrics"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
)

// The retry sweeper's frozen-program re-admission (ADR-167 T4a), on the fake
// retry store and a resumer that records what it is asked: which program a
// due capacity park resumes, and what ends or re-arms it before anything is
// metered.

// frozenRetryStore is the fake retry store with an IR blob store.
type frozenRetryStore struct {
	*fakeRetryStore
	mu     sync.Mutex
	blobs  map[string][]byte
	getErr error
}

func (s *frozenRetryStore) Unwrap() store.RunStore { return s.fakeRetryStore }

func (s *frozenRetryStore) PutIRBlob(_ context.Context, id string, body []byte) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := "ir/" + id + ".json"
	s.blobs[key] = append([]byte(nil), body...)
	return key, nil
}

func (s *frozenRetryStore) GetIRBlob(_ context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getErr != nil {
		return nil, s.getErr
	}
	body, ok := s.blobs[key]
	if !ok {
		return nil, fmt.Errorf("ir blob %s: %w", key, os.ErrNotExist)
	}
	return append([]byte(nil), body...), nil
}

func (s *frozenRetryStore) IRBlobBackend() string { return "s3" }

// frozenDue is a capacity park of a started run whose program a runner froze:
// the record, its envelope stored, the run as the sweeper reads it, its due
// row. tier is the run's stored-bot tier ("" for an inline or baked run).
func frozenDue(t *testing.T, st *frozenRetryStore, id, tier string) (*store.Run, mongostore.RetryDueRef) {
	t.Helper()
	q1 := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	env := runview.FrozenEnvelope{V: 1, RunID: id, WorkflowName: "w", WorkflowHash: "h1", IR: []byte(`{"workflows":[]}`)}
	body, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	key, _ := st.PutIRBlob(context.Background(), id+"-bundle-program-"+digest, body)
	run := &store.Run{
		ID: id, TenantID: "team-1", Status: store.RunStatusFailedResumable, FailureCode: store.FailureCredSlotWait,
		WorkflowHash: "h1", QueuedAt: &q1, Checkpoint: &store.Checkpoint{NodeID: "one"},
		BotSourceTenant: tier, FilePath: "bots/feed-watch/main.bot",
		FrozenProgram: &store.FrozenProgram{Key: key, Digest: digest, WorkflowHash: "h1", QueuedAt: q1, Source: store.FrozenProgramSourceCapacity},
	}
	st.loadRun[id] = run
	st.claimWins[id] = true
	ref := dueRef(id, time.Now().UTC().Add(-time.Minute))
	ref.RetryState.Reason, ref.RetryState.Code = store.RetryReasonCredSlot, string(store.FailureCredSlotWait)
	ref.BotSourceTenant = tier
	return run, ref
}

func newFrozenSweeper(t *testing.T) (*Server, *frozenRetryStore) {
	t.Helper()
	st := &frozenRetryStore{fakeRetryStore: newFakeRetryStore(), blobs: map[string][]byte{}}
	s := newRetrySweeperServer(t, st, nil)
	s.cfg.Metrics = metrics.New()
	return s, st
}

func sweepOne(s *Server, resumer *fakeResumer, ref mongostore.RetryDueRef) {
	s.sweepDueRetries(context.Background(), &fakeRetryLister{refs: []mongostore.RetryDueRef{ref}}, resumer, time.Now().UTC())
}

func retryResult(t *testing.T, s *Server, result string) float64 {
	t.Helper()
	return counterValue(t, s.cfg.Metrics.RunsRetryResumed.WithLabelValues(result))
}

// The hero of the fake: a bound record re-admits its program — the resume
// carries the record and its verified envelope, no file or source, never
// forced — and the success is counted. An inline-source run (no stored tier)
// is re-admitted alike (F24): it has no row to delete. Red when the record is
// ignored, or when the existence check also runs for an untiered run.
func TestSweepFrozen_ABoundRecordReadmitsItsProgram(t *testing.T) {
	for _, tier := range []string{"", "team-1"} {
		t.Run("tier="+tier, func(t *testing.T) {
			s, st := newFrozenSweeper(t)
			if tier != "" {
				bots := botsource.NewMemoryStore()
				if _, err := bots.Create(context.Background(), botsource.BotSource{TenantID: tier, Slug: "feed-watch", Files: map[string]string{"main.bot": "workflow w:\n"}}); err != nil {
					t.Fatal(err)
				}
				s.botSources = bots
			}
			run, ref := frozenDue(t, st, "run-frozen", tier)
			resumer := &fakeResumer{}
			sweepOne(s, resumer, ref)
			if len(resumer.calls) != 1 {
				t.Fatalf("Resume called %d times, want 1", len(resumer.calls))
			}
			spec := resumer.calls[0]
			if spec.Frozen == nil || spec.Frozen.Envelope == nil || spec.Frozen.Program.Key != run.FrozenProgram.Key {
				t.Fatalf("the resume carries no frozen program: %+v", spec.Frozen)
			}
			if spec.FilePath != "" || spec.Source != "" || spec.Force || !spec.Automatic {
				t.Fatalf("spec file %q source %q force %v automatic %v, want the frozen program alone, unforced, automatic", spec.FilePath, spec.Source, spec.Force, spec.Automatic)
			}
			if len(st.cleared) != 1 || retryResult(t, s, retryResultFrozenReadmitted) != 1 {
				t.Fatalf("cleared %v, frozen_readmitted %v, want the retry cleared and counted", st.cleared, retryResult(t, s, retryResultFrozenReadmitted))
			}
		})
	}
}

// F23. The stored bot the run launched from was deleted while it waited:
// deletion is the operator's lever to stop its automation, frozen program or
// not — abandoned with the resolution's own words, nothing resumed. Red when
// the existence check is skipped.
func TestSweepFrozen_ADeletedStoredBotEndsTheAutomation(t *testing.T) {
	s, st := newFrozenSweeper(t)
	s.botSources = botsource.NewMemoryStore()
	_, ref := frozenDue(t, st, "run-frozen", "team-1")
	resumer := &fakeResumer{}
	sweepOne(s, resumer, ref)
	if len(resumer.calls) != 0 {
		t.Fatalf("Resume called %d times on a deleted bot, want none", len(resumer.calls))
	}
	reason := st.abandoned["run-frozen"]
	if !strings.Contains(reason, "auto-retry abandoned:") || !strings.Contains(reason, "no longer exists") {
		t.Fatalf("abandoned %q, want the deleted bot named in today's words", reason)
	}
}

// F25. A record that no longer binds — its attempt marker moved, or its hash
// is not the run's — is counted and ignored: the run resumes its bot's
// current program, never abandoned on. One scenario per guard. Red when a
// stale record is abandoned on, or when either guard is dropped (the record
// would be re-admitted).
func TestSweepFrozen_AStaleRecordTakesTheCurrentPath(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stale func(r *store.Run)
	}{
		{"queued_at moved", func(r *store.Run) { q := r.QueuedAt.Add(time.Minute); r.QueuedAt = &q }},
		{"hash moved", func(r *store.Run) { r.WorkflowHash = "h2" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, st := newFrozenSweeper(t)
			run, ref := frozenDue(t, st, "run-frozen", "")
			tc.stale(run)
			resumer := &fakeResumer{}
			sweepOne(s, resumer, ref)
			if _, ok := st.abandoned["run-frozen"]; ok {
				t.Fatalf("a stale record was abandoned on: %q", st.abandoned["run-frozen"])
			}
			if len(resumer.calls) != 1 || resumer.calls[0].Frozen != nil || resumer.calls[0].FilePath == "" {
				t.Fatalf("resume %+v, want the current path (a file, no frozen program)", resumer.calls)
			}
			if n := retryResult(t, s, retryResultFrozenStale); n != 1 {
				t.Fatalf("frozen_stale %v, want 1", n)
			}
		})
	}
}

// F26. An envelope that is gone, or whose body does not hash to the record's
// digest, is counted and the run takes its bot's current program: the hash
// check still guards it, so a missing envelope never kills a run that could
// resume. Red when a missing envelope abandons the run.
func TestSweepFrozen_AnUnavailableEnvelopeTakesTheCurrentPath(t *testing.T) {
	for _, tc := range []struct {
		name    string
		corrupt func(st *frozenRetryStore, r *store.Run)
	}{
		{"missing", func(st *frozenRetryStore, r *store.Run) { delete(st.blobs, r.FrozenProgram.Key) }},
		{"bad digest", func(st *frozenRetryStore, r *store.Run) {
			st.blobs[r.FrozenProgram.Key] = []byte(`{"v":1,"run_id":"run-frozen"}`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, st := newFrozenSweeper(t)
			run, ref := frozenDue(t, st, "run-frozen", "")
			tc.corrupt(st, run)
			resumer := &fakeResumer{}
			sweepOne(s, resumer, ref)
			if _, ok := st.abandoned["run-frozen"]; ok {
				t.Fatalf("an unavailable envelope abandoned the run: %q", st.abandoned["run-frozen"])
			}
			if len(resumer.calls) != 1 || resumer.calls[0].Frozen != nil {
				t.Fatalf("resume %+v, want the current path", resumer.calls)
			}
			if n := retryResult(t, s, retryResultFrozenUnavailable); n != 1 {
				t.Fatalf("frozen_unavailable %v, want 1", n)
			}
		})
	}
}

// F27. The envelope store did not answer: the retry is re-armed before
// anything is metered or resumed — not taken for a missing envelope, which
// would resume the current program. Red when a store failure is treated as
// an unavailable envelope.
func TestSweepFrozen_AnEnvelopeStoreFailureReArms(t *testing.T) {
	s, st := newFrozenSweeper(t)
	_, ref := frozenDue(t, st, "run-frozen", "")
	st.getErr = errors.New("s3: connection reset")
	resumer := &fakeResumer{}
	sweepOne(s, resumer, ref)
	if len(resumer.calls) != 0 {
		t.Fatalf("Resume called %d times on a store failure, want none", len(resumer.calls))
	}
	if _, ok := st.rearmed["run-frozen"]; !ok {
		t.Fatal("the retry was not re-armed")
	}
}

// F28. A run that never started carries no program to re-admit, whatever its
// record says: it takes its bot's current program, and a changed hash there
// is the charged refusal it always was (ADR-167 §6's residual, counted
// not_started by the runner). Red when the binding ignores whether the run
// started.
func TestSweepFrozen_ANotStartedRunTakesTheCurrentPath(t *testing.T) {
	s, st := newFrozenSweeper(t)
	run, ref := frozenDue(t, st, "run-frozen", "")
	run.Checkpoint, run.Attempts = nil, nil
	resumer := &fakeResumer{failing: runtime.ValidateResumeWorkflowHash("run-frozen", "h1", "h2", false)}
	sweepOne(s, resumer, ref)
	if len(resumer.calls) != 1 || resumer.calls[0].Frozen != nil {
		t.Fatalf("resume %+v, want the current path", resumer.calls)
	}
	if _, ok := st.armBudget["run-frozen"]; !ok {
		t.Fatal("the hash refusal was not re-armed against the budget")
	}
	if _, ok := st.abandoned["run-frozen"]; ok {
		t.Fatalf("the hash refusal abandoned the run: %q", st.abandoned["run-frozen"])
	}
}

// F29. A refusal only a person lifts ends the automation at the first due,
// named, on the frozen path and on the current one alike: an undecided
// effect, a scratch that does not travel, an artifact contract the program no
// longer meets, a required secret deleted during the wait. No attempt is
// spent re-trying into it. Red when such a refusal is re-armed.
func TestSweepFrozen_AConsentRefusalEndsTheAutomation(t *testing.T) {
	refusals := map[string]error{
		"undecided effect":  runtime.CheckApprovalReplay(&store.Run{UnresolvedToolApproval: "tool-1"}),
		"scratch":           &runtime.RuntimeError{Code: runtime.ErrCodeScratchNotPortable, Message: "the scratch did not travel"},
		"artifact contract": fmt.Errorf("runview: %w", runtime.ErrArtifactContractIncompatible),
		"required secret":   fmt.Errorf("cloudpublisher: %w", secrets.RequiredSecretsError([]string{"forge_token"}, "this team/bot")),
	}
	for name, refusal := range refusals {
		for _, frozen := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/frozen=%v", name, frozen), func(t *testing.T) {
				s, st := newFrozenSweeper(t)
				run, ref := frozenDue(t, st, "run-frozen", "")
				if !frozen {
					run.FrozenProgram = nil
				}
				resumer := &fakeResumer{failing: refusal}
				sweepOne(s, resumer, ref)
				if len(resumer.calls) != 1 || (resumer.calls[0].Frozen != nil) != frozen {
					t.Fatalf("resume %+v, want one on the %v path", resumer.calls, frozen)
				}
				reason, ok := st.abandoned["run-frozen"]
				if !ok || !strings.Contains(reason, "resuming needs an operator") {
					t.Fatalf("abandoned %q (%v), want the automation ended naming an operator", reason, ok)
				}
				if _, paid := st.armBudget["run-frozen"]; paid {
					t.Fatal("a consent refusal spent an attempt")
				}
			})
		}
	}
}
