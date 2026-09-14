package runtime

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

var errPortInjectedCommit = errors.New("fixture: interrupted native commit")

// Fault injection is at the actual RunStore CAS used by Engine, not a fake
// scheduler. Both filesystem and Mongo run this suite, including a lost
// acknowledgment after the database has durably accepted a publication.
type portInterruptedStore struct {
	store.RunStore
	status store.PortInvocationStatus
	after  bool
	fired  atomic.Bool
}

func (s *portInterruptedStore) LoadPortActivation(ctx context.Context) (*store.PortActivation, error) {
	return store.AsPortActivationStore(s.RunStore).LoadPortActivation(ctx)
}
func (s *portInterruptedStore) SavePortActivation(ctx context.Context, revision uint64, next *store.PortActivation) error {
	return store.AsPortActivationStore(s.RunStore).SavePortActivation(ctx, revision, next)
}
func (s *portInterruptedStore) LoadPortDistributedProof(ctx context.Context) (*store.PortDistributedProof, error) {
	p, ok := s.RunStore.(store.PortDistributedProofStore)
	if !ok {
		return nil, store.ErrPortActivation
	}
	return p.LoadPortDistributedProof(ctx)
}
func (s *portInterruptedStore) SavePortDistributedProof(ctx context.Context, revision uint64, proof *store.PortDistributedProof) error {
	p, ok := s.RunStore.(store.PortDistributedProofStore)
	if !ok {
		return store.ErrPortActivation
	}
	return p.SavePortDistributedProof(ctx, revision, proof)
}
func (s *portInterruptedStore) PortBackendIdentity() string {
	if identified, ok := s.RunStore.(interface{ PortBackendIdentity() string }); ok {
		return identified.PortBackendIdentity()
	}
	return ""
}
func (s *portInterruptedStore) VerifyPortDistributedActivation(ctx context.Context, record *store.PortActivation, now time.Time) error {
	return verifyWrappedPortsTestActivation(ctx, s.RunStore, record, now)
}

func (s *portInterruptedStore) SaveRun(ctx context.Context, r *store.Run) error {
	match := r.PortExecution != nil && r.PortExecution.Invocations["a"] != nil && r.PortExecution.Invocations["a"].Status == s.status
	if !match || !s.fired.CompareAndSwap(false, true) {
		return s.RunStore.SaveRun(ctx, r)
	}
	if s.after {
		if err := s.RunStore.SaveRun(ctx, r); err != nil {
			return err
		}
	}
	return errPortInjectedCommit
}

func testPortsEngineInterruptedCommitRecovery(t *testing.T, newStore portsTestStoreFactory) {
	for _, test := range []struct {
		name   string
		status store.PortInvocationStatus
		after  bool
		wantA  int32
	}{
		{"BeforeAdmission", store.PortAdmitted, false, 1},
		{"AfterAdmission", store.PortAdmitted, true, 1},
		{"BeforeDispatch", store.PortRunning, false, 1},
		{"AfterDispatch", store.PortRunning, true, 1},
		{"BeforePublication", store.PortSucceeded, false, 2},
		{"AfterPublicationLostAck", store.PortSucceeded, true, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			executor := portsExecutorFunc(func(ctx context.Context, node ir.Node, input map[string]any) (map[string]any, error) {
				if node.NodeID() == "a" {
					calls.Add(1)
				}
				if node.NodeID() == "c" {
					return map[string]any{"result": input["left"].(string) + input["right"].(string)}, nil
				}
				return map[string]any{"value": node.NodeID()}, nil
			})
			engine, s := portsTestEngine(t, newStore, portsJoinSource, executor)
			wrapped := &portInterruptedStore{RunStore: s, status: test.status, after: test.after}
			engine.store = wrapped
			ctx := portsTestContext(t)
			if err := engine.Run(ctx, "pc1_crash", map[string]any{"seed": "start"}); !errors.Is(err, errPortInjectedCommit) {
				t.Fatal(err)
			}
			before := portsTestRun(t, s, "pc1_crash")
			if before.Status != store.RunStatusFailedResumable || !wrapped.fired.Load() {
				t.Fatal("injected fault stranded the root or was never exercised")
			}
			resume := New(portsTestWorkflow(t, portsJoinSource), s, executor, WithWorkDir(engine.workDir), WithSandboxOverride("none"))
			if err := resume.Resume(ctx, "pc1_crash", nil); err != nil {
				t.Fatal(err)
			}
			after := portsTestRun(t, s, "pc1_crash")
			if calls.Load() != test.wantA || after.Status != store.RunStatusFinished || portsTestExport(t, after, "result") != "ab" {
				t.Fatalf("incorrect recovery: calls=%d, want %d", calls.Load(), test.wantA)
			}
			if len(after.PortExecution.Budget.Reservations) != 0 {
				t.Fatal("recovery leaked a reservation")
			}
		})
	}
}
