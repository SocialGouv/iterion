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
	CreateRun(ctx context.Context, id, workflowName string, inputs map[string]interface{}) (*Run, error)
	LoadRun(ctx context.Context, id string) (*Run, error)
	SaveRun(ctx context.Context, r *Run) error
	ListRuns(ctx context.Context) ([]string, error)

	// Status & checkpoint
	UpdateRunStatus(ctx context.Context, id string, status RunStatus, runErr string) error
	SaveCheckpoint(ctx context.Context, id string, cp *Checkpoint) error
	PauseRun(ctx context.Context, id string, cp *Checkpoint) error
	FailRunResumable(ctx context.Context, id string, cp *Checkpoint, runErr string) error

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

	// Attachments — binary inputs declared by `attachments:` in the
	// workflow and uploaded at launch. Streaming I/O keeps large
	// uploads off the heap. Implementations persist bytes in their
	// native storage (filesystem dir, S3 object) and reflect the
	// metadata into Run.Attachments.
	WriteAttachment(ctx context.Context, runID string, rec AttachmentRecord, body io.Reader) error
	OpenAttachment(ctx context.Context, runID, name string) (io.ReadCloser, AttachmentRecord, error)
	ListAttachments(ctx context.Context, runID string) ([]AttachmentRecord, error)
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

	// Capabilities — déclarées par chaque impl pour que les
	// consommateurs (runview, server) puissent brancher le bon code.
	Capabilities() Capabilities
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

// RunFilesStore is an optional interface implemented by stores that
// can host arbitrary tool-produced files alongside a run. Tools running
// inside the sandbox write into a per-run scratch directory bind-mounted
// from the host (see ITERION_ARTIFACT_FILES_DIR in the runtime sandbox
// wiring); iterion surfaces the contents via the editor's Artifacts
// panel + the /api/runs/<id>/artifact-files endpoints.
//
// FilesystemRunStore implements it because it owns a real on-disk
// runs/<id>/ tree. Cloud stores currently do NOT implement it: the
// sandbox runs on a different machine from the Mongo/S3 backend, so
// the tool can't write directly to a shared mount; cloud-mode artifact-
// files would need an S3-backed scratch area + a sandbox-side uploader.
// AsRunFilesStore returns nil for stores that don't implement this
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
// per-run file artifacts, or nil otherwise. Filesystem stores satisfy
// it; cloud (Mongo) stores currently do not.
func AsRunFilesStore(s RunStore) RunFilesStore {
	if s == nil {
		return nil
	}
	f, _ := s.(RunFilesStore)
	return f
}

// ToolBlobStore is an optional interface implemented by stores that
// host per-tool-call I/O blobs (the bytes the LLM sent to and received
// from a tool). Used by the editor's per-node Tools tab to render full
// command outputs / large inputs without materialising them in
// events.jsonl. Events carry a small inline preview plus a `ref` that
// names the (run_id, tool_use_id, kind) tuple; the editor fetches the
// rest from the server's paginated endpoint on demand.
//
// Filesystem stores satisfy it (`<root>/runs/<id>/tools/<toolUseID>/{input,output}`).
// Cloud (Mongo) stores currently do NOT — the hooks layer falls back to
// inline-only persistence when this interface is not implemented.
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
