package store

import (
	"context"
	"io"
	"time"
)

// Capabilities expose les caractéristiques optionnelles d'un backend
// pour permettre aux consommateurs (runview, server) de choisir le bon
// chemin de code (par exemple : utiliser fsnotify ou les change streams
// Mongo, demander un PID-file ou non).
//
// See cloud-ready plan §C.1.
type Capabilities struct {
	// LiveStream — true si le backend peut pousser les events en
	// temps-réel (filesystem fsnotify, Mongo change streams).
	LiveStream bool
	// CrossProcessLock — true si LockRun protège contre une autre
	// instance d'iterion accédant au même run (flock cross-process en
	// local, NATS KV en cloud).
	CrossProcessLock bool
	// PIDFile — true si le backend implémente PIDStore (filesystem
	// uniquement). Les call sites doivent type-asserter avant usage.
	PIDFile bool
	// GitWorktree — true si le backend supporte la finalisation de
	// worktree git. False en cloud (runner clone éphémère).
	GitWorktree bool
}

// RunStore est l'interface unique adoptée par les call sites externes.
// L'implémentation locale est *FilesystemRunStore (filesystem-backed,
// JSON sur disque). L'implémentation cloud sera *MongoRunStore (à
// venir, plan §F T-17).
//
// Toutes les méthodes I/O prennent un ctx context.Context pour
// permettre la cancellation/timeout côté cloud (Mongo+S3+NATS). Les
// implémentations filesystem ignorent ctx (ops synchrones disque)
// mais doivent quand même l'accepter pour respecter l'interface.
// Root() et Capabilities() sont des accesseurs déclaratifs et n'ont
// pas besoin de ctx.
type RunStore interface {
	// Lifecycle
	Root() string
	CreateRun(ctx context.Context, id, workflowName string, inputs map[string]any) (*Run, error)
	LoadRun(ctx context.Context, id string) (*Run, error)
	SaveRun(ctx context.Context, r *Run) error
	ListRuns(ctx context.Context) ([]string, error)

	// Run-tree reverse queries (T4b, refs #125). Both project run ids
	// only, sorted by created_at ascending (oldest first), and return
	// an empty slice (no error) when nothing matches.
	//
	// ListRunsBySourceIssue returns every run whose Source.IssueID equals
	// issueID — the card←run reverse edge, i.e. every run a native-kanban
	// card triggered. ListChildRuns returns every run whose ParentRunID
	// equals parentRunID — a run's shard/child subtree (child points UP
	// via ParentRunID; there is no children list on the parent).
	ListRunsBySourceIssue(ctx context.Context, issueID string) ([]string, error)
	ListChildRuns(ctx context.Context, parentRunID string) ([]string, error)

	// ListRunsBySchedule returns every run whose Source.ScheduleID
	// equals scheduleID — the schedule←run reverse edge the overlap
	// gate (pkg/schedgate) counts live runs through. Same projection
	// contract as the other reverse queries: ids only, created_at
	// ascending, empty slice (no error) when nothing matches or
	// scheduleID is empty.
	ListRunsBySchedule(ctx context.Context, scheduleID string) ([]string, error)

	// Credential-concurrency meter (secrets.ApiKey.MaxConcurrentRuns).
	//
	// SetRunCredFingerprints stamps the run with the stable audit
	// identities of the credentials the publisher sealed for it — never
	// secrets. Re-stamped on every resume: re-resolution may pick
	// different credentials, and a stale stamp would meter a key the run
	// no longer holds.
	//
	// CountAliveRunsWithCredFingerprint counts runs in the queued or
	// running states stamped with fingerprint, excluding excludeRunID
	// (the run being resolved is already persisted and must not count
	// itself toward its own ceiling). Deliberately NOT tenant-scoped:
	// a platform key serves every tenant on one ceiling, and a tenant
	// key's runs all live in one tenant anyway.
	SetRunCredFingerprints(ctx context.Context, runID string, fingerprints []string) error
	CountAliveRunsWithCredFingerprint(ctx context.Context, fingerprint, excludeRunID string) (int, error)

	// SetRunBudgetOverrides updates the persisted launch-time budget
	// ask on the run — the resume path's replay source. Called by
	// SubmitResume when the operator raises a cap on THIS resume, so
	// a subsequent unattended auto-retry keeps the raised cap rather
	// than reverting to the launch ask that already killed the run
	// (E4, #652 review round 1). Nil clears the field. Granular:
	// touches only budget_overrides + updated_at, no other field is
	// disturbed — SaveRun's whole-doc replace is unsafe here because
	// the resume has already CAS-transitioned the doc to `queued`.
	SetRunBudgetOverrides(ctx context.Context, runID string, o *RunBudgetOverrides) error

	// PatchRunSteering persists the live-steering state (accumulated
	// loop grants + absolute budget raises) on the run record so a
	// resume re-applies them. nil map / nil raises leave the stored
	// field untouched (partial patch). Written by the engine goroutine
	// right after applying an override in memory.
	PatchRunSteering(ctx context.Context, runID string, loopOverrides map[string]int, budgetRaises *RunBudgetRaises) error

	// PatchRunPermissionGrants persists the `allow always` rules earned
	// at permission-gate pauses, keyed by the node that earned them, so a
	// later resume — which builds a fresh engine and a fresh runState —
	// still carries them. Replaces the stored map wholesale with the
	// caller's accumulated set; a nil map leaves it untouched. Kept
	// separate from PatchRunSteering because a grant is an authorization
	// the operator gave, not a steering knob an operator turned.
	PatchRunPermissionGrants(ctx context.Context, runID string, grants map[string][]string) error

	// RecordNodeServed persists the last (backend, model) that served
	// nodeID onto Run.NodesServed. Last write wins per node, so parallel
	// branches writing distinct keys do not clobber each other. Empty
	// nodeID is a no-op. Missing run → ErrRunNotFound.
	RecordNodeServed(ctx context.Context, runID, nodeID string, served NodeServed) error

	// PruneDeletionMarkers reaps the durable tombstones DeleteRun
	// leaves behind (the resurrection guard) once they are older than
	// cutoff — the marker must outlive any plausible late writer, not
	// live forever. Returns how many were reaped. `iterion runs prune`
	// is the designated caller.
	PruneDeletionMarkers(ctx context.Context, cutoff time.Time) (int, error)

	// Watch subscriptions (MVP3b) — the set of native-kanban issue IDs
	// this run is subscribed to. Concurrency-safe read-modify-write of
	// Run.WatchedIssueIDs (parallel branches' onNodeFinished hooks and
	// the watch API endpoints can mutate concurrently). Both return the
	// resulting deduped set.
	AddWatchedIssues(ctx context.Context, runID string, issueIDs []string) ([]string, error)
	RemoveWatchedIssues(ctx context.Context, runID string, issueIDs []string) ([]string, error)

	// Subbot re-attach map (subbot restart-safety). SetSubbotChild records
	// childRunID under key in the parent run's SubbotChildren map;
	// ClearSubbotChild removes it. Both are atomic per-key writes so
	// concurrent fan-out branches (distinct keys) don't clobber each other.
	// key is the subbot node's execution key (node id + loop-iteration path
	// + branch id). A resumed parent looks the key up (via LoadRun) to
	// re-attach to an in-flight/finished child instead of spawning a fresh
	// one. Both no-op silently when key is empty.
	SetSubbotChild(ctx context.Context, parentRunID, key, childRunID string) error
	ClearSubbotChild(ctx context.Context, parentRunID, key string) error

	// Status & checkpoint. The Coded variants carry the typed
	// FailureCode (ADR-095) in the SAME write as the status — never a
	// separate read-modify-write; the plain forms delegate with an
	// empty (unknown) code. Every transition to a non-failure status
	// clears the code, which is the invariant that keeps a resumed run
	// from lying about a past failure.
	UpdateRunStatus(ctx context.Context, id string, status RunStatus, runErr string) error
	UpdateRunStatusCoded(ctx context.Context, id string, status RunStatus, runErr string, code FailureCode) error
	// UpdateRunStatusIf is a compare-and-set on the status field: the
	// write only lands when the current status is in expectedFrom.
	// Returns changed=true when the write applied, false when the
	// status had drifted (different goroutine racing — e.g. a Cancel
	// firing concurrently with a Resume republish). Used by the
	// cloud publisher to avoid stomping on raced state transitions.
	UpdateRunStatusIf(ctx context.Context, id string, status RunStatus, runErr string, expectedFrom []RunStatus) (changed bool, err error)
	UpdateRunStatusIfCoded(ctx context.Context, id string, status RunStatus, runErr string, code FailureCode, expectedFrom []RunStatus) (changed bool, err error)
	// UpdateRunOutcome is the typed status transition: UpdateRunStatusIf
	// plus the outcome metadata (failure classification + continuation
	// ownership) persisted atomically with it. nil expectedFrom makes
	// the write unconditional. This is the RUNNER-side choke point that
	// stops the class of "the run document says failed_resumable and
	// nothing else" — a consumer reads WHY and WHO owns the continuation
	// from the document, not from event-prose archaeology. Engine paths
	// keep the code-only writers below (the engine cannot know the queue
	// topology, so it never states a continuation — F3 of the
	// adversarial gate).
	UpdateRunOutcome(ctx context.Context, id string, status RunStatus, runErr string, meta RunOutcomeMeta, expectedFrom []RunStatus) (changed bool, err error)
	// ClaimMerge is the compare-and-set entry to the merge state
	// machine: it flips MergeStatus to "merging" and stamps
	// MergeClaimedAt, iff the current status is claimable — "",
	// "pending", "failed", "skipped" or "conflicted", or a "merging"
	// whose MergeClaimedAt is before staleBefore or unset (the previous
	// claimant crashed; a wedged claim must not block the run forever).
	// Returns the status the run held before the claim so an aborted
	// attempt can restore it, plus the claim token (the exact
	// MergeClaimedAt stamp written — pass it as ExpectClaimedAt on
	// every exit so a stolen claim cannot consume its successor's), and
	// claimed=false (with the current status) when someone else holds a
	// fresh claim or the run is already merged.
	ClaimMerge(ctx context.Context, id string, staleBefore time.Time) (claimed bool, prior MergeStatus, claimToken time.Time, err error)
	// UpdateRunMergeIf is the compare-and-set exit from the merge state
	// machine: it persists upd's merge fields iff the current
	// MergeStatus is in expectedFrom (empty string matches an unset
	// field). Returns changed=false when the state drifted — the caller
	// lost its claim or raced another writer — in which case nothing
	// was written.
	UpdateRunMergeIf(ctx context.Context, id string, upd RunMergeUpdate, expectedFrom []MergeStatus) (changed bool, err error)
	SaveCheckpoint(ctx context.Context, id string, cp *Checkpoint) error
	PauseRun(ctx context.Context, id string, cp *Checkpoint) error
	FailRunResumable(ctx context.Context, id string, cp *Checkpoint, runErr string, code FailureCode) error
	// FailRunTerminal is FailRunResumable's terminal counterpart: status
	// failed (no auto-resume) but the checkpoint is kept so the run stays
	// rewindable on an explicit operator action.
	FailRunTerminal(ctx context.Context, id string, cp *Checkpoint, runErr string, code FailureCode) error

	// Events (append-only, monotonic seq per run)
	AppendEvent(ctx context.Context, runID string, evt Event) (*Event, error)
	LoadEvents(ctx context.Context, runID string) ([]*Event, error)
	LoadEventsRange(ctx context.Context, runID string, from, to int64, limit int) ([]*Event, error)
	ScanEvents(ctx context.Context, runID string, visit func(*Event) bool) error

	// Artifacts (versionnés)
	WriteArtifact(ctx context.Context, a *Artifact) error
	LoadArtifact(ctx context.Context, runID, nodeID string, version int) (*Artifact, error)
	LoadLatestArtifact(ctx context.Context, runID, nodeID string) (*Artifact, error)
	ListArtifactVersions(ctx context.Context, runID, nodeID string) ([]ArtifactVersionInfo, error)

	// Interactions
	WriteInteraction(ctx context.Context, i *Interaction) error
	LoadInteraction(ctx context.Context, runID, interactionID string) (*Interaction, error)
	ListInteractions(ctx context.Context, runID string) ([]string, error)

	// User-message inbox — operator-typed chat messages queued
	// against a running agent and drained cooperatively at safe
	// boundaries (between agent-loop iterations for claw, at the
	// next human pause for claude_code / codex). Persisted as a
	// JSONL transition log; the live state of each message is the
	// last status seen for its ID. See user_messages.go.
	AppendQueuedMessage(ctx context.Context, runID string, msg QueuedUserMessage) error
	UpdateQueuedMessageStatus(ctx context.Context, runID, msgID string, status QueuedMessageStatus, expectedFrom ...QueuedMessageStatus) error
	LoadPendingQueuedMessages(ctx context.Context, runID string) ([]QueuedUserMessage, error)
	ListQueuedMessages(ctx context.Context, runID string) ([]QueuedUserMessage, error)

	// Attachments — binary inputs declared by `attachments:` in the
	// workflow and uploaded at launch. Streaming I/O keeps large
	// uploads off the heap. Implementations persist bytes in their
	// native storage (filesystem dir, S3 object) and reflect the
	// metadata into Run.Attachments.
	WriteAttachment(ctx context.Context, runID string, rec AttachmentRecord, body io.Reader) error
	OpenAttachment(ctx context.Context, runID, name string) (io.ReadCloser, AttachmentRecord, error)
	ListAttachments(ctx context.Context, runID string) ([]AttachmentRecord, error)
	// RemoveAttachment deletes a single attachment by name, including
	// its metadata in Run.Attachments. Used for transactional rollback
	// of partial multi-attachment writes (cf. promoteStaged).
	RemoveAttachment(ctx context.Context, runID, name string) error
	DeleteRunAttachments(ctx context.Context, runID string) error
	// PresignAttachment returns a URL the caller can hand to a third
	// party (browser, agent's HTTP fetch) to retrieve the bytes
	// without going through this process. Local backends emit an
	// HMAC-signed `/api/runs/<id>/attachments/<name>` URL; cloud
	// backends emit a presigned S3 URL. ttl bounds the URL's
	// validity. The URL form is opaque to callers.
	PresignAttachment(ctx context.Context, runID, name string, ttl time.Duration) (string, error)

	// Locks (advisory ; cross-process en local via flock,
	// distribué en cloud via NATS KV)
	LockRun(ctx context.Context, runID string) (RunLock, error)

	// DeleteRun permanently removes a run and ALL of its data (the run
	// document, events, seq counter, interactions, queued user-messages,
	// artifacts and attachments). Backs the DELETE /api/runs/{id} admin
	// action. Idempotent: deleting an already-gone run is a no-op, not an
	// error. Tenant-scoped when the ctx carries a tenant.
	DeleteRun(ctx context.Context, id string) error

	// Capabilities — déclarées par chaque impl pour que les
	// consommateurs (runview, server) puissent brancher le bon code.
	Capabilities() Capabilities
}

// QueuedAttemptStore is the optional atomic guard used when a queue delivery
// is about to terminally fail a run before the runner has claimed it. Status
// alone is not enough to identify that delivery: an operator resume creates a
// NEW queued attempt for the same run id, and a stale delivery must not fail
// that newer attempt. Both production stores implement this capability.
type QueuedAttemptStore interface {
	// FailQueuedRunIfAttempt moves a queued run to failed_resumable only when
	// its current QueuedAt is not newer than the delivery's PublishedAt. The
	// comparison and status transition are one atomic store operation.
	//
	// meta is the typed WHY of the flip, persisted with it exactly like
	// UpdateRunOutcome's: the queue's admission park is a DLQ park
	// (FailureDLQParked, ContinuationFinal — nothing wakes the run but an
	// operator's replay), and every reader of the code must see the same
	// value whichever of the two DLQ writers flipped the doc.
	FailQueuedRunIfAttempt(ctx context.Context, id, runErr string, publishedAt time.Time, meta RunOutcomeMeta) (changed bool, err error)
}

// AsQueuedAttemptStore returns the attempt-aware status capability, or nil
// for third-party stores. Callers must fail safe when it is absent rather
// than falling back to a status-only write that can clobber a newer attempt.
func AsQueuedAttemptStore(s RunStore) QueuedAttemptStore {
	if s == nil {
		return nil
	}
	q, _ := s.(QueuedAttemptStore)
	return q
}

// PIDStore is an optional interface implemented only by
// FilesystemRunStore (Capabilities.PIDFile == true). Cloud (Mongo)
// stores deliberately do not implement it: detached/reattach is a
// filesystem-only feature. Callers go through AsPIDStore so a nil
// return cleanly disables the feature.
type PIDStore interface {
	PIDFilePath(runID string) string
	WritePIDFile(runID string, pid int) error
	ReadPIDFile(runID string) (int, error)
	RemovePIDFile(runID string) error
}

// AsPIDStore returns s as PIDStore when the backend supports PID
// files, or nil otherwise. Always check the return for nil before
// dereferencing — local stores satisfy it, cloud stores do not.
func AsPIDStore(s RunStore) PIDStore {
	if s == nil {
		return nil
	}
	p, _ := s.(PIDStore)
	return p
}

// QueuedRunCreator is the optional interface for stores that can persist
// a run document directly in the `queued` state (so it surfaces on the
// pipeline board's TODO lane while it waits for a local concurrency
// slot). Cloud stores already create runs queued via CreateRun; only the
// filesystem store — which creates runs `running` — needs this extra
// entry point. The local pipeline-concurrency queue reaches it through
// AsQueuedRunCreator, so a store that does not implement it cleanly
// disables the persisted-queue behaviour (the run then starts eagerly).
type QueuedRunCreator interface {
	// CreateQueuedRun persists a new run with status "queued", stamping
	// the file path + bot id so the board can render it and the local
	// scheduler can start it later via the engine's queued→running
	// pickup. No-clobber, like CreateRun.
	CreateQueuedRun(ctx context.Context, id, workflowName, filePath, botID string, inputs map[string]any) (*Run, error)
}

// AsQueuedRunCreator returns s as QueuedRunCreator when the backend can
// persist queued run docs, or nil otherwise. Check for nil before use.
func AsQueuedRunCreator(s RunStore) QueuedRunCreator {
	if s == nil {
		return nil
	}
	q, _ := s.(QueuedRunCreator)
	return q
}

// ParentedRunCreator is the optional interface for stores that can persist
// a child run's ParentRunID in the SAME write as the run document. Without
// it, spawnRun must CreateRun then SaveRun(ParentRunID) as two writes: if
// the second fails, the child doc is already `running` with no goroutine
// attached to it, and only the orphan reconciler eventually cleans it up.
// CreateChildRun collapses the two writes into one exclusive create, so
// there is no such window. Both production stores (filesystem, mongo)
// implement it; a store that does not cleanly degrades to the two-write
// path.
type ParentedRunCreator interface {
	// CreateChildRun persists a new run stamping ParentRunID atomically.
	// Same no-clobber contract as CreateRun (a reused run ID fails). The
	// filesystem store creates it `running`, the cloud store `queued`,
	// exactly like their CreateRun.
	CreateChildRun(ctx context.Context, id, workflowName, parentRunID string, inputs map[string]any) (*Run, error)
}

// AsParentedRunCreator returns s as ParentedRunCreator when the backend can
// persist ParentRunID in the create write, or nil otherwise. Check for nil
// before use.
func AsParentedRunCreator(s RunStore) ParentedRunCreator {
	if s == nil {
		return nil
	}
	p, _ := s.(ParentedRunCreator)
	return p
}

// RunFilesStore is an optional interface implemented by stores that
// can host arbitrary tool-produced files alongside a run. Tools running
// inside the sandbox write into a per-run scratch directory bind-mounted
// from the host (see ITERION_ARTIFACT_FILES_DIR in the runtime sandbox
// wiring); iterion surfaces the contents via the studio's Artifacts
// panel + the /api/runs/<id>/artifact-files endpoints.
//
// FilesystemRunStore implements it because it owns a real on-disk
// runs/<id>/ tree — its EnsureRunFilesDir directory IS the read source.
//
// The Mongo (cloud) store also implements it, but its WRITE target and
// READ source differ: EnsureRunFilesDir returns a runner-local scratch
// dir (bind-mounted into the sandbox on the runner pod), while
// ListRunFiles/OpenRunFile read from the S3 blob backend the server pod
// serves. The runner bridges the two after the run via the optional
// RunFilesUploader seam (UploadRunFiles walks the scratch dir → S3). So
// cloud artifact files become visible at run completion, not streamed
// live. AsRunFilesStore returns nil for stores that don't implement this
// interface — callers MUST nil-check.
type RunFilesStore interface {
	// EnsureRunFilesDir creates the per-run files area if missing
	// and returns its absolute path on the local filesystem. Called
	// at run-start, BEFORE the sandbox container is created so the
	// bind-mount source exists.
	EnsureRunFilesDir(ctx context.Context, runID string) (string, error)
	// ListRunFiles enumerates files under the area recursively.
	// Returns an empty slice (no error) when the area doesn't exist
	// — i.e. the run never produced any artifact files.
	ListRunFiles(ctx context.Context, runID string) ([]RunFileInfo, error)
	// OpenRunFile returns a reader + metadata for a single file at
	// the given area-relative path. Implementations MUST reject
	// paths that escape the area (`..`, absolute paths, symlink
	// traversal) before opening. Errors out with a clean
	// "not found" when the path is invalid OR doesn't exist —
	// callers don't need to distinguish the two cases for the HTTP
	// surface (both 404 to the client).
	OpenRunFile(ctx context.Context, runID, path string) (io.ReadCloser, RunFileInfo, error)
}

// RunFileInfo is the metadata returned by RunFilesStore.ListRunFiles
// and OpenRunFile. Path is area-relative (e.g. "renovacy/dbug-1.0-to-
// 1.7.md"), never absolute, never starts with "/".
type RunFileInfo struct {
	Path       string    `json:"path" bson:"path"`
	Size       int64     `json:"size" bson:"size"`
	ModifiedAt time.Time `json:"modified_at" bson:"modified_at"`
}

// AsRunFilesStore returns s as RunFilesStore when the backend supports
// per-run file artifacts, or nil otherwise. Filesystem and Mongo (cloud)
// stores both satisfy it; third-party stores may not.
func AsRunFilesStore(s RunStore) RunFilesStore {
	if s == nil {
		return nil
	}
	f, _ := s.(RunFilesStore)
	return f
}

// RunFilesUploader is an optional companion to RunFilesStore implemented
// only by stores whose EnsureRunFilesDir WRITE target differs from their
// ListRunFiles/OpenRunFile READ source. The Mongo store's scratch dir
// lives on the runner pod's local disk (the sandbox bind-mount source);
// after the run the runner calls UploadRunFiles to copy the produced
// files to the durable S3 read source the server pod serves from.
//
// FilesystemRunStore does NOT implement it — its scratch dir already IS
// the read source, so no bridge is needed. Callers MUST nil-check via
// AsRunFilesUploader (a nil return means "no bridge required").
type RunFilesUploader interface {
	// UploadRunFiles copies every file under the run's local scratch dir
	// to the durable read backend, returning the count uploaded. A run
	// that produced no files (or has no scratch dir) returns (0, nil).
	// Idempotent: re-uploading replaces the prior bytes.
	UploadRunFiles(ctx context.Context, runID string) (int, error)
}

// AsRunFilesUploader returns s as RunFilesUploader when the backend needs
// a scratch→durable bridge for artifact files, or nil otherwise (the
// common case — filesystem stores serve straight from the scratch dir).
func AsRunFilesUploader(s RunStore) RunFilesUploader {
	if s == nil {
		return nil
	}
	u, _ := s.(RunFilesUploader)
	return u
}

// ToolBlobStore is an optional interface implemented by stores that
// host per-tool-call I/O blobs (the bytes the LLM sent to and received
// from a tool). Used by the studio's per-node Tools tab to render full
// command outputs / large inputs without materialising them in
// events.jsonl. Events carry a small inline preview plus a `ref` that
// names the (run_id, tool_use_id, kind) tuple; the studio fetches the
// rest from the server's paginated endpoint on demand.
//
// Filesystem stores satisfy it (`<root>/runs/<id>/tools/<toolUseID>/{input,output}`).
// The Mongo (cloud) store also satisfies it, backed by the S3 blob client
// at `tools/<runID>/<toolUseID>/<kind>` (tool bodies can exceed the 16 MiB
// BSON ceiling, so they live in S3, not Mongo) — see
// pkg/store/mongo/toolblobs.go. The hooks layer still falls back to
// inline-only persistence for any store that does NOT implement it.
type ToolBlobStore interface {
	// WriteToolBlob writes body under runs/<id>/tools/<toolUseID>/<kind>.
	// kind ∈ {"input", "output"}. Returns the total byte size written.
	// Idempotent: re-writing the same key replaces the prior bytes.
	WriteToolBlob(ctx context.Context, runID, toolUseID, kind string, body []byte) (int64, error)
	// ReadToolBlob reads up to `limit` bytes starting at `offset`.
	// limit == 0 means "all from offset". Returns the bytes read, the
	// full blob size, and eof=true when offset+len(data) == size. A
	// missing blob returns (nil, 0, true, error) with an os.IsNotExist-
	// compatible error.
	ReadToolBlob(ctx context.Context, runID, toolUseID, kind string, offset, limit int64) (data []byte, total int64, eof bool, err error)
}

// AsToolBlobStore returns s as ToolBlobStore when the backend supports
// per-tool-call blob persistence, or nil otherwise. Callers MUST
// nil-check (cloud stores return nil today).
func AsToolBlobStore(s RunStore) ToolBlobStore {
	if s == nil {
		return nil
	}
	t, _ := s.(ToolBlobStore)
	return t
}

// BackendSessionStore persists packed CLI session files for session: persist
// (ADR-089). Filesystem: runs/<id>/backend-sessions/<ref>. Cloud: S3 under
// sessions/<runID>/<ref>. Get must work on resume *before* the next Execute.
type BackendSessionStore interface {
	PutBackendSession(ctx context.Context, runID, ref string, blob []byte) error
	GetBackendSession(ctx context.Context, runID, ref string) ([]byte, error)
	DeleteBackendSession(ctx context.Context, runID, ref string) error
}

// AsBackendSessionStore returns s as BackendSessionStore, or nil.
func AsBackendSessionStore(s RunStore) BackendSessionStore {
	if s == nil {
		return nil
	}
	t, _ := s.(BackendSessionStore)
	return t
}

// RunLogStore is an optional interface implemented by stores that
// persist the run's raw log byte stream (ADR-053). The filesystem
// store backs it with runs/<id>/run.log (the same file the local
// logger tee writes); the Mongo store backs it with the append-only
// run_logs chunk collection the cloud runner writes and the server
// pod's change-stream log source tails.
//
// Offsets are allocated by the single writer per run (locally the
// RunLogBuffer's running total, in cloud the runner's batching writer
// seeded from RunLogSize at claim time), so implementations treat a
// duplicate (run_id, offset) append as an idempotent redelivery.
type RunLogStore interface {
	// AppendRunLog persists one chunk of log bytes at the given
	// absolute byte offset.
	AppendRunLog(ctx context.Context, runID string, offset int64, data []byte) error
	// ReadRunLogRange returns bytes [from, until); until <= 0 means
	// "to the current end". A missing log yields (nil, nil) — the run
	// simply produced no log yet.
	ReadRunLogRange(ctx context.Context, runID string, from, until int64) ([]byte, error)
	// RunLogSize returns the total persisted byte count — i.e. the
	// offset the next append should use. 0 for a run with no log.
	RunLogSize(ctx context.Context, runID string) (int64, error)
}

// AsRunLogStore returns s as RunLogStore when the backend persists run
// logs, or nil otherwise. Callers MUST nil-check.
func AsRunLogStore(s RunStore) RunLogStore {
	if s == nil {
		return nil
	}
	l, _ := s.(RunLogStore)
	return l
}

// TurnStore is an optional interface implemented by stores that
// persist per-LLM-turn checkpoints. The interactivity feature set
// (operator pause, fork-from-here, per-node timeline) anchors on
// these — without a TurnStore, a fork has no place to load its
// session-id / messages snapshot from.
//
// FilesystemRunStore satisfies it (writes under
// runs/<id>/turns/<node>/<iter>/<turn>.json plus a per-node
// index.json for fast O(1) "latest turn" lookups). The Mongo (cloud)
// store also satisfies it via the tenant-stamped, TTL-parity
// `run_turns` collection (one doc per (run_id, node_id, loop_iter,
// turn_index), messages blob inline), so fork-from-turn + the per-node
// timeline work identically in cloud (see pkg/store/mongo/turns.go).
// Callers MUST still nil-check via AsTurnStore for third-party stores.
type TurnStore interface {
	// WriteTurn persists a TurnCheckpoint under
	// runs/<runID>/turns/<NodeID>/<LoopIter>/<TurnIndex>.json. The
	// caller is responsible for setting WrittenAt; implementations
	// are free to overwrite it for monotonic-clock safety.
	WriteTurn(ctx context.Context, t *TurnCheckpoint) error
	// LoadTurn returns the TurnCheckpoint at the exact
	// (NodeID, LoopIter, TurnIndex) coordinates, or a typed
	// not-found error.
	LoadTurn(ctx context.Context, runID, nodeID string, loopIter, turn int) (*TurnCheckpoint, error)
	// ListTurns enumerates all turns for one (NodeID, LoopIter)
	// in ascending TurnIndex order. Returns an empty slice (no
	// error) when no turns exist for that node yet.
	ListTurns(ctx context.Context, runID, nodeID string, loopIter int) ([]*TurnCheckpoint, error)
	// LatestTurn returns the highest-indexed turn for a node across
	// all loop iterations, or a typed not-found error when none
	// exists. Used by Fork to default `turn_index` to "the last
	// completed turn".
	LatestTurn(ctx context.Context, runID, nodeID string) (*TurnCheckpoint, error)
	// LoadTurnAtIndex returns the turn at the given TurnIndex on the
	// highest loop iteration that has one (scanning all iterations).
	// Used by Fork for an explicit turn_index so a node that only ran
	// on a higher loop iteration still resolves instead of failing.
	LoadTurnAtIndex(ctx context.Context, runID, nodeID string, turn int) (*TurnCheckpoint, error)
	// LoadTurnMessages reads the sibling messages.json blob
	// referenced by a claw TurnCheckpoint.MessagesRef. Returns a
	// typed not-found error when the blob is missing (e.g. legacy
	// turn or non-claw backend).
	LoadTurnMessages(ctx context.Context, runID, nodeID string, loopIter, turn int) ([]byte, error)
}

// AsTurnStore returns s as TurnStore when the backend supports
// per-LLM-turn checkpointing, or nil otherwise.
func AsTurnStore(s RunStore) TurnStore {
	if s == nil {
		return nil
	}
	t, _ := s.(TurnStore)
	return t
}

// SpendStore is an optional interface implemented by stores that can
// persist a per-(store, UTC-day) LLM spend ledger backing the daily
// spend cap. FilesystemRunStore satisfies it (<root>/spend/<date>.json).
// Cloud (Mongo) stores currently do NOT — the daily cap is a local-mode
// feature for now; the spend-cap guard treats a nil SpendStore as
// "cap disabled". Callers MUST nil-check via AsSpendStore.
type SpendStore interface {
	// LoadDailySpend returns the ledger for a UTC day (YYYY-MM-DD),
	// or a zero-valued ledger (no error) when the day is absent.
	LoadDailySpend(ctx context.Context, date string) (*DailySpend, error)
	// AddSpend records a run's latest cumulative cost into the day's
	// ledger (idempotent) and returns the updated ledger.
	AddSpend(ctx context.Context, date, runID string, cumulativeRunCostUSD float64) (*DailySpend, error)
	// SetSpendOverride sets/clears the day's override flag and returns
	// the updated ledger.
	SetSpendOverride(ctx context.Context, date string, ov *SpendOverride) (*DailySpend, error)
}

// AsSpendStore returns s as SpendStore when the backend persists a daily
// spend ledger, or nil otherwise. Filesystem stores satisfy it; cloud
// (Mongo) stores currently do not.
func AsSpendStore(s RunStore) SpendStore {
	if s == nil {
		return nil
	}
	sp, _ := s.(SpendStore)
	return sp
}
