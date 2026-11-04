package store

import (
	"context"
	"fmt"
	"slices"
	"time"
)

// Filesystem twin of CredSlotStore. Process-local by design: the local
// runtime is one process, and the cloud publisher — the ledger's only caller
// — runs on this store in tests alone. The ledger therefore lives in memory
// under its own mutex while the run documents on disk stay the truth it
// reconciles against, on the same admission, adoption and eviction rules as
// the Mongo ledger (Run.SpendsCredential, CredSlotHolderStale).

// credSlotHolder is one admitted run and the instant of its latest
// admission — the instant CredSlotGrace is measured from when its document
// is missing.
type credSlotHolder struct {
	runID string
	at    time.Time
}

var _ CredSlotStore = (*FilesystemRunStore)(nil)

// ReserveCredSlot implements CredSlotStore: one admission under the ledger
// mutex, so concurrent callers of one process are serialized exactly as
// Mongo serializes a conditional update. The mutex also spans the
// reconcile's judgment and its eviction, so no re-admission can land between
// the two — the window the Mongo twin closes by moving a re-admitted
// holder's instant forward, which this twin does too, for one rule on both.
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
	if err := s.adoptUnledgeredCredSlotHolders(ctx, fingerprint, runID, now); err != nil {
		return CredSlotAdmission{}, err
	}
	for attempt := 0; ; attempt++ {
		holders := s.credSlots[fingerprint]
		held := -1
		for i, h := range holders {
			if h.runID == runID {
				held = i
				break
			}
		}
		switch {
		case held >= 0 && len(holders) <= max:
			at := NextCredSlotInstant(holders[held].at, now)
			holders[held].at = at
			return CredSlotAdmission{Granted: true, Held: len(holders), At: at}, nil
		case held < 0 && len(holders) < max:
			at := NextCredSlotInstant(time.Time{}, now)
			s.credSlots[fingerprint] = append(holders, credSlotHolder{runID: runID, at: at})
			return CredSlotAdmission{Granted: true, Held: len(holders) + 1, At: at}, nil
		}
		// Full of other runs — or, under a cap lowered below its holders,
		// over it with this run among them. Once, re-read the holders
		// against their run documents: a slot whose run is no longer
		// spending is free, it just has not been given back yet.
		if attempt > 0 || s.evictStaleCredSlotHolders(ctx, fingerprint, now) == 0 {
			return CredSlotAdmission{Granted: false, Held: len(holders)}, nil
		}
	}
}

// adoptUnledgeredCredSlotHolders names in the row of fingerprint every run
// other than runID whose document spends it with no admission of its own
// still to come (Run.CredSlotAdoptable) and that the row does not name —
// the adoption store.CredSlotStore's admission performs before it decides,
// outside the cap. Called under credSlotMu; the scan takes no store lock.
func (s *FilesystemRunStore) adoptUnledgeredCredSlotHolders(ctx context.Context, fingerprint, runID string, now time.Time) error {
	spenders, err := s.filterRunsSorted(ctx, func(r *Run) bool {
		return r.ID != runID && r.CredSlotAdoptable(fingerprint)
	})
	if err != nil {
		return fmt.Errorf("store: reserve cred slot %s for run %s: list the runs that spend it: %w", fingerprint, runID, err)
	}
	holders := s.credSlots[fingerprint]
	for _, id := range spenders {
		if slices.ContainsFunc(holders, func(h credSlotHolder) bool { return h.runID == id }) {
			continue
		}
		holders = append(holders, credSlotHolder{runID: id, at: NextCredSlotInstant(time.Time{}, now)})
	}
	if len(holders) > 0 {
		s.credSlots[fingerprint] = holders
	}
	return nil
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

// ReleaseCredSlot implements CredSlotStore: the entry of runID admitted at
// `at`, and no other.
func (s *FilesystemRunStore) ReleaseCredSlot(_ context.Context, fingerprint, runID string, at time.Time) error {
	if fingerprint == "" || runID == "" {
		return fmt.Errorf("store: release cred slot: fingerprint and run id are required (fingerprint=%q run=%q)", fingerprint, runID)
	}
	if at.IsZero() {
		return fmt.Errorf("store: release cred slot %s of run %s: no admission instant — a release names the admission it undoes", fingerprint, runID)
	}
	s.credSlotMu.Lock()
	defer s.credSlotMu.Unlock()
	s.pullCredSlotHolder(fingerprint, func(h credSlotHolder) bool { return h.runID == runID && h.at.Equal(at) })
	return nil
}

// SettleCredSlotAdmission implements CredSlotStore: under the store lock,
// like the flip it settles, so it lands on the document that flip wrote or
// on none.
func (s *FilesystemRunStore) SettleCredSlotAdmission(_ context.Context, runID string, queuedAt time.Time) error {
	if runID == "" || queuedAt.IsZero() {
		return fmt.Errorf("store: settle the cred slot admission of run %q: run id and the flip's queued_at are required (queued_at=%v)", runID, queuedAt)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.loadRunRaw(runID)
	if err != nil {
		return fmt.Errorf("store: settle the cred slot admission of run %s: %w", runID, err)
	}
	if !r.AwaitsCredSlotAdmission() || r.QueuedAt == nil || !r.QueuedAt.Equal(queuedAt) {
		return nil
	}
	r.CredAdmissionPending = false
	if err := s.writeRun(r); err != nil {
		return fmt.Errorf("store: settle the cred slot admission of run %s: %w", runID, err)
	}
	return nil
}

// dropCredSlotHolder pulls runID from every row of the ledger — what
// DeleteRun owes the ledger, as the Mongo twin's DeleteRun pulls the run from
// every cred_slots document at once.
func (s *FilesystemRunStore) dropCredSlotHolder(runID string) {
	s.credSlotMu.Lock()
	defer s.credSlotMu.Unlock()
	for fingerprint := range s.credSlots {
		s.pullCredSlotHolder(fingerprint, func(h credSlotHolder) bool { return h.runID == runID })
	}
}

// pullCredSlotHolder removes the entries of one row that match, dropping
// the row once it names nobody. Called under credSlotMu.
func (s *FilesystemRunStore) pullCredSlotHolder(fingerprint string, match func(credSlotHolder) bool) {
	holders := s.credSlots[fingerprint]
	kept := holders[:0:0]
	for _, h := range holders {
		if !match(h) {
			kept = append(kept, h)
		}
	}
	if len(kept) == 0 {
		delete(s.credSlots, fingerprint)
		return
	}
	s.credSlots[fingerprint] = kept
}
