package store

import "time"

// A frozen program (ADR-167 T4a, frozen-program re-admission): the program a
// started run executed, kept when the run is parked on a capacity wait, so the
// retry sweeper's re-admission resumes that program — the one its checkpoint
// was taken under — and not whatever the bot became during the wait. A
// redelivery of the run's message would have run it verbatim; the re-admission
// is its stand-in once the place it held is gone.
//
// The record lives on the run root and names an envelope (an IR blob holding
// the transport IR and the bundle reference the parked delivery carried). It
// is written by the park itself (CapacityParker.ParkRunOnCapacity), in the
// same single write, bound to the attempt marker the park found (queued_at)
// and to the run's workflow hash. Nothing else writes it: every reader asks
// ReadmitFrozen, and a record whose attempt or hash moved since is stale —
// ignored, never cleared, never an error.

// FrozenProgramSourceCapacity names a record a capacity park wrote: the run
// lost the place its admission reserved while the queue redelivered it.
const FrozenProgramSourceCapacity = "capacity"

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
	// QueuedAt is the attempt marker the park found on the document, copied
	// by the park's own pipeline: the record binds that attempt only.
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
