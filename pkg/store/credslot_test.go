package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// The filesystem twin of the credential-slot ledger, on the parts the
// cross-backend conformance row cannot reach: the race itself (N callers of
// one process asking for K slots at once) and the grace a holder with no run
// document keeps, which needs the holder's own instant moved.

func credSlotRun(t *testing.T, s *FilesystemRunStore, id string, status RunStatus, fps ...string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.CreateRun(ctx, id, "demo", nil); err != nil {
		t.Fatalf("CreateRun %s: %v", id, err)
	}
	r, err := s.LoadRun(ctx, id)
	if err != nil {
		t.Fatalf("LoadRun %s: %v", id, err)
	}
	r.Status = status
	if err := s.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun %s: %v", id, err)
	}
	if len(fps) > 0 {
		if err := s.SetRunCredStamp(ctx, id, RunCredStamp{Fingerprints: fps}); err != nil {
			t.Fatalf("SetRunCredStamp %s: %v", id, err)
		}
	}
}

// Of N runs asking at once for K slots of one key, exactly K are admitted —
// the property the walk's own count cannot give (it reads the run documents
// before the run is written, so every racer reads the same count).
func TestCredSlotLedger_ConcurrentReservationsAdmitExactlyTheCap(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	const fp, max, racers = "fp-capped", 3, 40
	ids := make([]string, racers)
	for i := range ids {
		ids[i] = "racer-" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		credSlotRun(t, s, ids[i], RunStatusQueued, fp)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	granted, refused := 0, 0
	heldSeen := map[int]bool{}
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			<-start
			adm, err := s.ReserveCredSlot(context.Background(), fp, id, max)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				t.Errorf("ReserveCredSlot(%s): %v", id, err)
				return
			}
			heldSeen[adm.Held] = true
			if adm.Granted {
				granted++
			} else {
				refused++
			}
		}(id)
	}
	close(start)
	wg.Wait()
	if granted != max || refused != racers-max {
		t.Fatalf("granted=%d refused=%d, want exactly %d granted of %d racers: the ledger admitted past the cap", granted, refused, max, racers)
	}
	for held := range heldSeen {
		if held > max {
			t.Fatalf("an admission reported %d holders on a cap of %d", held, max)
		}
	}
}

// A holder whose run document is missing keeps its slot for CredSlotGrace —
// a launch reserves before it persists — and loses it once the grace has
// passed: the document is not coming, the launch died between its writes.
func TestCredSlotLedger_MissingRunDocumentKeepsItsSlotForTheGraceOnly(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := context.Background()
	const fp = "fp-capped"
	if adm, err := s.ReserveCredSlot(ctx, fp, "ghost", 1); err != nil || !adm.Granted {
		t.Fatalf("ghost reservation: %+v %v", adm, err)
	}
	credSlotRun(t, s, "live", RunStatusRunning, fp)
	if adm, err := s.ReserveCredSlot(ctx, fp, "live", 1); err != nil || adm.Granted {
		t.Fatalf("a holder reserved moments ago with no document yet was evicted: %+v %v", adm, err)
	}
	// Age the ghost's reservation past the grace: the next refusal evicts it.
	s.credSlotMu.Lock()
	s.credSlots[fp][0].at = time.Now().UTC().Add(-CredSlotGrace - time.Second)
	s.credSlotMu.Unlock()
	if adm, err := s.ReserveCredSlot(ctx, fp, "live", 1); err != nil || !adm.Granted || adm.Held != 1 {
		t.Fatalf("a holder with no document past the grace kept its slot: %+v %v", adm, err)
	}
}

// The eviction rule both twins read, case by case.
func TestCredSlotHolderStale(t *testing.T) {
	now := time.Now().UTC()
	fresh, old := now.Add(-time.Second), now.Add(-CredSlotGrace-time.Second)
	spending := &Run{Status: RunStatusRunning, CredFingerprints: []string{"fp"}}
	idle := &Run{Status: RunStatusRunning, CredFingerprints: []string{"fp"}, LLMIdleSince: &now}
	parked := &Run{Status: RunStatusFailedResumable, CredFingerprints: []string{"fp"}}
	restamped := &Run{Status: RunStatusRunning, CredFingerprints: []string{"other"}}
	cases := []struct {
		name  string
		run   *Run
		err   error
		at    time.Time
		stale bool
	}{
		{"spending holder stays", spending, nil, old, false},
		{"model-idle holder goes", idle, nil, old, true},
		{"parked holder goes", parked, nil, old, true},
		{"re-stamped holder goes", restamped, nil, old, true},
		{"missing document inside the grace stays", nil, ErrRunNotFound, fresh, false},
		{"missing document past the grace goes", nil, ErrRunNotFound, old, true},
		{"deleted document past the grace goes", nil, ErrRunDeleted, old, true},
		{"a store that cannot answer keeps the holder", nil, errors.New("timeout"), old, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := CredSlotHolderStale(c.run, c.err, c.at, now, "fp"); got != c.stale {
				t.Fatalf("stale=%v, want %v", got, c.stale)
			}
		})
	}
}
