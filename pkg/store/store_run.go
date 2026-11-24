package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/SocialGouv/iterion/pkg/scripthost"
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
func (s *FilesystemRunStore) CreateRun(ctx context.Context, id, workflowName string, inputs map[string]any) (*Run, error) {
	return s.CreateRunWithValueFormat(ctx, RunCreateSpec{ID: id, WorkflowName: workflowName, Inputs: inputs, Format: LegacyValueFormat})
}

var _ ParentedRunCreator = (*FilesystemRunStore)(nil)

// CreateChildRun persists a new run with status "running", stamping
// ParentRunID in the same exclusive-create write as the run document.
// It exists so spawnRun's precreate never leaves a running child doc
// behind a failed second SaveRun (see store.ParentedRunCreator).
//
// The parent is READ, never assumed: the child inherits its workspace
// provenance (ChildTrustFromParent), and a parent this store cannot read is
// refused, typed (ErrChildParentUnreadable) — an unknown provenance must not
// resolve to the trusted zero value.
func (s *FilesystemRunStore) CreateChildRun(ctx context.Context, id, workflowName, parentRunID string, inputs map[string]any) (*Run, error) {
	if parentRunID == "" {
		return nil, fmt.Errorf("%w: child run %s names an empty parent", ErrChildParentUnreadable, id)
	}
	return s.CreateRunWithValueFormat(ctx, RunCreateSpec{ID: id, WorkflowName: workflowName, ParentRunID: parentRunID, Inputs: inputs, Format: LegacyValueFormat})
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
func (s *FilesystemRunStore) CreateQueuedRun(ctx context.Context, id, workflowName, filePath, botID string, inputs map[string]any) (*Run, error) {
	return s.CreateRunWithValueFormat(ctx, RunCreateSpec{ID: id, WorkflowName: workflowName, FilePath: filePath, BotID: botID, Inputs: inputs, Status: RunStatusQueued, Format: LegacyValueFormat})
}

// SaveRun persists the run metadata to disk. Protected by mu so it
// cannot race against UpdateRunStatus / SaveCheckpoint / WriteArtifact
// — two runners reconciling the same orphan via RecoverFinalize, or a
// finalize path concurrent with an engine status update, would
// otherwise read-modify-write through each other and lose fields.
func (s *FilesystemRunStore) SaveRun(ctx context.Context, r *Run) error {
	if _, err := CheckRunFormatReplacement(ctx, r, r); err != nil {
		return err
	}
	if err := CheckRunDataReplacement(ctx, r, r); err != nil {
		return err
	}
	if err := GuardProjectedWrite(r); err != nil {
		return err
	}
	if r.Status != RunStatusRunning {
		r.AwaitAnswersWaits = nil
	}
	if err := s.guardNotDeleted(r.ID); err != nil {
		return err
	}
	// Best-effort guard: a copy whose STATUS is already non-failure
	// must not resurrect its failure code through this full-document
	// write. A copy stale on the status itself still rewrites
	// status+code together only if its loaded version still matches.
	// A concurrent write is refused at writeRun, before replacing the file.
	if !r.Status.CarriesFailureCode() {
		r.FailureCode = ""
		r.EndReason = ""
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
	writeLock, err := s.acquireRunWriteLock(r.ID)
	if err != nil {
		return err
	}
	defer func() { _ = writeLock.Unlock() }()
	// Outcome bookkeeping does not belong to full-document savers: a
	// caller replaying a stale in-memory Run must not rewind the
	// episode counter or resurrect metadata a status transition wrote
	// meanwhile. Same status ⇒ keep the persisted values; a status
	// change through SaveRun IS a transition ⇒ stamp it (untyped).
	if persisted, err := s.loadRunRawContext(ctx, r.ID); err == nil {
		// Work on a copy: the caller's struct must not observe the
		// bookkeeping (the Mongo implementation doesn't mutate either).
		rr := *r
		rr.SpentToolApprovals = persisted.SpentToolApprovals
		rr.UnresolvedToolApproval = persisted.UnresolvedToolApproval
		rr.ToolApprovalClaim = nil
		if rr.Status == RunStatusRunning && persisted.Status == RunStatusRunning {
			rr.ToolApprovalClaim = persisted.ToolApprovalClaim
		}
		// The persisted handoff wins over ordinary saves. In particular, the
		// cancelled claim at the start of a rewind invalidates the handoff.
		rr.ResumePauseHandoff = nil
		if rr.Status.CarriesResumePauseHandoff() {
			rr.ResumePauseHandoff = persisted.ResumePauseHandoff
		}
		rr.PreEngineRefusal = preEngineRefusalForSave(&rr, persisted)
		if rr.PreEngineRefusal != nil {
			rr.Error = rr.PreEngineRefusal.Message
		}
		// The merge claim is owned by ClaimMerge/UpdateRunMergeIf. A
		// caller whose copy predates a live claim (rename, rewind
		// bookkeeping) must not disavow it through this full-document
		// write: clobbering merge_status+merge_claimed_at lets the
		// next claimant through while the first is mid-merge — the
		// double-squash the claim exists to prevent.
		if persisted.MergeStatus == MergeStatusMerging && rr.MergeStatus != MergeStatusMerging {
			rr.MergeStatus = persisted.MergeStatus
			rr.MergeClaimedAt = persisted.MergeClaimedAt
		}
		// The launch-frozen contract is immutable: the persisted value
		// wins over whatever the saver carries, and the first-write
		// window only stays open while the run has not produced yet —
		// adding a contract to already-terminal work would decide
		// retroactively.
		if persisted.RoutingPolicy != nil {
			rr.RoutingPolicy = persisted.RoutingPolicy
		} else if !persisted.Status.CountsAgainstLaunchLimit() {
			rr.RoutingPolicy = nil
		}
		// Trust and its pin are write-once: a run's answer to "who wrote this
		// code" never legitimately changes, and every capability the run is
		// denied is read back from it. A saver that carries the zero value —
		// a binary too old to know the field, a stale full-document save —
		// must not clear it. No first-write window, unlike the contract
		// above: a marker that can be cleared is one an attacker only has to
		// race.
		if persisted.Trust != "" {
			rr.Trust = persisted.Trust
		}
		if persisted.RepoSHAExpected != "" {
			rr.RepoSHAExpected = persisted.RepoSHAExpected
		}
		// Same write-once contract for the fork lane's metering subject:
		// a saver that carries the zero value must not silently un-meter
		// a run whose every later re-entry (resume, retry, the runner's
		// second charge) reads it back from this document.
		if persisted.ForkAuthorSubject != "" {
			rr.ForkAuthorSubject = persisted.ForkAuthorSubject
		}
		if persisted.Status == rr.Status {
			rr.OutcomeSeq = persisted.OutcomeSeq
			rr.ContinuationState = persisted.ContinuationState
			// The typed cause too: transitions own it, and a stale
			// same-status copy clearing it would erase what a park
			// wrote meanwhile.
			rr.FailureCode = persisted.FailureCode
		} else {
			rr.OutcomeSeq = persisted.OutcomeSeq
			if rr.Status.IsFinalSuccess() || rr.Status.IsFinalFailure() || rr.Status.IsTerminalResumable() {
				rr.OutcomeSeq++
			}
			rr.ContinuationState = ""
		}
		if err := s.writeRunLockedContext(ctx, &rr, false, nil); err != nil {
			return err
		}
		r.CASVersion = rr.CASVersion
		r.BirthID = rr.BirthID
		return nil
	}
	// Create branch (no persisted document): mirror the Mongo upsert —
	// the store owns the outcome bookkeeping even on first write, so a
	// caller cannot seed a fabricated episode counter or continuation
	// (adversarial gate F7: the FS branch used to trust the caller's
	// bookkeeping verbatim). A run BORN terminal has episode 0: a
	// creation is not a transition, and a reactor must not treat an
	// imported/fixture document as a fresh outcome.
	rr := *r
	rr.SpentToolApprovals = nil
	rr.UnresolvedToolApproval = ""
	rr.ToolApprovalClaim = nil
	rr.ResumePauseHandoff = nil
	rr.PreEngineRefusal = nil
	rr.OutcomeSeq = 0
	rr.ContinuationState = ""
	if err := s.writeRunLockedContext(ctx, &rr, false, nil); err != nil {
		return err
	}
	r.CASVersion = rr.CASVersion
	r.BirthID = rr.BirthID
	return nil
}

// PatchRunBudgetOverrides atomically replaces only the raw replay source.
func (s *FilesystemRunStore) PatchRunBudgetOverrides(ctx context.Context, runID string, overrides *RunBudgetOverrides) error {
	if err := s.guardNotDeleted(runID); err != nil {
		return err
	}
	r, unlock, err := s.lockExistingRunMetadata(ctx, runID)
	if err != nil {
		return err
	}
	defer unlock()
	if overrides == nil {
		r.BudgetOverrides = nil
	} else {
		copy := *overrides
		r.BudgetOverrides = &copy
	}
	r.UpdatedAt = time.Now().UTC()
	return s.writeRunLockedContext(ctx, r, false, nil)
}

// loadRunRawContext is the pure-read variant of LoadRun: it parses run.json
// and returns the Run without firing the name backfill or
// finished_at heal. Used by every method that holds s.mu around its
// own read-modify-write — the public LoadRun's healing side-effects
// would otherwise sneak a second writeRun into a critical section
// the caller didn't account for (its own follow-up writeRun would
// then race the persisted state against its own in-memory copy).
func (s *FilesystemRunStore) loadRunRawContext(ctx context.Context, id string) (*Run, error) {
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
	// A document that does not decode is reported as ErrRunMetadataUndecodable
	// (final): reading it again answers the same.
	format, err := RunValueFormatFromJSON(context.Background(), data)
	if err != nil {
		return nil, runJSONUndecodable(id, data, err)
	}
	header, err := RunDataHeaderFromJSON(ctx, data)
	if err != nil {
		return nil, runJSONUndecodable(id, data, err)
	}
	if err := RequireRunDataStorage(header.Protocol); err != nil {
		return nil, err
	}
	if header.RunID != id {
		return nil, ErrRunDataIdentityChanged
	}
	// Legacy format-only callers also load parent metadata while preparing a
	// child. Only a retained data binding asserts snapshot identity here.
	if err := CheckRunDataBinding(ctx, header); err != nil {
		return nil, err
	}
	if err := checkRunJSONObservation(ctx, data); err != nil {
		return nil, err
	}

	if err := DecodeValueRecordJSON(ctx, format, ExactRun, data, &r); err != nil {
		return nil, runJSONUndecodable(id, data, fmt.Errorf("store: decode run %s: %w", id, err))
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
	// A failure code, and the end reason beside it, may only persist on a
	// status that carries an outcome. The transition machinery clears them
	// and SaveRun normalizes, so the remaining sources are historical rows
	// written before those guards and hand-edited run.json — heal on read.
	if !r.Status.CarriesFailureCode() {
		if r.FailureCode != "" {
			r.FailureCode = ""
			changed = true
		}
		if r.EndReason != "" {
			r.EndReason = ""
			changed = true
		}
	}
	return changed
}

// LoadRun reads run.json for the given run ID.
//
// The run ID is sanitised before path-joining so a hostile or
// network-sourced ID cannot escape the store root. The write side
// (CreateRun/WriteArtifact/WriteInteraction) already sanitises its inputs;
// the read paths must do the same so the defence is symmetric.
//
// A legacy or in-flight shape is healed IN MEMORY only (healRun): a
// deterministic friendly Name for a run persisted without one, a
// FinishedAt cleared on a running run, a failure code dropped off a
// non-failure status. The read never writes the healed copy back. A
// reader is not the run's owner: the engine executing the run holds the
// per-run lock and writes run.json from a different store instance —
// often a different process (studio, `iterion inspect`, the dispatcher's
// poll) — and no lock covers the read-modify-write a persisting heal
// would perform against it. Every freshly created run passes through
// exactly that window (CreateRun writes no Name; the engine's first
// SaveRun stamps Name, FilePath, WorkflowHash, ParentRunID…), so a
// concurrent heal write could land on top of the engine's stamp and
// erase it for good: the engine's later writes are load-patch-write on
// the disk copy, and the run would read as a nameless top-level run with
// no source path for the rest of its life. The next legitimate write
// (any status transition or checkpoint) normalises the on-disk copy.
//
// Callers that already hold s.mu should use loadRunRaw
// from the heal path interleaving with their own write.
func (s *FilesystemRunStore) LoadRun(ctx context.Context, id string) (*Run, error) {
	r, err := s.loadRunRawContext(ctx, id)
	if err != nil {
		return nil, err
	}
	healRun(r)
	return r, nil
}

var _ RunListingStore = (*FilesystemRunStore)(nil)

// LoadRunForListing is LoadRun without the recorded workflow source
// (store.RunListingStore). The decode still reads the whole file — this
// backend has no projection — but the text is dropped before the record is
// returned, so a listing holding hundreds of them holds none of it.
func (s *FilesystemRunStore) LoadRunForListing(ctx context.Context, id string) (*Run, error) {
	r, err := s.LoadRun(ctx, id)
	if err != nil {
		return nil, err
	}
	return stripRecordedSource(r), nil
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
	r, unlock, err := s.lockExistingRunMetadata(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	return s.applyStatusTransition(ctx, r, status, runErr, code)
}

// PatchRunSteering persists the live-steering state on run.json.
// Partial: a nil loopOverrides / nil budgetRaises leaves the stored
// field untouched, so the two commands patch independently.
func (s *FilesystemRunStore) PatchRunSteering(ctx context.Context, id string, loopOverrides map[string]int, budgetRaises *RunBudgetRaises) error {
	r, unlock, err := s.lockExistingRunMetadata(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	if loopOverrides != nil {
		r.LoopOverrides = loopOverrides
	}
	if budgetRaises != nil {
		r.BudgetRaises = budgetRaises
	}
	r.UpdatedAt = time.Now().UTC()
	return s.writeRunLockedContext(ctx, r, false, nil)
}

// PatchRunPermissionGrants persists the permission-gate allow rules
// earned by the operator. Replaces the stored slice wholesale (the
// caller owns the accumulated set); a nil slice is a no-op patch.
func (s *FilesystemRunStore) PatchRunPermissionGrants(ctx context.Context, id string, grants map[string][]string) error {
	if grants == nil {
		return nil
	}
	r, unlock, err := s.lockExistingRunMetadata(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	r.PermissionGrants = grants
	r.UpdatedAt = time.Now().UTC()
	return s.writeRunLockedContext(ctx, r, false, nil)
}

// RecordNodeServed persists the last (backend, model) that served
// nodeID. Last write wins per key; empty nodeID is a no-op.
func (s *FilesystemRunStore) RecordNodeServed(ctx context.Context, id, nodeID string, served NodeServed) error {
	if nodeID == "" {
		return nil
	}
	r, unlock, err := s.lockExistingRunMetadata(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	if r.NodesServed == nil {
		r.NodesServed = make(map[string]NodeServed, 1)
	}
	r.NodesServed[nodeID] = served
	r.UpdatedAt = time.Now().UTC()
	return s.writeRunLockedContext(ctx, r, false, nil)
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
		return false, fmt.Errorf("store: update status if %s: empty expectedFrom", id)
	}
	return s.UpdateRunOutcome(ctx, id, status, runErr, RunOutcomeMeta{Code: code}, expectedFrom)
}

var _ CodemodeQuotaResumeClaimer = (*FilesystemRunStore)(nil)

// ClaimResumeWithCodemodeQuota implements CodemodeQuotaResumeClaimer.
func (s *FilesystemRunStore) ClaimResumeWithCodemodeQuota(ctx context.Context, id string, claim CodemodeQuotaResumeClaim, quota CodemodeScratchLimits) (*scripthost.Limits, error) {
	if err := claim.Validate(quota); err != nil {
		return nil, fmt.Errorf("store: claim resume with codemode quota %s: %w", id, err)
	}
	execution, fenced := ResumeExecutionClaimFromContext(ctx)
	if fenced {
		if err := execution.Validate(); err != nil {
			return nil, fmt.Errorf("store: claim resume with codemode quota %s: %w", id, err)
		}
	}
	r, unlock, err := s.lockExistingRunMetadata(ctx, id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if r.Status != claim.ExpectedStatus {
		return nil, nil
	}
	if (fenced || claim.ExpectedStatus != RunStatusQueued) && r.CASVersion != claim.ExpectedVersion {
		return nil, nil
	}
	if fenced && r.ExecutionEpoch != execution.ExpectedEpoch {
		return nil, nil
	}
	if !MatchesExactQueuedAt(r.QueuedAt, claim.QueuedAt) {
		return nil, nil
	}
	if quota == (CodemodeScratchLimits{}) {
		if !MatchesExactQueuedAt(r.CodemodeQuotaClaimedAt, claim.QueuedAt) || r.CodemodeBoundsOverride == nil {
			return nil, nil
		}
	} else {
		if !claim.QueuedAt.IsZero() {
			r.CodemodeQuotaClaimedAt = &claim.QueuedAt
		}
		if r.CodemodeBoundsOverride == nil {
			r.CodemodeBoundsOverride = &scripthost.Limits{}
		}
		if quota.Entries != 0 {
			r.CodemodeBoundsOverride.StoreEntries = quota.Entries
		}
		if quota.Bytes != 0 {
			r.CodemodeBoundsOverride.StoreBytes = quota.Bytes
		}
	}
	if claim.PauseHandoff != nil {
		r.ResumePauseHandoff = claim.PauseHandoff
	}
	transitionRunStatus(r, RunStatusRunning, "", RunOutcomeMeta{})
	var expectedEpoch *string
	if fenced {
		expectedEpoch = &execution.ExpectedEpoch
	}
	if err := s.writeRunLockedWithNextEpoch(ctx, r, false, expectedEpoch, execution.NextEpoch); err != nil {
		if errors.Is(err, ErrRunConflict) {
			return nil, nil
		}
		return nil, err
	}
	return r.CodemodeBoundsOverride, nil
}

func newerQueuedAttempt(r *Run, publishedAt time.Time) bool {
	return !publishedAt.IsZero() && r.QueuedAt != nil && r.QueuedAt.After(publishedAt)
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
// claimants in one process serialize here; the expected birth is compared
// under the same lock.
func (s *FilesystemRunStore) ClaimMerge(ctx context.Context, id string, staleBefore time.Time, expect ExpectedBirth) (bool, MergeStatus, time.Time, error) {
	r, unlock, err := s.lockExistingRunMetadata(ctx, id)
	if err != nil {
		return false, "", time.Time{}, err
	}
	defer unlock()
	if err := expect.Verify(id, r.BirthID); err != nil {
		return false, "", time.Time{}, err
	}
	prior := r.MergeStatus
	if !mergeClaimable(prior, r.MergeClaimedAt, staleBefore) {
		return false, prior, time.Time{}, nil
	}
	// Millisecond precision: the token must survive a Mongo round-trip
	// identically on both backends, and BSON stores times in ms.
	now := time.Now().UTC().Truncate(time.Millisecond)
	// Strictly monotonic vs the stamp being stolen: a steal landing in
	// the SAME millisecond as the claim it replaces would mint an equal
	// token, and the loser's token-scoped exits would pass as the
	// winner's — the fencing collapses exactly when two claimants are
	// closest.
	if !now.After(r.MergeClaimedAt) {
		now = r.MergeClaimedAt.Add(time.Millisecond)
	}
	r.MergeStatus = MergeStatusMerging
	r.MergeClaimedAt = now
	r.UpdatedAt = now
	if err := s.writeRunLockedContext(ctx, r, false, nil); err != nil {
		return false, prior, time.Time{}, err
	}
	return true, prior, now, nil
}

// UpdateRunMergeIf is the compare-and-set exit from the merge state
// machine (see store.RunStore): the write only lands when the current
// MergeStatus is in expectedFrom and, when upd expects a birth, the run
// under the lock holds that birth.
func (s *FilesystemRunStore) UpdateRunMergeIf(ctx context.Context, id string, upd RunMergeUpdate, expectedFrom []MergeStatus) (bool, error) {
	r, unlock, err := s.lockExistingRunMetadata(ctx, id)
	if err != nil {
		return false, err
	}
	defer unlock()
	if err := upd.ExpectBirth.Verify(id, r.BirthID); err != nil {
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
	if err := s.writeRunLockedContext(ctx, r, false, nil); err != nil {
		return false, err
	}
	return true, nil
}

// UpdateRunOutcome is the typed status transition (see store.RunStore):
// UpdateRunStatusIf plus the outcome metadata persisted atomically.
func (s *FilesystemRunStore) UpdateRunOutcome(ctx context.Context, id string, status RunStatus, runErr string, meta RunOutcomeMeta, expectedFrom []RunStatus) (bool, error) {
	return s.updateRunOutcome(ctx, id, status, runErr, meta, expectedFrom, nil)
}
func (s *FilesystemRunStore) UpdateRunOutcomeIf(ctx context.Context, id string, expected RunOutcomeExpectation, status RunStatus, runErr string, meta RunOutcomeMeta, expectedFrom []RunStatus) (bool, error) {
	if err := expected.Validate(); err != nil {
		return false, err
	}
	return s.updateRunOutcome(ctx, id, status, runErr, meta, expectedFrom, &expected)
}
func (s *FilesystemRunStore) updateRunOutcome(ctx context.Context, id string, status RunStatus, runErr string, meta RunOutcomeMeta, expectedFrom []RunStatus, expected *RunOutcomeExpectation) (bool, error) {
	r, unlock, err := s.lockExistingRunMetadata(ctx, id)
	if err != nil {
		return false, err
	}
	defer unlock()
	if (expected != nil && !expected.Matches(r)) || !MatchesExactQueuedAt(r.QueuedAt, meta.ExactQueuedAt) {
		return false, nil
	}
	if len(expectedFrom) > 0 {
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
	}
	// A cancel cannot prove that an undecided command stopped. Keep its
	// failure and checkpoint atomically, including when the caller read running.
	if status == RunStatusCancelled && r.FailureCode == FailureAmbiguousEffect {
		return false, nil
	}
	if err := s.applyStatusTransitionOutcome(ctx, r, status, runErr, meta); err != nil {
		return false, err
	}
	return true, nil
}

// FailQueuedRunIfAttempt atomically fails only the queue attempt represented
// by publishedAt. A later resume refreshes QueuedAt before publishing, so an
// older delivery cannot clobber that new attempt during its queued→running
// hand-off window.
func (s *FilesystemRunStore) FailQueuedRunIfAttempt(ctx context.Context, id, runErr string, publishedAt time.Time, meta RunOutcomeMeta) (bool, error) {
	if publishedAt.IsZero() {
		return false, fmt.Errorf("store: fail queued attempt %s without published_at", id)
	}
	return s.transitionQueuedAttempt(ctx, id, RunStatusFailedResumable, runErr, publishedAt, meta)
}

var _ QueuedResumeReleaser = (*FilesystemRunStore)(nil)

// ReleaseQueuedRunIfAttempt puts a queued attempt nobody claimed back in the
// status its resume came from — see QueuedResumeReleaser.
func (s *FilesystemRunStore) ReleaseQueuedRunIfAttempt(ctx context.Context, id string, to RunStatus, runErr string, publishedAt time.Time, meta RunOutcomeMeta) (bool, error) {
	if publishedAt.IsZero() {
		return false, fmt.Errorf("store: release queued attempt %s without published_at", id)
	}
	if !to.CanOperatorResume() {
		return false, fmt.Errorf("store: release queued attempt %s to %q: not a status a resume comes from", id, to)
	}
	return s.transitionQueuedAttempt(ctx, id, to, runErr, publishedAt, meta)
}

var _ QueuedFlipper = (*FilesystemRunStore)(nil)

// FlipToQueued flips a run to queued for a resume, returning what it
// replaced — see QueuedFlipper.
func (s *FilesystemRunStore) FlipToQueued(ctx context.Context, id string, from RunStatus, at time.Time) (QueuedFlip, bool, error) {
	return s.flipToQueued(ctx, id, from, at, nil, false)
}

func (s *FilesystemRunStore) FlipRevisionToQueued(ctx context.Context, id string, from RunStatus, at time.Time, version int64) (QueuedFlip, bool, error) {
	if err := ctx.Err(); err != nil {
		return QueuedFlip{}, false, err
	}
	return s.flipToQueued(ctx, id, from, at, &version, false)
}

func (s *FilesystemRunStore) FlipAcknowledgedRevisionToQueued(ctx context.Context, id string, from RunStatus, at time.Time, version int64) (QueuedFlip, bool, error) {
	if err := ctx.Err(); err != nil {
		return QueuedFlip{}, false, err
	}
	return s.flipToQueued(ctx, id, from, at, &version, true)
}

func (s *FilesystemRunStore) flipToQueued(ctx context.Context, id string, from RunStatus, at time.Time, version *int64, acknowledged bool) (QueuedFlip, bool, error) {
	if at.IsZero() {
		return QueuedFlip{}, false, fmt.Errorf("store: flip %s to queued without an attempt marker", id)
	}
	at = QueuedFlipAt(at)
	r, unlock, err := s.lockExistingRunMetadata(ctx, id)
	if err != nil {
		return QueuedFlip{}, false, err
	}
	defer unlock()
	if r.Status != from || (r.UnresolvedToolApproval != "" && !acknowledged) || (version != nil && r.CASVersion != *version) {
		return QueuedFlip{}, false, nil
	}
	marker := NextQueuedAt(at, r.QueuedAt)
	flip := QueuedFlip{At: marker, Prior: QueuedFlipPrior{
		Status: r.Status, QueuedAt: cloneTimePtr(r.QueuedAt), Error: r.Error, FailureCode: r.FailureCode,
		EndReason: r.EndReason, OutcomeSeq: r.OutcomeSeq, ContinuationState: r.ContinuationState, FinishedAt: cloneTimePtr(r.FinishedAt),
		WorkflowSource: r.WorkflowSource, WorkflowSources: slices.Clone(r.WorkflowSources), WorkflowHash: r.WorkflowHash,
	}}
	transitionRunStatus(r, RunStatusQueued, "", RunOutcomeMeta{})
	r.QueuedAt = &marker
	r.CredAdmissionPending = true
	if err := s.writeRunLockedContext(ctx, r, false, nil); err != nil {
		return QueuedFlip{}, false, err
	}
	return flip, true, nil
}

// RevertQueuedFlip puts back what a resume's flip replaced, while the run is
// still queued for that flip — see QueuedFlipper.
func (s *FilesystemRunStore) RevertQueuedFlip(ctx context.Context, id string, flip QueuedFlip, runErr string) (bool, error) {
	if !flip.Prior.Status.CanOperatorResume() {
		return false, fmt.Errorf("store: revert queued run %s to %q: not a status a resume comes from", id, flip.Prior.Status)
	}
	if flip.At.IsZero() {
		return false, fmt.Errorf("store: revert queued run %s without its flip's marker", id)
	}
	r, unlock, err := s.lockExistingRunMetadata(ctx, id)
	if err != nil {
		return false, err
	}
	defer unlock()
	if !r.Status.IsQueued() || r.QueuedAt == nil || !r.QueuedAt.Equal(flip.At) {
		return false, nil
	}
	prior := flip.Prior
	r.Status = prior.Status
	if !prior.Status.CarriesResumePauseHandoff() {
		r.ResumePauseHandoff = nil
	}
	r.PreEngineRefusal = nil
	r.QueuedAt = cloneTimePtr(prior.QueuedAt)
	r.Error = prior.Error
	if runErr != "" {
		r.Error = runErr
	}
	r.FailureCode = prior.FailureCode
	r.EndReason = prior.EndReason
	r.OutcomeSeq = prior.OutcomeSeq
	r.ContinuationState = prior.ContinuationState
	r.FinishedAt = cloneTimePtr(prior.FinishedAt)
	r.WorkflowSource = prior.WorkflowSource
	r.WorkflowSources = slices.Clone(prior.WorkflowSources)
	r.WorkflowHash = prior.WorkflowHash
	r.AwaitAnswersWaits = nil
	r.CredAdmissionPending = false
	r.UpdatedAt = time.Now().UTC()
	if err := s.writeRunLockedContext(ctx, r, false, nil); err != nil {
		return false, err
	}
	return true, nil
}

func cloneTimePtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}

var _ QueuedAttemptClaimer = (*FilesystemRunStore)(nil)

// ClaimQueuedRunIfAttempt claims the queued attempt publishedAt names — see
// QueuedAttemptClaimer.
func (s *FilesystemRunStore) ClaimQueuedRunIfAttempt(ctx context.Context, id string, publishedAt time.Time) (bool, error) {
	if publishedAt.IsZero() {
		return false, fmt.Errorf("store: claim queued attempt %s without published_at", id)
	}
	return s.transitionQueuedAttempt(ctx, id, RunStatusRunning, "", publishedAt, RunOutcomeMeta{})
}

// transitionQueuedAttempt moves the queue attempt publishedAt names, and
// only that one, out of queued: a later resume refreshes QueuedAt before
// publishing, so an older delivery cannot touch that new attempt during its
// queued→running hand-off window.
func (s *FilesystemRunStore) transitionQueuedAttempt(ctx context.Context, id string, to RunStatus, runErr string, publishedAt time.Time, meta RunOutcomeMeta) (bool, error) {
	r, unlock, err := s.lockExistingRunMetadata(ctx, id)
	if err != nil {
		return false, err
	}
	defer unlock()
	if r.Status != RunStatusQueued || !MatchesExactQueuedAt(r.QueuedAt, meta.ExactQueuedAt) || (meta.ExactQueuedAt.IsZero() && newerQueuedAttempt(r, publishedAt)) {
		return false, nil
	}
	if err := s.applyStatusTransitionOutcome(ctx, r, to, runErr, meta); err != nil {
		return false, err
	}
	return true, nil
}

// applyStatusTransition is the shared tail of UpdateRunStatus and
// UpdateRunStatusIf: mutate r in-place (status, timestamps, terminal
// finished_at / resume FinishedAt clear, checkpoint clear when leaving
// paused state), then persist without reacquiring locks. Caller must hold s.mu and the stable run lock.
// The failure code follows the same discipline as Error: set on a
// failure status, cleared by every transition to a non-failure one —
// which is what makes a stale code after a resume impossible.
func (s *FilesystemRunStore) applyStatusTransition(ctx context.Context, r *Run, status RunStatus, runErr string, code FailureCode) error {
	return s.applyStatusTransitionOutcome(ctx, r, status, runErr, RunOutcomeMeta{Code: code})
}

// applyStatusTransitionOutcome adds the outcome bookkeeping to the
// shared transition tail:
//   - OutcomeSeq increments on a TRANSITION into a terminal status,
//     never on a same-status rewrite (a drain's markInterrupted or a
//     repeated flip must not invent an episode — adversarial gate F1);
//   - ContinuationState is a RUNNER-side statement: it is written when
//     the meta states one, cleared when the run genuinely changes
//     state without a statement (unknown, honest), and PRESERVED on a
//     same-status rewrite (an untyped rewrite of an already-parked run
//     must not erase a live retry_armed).
//
// The publisher's resume rollback is not a transition: it puts back what
// its own flip replaced (RevertQueuedFlip), the episode and continuation
// included.
func (s *FilesystemRunStore) applyStatusTransitionOutcome(ctx context.Context, r *Run, status RunStatus, runErr string, meta RunOutcomeMeta) error {
	if status == RunStatusCancelled && r.FailureCode == FailureAmbiguousEffect {
		return nil
	}
	transitionRunStatus(r, status, runErr, meta)
	return s.writeRunLockedContext(ctx, r, false, nil)
}

// transitionRunStatus applies applyStatusTransitionOutcome's discipline to r
// in memory; the caller persists it.
func transitionRunStatus(r *Run, status RunStatus, runErr string, meta RunOutcomeMeta) {
	refusalCauseChanged := r.PreEngineRefusal != nil && ((runErr != "" && runErr != r.PreEngineRefusal.Message) || (meta.Code != "" && meta.Code != r.PreEngineRefusal.Code))
	if refusalCauseChanged || r.Status != status || status.IsQueued() || status == RunStatusRunning || status.IsFinalSuccess() || status.IsFinalFailure() {
		r.PreEngineRefusal = nil
	}
	terminal := status.IsFinalSuccess() || status.IsFinalFailure() || status.IsTerminalResumable()
	transition := r.Status != status
	if transition || status != RunStatusRunning {
		r.ToolApprovalClaim = nil
		r.AwaitAnswersWaits = nil
	}
	if terminal && transition {
		r.OutcomeSeq++
	}
	if meta.Continuation != "" {
		r.ContinuationState = meta.Continuation
	} else if transition {
		r.ContinuationState = ""
	}
	// A final continuation and an armed retry contradict each other (see
	// the Mongo twin, statusTransitionSet): disarm at the tail every final
	// writer goes through, keeping the rest of the retry bookkeeping.
	if meta.Continuation == ContinuationFinal && r.RetryState != nil {
		r.RetryState.RetryAfter = nil
	}
	if !status.CarriesResumePauseHandoff() {
		r.ResumePauseHandoff = nil
	}
	r.Status = status
	r.UpdatedAt = time.Now().UTC()
	// A transition always states its own message (empty included); a
	// same-status rewrite that states nothing keeps the transition's —
	// the runner's continuation promote must not blank the engine's
	// failure text. Same rule for the typed cause below.
	if runErr != "" || transition {
		r.Error = runErr
	}
	switch {
	case status.CarriesFailureCode() && meta.Code != "":
		r.FailureCode = meta.Code
	case status.CarriesFailureCode():
		if transition {
			r.FailureCode = ""
		}
	default:
		r.FailureCode = ""
	}
	// The end reason follows the same discipline, on the same statuses.
	switch {
	case status.CarriesFailureCode() && meta.EndReason != "":
		r.EndReason = meta.EndReason
	case status.CarriesFailureCode():
		if transition {
			r.EndReason = ""
		}
	default:
		r.EndReason = ""
	}
	switch status {
	case RunStatusFinished, RunStatusFailed, RunStatusFailedResumable, RunStatusCancelled:
		t := r.UpdatedAt
		r.FinishedAt = &t
	case RunStatusQueued:
		// Every queue publication is a distinct attempt. Refresh the marker
		// before publishing so a stale delivery can be rejected by identity,
		// not merely by the shared `queued` status — and never to a marker
		// the attempt it replaces already carried (NextQueuedAt).
		m := NextQueuedAt(r.UpdatedAt, r.QueuedAt)
		r.QueuedAt = &m
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
			// A successful rewind requires a deliberate Resume while it is
			// parked. Reaching running proves that happened, so old cloud
			// deliveries must no longer be held back on later failures.
			r.ResumeRequiresExplicit = false
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
}

// SaveCheckpoint persists a checkpoint on a paused run.
// Protected by mu to prevent concurrent read-modify-write races.
func (s *FilesystemRunStore) SaveCheckpoint(ctx context.Context, id string, cp *Checkpoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	writeLock, err := s.acquireRunWriteLock(id)
	if err != nil {
		return err
	}
	defer func() { _ = writeLock.Unlock() }()
	if _, _, err := s.valueRecordFormatLocked(ctx, id, true); err != nil {
		return err
	}

	r, err := s.loadRunRawContext(ctx, id)
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
	return s.writeRunLockedContext(ctx, r, false, nil)
}

// PauseRun atomically sets the checkpoint and updates the status to paused
// in a single write, preventing inconsistency if one of two separate
// operations were to fail.
func (s *FilesystemRunStore) PauseRun(ctx context.Context, id string, cp *Checkpoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	writeLock, err := s.acquireRunWriteLock(id)
	if err != nil {
		return err
	}
	defer func() { _ = writeLock.Unlock() }()
	if _, _, err := s.valueRecordFormatLocked(ctx, id, true); err != nil {
		return err
	}

	r, err := s.loadRunRawContext(ctx, id)
	if err != nil {
		return err
	}
	r.Checkpoint = cp
	r.Status = RunStatusPausedWaitingHuman
	r.ToolApprovalClaim = nil
	r.ResumePauseHandoff = nil
	r.PreEngineRefusal = nil
	r.AwaitAnswersWaits = nil
	// A fresh exact pause re-arms the approval cycle: its own claim/spend
	// identity is the durable continuation of the cycle it replaces, so a
	// marker an earlier ambiguous dispatch left (the force-resume path)
	// must not block answering THIS pause.
	if cp != nil && len(cp.BackendPermissionInput) != 0 {
		r.UnresolvedToolApproval = ""
	}
	// A paused run has no platform continuation statement — same
	// discipline as FailureCode below.
	r.ContinuationState = ""
	// A paused run carries no failure classification — same discipline
	// as the transition choke point, which this checkpoint-coupled
	// write bypasses. FinishedAt likewise: a paused run is not over,
	// and a stale terminal timestamp freezes the studio duration
	// ticker (mirrors the Mongo twin's $unset).
	r.FailureCode = ""
	r.FinishedAt = nil
	r.UpdatedAt = time.Now().UTC()
	return s.writeRunLockedContext(ctx, r, false, nil)
}

// failRunCheckpointed is the shared body of FailRunResumable and
// FailRunTerminal: the atomic cancelled-wins guard, the checkpoint, and
// the ordinary transition tail (which owns the failure-code and
// outcome-bookkeeping discipline). An operator cancel is terminal and
// outranks a failure racing in behind it — the two race whenever an
// interruption and a cancel arrive together, and the failure would win
// simply by writing last, auto-resuming a run somebody deliberately
// stopped. An ambiguous effect is the exception: it cannot auto-resume, and
// a cancel is no evidence that the command stopped.
func (s *FilesystemRunStore) failRunCheckpointed(ctx context.Context, id string, status RunStatus, cp *Checkpoint, runErr string, code FailureCode) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	writeLock, err := s.acquireRunWriteLock(id)
	if err != nil {
		return err
	}
	defer func() { _ = writeLock.Unlock() }()
	if _, _, err := s.valueRecordFormatLocked(ctx, id, true); err != nil {
		return err
	}

	r, err := s.loadRunRawContext(ctx, id)
	if err != nil {
		return err
	}
	if r.Status == RunStatusCancelled && code != FailureAmbiguousEffect {
		return nil
	}
	r.Checkpoint = cp
	transitionRunStatus(r, status, runErr, RunOutcomeMeta{Code: code})
	return s.writeRunLockedContext(ctx, r, false, nil)
}

// FailRunResumable atomically sets the checkpoint, error message, and status
// to failed_resumable in a single write, enabling resume from the last
// successfully completed node.
func (s *FilesystemRunStore) FailRunResumable(ctx context.Context, id string, cp *Checkpoint, runErr string, code FailureCode) error {
	return s.failRunCheckpointed(ctx, id, RunStatusFailedResumable, cp, runErr, code)
}

// FailRunTerminal atomically sets the checkpoint, error message, and status
// to failed in a single write. Unlike FailRunResumable the run is terminal —
// no auto-resume — but the checkpoint is preserved so the operator can still
// rewind it explicitly (a run that reached the DSL fail node has a coherent
// on-disk state worth recovering from).
func (s *FilesystemRunStore) FailRunTerminal(ctx context.Context, id string, cp *Checkpoint, runErr string, code FailureCode) error {
	return s.failRunCheckpointed(ctx, id, RunStatusFailed, cp, runErr, code)
}

// AddWatchedIssues merges issueIDs into the run's WatchedIssueIDs set
// (dedup, insertion order preserved) and returns the resulting set.
func (s *FilesystemRunStore) AddWatchedIssues(ctx context.Context, runID string, issueIDs []string) ([]string, error) {
	return s.mutateWatched(ctx, runID, func(cur []string) []string { return mergeWatchedIssues(cur, issueIDs) })
}

// RemoveWatchedIssues drops issueIDs from the run's WatchedIssueIDs set
// and returns the resulting set.
func (s *FilesystemRunStore) RemoveWatchedIssues(ctx context.Context, runID string, issueIDs []string) ([]string, error) {
	return s.mutateWatched(ctx, runID, func(cur []string) []string { return removeWatchedIssues(cur, issueIDs) })
}

// mutateWatched applies apply to the run's WatchedIssueIDs under mu —
// parallel branches' onNodeFinished hooks and the watch API can call
// this concurrently.
func (s *FilesystemRunStore) mutateWatched(ctx context.Context, runID string, apply func([]string) []string) ([]string, error) {
	r, unlock, err := s.lockExistingRunMetadata(ctx, runID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	r.WatchedIssueIDs = apply(r.WatchedIssueIDs)
	r.UpdatedAt = time.Now().UTC()
	if err := s.writeRunLockedContext(ctx, r, false, nil); err != nil {
		return nil, err
	}
	return r.WatchedIssueIDs, nil
}

// SetSubbotChild records childRunID under key in the parent run's
// SubbotChildren map (atomic RMW under mu — concurrent fan-out branches
// write distinct keys). No-op when key is empty.
func (s *FilesystemRunStore) SetSubbotChild(ctx context.Context, parentRunID, key, childRunID string) error {
	if key == "" {
		return nil
	}
	return s.mutateSubbotChildren(ctx, parentRunID, func(m map[string]string) map[string]string {
		if m == nil {
			m = make(map[string]string, 1)
		}
		m[key] = childRunID
		return m
	})
}

// ClearSubbotChild removes key from the parent run's SubbotChildren map.
// No-op when key is empty or already absent.
func (s *FilesystemRunStore) ClearSubbotChild(ctx context.Context, parentRunID, key string) error {
	if key == "" {
		return nil
	}
	return s.mutateSubbotChildren(ctx, parentRunID, func(m map[string]string) map[string]string {
		delete(m, key)
		if len(m) == 0 {
			return nil
		}
		return m
	})
}

// mutateSubbotChildren applies apply to the run's SubbotChildren under mu.
func (s *FilesystemRunStore) mutateSubbotChildren(ctx context.Context, runID string, apply func(map[string]string) map[string]string) error {
	r, unlock, err := s.lockExistingRunMetadata(ctx, runID)
	if err != nil {
		return err
	}
	defer unlock()
	r.SubbotChildren = apply(r.SubbotChildren)
	r.UpdatedAt = time.Now().UTC()
	return s.writeRunLockedContext(ctx, r, false, nil)
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
// are deliberate deletions, not leftovers. Strict reverse queries also use
// this enumeration so a failed stat cannot hide an unreadable candidate.
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

// ListDeliverableWatchers returns the ids of runs carrying at least one
// watched card (WatchedIssueIDs non-empty) in a status a notification can
// be delivered to (WatchDeliverableStatuses), sorted by created_at
// ascending. Same scan-and-filter strategy as ListRunsBySourceIssue: the
// filesystem store serves one process over a local tree, where the scan
// is the index.
func (s *FilesystemRunStore) ListDeliverableWatchers(ctx context.Context) ([]string, error) {
	return s.filterRunsSorted(ctx, func(r *Run) bool {
		return len(r.WatchedIssueIDs) > 0 && r.Status.WatchDeliverable()
	})
}

// ListRunsForPR returns the ids of runs whose `pr_url` input equals prURL,
// sorted by created_at then ID. Without a local projection index, every
// scanned Run must be readable to decide the complete result: an unsupported
// or malformed format refuses even when that Run might not match this URL.
// Other reverse views retain their historical best-effort scan.
func (s *FilesystemRunStore) ListRunsForPR(ctx context.Context, prURL string) ([]string, error) {
	if prURL == "" {
		return []string{}, nil
	}
	return s.filterRunsSortedChecked(ctx, func(r *Run) bool {
		got, _ := r.Inputs["pr_url"].(string)
		return got == prURL
	}, true)
}

// SetRunCredStamp writes a credential resolution's stamp on the run
// document (see RunStore). Load-modify-save, like the other fs-side
// patches: the filesystem store has no partial update.
func (s *FilesystemRunStore) SetRunCredStamp(ctx context.Context, runID string, stamp RunCredStamp) error {
	r, unlock, err := s.lockExistingRunMetadata(ctx, runID)
	if err != nil {
		return err
	}
	defer unlock()
	stamp.Apply(r)
	return s.writeRunLockedContext(ctx, r, false, nil)
}

// SetRunRecordedSource stamps the executed source and the hash of that same
// compile in one write (see RunStore). Load-modify-save under the store
// mutex, like the budget patches, so a status transition racing it is never
// reverted.
func (s *FilesystemRunStore) SetRunRecordedSource(ctx context.Context, runID, src string, files []WorkflowSourceFile, hash string) error {
	r, unlock, err := s.lockExistingRunMetadata(ctx, runID)
	if err != nil {
		return err
	}
	defer unlock()
	// All three, empty included: the caller states a whole triple, and a hash
	// left standing beside a replaced source is the mismatched pair this write
	// exists to prevent.
	r.WorkflowSource = src
	r.WorkflowSources = files
	r.WorkflowHash = hash
	r.UpdatedAt = time.Now().UTC()
	return s.writeRunLockedContext(ctx, r, false, nil)
}

// SetRunLLMIdle toggles the model-idle marker (see RunStore). Load-modify-
// save under the store mutex, like the budget patches, so a status
// transition racing this write is never reverted.
func (s *FilesystemRunStore) SetRunLLMIdle(ctx context.Context, runID string, idleSince *time.Time) error {
	r, unlock, err := s.lockExistingRunMetadata(ctx, runID)
	if err != nil {
		return err
	}
	defer unlock()
	r.LLMIdleSince = idleSince
	r.UpdatedAt = time.Now().UTC()
	return s.writeRunLockedContext(ctx, r, false, nil)
}

// SetWatcherCursor updates one durable supervisor cursor under the store
// mutex, without replacing status/checkpoint fields owned by the engine.
func (s *FilesystemRunStore) SetWatcherCursor(ctx context.Context, runID, watcherID string, cursor WatcherCursor) error {
	if watcherID == "" {
		return fmt.Errorf("store: watcher cursor id is empty")
	}
	r, unlock, err := s.lockExistingRunMetadata(ctx, runID)
	if err != nil {
		return err
	}
	defer unlock()
	if r.WatcherCursors == nil {
		r.WatcherCursors = make(map[string]WatcherCursor)
	}
	r.WatcherCursors[watcherID] = cursor
	r.UpdatedAt = time.Now().UTC()
	return s.writeRunLockedContext(ctx, r, false, nil)
}

// SetRunOutputCorrection updates one node's correction episode under the
// filesystem store mutex. It is intentionally granular: a correction call
// must not replace a run document that an operator or runner concurrently
// transitioned.
func (s *FilesystemRunStore) SetRunOutputCorrection(ctx context.Context, runID, ledgerKey string, episode OutputCorrectionEpisode) error {
	if ledgerKey == "" {
		return fmt.Errorf("store: output correction ledger key is empty")
	}
	r, unlock, err := s.lockExistingRunMetadata(ctx, runID)
	if err != nil {
		return err
	}
	defer unlock()
	if r.OutputCorrections == nil {
		r.OutputCorrections = make(map[string]OutputCorrectionEpisode)
	}
	r.OutputCorrections[ledgerKey] = episode
	r.UpdatedAt = time.Now().UTC()
	return s.writeRunLockedContext(ctx, r, false, nil)
}

// SetRunBudgetOverrides persists the operator's launch-time budget ask
// (see RunStore). Load-modify-save under the store mutex, like
// SetRunBudgetSnapshot below, so a status transition racing this write
// is never reverted: SaveRun replaces the whole document from a copy
// loaded before the lock, which would undo a cancel — and would apply
// its own failure-code and pause-pointer normalisation to a field-scoped
// write that has no business touching either.
func (s *FilesystemRunStore) SetRunBudgetOverrides(ctx context.Context, runID string, o *RunBudgetOverrides) error {
	r, unlock, err := s.lockExistingRunMetadata(ctx, runID)
	if err != nil {
		return err
	}
	defer unlock()
	r.BudgetOverrides = o
	r.UpdatedAt = time.Now().UTC()
	return s.writeRunLockedContext(ctx, r, false, nil)
}

// SetRunnerVersion records the build that executed the run (see RunStore).
// Load-modify-save under the store mutex, like the other fs-side patches,
// so a status transition racing this write is never reverted.
func (s *FilesystemRunStore) SetRunnerVersion(ctx context.Context, runID, version string) error {
	r, unlock, err := s.lockExistingRunMetadata(ctx, runID)
	if err != nil {
		return err
	}
	defer unlock()
	r.RunnerVersion = version
	r.UpdatedAt = time.Now().UTC()
	return s.writeRunLockedContext(ctx, r, false, nil)
}

// SetRunBudgetSnapshot persists the effective caps (see RunStore).
// Load-modify-save under the store mutex, like the other fs-side
// patches, so a status transition racing this write is never reverted.
func (s *FilesystemRunStore) SetRunBudgetSnapshot(ctx context.Context, runID string, b *RunBudget) error {
	r, unlock, err := s.lockExistingRunMetadata(ctx, runID)
	if err != nil {
		return err
	}
	defer unlock()
	r.Budget = b
	r.UpdatedAt = time.Now().UTC()
	return s.writeRunLockedContext(ctx, r, false, nil)
}

// CountAliveRunsWithCredFingerprint counts queued/running runs stamped with
// fingerprint — a credential's, or a capacity subject — model-idle or not
// (see RunStore). Scan-and-filter like the other fs-side reverse queries —
// local scale, no secondary index.
func (s *FilesystemRunStore) CountAliveRunsWithCredFingerprint(ctx context.Context, fingerprint, excludeRunID string) (int, error) {
	if fingerprint == "" {
		return 0, nil
	}
	ids, err := s.filterRunsSorted(ctx, func(r *Run) bool {
		return r.ID != excludeRunID && r.SpendsCredential(fingerprint)
	})
	if err != nil {
		return 0, err
	}
	return len(ids), nil
}

// filterRunsSorted scans every run, keeps those matching pred, and
// returns their ids sorted by CreatedAt ascending. Shared by the
// fs-side reverse-tree queries.
func (s *FilesystemRunStore) filterRunsSorted(ctx context.Context, pred func(*Run) bool) ([]string, error) {
	return s.filterRunsSortedChecked(ctx, pred, false)
}

func (s *FilesystemRunStore) filterRunsSortedChecked(ctx context.Context, pred func(*Run) bool, strict bool) ([]string, error) {
	if ctx == nil {
		return nil, ErrInvalidValueFormat
	}
	if strict {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	var ids []string
	var err error
	if strict {
		// ListRuns hides stat failures before LoadRun can distinguish an
		// orphan from an unreadable candidate. A complete PR view cannot.
		ids, err = s.ListRunDirs(ctx)
	} else {
		ids, err = s.ListRuns(ctx)
	}
	if err != nil {
		return nil, err
	}
	type match struct {
		id string
		at time.Time
	}
	var matches []match
	for _, id := range ids {
		// A scan is the local index, so it is the one place a cancelled
		// caller (a sweep drained at shutdown, a project switch) must be
		// heard: LoadRun itself ignores ctx.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		r, err := s.LoadRun(ctx, id)
		if err != nil {
			if strict && !errors.Is(err, ErrRunNotFound) && !errors.Is(err, ErrRunDeleted) && !os.IsNotExist(err) {
				return nil, err
			}
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
		if matches[i].at.Equal(matches[j].at) {
			return matches[i].id < matches[j].id
		}
		return matches[i].at.Before(matches[j].at)
	})
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m.id)
	}
	// LoadRun ignores ctx; cancellation during the final real read (or an
	// empty scan) must not turn into a successful strict result.
	if strict {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *FilesystemRunStore) writeRunNewContext(ctx context.Context, r *Run) error {
	// Defence in depth for CreateRun's exclusive create path: sanitise here as
	// well as at the public entry point so future internal callers cannot path
	// join a tampered Run.ID outside the store root.
	if err := sanitizePathComponent("run ID", r.ID); err != nil {
		return err
	}
	writeLock, err := s.acquireRunWriteLock(r.ID)
	if err != nil {
		return err
	}
	defer func() { _ = writeLock.Unlock() }()
	if err := s.guardNotDeleted(r.ID); err != nil {
		return err
	}
	next := *r
	next.BirthID = NewRunBirthID()
	next.ExecutionEpoch = NewExecutionEpoch()
	next.CASVersion = 1

	data, err := marshalRunValueJSON(ctx, &next)
	if err != nil {
		return fmt.Errorf("store: marshal run: %w", err)
	}
	if ValueFormat(r.FormatVersion) == ExactValueFormat {
		if err := s.checkExactBirthNamespace(r.ID); err != nil {
			return err
		}
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
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := WriteFileAtomicNew(s.runJSONPath(r.ID), data, filePerm); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("store: run %s already exists: %w", r.ID, fs.ErrExist)
		}
		return err
	}
	r.CASVersion = next.CASVersion
	r.BirthID = next.BirthID
	r.ExecutionEpoch = next.ExecutionEpoch
	return nil
}

func (s *FilesystemRunStore) writeRun(r *Run) error {
	return s.writeRunWithAcquisition(r, false)
}

// This file-locked CAS tail owns the epoch for every FS writer, including
// whole-document saves, pause and queued rollback. Incoming epochs are ignored.
func (s *FilesystemRunStore) writeRunWithAcquisition(r *Run, acquire bool) error {
	return s.writeRunWithExpectedEpoch(r, acquire, nil)
}

// expectedEpoch pins the loaded incarnation as well as its version. Legacy
// setters still derive their epoch exclusively from the persisted document.
func (s *FilesystemRunStore) writeRunWithExpectedEpoch(r *Run, acquire bool, expectedEpoch *string) error {
	// Defence in depth: every public entry point that mutates a run
	// (SaveRun, UpdateRunStatus, SaveCheckpoint, PauseRun,
	// FailRunResumable) flows through here. Sanitise once, here, so
	// e.g. a Run loaded with a tampered ID can't be re-serialised to a
	// path outside the store root.
	if err := sanitizePathComponent("run ID", r.ID); err != nil {
		return err
	}
	writeLock, err := s.acquireRunWriteLock(r.ID)
	if err != nil {
		return err
	}
	defer func() { _ = writeLock.Unlock() }()
	return s.writeRunLockedContext(context.Background(), r, acquire, expectedEpoch)
}

// Caller holds s.mu and the stable run lock. This is the single CAS/epoch tail.
func (s *FilesystemRunStore) writeRunLockedContext(ctx context.Context, r *Run, acquire bool, expectedEpoch *string) error {
	return s.writeRunLockedWithNextEpoch(ctx, r, acquire, expectedEpoch, "")
}

// Only an authority acquisition may supply a next epoch. Ordinary writers
// derive the epoch exclusively from the persisted document.
func (s *FilesystemRunStore) writeRunLockedWithNextEpoch(ctx context.Context, r *Run, acquire bool, expectedEpoch *string, nextEpoch string) error {
	if err := s.guardNotDeleted(r.ID); err != nil {
		return err
	}
	current, loadErr := s.loadRunRawContext(ctx, r.ID)
	if loadErr != nil && !errors.Is(loadErr, ErrRunNotFound) {
		return loadErr
	}
	preservedFormat, err := CheckRunFormatReplacement(ctx, current, r)
	if err != nil {
		return err
	}
	if err := CheckRunDataReplacement(ctx, current, r); err != nil {
		return err
	}
	if expectedEpoch != nil && (current == nil || current.ExecutionEpoch != *expectedEpoch) {
		return fmt.Errorf("store: run %s execution epoch: %w", r.ID, ErrRunConflict)
	}
	if current != nil && current.CASVersion != r.CASVersion || current == nil && r.CASVersion != 0 {
		return fmt.Errorf("store: run %s: %w", r.ID, ErrRunConflict)
	}
	if current != nil && r.BirthID != "" && r.BirthID != current.BirthID {
		return fmt.Errorf("store: run %s birth identity: %w", r.ID, ErrRunConflict)
	}
	next := *r
	next.BirthID = NewRunBirthID()
	if current != nil {
		next.BirthID = current.BirthID
	}
	next.FormatVersion = preservedFormat
	next.CASVersion++
	next.ExecutionEpoch = NewExecutionEpoch()
	if current != nil {
		next.ExecutionEpoch = current.ExecutionEpoch
		if nextEpoch != "" {
			next.ExecutionEpoch = nextEpoch
		} else if acquire || current.Status != next.Status || next.Status == RunStatusQueued && !sameExecutionTime(current.QueuedAt, next.QueuedAt) {
			next.ExecutionEpoch = NewExecutionEpoch()
		}
	}

	data, err := marshalRunValueJSON(ctx, &next)
	if err != nil {
		return fmt.Errorf("store: marshal run: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	dir := s.runDir(r.ID)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("store: mkdir run: %w", err)
	}
	if err := os.Chmod(dir, dirPerm); err != nil {
		return fmt.Errorf("store: chmod run: %w", err)
	}
	// A checkpoint cannot become durable ahead of its selected event log.
	// The preceding append may belong to another handle or process, so a
	// process-local "unsynced" flag cannot decide whether this barrier is owed.
	eventPath, err := s.openRunPayloadFile(ctx, r.ID, "events.jsonl", false)
	if err != nil {
		return err
	}
	defer eventPath.Close()
	if err := s.syncEventsPathLocked(eventPath); err != nil {
		return fmt.Errorf("store: write run %s blocked by events durability barrier: %w", r.ID, err)
	}
	// Atomic write: run.json is the authoritative resume checkpoint
	// (per CLAUDE.md). A torn write would lose all prior checkpoint state.
	if err := writeFileAtomic(s.runJSONPath(r.ID), data, filePerm); err != nil {
		return err
	}
	r.CASVersion = next.CASVersion
	r.BirthID = next.BirthID
	r.ExecutionEpoch = next.ExecutionEpoch
	return nil
}
