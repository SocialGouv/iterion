package dispatcher

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// claimEntry is one issue claim this dispatcher process placed on the
// tracker and has not yet released.
type claimEntry struct {
	IssueID    string    `json:"issue_id"`
	Identifier string    `json:"identifier"`
	Marker     string    `json:"marker"` // "<host>-<pid>"
	ClaimedAt  time.Time `json:"claimed_at"`
}

// claimJournal persists the dispatcher's in-flight tracker claims to
// <storeDir>/dispatcher/claims.json so a SIGKILL'd daemon's successor
// can release them at boot. The native tracker stores host-pid markers
// on the issues themselves and sweeps them via SweepStaleClaims;
// external adapters (github/forgejo/gitlab) carry the claim as a
// markerless label, so without this journal an issue claimed by a
// crashed dispatcher stays filtered out of ListCandidates until an
// operator removes the label by hand.
//
// Safety property: a journal entry only ever names an issue THIS
// dispatcher claimed (peers never claim an already-labelled issue —
// ListCandidates excludes them), so releasing journalled claims whose
// PID is dead can't steal a claim legitimately held elsewhere. A label
// placed by hand has no journal entry and is never touched.
//
// The journal lives on the local store dir; an ephemeral store (a pod
// without a PVC) loses it on eviction — the same durability the
// in-memory retry schedule has today.
type claimJournal struct {
	path   string
	logger *iterlog.Logger

	mu      sync.Mutex
	entries map[string]claimEntry // issue id → entry
}

// newClaimJournal loads (or initialises) the journal under storeDir.
// Empty storeDir returns nil; all methods are nil-safe no-ops so
// store-less dispatchers keep today's behaviour.
func newClaimJournal(storeDir string, logger *iterlog.Logger) *claimJournal {
	if storeDir == "" {
		return nil
	}
	j := &claimJournal{
		path:    filepath.Join(storeDir, "dispatcher", "claims.json"),
		logger:  logger,
		entries: make(map[string]claimEntry),
	}
	raw, err := os.ReadFile(j.path)
	if err == nil {
		var list []claimEntry
		if uerr := json.Unmarshal(raw, &list); uerr != nil {
			// A corrupt journal must not wedge startup; the cost is one
			// manual label cleanup, same as pre-journal behaviour.
			logger.Warn("dispatcher: claim journal %s unreadable (%v) — starting empty", j.path, uerr)
		} else {
			for _, e := range list {
				j.entries[e.IssueID] = e
			}
		}
	} else if !os.IsNotExist(err) {
		logger.Warn("dispatcher: claim journal %s: %v — starting empty", j.path, err)
	}
	return j
}

// Record journals a claim about to be placed. Called BEFORE the
// tracker Claim so a crash between the two leaves an entry that the
// boot sweep resolves with an idempotent Release (releasing an
// unclaimed issue is not an error per the Tracker contract).
func (j *claimJournal) Record(e claimEntry) {
	if j == nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.entries[e.IssueID] = e
	j.persistLocked()
}

// Remove drops an issue's entry after its claim is released (or was
// never placed because Claim errored).
func (j *claimJournal) Remove(issueID string) {
	if j == nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, ok := j.entries[issueID]; !ok {
		return
	}
	delete(j.entries, issueID)
	j.persistLocked()
}

// Load snapshots the journalled entries (boot sweep input).
func (j *claimJournal) Load() []claimEntry {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]claimEntry, 0, len(j.entries))
	for _, e := range j.entries {
		out = append(out, e)
	}
	return out
}

// Contains reports whether this dispatcher still owns an unreleased tracker
// claim for issueID. It lets candidate-based UI pruning retain diagnostics for
// deliberately parked, claimed cards that ListCandidates correctly omits.
func (j *claimJournal) Contains(issueID string) bool {
	if j == nil {
		return false
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	_, ok := j.entries[issueID]
	return ok
}

// persistLocked rewrites the journal atomically (tmp + rename). Called
// under j.mu. A write failure is logged, not fatal: the in-memory view
// stays correct for this process; only crash recovery degrades.
func (j *claimJournal) persistLocked() {
	list := make([]claimEntry, 0, len(j.entries))
	for _, e := range j.entries {
		list = append(list, e)
	}
	raw, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		j.logger.Warn("dispatcher: claim journal marshal: %v", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(j.path), 0o755); err != nil {
		j.logger.Warn("dispatcher: claim journal dir: %v", err)
		return
	}
	tmp := j.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		j.logger.Warn("dispatcher: claim journal write: %v", err)
		return
	}
	if err := os.Rename(tmp, j.path); err != nil {
		j.logger.Warn("dispatcher: claim journal rename: %v", err)
	}
}
