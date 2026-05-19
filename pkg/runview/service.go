package runview

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runtime/recovery"
	"github.com/SocialGouv/iterion/pkg/store"
)

// LaunchSpec describes a workflow invocation. Mirrors the inputs of
// `iterion run` but framed as data so HTTP handlers (and any future
// programmatic caller) construct it without going through cobra flags.
type LaunchSpec struct {
	FilePath string // absolute .iter path; sandbox check is the caller's job
	// Source carries the .iter source verbatim. Used by cloud-mode
	// callers when the server pod has no local copy of the workflow
	// (the studio SPA uploads the source inline). When non-empty it
	// takes precedence over FilePath for parsing; FilePath is still
	// retained for display and for the runner to recompile against
	// the same logical workflow.
	Source string
	Vars   map[string]string // --var-style overrides
	// Preset is the name of an in-source preset (presets: block) to
	// apply before Vars. Unknown name → launch error. Empty means no
	// preset.
	Preset  string
	RunID   string        // optional explicit ID; auto-generated when empty
	Timeout time.Duration // 0 disables
	// MergeInto controls the worktree-finalization fast-forward target
	// for `worktree: auto` runs. "" or "current" → FF the user's
	// currently-checked-out branch (default); "none" → skip FF;
	// <branch-name> → FF that branch (only honoured when it matches
	// the currently-checked-out branch).
	MergeInto string
	// BranchName overrides the default storage branch
	// `iterion/run/<friendly>` created on the worktree's HEAD.
	BranchName string
	// MergeStrategy selects how the run's commits are landed on the
	// merge target: "squash" (default — collapse into one commit) or
	// "merge" (fast-forward, preserve history). Persisted on run.json
	// so the deferred-merge UI can pre-fill the same choice.
	MergeStrategy store.MergeStrategy
	// AutoMerge captures the launch-time intent. When true, the engine
	// applies MergeStrategy synchronously at end of run; when false the
	// engine creates the storage branch only and leaves merge_status
	// pending for the UI to drive via POST /api/runs/{id}/merge.
	AutoMerge bool
	// AttachmentPromote, when set, is invoked after CreateRun and
	// before the engine starts. It is expected to materialise every
	// attachment declared in `attachments:` into the run-scoped store
	// (typically by promoting uploads from a staging area). Errors
	// abort the launch.
	AttachmentPromote runtime.AttachmentPromoteFunc
	// Backend, when non-empty, overrides the workflow's `default_backend:`
	// for this run. Node-level explicit `backend:` declarations still
	// win — this is a soft default override, not a hard force. Empty
	// preserves the resolver chain. NOTE: the detached runner path
	// (ITERION_RUNS_DETACHED=1) does not yet honor this field; the
	// service layer logs a warning and ignores it there.
	Backend string
	// ParentRunID, ShardIndex, ShardCount, ShardLabel are set when a
	// parent run dispatches this as a shard child (see Cap. 3 in
	// docs/security-bots-distributed.md). The cloudpublisher copies
	// them onto the persisted Run document AND onto the published
	// RunMessage so the runner pod that picks up the work knows it's
	// part of a sharded set.
	ParentRunID string
	ShardIndex  int
	ShardCount  int
	ShardLabel  string
}

// ResumeSpec describes a resume request.
type ResumeSpec struct {
	RunID    string
	FilePath string // .iter file (loaded fresh; must match the run's WorkflowHash unless Force)
	// Source mirrors LaunchSpec.Source: cloud-mode callers can supply
	// the .iter contents inline so the server pod does not need to
	// resolve FilePath against a local filesystem.
	Source  string
	Answers map[string]interface{} // answers for human nodes; ignored for failed_resumable
	Force   bool                   // skip workflow hash check
	Timeout time.Duration          // 0 disables
}

// RunSummary is the lightweight per-row shape returned by List.
// Heavier fields (events, artifacts, checkpoint detail) live in
// RunSnapshot — call Snapshot for the full view.
type RunSummary struct {
	ID string `json:"id"`
	// Name is the deterministic, human-friendly label for the run.
	// Empty for legacy runs persisted before this field existed.
	Name         string          `json:"name,omitempty"`
	WorkflowName string          `json:"workflow_name"`
	Status       store.RunStatus `json:"status"`
	FilePath     string          `json:"file_path,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
	FinishedAt   *time.Time      `json:"finished_at,omitempty"`
	Error        string          `json:"error,omitempty"`
	// Active reports whether the run is currently held by this
	// process's manager. A run with status "running" but Active=false
	// belongs to another process or to a previous boot — Cancel won't
	// reach it from here.
	Active bool `json:"active"`
	// Worktree finalization summary (only populated for `worktree:
	// auto` runs that reached a clean exit). See store.Run for the
	// full semantics.
	FinalCommit      string              `json:"final_commit,omitempty"`
	FinalBranch      string              `json:"final_branch,omitempty"`
	FinalBranchError string              `json:"final_branch_error,omitempty"`
	MergedInto       string              `json:"merged_into,omitempty"`
	MergedCommit     string              `json:"merged_commit,omitempty"`
	MergeStrategy    store.MergeStrategy `json:"merge_strategy,omitempty"`
	MergeStatus      store.MergeStatus   `json:"merge_status,omitempty"`
	AutoMerge        bool                `json:"auto_merge,omitempty"`
	// QueuePosition is set only for cloud-mode runs whose Status is
	// "queued"; nil otherwise. 1 means "next to be picked up". Computed
	// by the server (Mongo aggregation), not persisted on the run doc.
	// See cloud-ready plan §F (T-03, T-31).
	QueuePosition *int `json:"queue_position,omitempty"`
}

// ListFilter scopes a List request. Empty fields mean no filter.
type ListFilter struct {
	Status   store.RunStatus // exact match
	Workflow string          // exact match on WorkflowName
	Since    time.Time       // UpdatedAt >= Since
	Limit    int             // 0 = no limit
	// Node filters runs to those whose persisted events include at
	// least one node_started for this IR node ID. Used by the studio
	// to surface "this node was touched by N runs" without scanning
	// every run on the client. Scanning happens at request time —
	// fine for hundreds of runs; wire an inverted index later if the
	// store grows past low thousands.
	Node string
}

// ArtifactSummary is the lightweight shape returned by ListArtifacts —
// just enough for the UI to populate a version selector without
// reading every artifact body.
type ArtifactSummary struct {
	Version   int       `json:"version"`
	WrittenAt time.Time `json:"written_at"`
}

// Service is the canonical façade over runtime + store + broker +
// manager. The HTTP server, the studio, and (optionally) the CLI all
// route through here — keeping a single source of truth for run
// lifecycle, validation, and event fan-out.
type Service struct {
	store    store.RunStore
	storeDir string
	// workDir is the directory the engine should treat as ${PROJECT_DIR}
	// and as the repo-lookup seed for worktree: auto. Empty means
	// "default to os.Getwd() at Run() time" — the right thing for the
	// CLI (which runs in the user's cwd) but wrong for the desktop
	// server (whose process cwd is the user's home).
	workDir string
	logger  *iterlog.Logger
	broker  *EventBroker
	manager *Manager

	// recoveryDispatch is built once on construction so each Launch /
	// Resume reuses the same dispatcher rather than allocating a new
	// recipes map + closure on the per-run hot path.
	recoveryDispatch runtime.RecoveryDispatch

	// extraObservers are runtime EventObservers chained alongside
	// the broker fan-out. Used to attach Prometheus / OTLP / custom
	// observers when constructing a server-side service.
	extraObservers []func(store.Event)

	// wireWFCache memoises WireWorkflow projections by (filePath, hash)
	// so /api/runs/{id}/workflow doesn't re-parse + re-compile on every
	// request. Invalidated implicitly when the .iter source changes
	// (hash mismatch). See workflow_export.go.
	wireWFCache wireWorkflowCache

	// runLogs holds a per-run log buffer for the lifetime of each
	// in-process run. Created in spawnRun, removed when the run
	// goroutine exits. The buffer captures the iterion logger output
	// scoped to that run and fans it out to live WS subscribers; see
	// runlog.go and the /api/runs/{id}/log endpoint.
	runLogsMu sync.RWMutex
	runLogs   map[string]*RunLogBuffer

	// draining is set by Drain at the start of graceful shutdown.
	// Once true, Launch and Resume early-return runtime.ErrServerDraining
	// so the HTTP layer can map it to 503 Service Unavailable.
	draining atomic.Bool

	// publisher, when non-nil, intercepts Launch/Resume/Cancel and
	// routes them through the cloud queue. When nil the service runs
	// the engine in-process (local mode). See LaunchPublisher and
	// WithLaunchPublisher.
	publisher LaunchPublisher

	// injectedStore captures the WithStore option so NewService can
	// honour a caller-supplied store. nil → fall back to the
	// filesystem auto-discovery path (local mode).
	injectedStore store.RunStore

	// eventSource, when non-nil, replaces the in-process EventBroker
	// for live + historical event delivery. Cloud mode injects an
	// eventstream.MongoSource via WithEventSource so the WS handler
	// streams from change streams instead of relying on the local
	// broker (which only sees this process's writes). Plan §F (T-21).
	eventSource EventStreamSource
}

// EventStreamSource is the small subset of pkg/runview/eventstream.Source
// the WS handler needs. Defined locally to avoid an import cycle —
// the eventstream package can't depend back on runview.
type EventStreamSource interface {
	Subscribe(ctx context.Context, runID string, fromSeq int64) (EventStreamSubscription, error)
}

// EventStreamSubscription mirrors eventstream.Subscription's surface.
type EventStreamSubscription interface {
	Events() <-chan *store.Event
	Errors() <-chan error
	Close() error
}

// ServiceOption configures a Service at construction time.
type ServiceOption func(*Service)

// WithWorkDir sets the working directory the engine should use for
// `${PROJECT_DIR}` expansion and as the seed for the worktree git-repo
// lookup. Without this, the engine falls back to os.Getwd() at Run()
// time, which in the desktop server case is whatever cwd the desktop
// process was launched from (typically the user's home dir, not the
// project root). Set this to the same directory the host server's
// WorkDir was configured with.
func WithWorkDir(dir string) ServiceOption {
	return func(s *Service) {
		s.workDir = dir
	}
}

// WithLogger sets the logger used for service-level diagnostics.
func WithLogger(l *iterlog.Logger) ServiceOption {
	return func(s *Service) {
		if l != nil {
			s.logger = l
		}
	}
}

// WithBroker injects an existing event broker. When omitted, the
// service creates its own.
func WithBroker(b *EventBroker) ServiceOption {
	return func(s *Service) {
		if b != nil {
			s.broker = b
		}
	}
}

// WithManager injects an existing lifecycle manager. When omitted,
// the service creates its own.
func WithManager(m *Manager) ServiceOption {
	return func(s *Service) {
		if m != nil {
			s.manager = m
		}
	}
}

// WithExtraEventObservers adds observers chained alongside the
// broker.Publish observer. Use this to wire Prometheus / OTLP
// exporters into the HTTP service's run goroutines.
func WithExtraEventObservers(observers ...func(store.Event)) ServiceOption {
	return func(s *Service) { s.extraObservers = append(s.extraObservers, observers...) }
}

// WithLaunchPublisher wires the cloud-mode publisher; when nil the
// service stays in local-mode (in-process engine).
func WithLaunchPublisher(p LaunchPublisher) ServiceOption {
	return func(s *Service) { s.publisher = p }
}

// WithStore replaces the default filesystem store with a caller-
// supplied implementation. When set, NewService skips the store
// auto-discovery and uses the supplied store directly. Used by
// cloud-mode entry points to inject the Mongo+S3 store. Plan §F
// (T-19, T-30).
func WithStore(s store.RunStore) ServiceOption {
	return func(svc *Service) { svc.injectedStore = s }
}

// WithEventSource installs an alternative event source (typically
// eventstream.MongoSource) so the WS handler streams from change
// streams instead of the in-process EventBroker. The argument must
// satisfy the EventStreamSource interface — a thin shape over
// pkg/runview/eventstream/Source. Plan §F (T-21).
func WithEventSource(s EventStreamSource) ServiceOption {
	return func(svc *Service) { svc.eventSource = s }
}

// NewService constructs a Service rooted at storeDir. When the
// caller wires WithStore, storeDir may be "" — the service uses the
// injected store directly without resolving a filesystem path.
func NewService(storeDir string, opts ...ServiceOption) (*Service, error) {
	logger := iterlog.New(iterlog.LevelInfo, os.Stderr)

	s := &Service{
		storeDir:         storeDir,
		logger:           logger,
		broker:           NewEventBroker(),
		manager:          NewManager(),
		recoveryDispatch: recovery.Dispatch(recovery.DefaultRecipes()),
		runLogs:          make(map[string]*RunLogBuffer),
	}
	for _, opt := range opts {
		opt(s)
	}

	switch {
	case s.injectedStore != nil:
		s.store = s.injectedStore
	case storeDir != "":
		st, err := store.New(storeDir, store.WithLogger(s.logger))
		if err != nil {
			return nil, fmt.Errorf("runview: open store: %w", err)
		}
		s.store = st
	default:
		// Fall back to the prior implicit ".iterion" behaviour so
		// pre-existing local callers keep working.
		st, err := store.New(".iterion", store.WithLogger(s.logger))
		if err != nil {
			return nil, fmt.Errorf("runview: open store: %w", err)
		}
		s.store = st
		s.storeDir = ".iterion"
	}

	// Wire log-position stamping when the store is a local
	// FilesystemRunStore. The closure reads the current byte total
	// from the per-run RunLogBuffer (created lazily by
	// prepareRunLog); a missing entry returns 0, which the studio
	// interprets as "no offset info — show live tail". Cloud
	// (Mongo) stores skip this wiring — they have no on-host log
	// buffer to attach.
	if fs, ok := s.store.(*store.FilesystemRunStore); ok {
		fs.SetLogPositionFn(s.logPositionForRun)
	}

	s.reconcileOrphans()
	s.reconcileSandboxContainers()
	return s, nil
}

// Broker exposes the event broker for transports that need to
// subscribe directly (the WS handler).
func (s *Service) Broker() *EventBroker { return s.broker }

// inboxBinder returns the runtime's operator-chatbox plumbing
// scoped to this service's store + broker. Built once per Build-
// Executor call so the binder closures see a consistent store
// handle even when a service is hot-swapped (project switch).
func (s *Service) inboxBinder() model.InboxBinder {
	if s == nil || s.store == nil {
		return nil
	}
	binder := &model.StoreInboxBinder{Store: s.store}
	if s.broker != nil {
		binder.Publish = s.broker.Publish
	}
	return binder
}

// StoreDir returns the on-disk store directory. Exposed so HTTP
// handlers can fall back to persisted run.log when the in-memory
// buffer is gone.
func (s *Service) StoreDir() string { return s.storeDir }

// StoreRoot returns the filesystem root the underlying RunStore
// operates on, or empty when the store has no filesystem (cloud
// stores). Used by the upload handlers to materialise a staging
// directory.
func (s *Service) StoreRoot() string {
	if s == nil || s.store == nil {
		return ""
	}
	return s.store.Root()
}

// HasEventSource reports whether an alternative event source has
// been wired (i.e. cloud mode). The WS handler keys its branch
// selection on this. Returns false for the default broker path.
func (s *Service) HasEventSource() bool { return s.eventSource != nil }

// SubscribeEventStream opens an eventstream.Source subscription
// when one is installed. Returns nil + a typed error when the
// service is in local broker mode — callers branch on HasEventSource
// before calling this.
func (s *Service) SubscribeEventStream(ctx context.Context, runID string, fromSeq int64) (EventStreamSubscription, error) {
	if s.eventSource == nil {
		return nil, errors.New("runview: no event source wired (local broker mode)")
	}
	return s.eventSource.Subscribe(ctx, runID, fromSeq)
}

// validatePathComponent delegates to store.SanitizePathComponent so
// the validation rules stay in lock-step with the storage layer.
func validatePathComponent(name, component string) error {
	return store.SanitizePathComponent(name, component)
}
