package dispatcher

import (
	"context"
	"errors"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
	"os"
	"path/filepath"
	"testing"
)

var errD1CallerBusiness = errors.New("ordinary builder refusal")

type d1CallerStore struct {
	*store.FilesystemRunStore
	mode                      string
	cancel                    context.CancelFunc
	childID                   string
	intercepted               bool
	releases, locks, outcomes int
	peer                      store.RunExecutionAuthority
}

func (l *d1CallerLock) Publish(ctx context.Context, spec store.RunCreateSpec) (*store.Run, error) {
	s := l.owner
	r, err := l.RunBirthReservation.Publish(ctx, spec)
	if err != nil {
		return r, err
	}
	s.childID = r.ID
	if s.mode == "publication-failure" {
		return r, errD1CallerBusiness
	}
	if s.mode == "peer-before-abort" {
		a, ok, err := s.AcquireRunExecutionIf(ctx, r.ID, store.RunExecutionExpectation{Version: r.CASVersion, Epoch: r.ExecutionEpoch, Status: r.Status, QueuedAt: r.QueuedAt}, store.ExecutionStart)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, errors.New("peer acquisition refused")
		}
		s.peer = a
	}
	if s.mode == "after-create" || s.mode == "peer-before-abort" {
		s.cancel()
	}
	return r, nil
}
func (s *d1CallerStore) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	if id == s.childID && !s.intercepted && (s.mode == "builder-cancel" || s.mode == "business-while-cancelled") {
		s.intercepted = true
		s.cancel()
		if s.mode == "business-while-cancelled" {
			return nil, errD1CallerBusiness
		}
		return nil, ctx.Err()
	}
	return s.FilesystemRunStore.LoadRun(ctx, id)
}

type d1CallerLock struct {
	store.RunBirthReservation
	owner *d1CallerStore
}

func (l *d1CallerLock) Unlock() error { l.owner.releases++; return l.RunBirthReservation.Unlock() }
func (s *d1CallerStore) BeginRunBirth(ctx context.Context, id string) (store.RunBirthReservation, error) {
	l, err := s.FilesystemRunStore.BeginRunBirth(ctx, id)
	if err != nil {
		return nil, err
	}
	s.locks++
	return &d1CallerLock{RunBirthReservation: l, owner: s}, nil
}
func (s *d1CallerStore) UpdateRunOutcomeIf(ctx context.Context, id string, x store.RunOutcomeExpectation, status store.RunStatus, msg string, meta store.RunOutcomeMeta, from []store.RunStatus) (bool, error) {
	s.outcomes++
	return s.FilesystemRunStore.UpdateRunOutcomeIf(ctx, id, x, status, msg, meta, from)
}
func TestD1PreparedChildAbortCallers(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"publication-failure", "normal", "after-create", "builder-cancel", "peer-before-abort", "business-while-cancelled"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			parent, err := st.CreateRun(t.Context(), "parent", "parent", nil)
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			child := filepath.Join(dir, "child.bot")
			body := `schema checked:
  ok: bool
compute check:
  output: checked
  expr:
    ok: "true"
workflow child:
  worktree: none
  sandbox: none
  entry: check
  check -> done
`
			if err = os.WriteFile(child, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			s := &d1CallerStore{FilesystemRunStore: st, mode: mode, cancel: cancel}
			run := subbotRunnerForDispatch(filepath.Join(dir, "parent.bot"), st.Root(), dir, s, nil, nil, iterlog.Nop())
			_, gotErr := run(boundLegacySubbotFixture(t, ctx, parent.ID), runtime.SubbotRequest{Source: "child.bot", ParentRunID: parent.ID, NodeID: "child", ReattachKey: "child", WorkDir: dir})
			if s.childID == "" {
				t.Fatalf("no child born: %v", gotErr)
			}
			final, err := st.LoadRun(t.Context(), s.childID)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("mode=%s status=%s err=%v locks=%d releases=%d outcomeCAS=%d", mode, final.Status, gotErr, s.locks, s.releases, s.outcomes)
			switch mode {
			case "normal":
				if gotErr != nil || final.Status != store.RunStatusFinished || s.outcomes != 0 {
					t.Fatalf("NORMAL_CHILD: %+v %v outcomes=%d", final, gotErr, s.outcomes)
				}
			case "after-create", "builder-cancel":
				if !errors.Is(gotErr, runtime.ErrRunCancelled) || final.Status != store.RunStatusCancelled || s.outcomes != 1 {
					t.Fatalf("BIRTH_NOT_WITHDRAWN: status=%s err=%v outcomes=%d", final.Status, gotErr, s.outcomes)
				}
			case "peer-before-abort":
				if !errors.Is(gotErr, runtime.ErrAdmissionStopped) || !s.peer.Matches(final) || final.Status != store.RunStatusRunning || s.outcomes != 0 {
					t.Fatalf("PEER_WITHDRAWN: status=%s err=%v outcomes=%d", final.Status, gotErr, s.outcomes)
				}
			case "publication-failure", "business-while-cancelled":
				if !errors.Is(gotErr, errD1CallerBusiness) || errors.Is(gotErr, runtime.ErrRunCancelled) || final.Status != store.RunStatusFailed || s.outcomes != 1 {
					t.Fatalf("BUSINESS_RECLASSIFIED: status=%s err=%v outcomes=%d", final.Status, gotErr, s.outcomes)
				}
			}
			if mode == "publication-failure" {
				p, e := st.LoadRun(t.Context(), parent.ID)
				if e != nil || len(p.SubbotChildren) != 0 {
					t.Fatalf("unacknowledged publication linked parent: %v %v", p, e)
				}
			}
			if s.releases != s.locks {
				t.Fatalf("CHILD_LOCK_LEAK: acquired=%d released=%d", s.locks, s.releases)
			}
		})
	}
}
