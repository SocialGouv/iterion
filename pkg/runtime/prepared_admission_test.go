package runtime

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/store"
	"testing"
	"time"
)

func TestPreparedQueuedPeerBeforeInitialRead(t *testing.T) {
	for _, mode := range []string{"prepared-queued-peer", "prepared-queued-control", "existing-peer", "historical-peer", "prepared-running-control"} {
		t.Run(mode, func(t *testing.T) {
			fs, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			status := store.RunStatusQueued
			if mode == "prepared-running-control" {
				status = store.RunStatusRunning
			}
			p, err := store.PrepareRunInvocation(t.Context(), fs, store.NewLegacyRunInvocation{Spec: store.RunCreateSpec{ID: "run", Status: status}})
			if err != nil {
				t.Fatal(err)
			}
			var peer store.RunExecutionAuthority
			peerCase := mode == "prepared-queued-peer" || mode == "existing-peer" || mode == "historical-peer"
			if peerCase {
				_, expected, ok := p.OriginalBirth()
				if !ok {
					t.Fatal("no birth")
				}
				var acquired bool
				peer, acquired, err = fs.AcquireRunExecutionIf(p.Context, "run", expected, store.ExecutionStart)
				if err != nil || !acquired {
					t.Fatal("peer acquisition", acquired, err)
				}
			}
			if mode == "existing-peer" {
				p, err = store.PrepareRunInvocation(p.Context, fs, store.ExistingRunInvocation{ID: "run"})
				if err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			spy := &queuedStartProbe{FilesystemRunStore: fs}
			engine := admissionSnapshotEngine(t, spy, nil, &calls)
			if mode == "historical-peer" {
				err = engine.Run(p.Context, "run", nil)
			} else {
				err = engine.RunPrepared(p.Context, "run", nil, p)
			}
			current, loadErr := fs.LoadRun(t.Context(), "run")
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			t.Logf("mode=%s err=%v nodes=%d own_acquire=%d legacy=%d status=%s peer_matches=%v", mode, err, calls, spy.acquisitions, spy.legacyTransitions, current.Status, peer.Matches(current))
			if mode == "prepared-queued-peer" {
				if !errors.Is(err, ErrAdmissionStopped) || calls != 0 || spy.acquisitions != 0 || !peer.Matches(current) {
					t.Fatalf("PREPARED_PEER_ADOPTED: err=%v nodes=%d own_acquire=%d status=%s", err, calls, spy.acquisitions, current.Status)
				}
			} else if err != nil || calls != 1 || current.Status != store.RunStatusFinished {
				t.Fatalf("control changed: %v %d %s", err, calls, current.Status)
			}
		})
	}
}

func TestBirthTimestampAccessorIsolation(t *testing.T) {
	fs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p, err := store.PrepareRunInvocation(t.Context(), fs, store.NewLegacyRunInvocation{Spec: store.RunCreateSpec{ID: "run", Status: store.RunStatusQueued}})
	if err != nil {
		t.Fatal(err)
	}
	_, a, ok := p.OriginalBirth()
	if !ok || a.QueuedAt == nil {
		t.Fatal("missing marker")
	}
	initial := *a.QueuedAt
	*a.QueuedAt = a.QueuedAt.Add(time.Hour)
	a.Epoch = "foreign"
	changed := initial.Add(2 * time.Hour)
	p.Run.QueuedAt = &changed
	_, b, ok := p.OriginalBirth()
	if !ok || b.QueuedAt == nil || !b.QueuedAt.Equal(initial) || b.Epoch == "foreign" {
		t.Fatalf("BIRTH_MUTABLE: %+v", b)
	}
	calls := 0
	e := admissionSnapshotEngine(t, fs, nil, &calls)
	if err = e.RunPrepared(p.Context, "run", nil, p); err != nil || calls != 1 {
		t.Fatalf("private marker not usable: %v %d", err, calls)
	}
}

type independentAdmissionPeerStore struct {
	*queuedStartProbe
	mode        string
	injected    bool
	conflicts   int
	peer        store.RunExecutionAuthority
	peerVersion int64
}

func (s *independentAdmissionPeerStore) SaveRun(ctx context.Context, r *store.Run) error {
	if r.Admission != nil && !s.injected {
		s.injected = true
		fresh, err := s.LoadRun(ctx, r.ID)
		if err != nil {
			return err
		}
		if s.mode == "metadata" {
			fresh.Name = "renamed during admission"
			err = s.FilesystemRunStore.SaveRun(ctx, fresh)
		} else {
			var ok bool
			s.peer, ok, err = s.FilesystemRunStore.AcquireRunExecutionIf(ctx, r.ID, store.RunExecutionExpectation{Version: fresh.CASVersion, Epoch: fresh.ExecutionEpoch, Status: fresh.Status, QueuedAt: fresh.QueuedAt}, store.ExecutionStart)
			if err == nil && !ok {
				return errors.New("peer fixture not acquired")
			}
			if err == nil {
				fresh, err = s.LoadRun(ctx, r.ID)
				s.peerVersion = fresh.CASVersion
			}
		}
		if err != nil {
			return err
		}
	}
	err := s.FilesystemRunStore.SaveRun(ctx, r)
	if errors.Is(err, store.ErrRunConflict) {
		s.conflicts++
	}
	return err
}
func TestPreparedGrantSurvivesAdmissionRetry(t *testing.T) {
	for _, mode := range []string{"prepared-peer", "metadata", "existing-peer", "historical-peer", "running-peer"} {
		t.Run(mode, func(t *testing.T) {
			fs, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			status := store.RunStatusQueued
			if mode == "running-peer" {
				status = store.RunStatusRunning
			}
			p, err := store.PrepareRunInvocation(t.Context(), fs, store.NewLegacyRunInvocation{Spec: store.RunCreateSpec{ID: "run", Status: status}})
			if err != nil {
				t.Fatal(err)
			}
			if mode == "existing-peer" {
				p, err = store.PrepareRunInvocation(p.Context, fs, store.ExistingRunInvocation{ID: "run"})
				if err != nil {
					t.Fatal(err)
				}
			}
			spy := &independentAdmissionPeerStore{queuedStartProbe: &queuedStartProbe{FilesystemRunStore: fs}, mode: mode}
			calls := 0
			e := admissionSnapshotEngine(t, spy, nil, &calls)
			if mode == "historical-peer" {
				err = e.Run(p.Context, "run", nil)
			} else {
				err = e.RunPrepared(p.Context, "run", nil, p)
			}
			r, loadErr := fs.LoadRun(t.Context(), "run")
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			t.Logf("mode=%s err=%v own_acquire=%d conflict=%d nodes=%d status=%s peer_matches=%v versions=%d/%d admission=%v", mode, err, spy.acquisitions, spy.conflicts, calls, r.Status, spy.peer.Matches(r), spy.peerVersion, r.CASVersion, r.Admission != nil)
			if spy.conflicts != 1 || !spy.injected {
				t.Fatal("no real conflict")
			}
			if mode == "prepared-peer" {
				if !errors.Is(err, ErrAdmissionStopped) || calls != 0 || spy.acquisitions != 1 || !spy.peer.Matches(r) || r.CASVersion != spy.peerVersion || r.Admission != nil {
					t.Fatalf("ADMISSION_PEER_ADOPTED: %v nodes=%d status=%s", err, calls, r.Status)
				}
			} else if err != nil || calls != 1 || r.Status != store.RunStatusFinished {
				t.Fatalf("control changed: %v nodes=%d status=%s", err, calls, r.Status)
			}
		})
	}
}
