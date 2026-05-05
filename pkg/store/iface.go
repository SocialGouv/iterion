package store

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
// Note : cette interface reflète la signature actuelle des méthodes
// — sans ctx context.Context. La migration vers des méthodes
// ctx-aware (plan §F T-07) est prévue en parallèle de la migration
// des call sites externes (T-09). Tant que les deux ne sont pas
// faites en même temps, l'interface reste sur les signatures
// existantes pour préserver la compilation.
type RunStore interface {
	// Lifecycle
	Root() string
	CreateRun(id, workflowName string, inputs map[string]interface{}) (*Run, error)
	LoadRun(id string) (*Run, error)
	SaveRun(r *Run) error
	ListRuns() ([]string, error)

	// Status & checkpoint
	UpdateRunStatus(id string, status RunStatus, runErr string) error
	SaveCheckpoint(id string, cp *Checkpoint) error
	PauseRun(id string, cp *Checkpoint) error
	FailRunResumable(id string, cp *Checkpoint, runErr string) error

	// Events (append-only, monotonic seq per run)
	AppendEvent(runID string, evt Event) (*Event, error)
	LoadEvents(runID string) ([]*Event, error)
	LoadEventsRange(runID string, from, to int64, limit int) ([]*Event, error)
	ScanEvents(runID string, visit func(*Event) bool) error

	// Artifacts (versionnés)
	WriteArtifact(a *Artifact) error
	LoadArtifact(runID, nodeID string, version int) (*Artifact, error)
	LoadLatestArtifact(runID, nodeID string) (*Artifact, error)
	ListArtifactVersions(runID, nodeID string) ([]ArtifactVersionInfo, error)

	// Interactions
	WriteInteraction(i *Interaction) error
	LoadInteraction(runID, interactionID string) (*Interaction, error)
	ListInteractions(runID string) ([]string, error)

	// Locks (advisory ; cross-process en local via flock,
	// distribué en cloud via NATS KV)
	LockRun(runID string) (RunLock, error)

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
