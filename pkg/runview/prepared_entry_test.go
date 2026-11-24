package runview

import (
	"context"
	"errors"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/supervise"
	"testing"
)

type servicePreparedStore struct {
	*store.FilesystemRunStore
	peerAtCAS bool
	cas       int
}

func (s *servicePreparedStore) UpdateRunOutcomeIf(ctx context.Context, id string, x store.RunOutcomeExpectation, status store.RunStatus, msg string, m store.RunOutcomeMeta, from []store.RunStatus) (bool, error) {
	s.cas++
	if s.peerAtCAS {
		s.peerAtCAS = false
		r, e := s.LoadRun(ctx, id)
		if e != nil {
			return false, e
		}
		_, ok, e := s.AcquireRunExecutionIf(ctx, id, store.RunExecutionExpectation{Version: r.CASVersion, Epoch: r.ExecutionEpoch, Status: r.Status}, store.ExecutionStart)
		if e != nil {
			return false, e
		}
		if !ok {
			return false, errors.New("probe peer acquisition failed")
		}
	}
	return s.FilesystemRunStore.UpdateRunOutcomeIf(ctx, id, x, status, msg, m, from)
}
func TestPreparedServiceAfterOwnStamp(t *testing.T) {
	for _, kind := range []string{"normal", "cancel", "peer-at-CAS"} {
		t.Run(kind, func(t *testing.T) {
			fs, e := store.New(t.TempDir())
			if e != nil {
				t.Fatal(e)
			}
			st := &servicePreparedStore{FilesystemRunStore: fs, peerAtCAS: kind == "peer-at-CAS"}
			svc, e := NewService("", WithStore(st), WithLogger(iterlog.Nop()))
			if e != nil {
				t.Fatal(e)
			}
			t.Cleanup(func() { stopService(t, svc) })
			pub := &invocationOutcomePublisher{}
			svc.SetEventPublisher(pub)
			var seamErr, outcome error
			var stamped bool
			restore := swapStartDeclared(func(ctx context.Context, _ supervise.Observer, _ supervise.Injector, id string, _ []supervise.Spec, _ *iterlog.Logger) func() {
				r, err := fs.LoadRun(ctx, id)
				if err != nil {
					seamErr = err
					return func() {}
				}
				stamped = r.LLMRoutePolicy != nil && r.CASVersion > 1
				if kind != "normal" {
					seamErr = svc.Cancel(id)
				}
				return func() {}
			})
			defer restore()
			res, e := svc.Launch(t.Context(), LaunchSpec{RunID: "run", Source: preparedServiceSource, WorkDir: t.TempDir(), Supervisors: "on", LLMRoutePolicy: &store.RunLLMRoutePolicy{Strict: true}, OnOutcome: func(err error) { outcome = err }})
			if e != nil {
				t.Fatal(e)
			}
			awaitRunCompletion(t, res.Done, "prepared Service did not finish")
			if seamErr != nil || !stamped {
				t.Fatalf("own stamp missing: %v %v", stamped, seamErr)
			}
			r, e := fs.LoadRun(t.Context(), "run")
			if e != nil {
				t.Fatal(e)
			}
			t.Logf("kind=%s status=%s outcome=%v externalEvents=%d CAS=%d", kind, r.Status, outcome, len(pub.events), st.cas)
			switch kind {
			case "normal":
				if outcome != nil || r.Status != store.RunStatusFinished || len(pub.events) != 1 || st.cas != 0 {
					t.Fatal(outcome, r.Status, len(pub.events), st.cas)
				}
			case "cancel":
				if !errors.Is(outcome, runtime.ErrRunCancelled) || r.Status != store.RunStatusCancelled || st.cas != 1 || len(pub.events) != 1 {
					t.Fatal(outcome, r.Status, len(pub.events), st.cas)
				}
			case "peer-at-CAS":
				if !errors.Is(outcome, runtime.ErrAdmissionStopped) || r.Status != store.RunStatusRunning || len(pub.events) != 0 || st.cas != 1 {
					t.Fatal("successor publication", outcome, r.Status, len(pub.events), st.cas)
				}
			}
		})
	}
}

const preparedServiceSource = `schema checked:
  ok: bool
prompt watch_policy:
  """
  Observe only.
  """
supervisor watchdog:
  watches: [check]
  model: "anthropic/fixture"
  system: watch_policy
  cooldown: "30s"
compute check:
  output: checked
  expr:
    ok: "true"
workflow probe:
  worktree: none
  sandbox: none
  entry: check
  check -> done
`
