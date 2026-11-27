package cloudpublisher

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The frozen branch of SubmitResume (ADR-167 T4a): the parked program is
// republished byte for byte under the envelope's identity, its binding
// checked on the document the claim reads, and everything an attempt owns —
// credentials, their stamp, the capacity admission — resolved again.

// frozenSpy is the capped fixture's store whose run carries a frozen program
// (only the Mongo park writes the field; the filesystem twin never persists
// it).
type frozenSpy struct {
	*ledgerRetrySpy
	mu sync.Mutex
	fp map[string]*store.FrozenProgram
}

func (s *frozenSpy) Unwrap() store.RunStore { return s.ledgerRetrySpy }

func (s *frozenSpy) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	r, err := s.ledgerRetrySpy.LoadRun(ctx, id)
	if err != nil || r == nil {
		return r, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if fp := s.fp[id]; fp != nil {
		cp := *fp
		r.FrozenProgram = &cp
	}
	return r, nil
}

// prettyIR is a transport IR as a launch could have stored it, indented: the
// republish must keep its bytes.
var prettyIR = []byte("{\n    \"workflows\": [\n        {\"name\": \"wf\"}\n    ]\n}")

// frozenFixture launches run-frozen on the capped key, parks it started, and
// records its frozen program; it returns the publisher, its captured
// messages and the re-admission to submit.
func frozenFixture(t *testing.T) (*cappedFixture, *frozenSpy, *Publisher, *capturedMessages, *runview.FrozenResume) {
	t.Helper()
	f := newCappedFixture(t, 2)
	spy := &frozenSpy{ledgerRetrySpy: f.spy, fp: map[string]*store.FrozenProgram{}}
	p := f.publisher()
	p.store = spy
	cap := &capturedMessages{}
	p.publishRun = cap.publish
	if err := f.launch(t, p, "run-frozen", nil); err != nil {
		t.Fatalf("launch: %v", err)
	}
	if err := f.spy.FailRunResumable(f.ctx, "run-frozen", &store.Checkpoint{NodeID: "implement"}, "parked: place lost", store.FailureCredSlotWait); err != nil {
		t.Fatalf("park: %v", err)
	}
	r := f.load(t, "run-frozen")
	if r.QueuedAt == nil || r.WorkflowHash != "hash" {
		t.Fatalf("precondition: queued_at %v hash %q", r.QueuedAt, r.WorkflowHash)
	}
	fp := &store.FrozenProgram{Key: "ir/run-frozen-bundle-program-d1.json", Digest: "d1", WorkflowHash: "hash",
		QueuedAt: *r.QueuedAt, Source: store.FrozenProgramSourceCapacity}
	spy.fp["run-frozen"] = fp
	env := &runview.FrozenEnvelope{V: 1, RunID: "run-frozen", WorkflowName: "wf", WorkflowHash: "hash", IR: prettyIR,
		BotBundle: &queue.BotBundleRef{TenantID: cappedTeam, Slug: "docs-refresh", Version: 4, SnapshotDigest: "sd1",
			SnapshotRef: &queue.IRRef{StorageKey: "ir/run-frozen-bundle-sd1.json", Backend: queue.IRBackendS3}}}
	return f, spy, p, cap, &runview.FrozenResume{Program: *fp, Envelope: env}
}

func submitFrozen(f *cappedFixture, p *Publisher, frozen *runview.FrozenResume) error {
	return p.SubmitResume(f.ctx, runview.ResumeSpec{RunID: "run-frozen", Automatic: true, Frozen: frozen},
		cappedWorkflow(), &runview.CompiledSource{Hash: frozen.Envelope.WorkflowHash})
}

// F17. The republished message carries the envelope's IR byte for byte
// (indentation included), its hash and workflow name, its bundle reference
// with the snapshot out of band — and is never forced. Red when the frozen
// branch marshals the program again.
func TestSubmitResumeFrozen_RepublishesTheEnvelopeVerbatim(t *testing.T) {
	f, _, p, cap, frozen := frozenFixture(t)
	if err := submitFrozen(f, p, frozen); err != nil {
		t.Fatalf("frozen SubmitResume: %v", err)
	}
	msgs := cap.all()
	if len(msgs) != 2 {
		t.Fatalf("published %d messages, want the launch and the re-admission", len(msgs))
	}
	m := msgs[1]
	if string(m.IRCompiled) != string(prettyIR) {
		t.Fatalf("republished IR:\n%s\nwant the envelope's bytes:\n%s", m.IRCompiled, prettyIR)
	}
	if m.WorkflowHash != "hash" || m.WorkflowName != "wf" || m.Resume == nil || m.Resume.Force {
		t.Fatalf("message hash %q name %q resume %+v, want the envelope's identity, unforced", m.WorkflowHash, m.WorkflowName, m.Resume)
	}
	ref := m.BotBundle
	if ref == nil || len(ref.Snapshot) != 0 || ref.SnapshotRef == nil || ref.SnapshotRef.StorageKey != "ir/run-frozen-bundle-sd1.json" ||
		ref.SnapshotDigest != "sd1" || ref.Version != 4 {
		t.Fatalf("bundle ref %+v, want the envelope's", ref)
	}
	if ref.SnapshotRef == frozen.Envelope.BotBundle.SnapshotRef {
		t.Fatal("the message shares the envelope's snapshot reference instead of a copy")
	}
}

// F18. A record that no longer binds the run read by the claim — its attempt
// marker moved since the sweeper read it — is refused before the flip:
// nothing claimed, nothing published. Red when the publisher does not check
// the binding again.
func TestSubmitResumeFrozen_StaleRecordIsRefusedBeforeTheFlip(t *testing.T) {
	f, spy, p, cap, frozen := frozenFixture(t)
	moved := frozen.Program
	moved.QueuedAt = moved.QueuedAt.Add(-time.Minute)
	spy.mu.Lock()
	spy.fp["run-frozen"] = &moved
	spy.mu.Unlock()
	before := f.load(t, "run-frozen")
	err := submitFrozen(f, p, frozen)
	if !errors.Is(err, runview.ErrFrozenProgramStale) {
		t.Fatalf("frozen SubmitResume on a moved attempt: %v, want ErrFrozenProgramStale", err)
	}
	after := f.load(t, "run-frozen")
	if after.Status != store.RunStatusFailedResumable || !after.QueuedAt.Equal(*before.QueuedAt) {
		t.Fatalf("run %s queued_at %v, want untouched (failed_resumable at %v)", after.Status, after.QueuedAt, before.QueuedAt)
	}
	if n := len(cap.all()); n != 1 {
		t.Fatalf("published %d messages, want the launch's alone", n)
	}
}

// F19. A key rotated while the run waited: the re-admission seals the new
// key, the run's stamp names it, the admission reserves on it and the
// message carries that admission's hold — the frozen program pins the
// program, never the credentials. Red when the frozen branch skips the
// stamp.
func TestSubmitResumeFrozen_ReResolvesTheCredentials(t *testing.T) {
	f, _, p, cap, frozen := frozenFixture(t)
	launch := cap.all()[0]
	if err := f.keys.Delete(context.Background(), f.keyID); err != nil {
		t.Fatalf("delete the launch's key: %v", err)
	}
	seedCappedKey(t, f.keys, f.sealer, cappedTeam, "anthropic", "sk-rotated", "fp-rotated", "anthropic-rotated", 2)
	if err := submitFrozen(f, p, frozen); err != nil {
		t.Fatalf("frozen SubmitResume: %v", err)
	}
	m := cap.all()[1]
	if m.SecretsRef == "" || string(m.BundleDEK) == string(launch.BundleDEK) {
		t.Fatalf("secrets ref %q, DEK reused from the launch: the credentials were not sealed again", m.SecretsRef)
	}
	r := f.load(t, "run-frozen")
	if !slices.Contains(r.CredFingerprints, "fp-rotated") || slices.Contains(r.CredFingerprints, cappedFP) {
		t.Fatalf("stamp %v, want the rotated key alone", r.CredFingerprints)
	}
	if m.Capacity == nil || len(m.Capacity.Holds) != 1 || m.Capacity.Holds[0].Subject != "fp-rotated" {
		t.Fatalf("capacity %+v, want the rotated key's hold", m.Capacity)
	}
	if !reassertHeld(t, f.spy, f.ctx, "fp-rotated", "run-frozen", m.Capacity.Holds[0].Token) {
		t.Fatal("the message's hold does not name the entry the re-admission's admission wrote")
	}
}

// A frozen re-admission is never forced, whoever asks.
func TestSubmitResumeFrozen_RefusesForce(t *testing.T) {
	f, _, p, cap, frozen := frozenFixture(t)
	err := p.SubmitResume(f.ctx, runview.ResumeSpec{RunID: "run-frozen", Automatic: true, Force: true, Frozen: frozen},
		cappedWorkflow(), &runview.CompiledSource{Hash: "hash"})
	if err == nil || len(cap.all()) != 1 {
		t.Fatalf("a forced frozen resume: %v (%d published), want refused", err, len(cap.all()))
	}
}
