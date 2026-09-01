package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/SocialGouv/iterion/pkg/internal/appinfo"
)

// ---------------------------------------------------------------------------
// Run lifecycle
// ---------------------------------------------------------------------------

// CreateRun persists a new run with status "running". Captures the
// iterion-relevant launch env (model/effort/provider knobs) and
// iterion build version so the run record is reproducible later —
// without these, "why did the same recipe + same inputs produce
// different outputs across two daemon builds" is unanswerable.
//
// CreateRun is intentionally no-clobber: reusing a run ID must fail
// instead of resetting an existing run's metadata/checkpoint. Resume and
// crash-recovery code relies on run.json being the authoritative identity
// and checkpoint record for a run.
func (s *FilesystemRunStore) CreateRun(_ context.Context, id, workflowName string, inputs map[string]any) (*Run, error) {
	if err := sanitizePathComponent("run ID", id); err != nil {
		return nil, err
	}
	// A deleted run id is never reusable: run ids are time-prefixed so
	// honest collisions are impraticable, and re-creating one is
	// exactly the resurrection the tombstone exists to block.
	if err := s.guardNotDeleted(id); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	r := &Run{
		FormatVersion:  RunFormatVersion,
		ID:             id,
		WorkflowName:   workflowName,
		Status:         RunStatusRunning,
		Inputs:         inputs,
		CreatedAt:      now,
		UpdatedAt:      now,
		LaunchEnv:      CaptureLaunchEnv(),
		IterionVersion: appinfo.FullVersion(),
	}
	if err := s.writeRunNew(r); err != nil {
		return nil, err
	}
	return r, nil
}

var _ ParentedRunCreator = (*FilesystemRunStore)(nil)

// CreateChildRun persists a new run with status "running", stamping
// ParentRunID in the same exclusive-create write as the run document.
// It exists so spawnRun's precreate never leaves a running child doc
// behind a failed second SaveRun (see store.ParentedRunCreator).
func (s *FilesystemRunStore) CreateChildRun(_ context.Context, id, workflowName, parentRunID string, inputs map[string]any) (*Run, error) {
	if err := sanitizePathComponent("run ID", id); err != nil {
		return nil, err
	}
	if err := s.guardNotDeleted(id); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	r := &Run{
		FormatVersion:  RunFormatVersion,
		ID:             id,
		WorkflowName:   workflowName,
		ParentRunID:    parentRunID,
		Status:         RunStatusRunning,
		Inputs:         inputs,
		CreatedAt:      now,
		UpdatedAt:      now,
		LaunchEnv:      CaptureLaunchEnv(),
		IterionVersion: appinfo.FullVersion(),
	}
	if err := s.writeRunNew(r); err != nil {
		return nil, err
	}
	return r, nil
}

// CreateQueuedRun persists a new run with status "queued" — the state
// the pipeline board renders in its TODO lane while the run waits for a
// local concurrency slot. It mirrors CreateRun's exclusive-create
// contract (a reused run ID fails) but additionally stamps the file path
// and bot id so the local scheduler can compile + start the run later,
// and the board can render a meaningful card, without an in-memory
// launch spec. The engine's runResolveDoc transitions the doc
// queued→running on pickup, so no engine change is needed to start it.
//
// Implements store.QueuedRunCreator (filesystem-only; cloud stores
// already create runs queued via CreateRun).
func (s *FilesystemRunStore) CreateQueuedRun(_ context.Context, id, workflowName, filePath, botID string, inputs map[string]any) (*Run, error) {
	if err := sanitizePathComponent("run ID", id); err != nil {
		return nil, err
	}
	// Same tombstone rule as CreateRun: a deleted run id is never
	// reusable. Without the guard the exclusive create would succeed —
	// a tombstoned dir has no run.json — and resurrect the run as an
	// unreadable zombie (run.json and the deletion marker side by side).
	if err := s.guardNotDeleted(id); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	r := &Run{
		FormatVersion:  RunFormatVersion,
		ID:             id,
		WorkflowName:   workflowName,
		FilePath:       filePath,
		BotID:          botID,
		Status:         RunStatusQueued,
		Inputs:         inputs,
		CreatedAt:      now,
		UpdatedAt:      now,
		QueuedAt:       &now,
		LaunchEnv:      CaptureLaunchEnv(),
		IterionVersion: appinfo.FullVersion(),
	}
	if err := s.writeRunNew(r); err != nil {
		return nil, err
	}
	return r, nil
}

// SaveRun persists the run metadata to disk. Protected by mu so it
// cannot race against UpdateRunStatus / SaveCheckpoint / WriteArtifact
// — two runners reconciling the same orphan via RecoverFinalize, or a
// finalize path concurrent with an engine status update, would
// otherwise read-modify-write through each other and lose fields.
func (s *FilesystemRunStore) SaveRun(_ context.Context, r *Run) error {
	if err := s.guardNotDeleted(r.ID); err != nil {
		return err
	}
	// Best-effort guard: a copy whose STATUS is already non-failure
	// must not resurrect its failure code through this full-document
	// write. A copy stale on the status itself still rewrites
	// status+code together (the inherent SaveRun read-modify-write
	// hazard — a version CAS is the real fix, follow-up); callers on
	// that path re-stamp the fields by hand (see rewind.go).
	if !r.Status.CarriesFailureCode() {
		r.FailureCode = ""
	}
	// Same discipline for the pause pointer: a full-document write on a
	// non-carrying status must not resurrect consumed interaction
	// evidence.
	if !r.Status.CarriesPausePointer() && r.Checkpoint != nil &&
		(r.Checkpoint.InteractionID != "" || len(r.Checkpoint.InteractionQuestions) > 0) {
		cp := *r.Checkpoint
		cp.InteractionID = ""
		cp.InteractionQuestions = nil
		r.Checkpoint = &cp
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// The merge claim is owned by ClaimMerge/UpdateRunMergeIf. A caller
	// whose copy predates a live claim (rename, rewind bookkeeping)
	// must not disavow it through this full-document write: clobbering
	// merge_status+merge_claimed_at lets the next claimant through
	// while the first is mid-merge — the double-squash the claim
	// exists to prevent. Atomic here (under s.mu, same as writeRun).
	if cur, err := s.loadRunRaw(r.ID); err == nil &&
		cur.MergeStatus == MergeStatusMerging && r.MergeStatus != MergeStatusMerging {
		r.MergeStatus = cur.MergeStatus
		r.MergeClaimedAt = cur.MergeClaimedAt
	}
	return s.writeRun(r)
}

// loadRunRaw is the pure-read variant of LoadRun: it parses run.json
// and returns the Run without firing the name backfill or
// finished_at heal. Used by every method that holds s.mu around its
// own read-modify-write — the public LoadRun's healing side-effects
// would otherwise sneak a second writeRun into a critical section
// the caller didn't account for (its own follow-up writeRun would
// then race the persisted state against its own in-memory copy).
func (s *FilesystemRunStore) loadRunRaw(id string) (*Run, error) {
	if err := sanitizePathComponent("run ID", id); err != nil {
		return nil, err
	}
	p := s.runJSONPath(id)
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			// A tombstoned run has no run.json but DOES carry the
			// deletion marker — surface the deliberate deletion (410)
			// distinctly from genuine absence (404).
			if s.runDeleted(id) {
				return nil, fmt.Errorf("store: load run %s: %w", id, ErrRunDeleted)
			}
			// Wrap the shared sentinel (alongside the underlying
			// os.ErrNotExist the ReadFile error already carries) so callers
			// can distinguish genuine absence from a transient read failure
			// via errors.Is(err, ErrRunNotFound) — see ErrRunNotFound.
			return nil, fmt.Errorf("store: load run %s: %w: %w", id, ErrRunNotFound, err)
		}
		return nil, fmt.Errorf("store: load run %s: %w", id, err)
	}
	var r Run
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("store: decode run %s: %w", id, err)
	}
	return &r, nil
}

// healRun applies the on-read fixups (legacy-name backfill,
// finished_at sanity check) and returns true if a write is needed.
// Pure data manipulation; the caller decides when to persist.
func healRun(r *Run) bool {
	changed := false
	if r.Name == "" {
		r.Name = GenerateRunName(r.FilePath + ":" + r.ID)
		changed = true
	}
	if r.Status == RunStatusRunning && r.FinishedAt != nil {
		r.FinishedAt = nil
		changed = true
	}
	// A failure code may only persist on a failure status. The
	// transition machinery clears it and SaveRun normalizes, so the
	// remaining sources are historical rows written before those
	// guards and hand-edited run.json — heal on read.
	if r.FailureCode != "" && !r.Status.CarriesFailureCode() {
		r.FailureCode = ""
		changed = true
	}
	return changed
}

// loadRunHealBeforeLockHook is a test hook used to deterministically
// exercise the stale-read window before LoadRun's heal persistence enters the
// run-mutation critical section. Nil in production.
var loadRunHealBeforeLockHook func()

// LoadRun reads run.json for the given run ID.
//
// The run ID is sanitised before path-joining so a hostile or
// network-sourced ID cannot escape the store root. The write side
// (CreateRun/WriteArtifact/WriteInteraction) already sanitises its inputs;
// the read paths must do the same so the defence is symmetric.
//
// As a one-shot migration step, a legacy run with empty Name gets a
// deterministic friendly label generated and persisted on read. After
// the first call the field is on disk; subsequent LoadRuns skip the
// fixup. The seed mirrors the CLI/launch path (file_path:run_id) so the
// backfill produces the exact name a new launch would have produced.
//
// Callers that already hold s.mu and intend to write the run
// themselves should use loadRunRaw to avoid the embedded writeRun
// from the heal path interleaving with their own write.
func (s *FilesystemRunStore) LoadRun(_ context.Context, id string) (*Run, error) {
	r, err := s.loadRunRaw(id)
	if err != nil {
		return nil, err
	}
	if healRun(r) {
		if loadRunHealBeforeLockHook != nil {
			loadRunHealBeforeLockHook()
		}
		// Persist heal-on-read under the same mutex as every other run.json
		// read-modify-write. The first load above may have observed a legacy
		// stale copy; before writing the heal, re-read the current on-disk run
		// while holding s.mu so a concurrent SaveCheckpoint/UpdateRunStatus/
		// SaveRun cannot have its authoritative fields clobbered by this
		// best-effort migration write.
		s.mu.Lock()
		fresh, reloadErr := s.loadRunRaw(id)
		if reloadErr == nil {
			if healRun(fresh) {
				if writeErr := s.writeRun(fresh); writeErr != nil && s.logger != nil {
					s.logger.Warn("store: heal-on-read for run %s failed: %v", id, writeErr)
				}
			}
			s.mu.Unlock()
			return fresh, nil
		}
		s.mu.Unlock()

		// Best-effort persist; a write/reload failure (read-only fs, racing
		// process, deleted run) leaves the in-memory heal applied and lets the
		// next successful write fix it up. Never fail LoadRun on this path.
		if s.logger != nil {
			s.logger.Warn("store: reload for heal-on-read for run %s failed: %v", id, reloadErr)
		}
	}
	return r, nil
}

// UpdateRunStatus updates the status (and optional error) of a run.
// Protected by mu to prevent concurrent read-modify-write races.
func (s *FilesystemRunStore) UpdateRunStatus(ctx context.Context, id string, status RunStatus, runErr string) error {
	return s.UpdateRunStatusCoded(ctx, id, status, runErr, "")
}

// UpdateRunStatusCoded is UpdateRunStatus carrying the typed failure
// classification; the code lands (or is cleared) in the same write as
// the status.
func (s *FilesystemRunStore) UpdateRunStatusCoded(ctx context.Context, id string, status RunStatus, runErr string, code FailureCode) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, err := s.loadRunRaw(id)
	if err != nil {
		return err
	}
	return s.applyStatusTransition(r, status, runErr, code)
}

// PatchRunSteering persists the live-steering state on run.json.
// Partial: a nil loopOverrides / nil budgetRaises leaves the stored
// field untouched, so the two commands patch independently.
func (s *FilesystemRunStore) PatchRunSteering(_ context.Context, id string, loopOverrides map[string]int, budgetRaises *RunBudgetRaises) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, err := s.loadRunRaw(id)
	if err != nil {
		return err
	}
	if loopOverrides != nil {
		r.LoopOverrides = loopOverrides
	}
	if budgetRaises != nil {
		r.BudgetRaises = budgetRaises
	}
	r.UpdatedAt = time.Now().UTC()
	return s.writeRun(r)
}

// PatchRunPermissionGrants persists the permission-gate allow rules
// earned by the operator. Replaces the stored slice wholesale (the
// caller owns the accumulated set); a nil slice is a no-op patch.
func (s *FilesystemRunStore) PatchRunPermissionGrants(_ context.Context, id string, grants map[string][]string) error {
	if grants == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	r, err := s.loadRunRaw(id)
	if err != nil {
		return err
	}
	r.PermissionGrants = grants
	r.UpdatedAt = time.Now().UTC()
	return s.writeRun(r)
}

// RecordNodeServed persists the last (backend, model) that served
// nodeID. Last write wins per key; empty nodeID is a no-op.
func (s *FilesystemRunStore) RecordNodeServed(_ context.Context, id, nodeID string, served NodeServed) error {
	if nodeID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	r, err := s.loadRunRaw(id)
	if err != nil {
		return err
	}
	if r.NodesServed == nil {
		r.NodesServed = make(map[string]NodeServed, 1)
	}
	r.NodesServed[nodeID] = served
	r.UpdatedAt = time.Now().UTC()
	return s.writeRun(r)
}

// UpdateRunStatusIf is a compare-and-set on the status field: the
// write only lands when the current status is in expectedFrom. Used
// by callers that need to avoid racing with a concurrent transition
// (e.g. a Cancel firing while a Resume is republishing). Returns
// changed=true on a successful write, false if the status had
// drifted since the caller's last read.
func (s *FilesystemRunStore) UpdateRunStatusIf(ctx context.Context, id string, status RunStatus, runErr string, expectedFrom []RunStatus) (bool, error) {
	return s.UpdateRunStatusIfCoded(ctx, id, status, runErr, "", expectedFrom)
}

// UpdateRunStatusIfCoded is the CAS variant carrying the typed failure
// classification — code and status land in one atomic write, never a
// separate read-modify-write.
func (s *FilesystemRunStore) UpdateRunStatusIfCoded(ctx context.Context, id string, status RunStatus, runErr string, code FailureCode, expectedFrom []RunStatus) (bool, error) {
	if len(expectedFrom) == 0 {
		// A CAS with no expected set is a bug at the caller (a derived
		// slice gone empty) — refuse loudly instead of silently
		// matching nothing (while the Mongo twin would write
		// unconditionally).
		return false, fmt.Errorf("store: update status if %s: empty expectedFrom", id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	r, err := s.loadRunRaw(id)
	if err != nil {
		return false, err
	}
	matched := false
	for _, want := range expectedFrom {
		if r.Status == want {
			matched = true
			break
		}
	}
	if !matched {
		return false, nil
	}
	if err := s.applyStatusTransition(r, status, runErr, code); err != nil {
		return false, err
	}
	return true, nil
}

// mergeClaimable reports whether a run in status cur (with claim time
// claimedAt) can be claimed for merging at staleBefore: unset, pending
// and failed are always claimable; a "merging" claim is claimable only
// once stale (the previous claimant crashed mid-merge).
func mergeClaimable(cur MergeStatus, claimedAt, staleBefore time.Time) bool {
	switch cur {
	case "", MergeStatusPending, MergeStatusFailed, MergeStatusSkipped, MergeStatusConflicted:
		// skipped and conflicted stay claimable: /merge is the only
		// path that re-materialises a lost server-side merge clone, and
		// a recovered run (RecoverFinalize lands "skipped") must stay
		// mergeable. The exit CAS still serialises the outcome.
		return true
	case MergeStatusMerging:
		// A zero claimedAt (a full-document writer dropped the stamp)
		// counts as infinitely stale — it must not wedge the run.
		return claimedAt.IsZero() || claimedAt.Before(staleBefore)
	default:
		return false
	}
}

// ClaimMerge is the compare-and-set entry to the merge state machine
// (see store.RunStore). Guarded by the store mutex, so concurrent
// claimants in one process serialize here.
func (s *FilesystemRunStore) ClaimMerge(_ context.Context, id string, staleBefore time.Time) (bool, MergeStatus, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, err := s.loadRunRaw(id)
	if err != nil {
		return false, "", time.Time{}, err
	}
	prior := r.MergeStatus
	if !mergeClaimable(prior, r.MergeClaimedAt, staleBefore) {
		return false, prior, time.Time{}, nil
	}
	// Millisecond precision: the token must survive a Mongo round-trip
	// identically on both backends, and BSON stores times in ms.
	now := time.Now().UTC().Truncate(time.Millisecond)
	r.MergeStatus = MergeStatusMerging
	r.MergeClaimedAt = now
	r.UpdatedAt = now
	if err := s.writeRun(r); err != nil {
		return false, prior, time.Time{}, err
	}
	return true, prior, now, nil
}

// UpdateRunMergeIf is the compare-and-set exit from the merge state
// machine (see store.RunStore): the write only lands when the current
// MergeStatus is in expectedFrom.
func (s *FilesystemRunStore) UpdateRunMergeIf(_ context.Context, id string, upd RunMergeUpdate, expectedFrom []MergeStatus) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, err := s.loadRunRaw(id)
	if err != nil {
		return false, err
	}
	matched := false
	for _, want := range expectedFrom {
		if r.MergeStatus == want {
			matched = true
			break
		}
	}
	if !matched {
		return false, nil
	}
	if !upd.ExpectClaimedAt.IsZero() && !r.MergeClaimedAt.Equal(upd.ExpectClaimedAt) {
		// The claim this writer holds was stolen — its exit consumes
		// nothing.
		return false, nil
	}
	r.MergeStatus = upd.Status
	r.MergedCommit = upd.MergedCommit
	r.MergedInto = upd.MergedInto
	r.MergeStrategy = upd.MergeStrategy
	r.PendingMergeMessage = upd.PendingMergeMessage
	r.PendingMergeInto = upd.PendingMergeInto
	r.MergeClaimedAt = time.Time{}
	r.UpdatedAt = time.Now().UTC()
	if err := s.writeRun(r); err != nil {
		return false, err
	}
	return true, nil
}

// FailQueuedRunIfAttempt atomically fails only the queue attempt represented
// by publishedAt. A later resume refreshes QueuedAt before publishing, so an
// older delivery cannot clobber that new attempt during its queued→running
// hand-off window.
func (s *FilesystemRunStore) FailQueuedRunIfAttempt(_ context.Context, id, runErr string, publishedAt time.Time) (bool, error) {
	if publishedAt.IsZero() {
		return false, fmt.Errorf("store: fail queued attempt %s without published_at", id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	r, err := s.loadRunRaw(id)
	if err != nil {
		return false, err
	}
	if r.Status != RunStatusQueued || (r.QueuedAt != nil && r.QueuedAt.After(publishedAt)) {
		return false, nil
	}
	// Classification of the queue-park writer is follow-up work; the
	// empty code reads as unknown, which is honest here.
	if err := s.applyStatusTransition(r, RunStatusFailedResumable, runErr, ""); err != nil {
		return false, err
	}
	return true, nil
}

// applyStatusTransition is the shared tail of UpdateRunStatus and
// UpdateRunStatusIf: mutate r in-place (status, timestamps, terminal
// finished_at / resume FinishedAt clear, checkpoint clear when leaving
// paused state), then persist via writeRun. Caller must hold s.mu.
// The failure code follows the same discipline as Error: set on a
// failure status, cleared by every transition to a non-failure one —
// which is what makes a stale code after a resume impossible.
func (s *FilesystemRunStore) applyStatusTransition(r *Run, status RunStatus, runErr string, code FailureCode) error {
	r.Status = status
	r.UpdatedAt = time.Now().UTC()
	r.Error = runErr
	if status.CarriesFailureCode() {
		r.FailureCode = code
	} else {
		r.FailureCode = ""
	}
	switch status {
	case RunStatusFinished, RunStatusFailed, RunStatusFailedResumable, RunStatusCancelled:
		t := r.UpdatedAt
		r.FinishedAt = &t
	case RunStatusQueued:
		// Every queue publication is a distinct attempt. Refresh the marker
		// before publishing so a stale delivery can be rejected by identity,
		// not merely by the shared `queued` status.
		t := r.UpdatedAt
		r.QueuedAt = &t
		r.FinishedAt = nil
	case RunStatusRunning, RunStatusPausedWaitingHuman:
		// Resume paths (failed_resumable/cancelled → running) must clear
		// FinishedAt — otherwise the studio's duration ticker uses the
		// stale terminal timestamp and freezes mid-run.
		r.FinishedAt = nil
		if status == RunStatusRunning {
			// Mirror the Mongo twin: a running run carries no failure
			// message, whatever the caller passed.
			r.Error = ""
		}
	}
	// The pause pointer is a consumable: a transition into a status
	// that cannot truthfully carry it (CarriesPausePointer) clears the
	// interaction evidence — the checkpoint itself survives (below).
	// Without this, a status-only cancel of a paused run kept the
	// pointer, and a cloud resume (cancelled → queued, no answers)
	// routed back into the pause path and crossed the human gate with
	// an empty answer. Copy-on-write: failRunCheckpointed aliases the
	// caller's checkpoint into r just before this tail.
	if !status.CarriesPausePointer() && r.Checkpoint != nil &&
		(r.Checkpoint.InteractionID != "" || len(r.Checkpoint.InteractionQuestions) > 0) {
		cp := *r.Checkpoint
		cp.InteractionID = ""
		cp.InteractionQuestions = nil
		r.Checkpoint = &cp
	}
	// A status transition NEVER destroys the checkpoint. The running
	// claim used to clear it here — which, on a cloud pod, destroyed
	// the resume point the moment a resumed run was claimed: every
	// park writer that follows (drain, usage-cap, orphan sweeps,
	// --force-stale) flips running→failed_resumable WITHOUT a
	// checkpoint of its own, and the next resume restarted from the
	// workflow entry. A fresh launch has no checkpoint to keep, a
	// resumed run has everything to lose, and the engine overwrites it
	// at its first node boundary anyway. Finished likewise keeps it:
	// `iterion fork` reads a terminal parent's checkpoint for its
	// outputs. Only DeleteRun and the rewind machinery may remove one.
	return s.writeRun(r)
}

// SaveCheckpoint persists a checkpoint on a paused run.
// Protected by mu to prevent concurrent read-modify-write races.
func (s *FilesystemRunStore) SaveCheckpoint(ctx context.Context, id string, cp *Checkpoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, err := s.loadRunRaw(id)
	if err != nil {
		return err
	}
	// Same pointer discipline as the transition tail: a checkpoint
	// carrying interaction evidence may only land while the run's
	// status carries it (CarriesPausePointer) — otherwise a stale
	// in-memory copy is being replayed (the rewind shape: SaveRun
	// normalizes its own copy, then SaveCheckpoint re-persists the
	// caller's original). On a paused run the write-through is
	// legitimate (bookkeeping updates on a live pause keep the
	// pointer). Strip on a copy — the caller's object stays whole.
	if cp != nil && !r.Status.CarriesPausePointer() &&
		(cp.InteractionID != "" || len(cp.InteractionQuestions) > 0) {
		c := *cp
		c.InteractionID = ""
		c.InteractionQuestions = nil
		cp = &c
	}
	r.Checkpoint = cp
	r.UpdatedAt = time.Now().UTC()
	return s.writeRun(r)
}

// PauseRun atomically sets the checkpoint and updates the status to paused
// in a single write, preventing inconsistency if one of two separate
// operations were to fail.
func (s *FilesystemRunStore) PauseRun(ctx context.Context, id string, cp *Checkpoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, err := s.loadRunRaw(id)
	if err != nil {
		return err
	}
	r.Checkpoint = cp
	r.Status = RunStatusPausedWaitingHuman
	// A paused run carries no failure classification — same discipline
	// as the transition choke point, which this checkpoint-coupled
	// write bypasses. FinishedAt likewise: a paused run is not over,
	// and a stale terminal timestamp freezes the studio duration
	// ticker (mirrors the Mongo twin's $unset).
	r.FailureCode = ""
	r.FinishedAt = nil
	r.UpdatedAt = time.Now().UTC()
	return s.writeRun(r)
}

// failRunCheckpointed is the shared body of FailRunResumable and
// FailRunTerminal: the atomic cancelled-wins guard, the checkpoint, and
// the ordinary transition tail (which owns the failure-code
// discipline). An operator cancel is terminal and outranks a failure
// racing in behind it — the two race whenever an interruption and a
// cancel arrive together, and the failure would win simply by writing
// last, auto-resuming a run somebody deliberately stopped.
func (s *FilesystemRunStore) failRunCheckpointed(id string, status RunStatus, cp *Checkpoint, runErr string, code FailureCode) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, err := s.loadRunRaw(id)
	if err != nil {
		return err
	}
	if r.Status == RunStatusCancelled {
		return nil
	}
	r.Checkpoint = cp
	return s.applyStatusTransition(r, status, runErr, code)
}

// FailRunResumable atomically sets the checkpoint, error message, and status
// to failed_resumable in a single write, enabling resume from the last
// successfully completed node.
func (s *FilesystemRunStore) FailRunResumable(ctx context.Context, id string, cp *Checkpoint, runErr string, code FailureCode) error {
	return s.failRunCheckpointed(id, RunStatusFailedResumable, cp, runErr, code)
}

// FailRunTerminal atomically sets the checkpoint, error message, and status
// to failed in a single write. Unlike FailRunResumable the run is terminal —
// no auto-resume — but the checkpoint is preserved so the operator can still
// rewind it explicitly (a run that reached the DSL fail node has a coherent
// on-disk state worth recovering from).
func (s *FilesystemRunStore) FailRunTerminal(ctx context.Context, id string, cp *Checkpoint, runErr string, code FailureCode) error {
	return s.failRunCheckpointed(id, RunStatusFailed, cp, runErr, code)
}

// AddWatchedIssues merges issueIDs into the run's WatchedIssueIDs set
// (dedup, insertion order preserved) and returns the resulting set.
func (s *FilesystemRunStore) AddWatchedIssues(_ context.Context, runID string, issueIDs []string) ([]string, error) {
	return s.mutateWatched(runID, func(cur []string) []string { return mergeWatchedIssues(cur, issueIDs) })
}

// RemoveWatchedIssues drops issueIDs from the run's WatchedIssueIDs set
// and returns the resulting set.
func (s *FilesystemRunStore) RemoveWatchedIssues(_ context.Context, runID string, issueIDs []string) ([]string, error) {
	return s.mutateWatched(runID, func(cur []string) []string { return removeWatchedIssues(cur, issueIDs) })
}

// mutateWatched applies apply to the run's WatchedIssueIDs under mu —
// parallel branches' onNodeFinished hooks and the watch API can call
// this concurrently.
func (s *FilesystemRunStore) mutateWatched(runID string, apply func([]string) []string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, err := s.loadRunRaw(runID)
	if err != nil {
		return nil, err
	}
	r.WatchedIssueIDs = apply(r.WatchedIssueIDs)
	r.UpdatedAt = time.Now().UTC()
	if err := s.writeRun(r); err != nil {
		return nil, err
	}
	return r.WatchedIssueIDs, nil
}

// SetSubbotChild records childRunID under key in the parent run's
// SubbotChildren map (atomic RMW under mu — concurrent fan-out branches
// write distinct keys). No-op when key is empty.
func (s *FilesystemRunStore) SetSubbotChild(_ context.Context, parentRunID, key, childRunID string) error {
	if key == "" {
		return nil
	}
	return s.mutateSubbotChildren(parentRunID, func(m map[string]string) map[string]string {
		if m == nil {
			m = make(map[string]string, 1)
		}
		m[key] = childRunID
		return m
	})
}

// ClearSubbotChild removes key from the parent run's SubbotChildren map.
// No-op when key is empty or already absent.
func (s *FilesystemRunStore) ClearSubbotChild(_ context.Context, parentRunID, key string) error {
	if key == "" {
		return nil
	}
	return s.mutateSubbotChildren(parentRunID, func(m map[string]string) map[string]string {
		delete(m, key)
		if len(m) == 0 {
			return nil
		}
		return m
	})
}

// mutateSubbotChildren applies apply to the run's SubbotChildren under mu.
func (s *FilesystemRunStore) mutateSubbotChildren(runID string, apply func(map[string]string) map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, err := s.loadRunRaw(runID)
	if err != nil {
		return err
	}
	r.SubbotChildren = apply(r.SubbotChildren)
	r.UpdatedAt = time.Now().UTC()
	return s.writeRun(r)
}

// ListRuns returns the IDs of all persisted runs.
// ListRuns returns the ids of the runs in the store: directories that
// carry a run.json, which is the authoritative identity of a run (see
// CreateRun). Everything listed here loads.
//
// That last property is the point. LockRun mkdirs the run directory to
// place its .lock, so an id that is locked and then never created — an
// abandoned launch, a crash between the lock and the first write —
// leaves a directory carrying only that lock. Listed as a run it is a
// permanent phantom: every LoadRun on it fails, and a consumer that
// reads the first id it is handed waits on a run that will never load.
//
// A caller whose job is precisely to find the leftovers — the retention
// sweep — wants the opposite and asks ListRunDirs.
func (s *FilesystemRunStore) ListRuns(ctx context.Context) ([]string, error) {
	return s.listRunEntries(ctx, true)
}

// ListRunDirs returns every run directory in the store, including those
// with no readable run.json. It is the janitor's view: a partial delete,
// a crash before the first write, or an abandoned lock leaves a
// directory that a retention sweep must be able to SEE (and report)
// rather than silently step over. Tombstoned runs stay excluded — those
// are deliberate deletions, not leftovers.
func (s *FilesystemRunStore) ListRunDirs(ctx context.Context) ([]string, error) {
	return s.listRunEntries(ctx, false)
}

func (s *FilesystemRunStore) listRunEntries(_ context.Context, requireDoc bool) ([]string, error) {
	runsDir := filepath.Join(s.root, "runs")
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		return nil, fmt.Errorf("store: list runs: %w", err)
	}
	var ids []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// Tombstoned runs (deletion marker, no data) are not listed —
		// they'd surface as phantom rows failing every LoadRun.
		if s.runDeleted(e.Name()) {
			continue
		}
		if requireDoc {
			if _, err := os.Stat(s.runJSONPath(e.Name())); err != nil {
				continue
			}
		}
		ids = append(ids, e.Name())
	}
	sort.Strings(ids)
	return ids, nil
}

// ListRunsBySourceIssue returns the ids of runs whose Source.IssueID
// equals issueID (the card←run reverse edge), sorted by created_at
// ascending. At local scale we scan ListRuns + LoadRun each and filter
// — mirroring how the other fs-side filters work (runview.List) — since
// the filesystem store has no secondary index. Runs that fail to load
// are skipped rather than failing the whole query.
func (s *FilesystemRunStore) ListRunsBySourceIssue(ctx context.Context, issueID string) ([]string, error) {
	if issueID == "" {
		return []string{}, nil
	}
	return s.filterRunsSorted(ctx, func(r *Run) bool {
		return r.Source != nil && r.Source.IssueID == issueID
	})
}

// ListRunsBySchedule returns the ids of runs whose Source.ScheduleID
// equals scheduleID (the schedule←run reverse edge used by the
// pkg/schedgate overlap gate), sorted by created_at ascending. Same
// scan-and-filter strategy as ListRunsBySourceIssue.
func (s *FilesystemRunStore) ListRunsBySchedule(ctx context.Context, scheduleID string) ([]string, error) {
	if scheduleID == "" {
		return []string{}, nil
	}
	return s.filterRunsSorted(ctx, func(r *Run) bool {
		return r.Source != nil && r.Source.ScheduleID == scheduleID
	})
}

// ListChildRuns returns the ids of runs whose ParentRunID equals
// parentRunID (a run's shard/child subtree), sorted by created_at
// ascending. Same scan-and-filter strategy as ListRunsBySourceIssue.
func (s *FilesystemRunStore) ListChildRuns(ctx context.Context, parentRunID string) ([]string, error) {
	if parentRunID == "" {
		return []string{}, nil
	}
	return s.filterRunsSorted(ctx, func(r *Run) bool {
		return r.ParentRunID == parentRunID
	})
}

// filterRunsSorted scans every run, keeps those matching pred, and
// returns their ids sorted by CreatedAt ascending. Shared by the
// fs-side reverse-tree queries.
func (s *FilesystemRunStore) filterRunsSorted(ctx context.Context, pred func(*Run) bool) ([]string, error) {
	ids, err := s.ListRuns(ctx)
	if err != nil {
		return nil, err
	}
	type match struct {
		id string
		at time.Time
	}
	var matches []match
	for _, id := range ids {
		r, err := s.LoadRun(ctx, id)
		if err != nil {
			if s.logger != nil {
				s.logger.Warn("store: skip run %s during reverse-query scan: %v", id, err)
			}
			continue
		}
		if pred(r) {
			matches = append(matches, match{id: r.ID, at: r.CreatedAt})
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		return matches[i].at.Before(matches[j].at)
	})
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m.id)
	}
	return out, nil
}

func (s *FilesystemRunStore) writeRunNew(r *Run) error {
	// Defence in depth for CreateRun's exclusive create path: sanitise here as
	// well as at the public entry point so future internal callers cannot path
	// join a tampered Run.ID outside the store root.
	if err := sanitizePathComponent("run ID", r.ID); err != nil {
		return err
	}
	dir := s.runDir(r.ID)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("store: mkdir run: %w", err)
	}
	// Tighten existing directories (MkdirAll is a no-op on them). A stale or
	// pre-created run directory must not remain world-readable.
	if err := os.Chmod(dir, dirPerm); err != nil {
		return fmt.Errorf("store: chmod run: %w", err)
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("store: marshal run: %w", err)
	}
	if err := WriteFileAtomicNew(s.runJSONPath(r.ID), data, filePerm); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("store: run %s already exists: %w", r.ID, fs.ErrExist)
		}
		return err
	}
	return nil
}

func (s *FilesystemRunStore) writeRun(r *Run) error {
	// Defence in depth: every public entry point that mutates a run
	// (SaveRun, UpdateRunStatus, SaveCheckpoint, PauseRun,
	// FailRunResumable) flows through here. Sanitise once, here, so
	// e.g. a Run loaded with a tampered ID can't be re-serialised to a
	// path outside the store root.
	if err := sanitizePathComponent("run ID", r.ID); err != nil {
		return err
	}
	dir := s.runDir(r.ID)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("store: mkdir run: %w", err)
	}
	if err := os.Chmod(dir, dirPerm); err != nil {
		return fmt.Errorf("store: chmod run: %w", err)
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("store: marshal run: %w", err)
	}
	// Write-ahead ordering barrier: if an earlier AppendEvent fsync
	// failed, re-sync events.jsonl BEFORE the checkpoint write. run.json
	// is written atomically with fsync, so without the barrier a power
	// loss could recover a checkpoint that references events which never
	// reached disk. Failing here is consistent with writeFileAtomic's
	// own hard-fail on fsync: if the disk can't persist the log, it
	// can't persist the checkpoint either.
	if s.eventsUnsynced[r.ID] {
		if err := s.syncEventsLocked(r.ID); err != nil {
			return fmt.Errorf("store: write run %s blocked — events.jsonl re-sync after an earlier fsync failure: %w", r.ID, err)
		}
	}
	// Atomic write: run.json is the authoritative resume checkpoint
	// (per CLAUDE.md). A torn write would lose all prior checkpoint state.
	return writeFileAtomic(s.runJSONPath(r.ID), data, filePerm)
}
