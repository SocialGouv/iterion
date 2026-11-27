package store

import (
	"context"
	"time"
)

// A frozen program (ADR-167 T4a, frozen-program re-admission): the program a
// started run executed, kept when the run is parked on a wait the retry
// sweeper ends, so the sweeper's re-admission can resume that program — the
// one its checkpoint was taken under — and not whatever the bot became during
// the wait. A redelivery of the run's message would have run it verbatim; the
// re-admission is its stand-in.
//
// The record lives on the run root and names an envelope (an IR blob holding
// the transport IR and the bundle reference the parked delivery carried). Two
// parks write it, each bound to the attempt marker of the attempt it parks
// (queued_at) and to the run's workflow hash:
//   - a capacity park, in its own single write
//     (CapacityParker.ParkRunOnCapacity): the re-admission resumes the frozen
//     program, always;
//   - a usage-window park, before it arms its retry
//     (FrozenProgramWriter.SetRunFrozenProgram): the re-admission resumes the
//     bot's current program, and the frozen one only when the current one is
//     refused because its source changed.
//
// Nothing else writes it: every reader asks ReadmitFrozen, and a record whose
// attempt or hash moved since is stale — ignored, never cleared, never an
// error.

// FrozenProgramSourceCapacity names a record a capacity park wrote: the run
// lost the place its admission reserved while the queue redelivered it.
const FrozenProgramSourceCapacity = "capacity"

// FrozenProgramSourceUsageWindow names a record a usage-window park wrote:
// the run met its provider's usage window and waits for it to reopen.
const FrozenProgramSourceUsageWindow = "usage_window"

// FrozenProgram is the record of a frozen program on the run root.
type FrozenProgram struct {
	// Key is the envelope's storage key (IRBlobStore.PutIRBlob of the
	// logical key blob.FrozenProgramLogicalKey), what GetIRBlob reads.
	Key string `bson:"key"`
	// Digest is the sha256 (hex) of the envelope body: the reader refuses a
	// body that does not hash to it.
	Digest string `bson:"digest"`
	// WorkflowHash is the hash the envelope's IR derives at freeze — the run's
	// WorkflowHash then, which the binding compares with the run's now.
	WorkflowHash string `bson:"workflow_hash"`
	// QueuedAt is the attempt marker of the attempt the park parked, copied
	// off the document by the write that records it: the record binds that
	// attempt only.
	QueuedAt time.Time `bson:"queued_at"`
	// Source names the park that froze the program (FrozenProgramSource*).
	Source   string    `bson:"source"`
	FrozenAt time.Time `bson:"frozen_at"`
}

// Valid reports that the record names what a park writes: a key, a digest, a
// hash and a source.
func (fp *FrozenProgram) Valid() bool {
	return fp != nil && fp.Key != "" && fp.Digest != "" && fp.WorkflowHash != "" && fp.Source != ""
}

// FrozenVerdict is ReadmitFrozen's answer.
type FrozenVerdict string

const (
	// FrozenAbsent: the run carries no record.
	FrozenAbsent FrozenVerdict = "absent"
	// FrozenNotStarted: the run never started (ProgramStarted), whatever its
	// record says — nothing ran, so the current bot is resumed (ADR-167 §6).
	FrozenNotStarted FrozenVerdict = "not_started"
	// FrozenStale: the record names another attempt or another program than
	// the run's — an operator's resume, a re-admission that ran, a source
	// restamped since. Ignored: the run takes the current-bot path.
	FrozenStale FrozenVerdict = "stale"
	// FrozenBound: the record names this attempt and this run's program.
	FrozenBound FrozenVerdict = "bound"
)

// ProgramStarted reports whether the run executed its program at least once:
// a checkpoint was taken or an attempt was recorded (run_started,
// run_resumed). A claim that ended before its first event leaves neither and
// ran no node.
func (r *Run) ProgramStarted() bool {
	return r != nil && (r.Checkpoint != nil || len(r.Attempts) > 0)
}

// ReadmitFrozen is the one binding of a frozen program to its run, shared by
// every reader (the retry sweeper's decision, the run service's resume, the
// publisher's republish): absent with no record; not_started when the run
// never started; stale when the record's attempt marker is not the run's
// queued_at, or its hash not the run's workflow hash; bound otherwise. The
// record is returned whatever the verdict.
func (r *Run) ReadmitFrozen() (*FrozenProgram, FrozenVerdict) {
	if r == nil || r.FrozenProgram == nil {
		return nil, FrozenAbsent
	}
	fp := r.FrozenProgram
	if !r.ProgramStarted() {
		return fp, FrozenNotStarted
	}
	if r.QueuedAt == nil || fp.QueuedAt.IsZero() || !fp.QueuedAt.Equal(*r.QueuedAt) || fp.WorkflowHash != r.WorkflowHash {
		return fp, FrozenStale
	}
	return fp, FrozenBound
}

// FrozenProgramWriter is the optional capability that records the program a
// usage-window park froze (FrozenProgram) on the run root. Mongo only, like
// RunRetryStore and CapacityParker: the filesystem store hosts no durable
// retry, so nothing there re-admits a run and AsFrozenProgramWriter answers
// nil.
type FrozenProgramWriter interface {
	// SetRunFrozenProgram writes fp on run runID, bound to the attempt
	// queuedAt names, in one conditional write: the run must be
	// failed_resumable and its queued_at exactly queuedAt. A run that moved
	// (another status, another attempt) is not written: set false, no error.
	// The record's QueuedAt is queuedAt; an incomplete fp (FrozenProgram.Valid)
	// or a zero queuedAt is an error.
	SetRunFrozenProgram(ctx context.Context, runID string, queuedAt time.Time, fp *FrozenProgram) (set bool, err error)
}

// AsFrozenProgramWriter returns s as FrozenProgramWriter when the backend
// hosts it, or nil otherwise. Callers MUST nil-check.
func AsFrozenProgramWriter(s RunStore) FrozenProgramWriter { return capability[FrozenProgramWriter](s) }
