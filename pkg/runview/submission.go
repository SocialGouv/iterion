package runview

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/SocialGouv/iterion/pkg/store"
)

// An idempotent submission (ADR-168 §8.3) is a launch an at-least-once caller
// may repeat — a connector pair re-claimed after a crash, a retry after a
// returned error — without ever producing a second run. The caller names the
// submission with a key; the launch path derives the run id from the key and
// the tenant, so the identity of the run IS the identity of the submission and
// no field on the run document has to carry it.
//
// Submitting a key whose run already exists answers from that run instead of
// failing or creating another:
//   - a run that never left its launch path (LAUNCH_FAILED) has spent its id:
//     the caller moves to its next key (SubmissionSpentError);
//   - a cloud run still queued by the launch path's own attempt — no runner
//     claimed it, nothing re-queued it (store.Run.SubmissionAttemptAt) — is
//     COMPLETED as a NEW attempt: re-persisted under compare-and-swap with a
//     queued_at of its own and published under a message identity of its
//     own. A runner claims a queued run only for a delivery published at or
//     after its queued_at, so a first publication that did land can no
//     longer claim it: one attempt is ever executable;
//   - any other run was handed on — claimed, parked, re-queued by a resume —
//     and the answer is that run, unpublished.

// submissionRunIDPrefix marks the run ids the launch path derives from a
// submission key. The namespace is reserved: a caller-chosen run id that
// carries the prefix is refused (ErrRunIDReserved), so no other launch can
// occupy the id a submission will need, in any team.
const submissionRunIDPrefix = "idem-"

// submissionNamespace is the UUIDv5 namespace of submission run ids.
var submissionNamespace = uuid.MustParse("6a1d3c55-1f0e-5b7a-9c2e-4d8f0b3a7e21")

// SubmissionRunID is the run id of the submission key in tenantID: stable
// across attempts, distinct across tenants and keys. The tenant is
// length-prefixed, so no byte of either can move across the boundary.
func SubmissionRunID(tenantID, key string) string {
	name := strconv.Itoa(len(tenantID)) + ":" + tenantID + key
	return submissionRunIDPrefix + uuid.NewSHA1(submissionNamespace, []byte(name)).String()
}

// IsSubmissionRunID reports whether id lies in the namespace reserved for
// submission run ids.
func IsSubmissionRunID(id string) bool {
	return strings.HasPrefix(id, submissionRunIDPrefix)
}

// ErrRunIDReserved refuses a caller-chosen run id inside the namespace
// reserved for submissions: it would occupy the id a submission derives, and
// the submission would then find a run it cannot tell from its own.
var ErrRunIDReserved = errors.New("runview: run id is reserved for idempotent submissions")

// ErrSubmissionSpent is the class of SubmissionSpentError, for errors.Is.
var ErrSubmissionSpent = errors.New("runview: the submission's run never left its launch path")

// SubmissionSpentError reports a submission whose run exists and never left
// its launch path (store.FailureLaunchFailed). That row is terminal by the
// store's doctrine — a launch failure is re-done by its caller, never by
// resuming the row — so this run id can launch nothing more: the caller
// retries under its next key, and the failed run stays visible.
type SubmissionSpentError struct {
	RunID string
	// Cause is the launch failure the run recorded.
	Cause string
}

func (e *SubmissionSpentError) Error() string {
	return fmt.Sprintf("runview: submission run %s never left its launch path (%s): its id is spent, retry under the next key", e.RunID, e.Cause)
}

// Is makes the error match ErrSubmissionSpent.
func (e *SubmissionSpentError) Is(target error) bool { return target == ErrSubmissionSpent }

// SubmissionSpent is the method an at-least-once caller reads without
// importing this package.
func (e *SubmissionSpentError) SubmissionSpent() bool { return true }

// SubmissionRefusedError reports a submission whose run id is held by
// something the submission cannot claim: a run of another tenant, or a run an
// operator deleted. Retrying the same key cannot change the answer.
type SubmissionRefusedError struct {
	RunID  string
	Reason string
	Err    error
}

func (e *SubmissionRefusedError) Error() string {
	return fmt.Sprintf("runview: submission run %s refused: %s", e.RunID, e.Reason)
}

func (e *SubmissionRefusedError) Unwrap() error { return e.Err }

// RefusalReason is the method an at-least-once caller reads without importing
// this package: a refusal is permanent for this key.
func (e *SubmissionRefusedError) RefusalReason() string { return e.Reason }

// SubmissionVerdict is what a submission owes the run its key already names.
type SubmissionVerdict int

const (
	// SubmissionFresh: no run carries the id; launch it.
	SubmissionFresh SubmissionVerdict = iota
	// SubmissionComplete: the run is persisted and nothing proves it was
	// handed on; complete its submission.
	SubmissionComplete
	// SubmissionAcknowledged: the run was handed on; answer it.
	SubmissionAcknowledged
)

// ClassifySubmission judges the run a submission's id already names. r nil is
// a fresh submission. publishes says whether the caller hands runs on through
// a queue: only there can a persisted run be unpublished. In process, the
// launch path creates the run and starts it in one call, a local queued run
// belongs to the pipeline scheduler, and a detached run to its runner — each
// handed on the moment its row exists.
func ClassifySubmission(r *store.Run, tenantID string, publishes bool) (SubmissionVerdict, error) {
	if r == nil {
		return SubmissionFresh, nil
	}
	// A run that records no tenant comes from a store that stamps none (the
	// filesystem one): its id, derived from this tenant's key, is the only
	// identity there is to compare.
	if r.TenantID != "" && r.TenantID != tenantID {
		return 0, &SubmissionRefusedError{RunID: r.ID, Reason: fmt.Sprintf("the run belongs to another tenant (%q)", r.TenantID), Err: ErrRunIDTaken}
	}
	if r.Status == store.RunStatusFailed && r.FailureCode == store.FailureLaunchFailed {
		return 0, &SubmissionSpentError{RunID: r.ID, Cause: r.Error}
	}
	if publishes && QueuedBySubmission(r) {
		return SubmissionComplete, nil
	}
	return SubmissionAcknowledged, nil
}

// QueuedBySubmission reports whether r is still queued by the attempt the
// launch path made for its submission: queued, at the queued_at that attempt
// stamped, with no checkpoint. A resume's flip, a runner's claim, a park all
// leave a run that is not — and that was handed on.
func QueuedBySubmission(r *store.Run) bool {
	return r.Status == store.RunStatusQueued && r.Checkpoint == nil &&
		r.QueuedAt != nil && r.SubmissionAttemptAt != nil && r.QueuedAt.Equal(*r.SubmissionAttemptAt)
}

// SubmissionDeleted is the refusal a deleted run's id answers: the run was
// submitted and an operator removed it, which a retry must not undo. err is
// the store's tombstone error.
func SubmissionDeleted(runID string, err error) error {
	return &SubmissionRefusedError{RunID: runID, Reason: "the run was deleted", Err: errors.Join(ErrRunIDTaken, err)}
}
