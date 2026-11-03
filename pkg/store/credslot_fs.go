package store

import (
	"context"
	"fmt"
	"time"
)

// Filesystem twin of CredSlotStore. Process-local by design: the local
// runtime is one process, and the cloud publisher — the ledger's only caller
// — runs on this store in tests alone. The ledger therefore lives in memory
// under its own mutex while the run documents on disk stay the truth it
// reconciles against, on the same admission and eviction rules as the Mongo
// ledger (CredSlotHolderStale).

// credSlotHolder is one admitted run and the instant it was admitted — the
// instant CredSlotGrace is measured from when its document is missing.
type credSlotHolder struct {
	runID string
	at    time.Time
}

var _ CredSlotStore = (*FilesystemRunStore)(nil)

// ReserveCredSlot implements CredSlotStore: one admission under the ledger
// mutex, so concurrent callers of one process are serialized exactly as
// Mongo serializes a conditional update.
func (s *FilesystemRunStore) ReserveCredSlot(ctx context.Context, fingerprint, runID string, max int) (CredSlotAdmission, error) {
	if fingerprint == "" || runID == "" {
		return CredSlotAdmission{}, fmt.Errorf("store: reserve cred slot: fingerprint and run id are required (fingerprint=%q run=%q)", fingerprint, runID)
	}
	if max <= 0 {
		return CredSlotAdmission{}, fmt.Errorf("store: reserve cred slot for run %s: max=%d — an uncapped key reserves nothing", runID, max)
	}
	s.credSlotMu.Lock()
	defer s.credSlotMu.Unlock()
	if s.credSlots == nil {
		s.credSlots = map[string][]credSlotHolder{}
	}
	now := time.Now().UTC()
	for attempt := 0; ; attempt++ {
		holders := s.credSlots[fingerprint]
		for _, h := range holders {
			if h.runID == runID {
				return CredSlotAdmission{Granted: true, Held: len(holders)}, nil
			}
		}
		if len(holders) < max {
			s.credSlots[fingerprint] = append(holders, credSlotHolder{runID: runID, at: now})
			return CredSlotAdmission{Granted: true, Held: len(holders) + 1}, nil
		}
		// Full. Once, re-read the holders against their run documents: a
		// slot whose run is no longer spending is free, it just has not
		// been given back yet.
		if attempt > 0 || s.evictStaleCredSlotHolders(ctx, fingerprint, now) == 0 {
			return CredSlotAdmission{Granted: false, Held: len(holders)}, nil
		}
	}
}

// evictStaleCredSlotHolders drops every holder of fingerprint whose run no
// longer spends it (CredSlotHolderStale) and reports how many it dropped.
// Called under credSlotMu; LoadRun takes no store lock.
func (s *FilesystemRunStore) evictStaleCredSlotHolders(ctx context.Context, fingerprint string, now time.Time) int {
	holders := s.credSlots[fingerprint]
	kept := holders[:0:0]
	for _, h := range holders {
		run, err := s.LoadRun(ctx, h.runID)
		if CredSlotHolderStale(run, err, h.at, now, fingerprint) {
			continue
		}
		kept = append(kept, h)
	}
	s.credSlots[fingerprint] = kept
	return len(holders) - len(kept)
}

// ReleaseCredSlot implements CredSlotStore.
func (s *FilesystemRunStore) ReleaseCredSlot(_ context.Context, fingerprint, runID string) error {
	if fingerprint == "" || runID == "" {
		return fmt.Errorf("store: release cred slot: fingerprint and run id are required (fingerprint=%q run=%q)", fingerprint, runID)
	}
	s.credSlotMu.Lock()
	defer s.credSlotMu.Unlock()
	s.pullCredSlotHolder(fingerprint, runID)
	return nil
}

// dropCredSlotHolder pulls runID from every row of the ledger — what
// DeleteRun owes the ledger, as the Mongo twin's DeleteRun pulls the run from
// every cred_slots document at once.
func (s *FilesystemRunStore) dropCredSlotHolder(runID string) {
	s.credSlotMu.Lock()
	defer s.credSlotMu.Unlock()
	for fingerprint := range s.credSlots {
		s.pullCredSlotHolder(fingerprint, runID)
	}
}

// pullCredSlotHolder removes runID from one row, dropping the row once it
// names nobody. Called under credSlotMu.
func (s *FilesystemRunStore) pullCredSlotHolder(fingerprint, runID string) {
	holders := s.credSlots[fingerprint]
	kept := holders[:0:0]
	for _, h := range holders {
		if h.runID != runID {
			kept = append(kept, h)
		}
	}
	if len(kept) == 0 {
		delete(s.credSlots, fingerprint)
		return
	}
	s.credSlots[fingerprint] = kept
}
