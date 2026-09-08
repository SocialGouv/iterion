package runview

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// ListArtifacts enumerates the persisted artifacts for one node.
//
// Uses context.Background with the mongo tenant filter bypassed — it does
// NOT carry caller identity. Use ListArtifactsCtx from cloud HTTP handlers
// so the tenant_id filter applies.
func (s *Service) ListArtifacts(runID, nodeID string) ([]ArtifactSummary, error) {
	return s.ListArtifactsCtx(store.WithoutTenantFilter(context.Background()), runID, nodeID)
}

// ListArtifactsCtx is the tenant-aware variant of ListArtifacts. It reads
// the node's artifact directory directly when it is on this host — which
// avoids the O(versions) JSON-decode of the full bodies that LoadArtifact
// would do just to extract the version number — and otherwise asks the
// store, the case of a cloud server pod (the directory lives on the runner
// that wrote it). Returns the versions in ascending order.
func (s *Service) ListArtifactsCtx(ctx context.Context, runID, nodeID string) ([]ArtifactSummary, error) {
	if err := validatePathComponent("run ID", runID); err != nil {
		return nil, err
	}
	if err := validatePathComponent("node ID", nodeID); err != nil {
		return nil, err
	}
	dir := filepath.Join(s.storeDir, "runs", runID, "artifacts", nodeID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return s.listArtifactVersionsFromStore(ctx, runID, nodeID)
		}
		return nil, fmt.Errorf("runview: list artifacts: %w", err)
	}
	out := make([]ArtifactSummary, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		v, parseErr := strconv.Atoi(strings.TrimSuffix(name, ".json"))
		if parseErr != nil {
			continue
		}
		info, statErr := e.Info()
		if statErr != nil {
			continue
		}
		out = append(out, ArtifactSummary{Version: v, WrittenAt: info.ModTime().UTC()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// listArtifactVersionsFromStore asks the store for a node's persisted
// versions when the artifact directory is not on this host — the same
// condition that sends the aggregate listing to the artifact index.
//
// Without it the per-node drill-in behind every card of that listing came
// back empty, and the studio's version picker falls back to v1 for both
// selectors (ArtifactDiff.tsx: `sorted[0]?.version ?? 1`). The engine's
// first version is v0, so the common case answered 404 on expand and a
// node whose latest is v3 rendered a stale v1. An empty version list here
// means the node published nothing; a store failure is an error, never an
// empty listing.
func (s *Service) listArtifactVersionsFromStore(ctx context.Context, runID, nodeID string) ([]ArtifactSummary, error) {
	versions, err := s.store.ListArtifactVersions(ctx, runID, nodeID)
	if err != nil {
		return nil, fmt.Errorf("runview: list artifacts: store versions: %w", err)
	}
	out := make([]ArtifactSummary, 0, len(versions))
	for _, v := range versions {
		out = append(out, ArtifactSummary{Version: v.Version, WrittenAt: v.WrittenAt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// RunArtifactSummary describes the latest published artifact for one node,
// for the studio's centralized, label-grouped artifact view. Title is a
// short human label derived from the artifact data (a `title`/`name`
// field) when present, else empty (the studio falls back to the node id).
type RunArtifactSummary struct {
	NodeID    string    `json:"node_id"`
	Version   int       `json:"version"`
	Labels    []string  `json:"labels,omitempty"`
	Title     string    `json:"title,omitempty"`
	WrittenAt time.Time `json:"written_at"`
}

// ListAllArtifacts enumerates the latest published artifact per node for a
// run — the data behind the centralized Artifacts view.
//
// Uses context.Background with the mongo tenant filter explicitly
// bypassed — it does NOT carry caller identity. The bypass is load-bearing,
// not decoration: the index fallback below goes through LoadRun, whose
// mongo implementation PANICS on a context carrying neither a tenant nor
// this marker. Use ListAllArtifactsCtx from cloud HTTP handlers so the
// tenant_id filter applies instead.
func (s *Service) ListAllArtifacts(runID string) ([]RunArtifactSummary, error) {
	return s.ListAllArtifactsCtx(store.WithoutTenantFilter(context.Background()), runID)
}

// ListAllArtifactsCtx is the tenant-aware variant of ListAllArtifacts. It
// walks runs/<id>/artifacts/*/ when the run's artifact directory is on this
// host, and otherwise serves the run document's artifact_index (the case
// of a cloud server pod: the directory lives on the runner that wrote it).
// Each node's latest version is loaded to surface its labels + title.
// Sorted by node id for stable rendering. Few artifacts per run, so the
// per-node body read is cheap.
func (s *Service) ListAllArtifactsCtx(ctx context.Context, runID string) ([]RunArtifactSummary, error) {
	if err := validatePathComponent("run ID", runID); err != nil {
		return nil, err
	}
	root := filepath.Join(s.storeDir, "runs", runID, "artifacts")
	nodes, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return s.listAllArtifactsFromIndex(ctx, runID)
		}
		return nil, fmt.Errorf("runview: list artifacts: %w", err)
	}
	out := make([]RunArtifactSummary, 0, len(nodes))
	for _, n := range nodes {
		if !n.IsDir() {
			continue
		}
		nodeID := n.Name()
		versions, verr := s.ListArtifactsCtx(ctx, runID, nodeID)
		if verr != nil || len(versions) == 0 {
			continue
		}
		latest := versions[len(versions)-1]
		art, lerr := s.LoadArtifactCtx(ctx, runID, nodeID, latest.Version)
		if lerr != nil || art == nil {
			continue
		}
		out = append(out, RunArtifactSummary{
			NodeID:    nodeID,
			Version:   latest.Version,
			Labels:    art.Labels,
			Title:     artifactTitle(art.Data),
			WrittenAt: latest.WrittenAt,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out, nil
}

// listAllArtifactsFromIndex serves the listing from run.ArtifactIndex
// (node id → latest version), which every store maintains on WriteArtifact.
// Reached when no artifact directory exists on this host. A run that is
// absent — never existed, or tombstoned — is an empty list, like the
// directory walk; any other store failure is an error, not an empty
// listing — on the cloud pod this fallback exists for, a transient outage
// must not read as "this run published nothing".
//
// The same reasoning one level down: an index entry is only ever written
// AFTER the body was successfully persisted, so a body that will not load
// is an outage or a lifecycle deletion, never a node that published
// nothing. Dropping it would serve a partial listing as an authoritative
// one — worse than the empty listing this fallback replaced, because it
// looks complete. The entry is emitted DEGRADED instead (node id + the
// indexed version, no labels or title) and logged: the studio already
// falls back to the node id for a missing title, so the card renders and
// still opens on the per-node endpoint, which reads the body itself.
// Returning the error is not an option here — the mongo store answers an
// untyped error for a body that is genuinely gone, so one lifecycle-
// deleted blob would take down the whole Artifacts view for that run.
//
// ctx is the caller's, so the mongo tenant filter scopes the LoadRun.
func (s *Service) listAllArtifactsFromIndex(ctx context.Context, runID string) ([]RunArtifactSummary, error) {
	run, err := s.store.LoadRun(ctx, runID)
	if err != nil {
		if store.RunAbsent(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("runview: list artifacts: load run: %w", err)
	}
	out := make([]RunArtifactSummary, 0, len(run.ArtifactIndex))
	for nodeID, version := range run.ArtifactIndex {
		entry := RunArtifactSummary{NodeID: nodeID, Version: version}
		art, lerr := s.store.LoadArtifact(ctx, runID, nodeID, version)
		if lerr != nil || art == nil {
			// A cancelled or expired context is not a broken body: every
			// remaining node would degrade for a reason that has nothing
			// to do with what this run published, and a wholly degraded
			// listing is the same lie about the store's state that the
			// silent drop was. Surface it as the outage it is.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, fmt.Errorf("runview: list artifacts: %w", ctxErr)
			}
			s.logger.Warn("runview: run %s node %s v%d is in the artifact index but its body did not load, listing it degraded: %v",
				runID, nodeID, version, lerr)
		} else {
			entry.Labels = art.Labels
			entry.Title = artifactTitle(art.Data)
			entry.WrittenAt = art.WrittenAt
		}
		if entry.WrittenAt.IsZero() {
			entry.WrittenAt = s.artifactWrittenAt(ctx, runID, nodeID, version)
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out, nil
}

// artifactWrittenAt recovers a published artifact's timestamp when its own
// body does not carry one.
//
// Only the FILESYSTEM store stamps Artifact.WrittenAt, and it does so on
// write; the mongo store marshals the artifact exactly as handed to it, and
// no engine writer sets the field. So on a cloud pod every entry of this
// listing would otherwise serve `"written_at":"0001-01-01T00:00:00Z"` — an
// epoch date for API clients, and a meaningless key for any date sort.
//
// The store's ListArtifactVersions is the store-agnostic answer: mongo
// folds the artifact_written events, whose ts is real, so this also repairs
// artifacts already written rather than only future ones. Best-effort — a
// lookup failure leaves the zero time rather than failing a listing that is
// otherwise complete. Guarded by the IsZero check at the call site, so the
// filesystem path never pays for it.
func (s *Service) artifactWrittenAt(ctx context.Context, runID, nodeID string, version int) time.Time {
	versions, err := s.store.ListArtifactVersions(ctx, runID, nodeID)
	if err != nil {
		s.logger.Warn("runview: run %s node %s v%d: version lookup for a missing timestamp failed: %v", runID, nodeID, version, err)
		return time.Time{}
	}
	for _, v := range versions {
		if v.Version == version {
			return v.WrittenAt
		}
	}
	return time.Time{}
}

// artifactTitle picks a short human title from artifact data, or "".
func artifactTitle(data map[string]any) string {
	for _, k := range []string{"title", "name"} {
		if s, ok := data[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// LoadArtifact returns one persisted artifact body.
//
// Uses context.Background — does NOT carry caller identity. Use
// LoadArtifactCtx from cloud HTTP handlers so the mongo tenant_id
// filter applies (cross-tenant LoadArtifact today leaks bodies).
func (s *Service) LoadArtifact(runID, nodeID string, version int) (*store.Artifact, error) {
	return s.store.LoadArtifact(context.Background(), runID, nodeID, version)
}

// LoadArtifactCtx is the tenant-aware variant of LoadArtifact.
func (s *Service) LoadArtifactCtx(ctx context.Context, runID, nodeID string, version int) (*store.Artifact, error) {
	return s.store.LoadArtifact(ctx, runID, nodeID, version)
}

// ListArtifactFiles enumerates the tool-produced files dropped under
// runs/<id>/artifact_files by in-sandbox tools (write_audit_md,
// emit_sbom, …). Returns nil when the store doesn't satisfy
// RunFilesStore (cloud mode) so the HTTP handler can surface an empty
// list cleanly without leaking the backend choice. Validates the run
// ID before delegating, mirroring ListArtifacts.
func (s *Service) ListArtifactFiles(runID string) ([]store.RunFileInfo, error) {
	return s.ListArtifactFilesCtx(context.Background(), runID)
}

// ListArtifactFilesCtx is the tenant-aware variant of ListArtifactFiles.
func (s *Service) ListArtifactFilesCtx(ctx context.Context, runID string) ([]store.RunFileInfo, error) {
	if err := validatePathComponent("run ID", runID); err != nil {
		return nil, err
	}
	rfs := store.AsRunFilesStore(s.store)
	if rfs == nil {
		return nil, nil
	}
	return rfs.ListRunFiles(ctx, runID)
}

// OpenArtifactFile streams one tool-produced file from the run's
// artifact_files area. Path-traversal protection lives in
// store.OpenRunFile (caller-side defence); the runview wrapper only
// validates the run-id component and delegates. Returns a nil reader
// when the store doesn't satisfy RunFilesStore.
func (s *Service) OpenArtifactFile(runID, relPath string) (io.ReadCloser, store.RunFileInfo, error) {
	return s.OpenArtifactFileCtx(context.Background(), runID, relPath)
}

// OpenArtifactFileCtx is the tenant-aware variant of OpenArtifactFile.
func (s *Service) OpenArtifactFileCtx(ctx context.Context, runID, relPath string) (io.ReadCloser, store.RunFileInfo, error) {
	if err := validatePathComponent("run ID", runID); err != nil {
		return nil, store.RunFileInfo{}, err
	}
	rfs := store.AsRunFilesStore(s.store)
	if rfs == nil {
		return nil, store.RunFileInfo{}, fmt.Errorf("runview: artifact files unavailable for this store")
	}
	return rfs.OpenRunFile(ctx, runID, relPath)
}

// ListPlanSnapshots returns the chronological plan snapshots captured for
// a run (agents' TodoWrite/todo_write living TODO lists — filesystem
// runs/<id>/plans/ or the Mongo run_plans collection). Returns nil when
// the store doesn't satisfy PlanStore so the HTTP handler surfaces a clean
// empty list without leaking the backend choice — mirroring
// ListArtifactFiles. Ascending Seq order (chronological): the sequence
// shows how the plan evolved.
func (s *Service) ListPlanSnapshots(runID string) ([]store.PlanSnapshot, error) {
	return s.ListPlanSnapshotsCtx(context.Background(), runID)
}

// ListPlanSnapshotsCtx is the tenant-aware variant of ListPlanSnapshots.
func (s *Service) ListPlanSnapshotsCtx(ctx context.Context, runID string) ([]store.PlanSnapshot, error) {
	if err := validatePathComponent("run ID", runID); err != nil {
		return nil, err
	}
	ps := store.AsPlanStore(s.store)
	if ps == nil {
		return nil, nil
	}
	return ps.ListPlanSnapshots(ctx, runID)
}

// ListRunNotesCtx returns the run's freeform operator notes in
// chronological order (filesystem runs/<id>/notes/ or the Mongo
// run_notes collection). Returns nil when the store doesn't satisfy
// RunNoteStore so the HTTP handler surfaces a clean empty list without
// leaking the backend choice — mirroring ListPlanSnapshotsCtx.
func (s *Service) ListRunNotesCtx(ctx context.Context, runID string) ([]store.RunNote, error) {
	if err := validatePathComponent("run ID", runID); err != nil {
		return nil, err
	}
	ns := store.AsRunNoteStore(s.store)
	if ns == nil {
		return nil, nil
	}
	return ns.ListRunNotes(ctx, runID)
}

// AddRunNoteCtx appends a freeform operator note (author + body) to the
// run and returns the persisted note with its seq + timestamp populated.
// author may be empty (the handler defaults it from the caller identity).
// Returns an error when the store doesn't back the note seam so the
// caller can surface a clear "not supported" rather than silently
// dropping the note.
func (s *Service) AddRunNoteCtx(ctx context.Context, runID, author, body string) (store.RunNote, error) {
	if err := validatePathComponent("run ID", runID); err != nil {
		return store.RunNote{}, err
	}
	if strings.TrimSpace(body) == "" {
		return store.RunNote{}, fmt.Errorf("runview: note body is required")
	}
	ns := store.AsRunNoteStore(s.store)
	if ns == nil {
		return store.RunNote{}, fmt.Errorf("runview: this store does not support run notes")
	}
	return ns.AppendRunNote(ctx, runID, store.RunNote{Author: author, Body: body})
}

// GetRunTagsCtx returns the run's operator-assigned tags (filter/group
// chips shown in the studio run header). Returns an empty slice — never
// nil — when the store doesn't satisfy RunTagStore or the run has none, so
// the HTTP surface serves a clean empty list without leaking the backend
// choice, mirroring ListPlanSnapshotsCtx.
func (s *Service) GetRunTagsCtx(ctx context.Context, runID string) ([]string, error) {
	if err := validatePathComponent("run ID", runID); err != nil {
		return nil, err
	}
	ts := store.AsRunTagStore(s.store)
	if ts == nil {
		return []string{}, nil
	}
	return ts.GetRunTags(ctx, runID)
}

// SetRunTagsCtx replaces the run's full tag set. tags must already be
// normalized (see store.NormalizeTags). Returns a clear "unavailable"
// error when the store doesn't persist tags — both the filesystem and
// Mongo stores satisfy RunTagStore, so this only fires for a degenerate
// store, which the PUT handler maps to a 500.
func (s *Service) SetRunTagsCtx(ctx context.Context, runID string, tags []string) error {
	if err := validatePathComponent("run ID", runID); err != nil {
		return err
	}
	ts := store.AsRunTagStore(s.store)
	if ts == nil {
		return fmt.Errorf("runview: run tags unavailable for this store")
	}
	return ts.SetRunTags(ctx, runID, tags)
}

// ReadToolBlob streams a slice of a tool's stored I/O body (sidecar
// blob written by the hooks layer when the call exceeded the inline
// threshold). offset is the byte offset to start at; limit caps the
// bytes returned (0 = "all from offset"). Returns the bytes read, the
// full blob size, eof when offset+len(data) == total, and an error
// wrapping os.ErrNotExist when the blob doesn't exist.
//
// Returns a clear "unavailable" error when the store doesn't satisfy
// ToolBlobStore. Both the filesystem and Mongo (cloud) stores satisfy it;
// for any store that does not, the hooks layer falls back to inline-only
// persistence, so the studio doesn't issue the fetch.
func (s *Service) ReadToolBlob(runID, toolUseID, kind string, offset, limit int64) ([]byte, int64, bool, error) {
	return s.ReadToolBlobCtx(context.Background(), runID, toolUseID, kind, offset, limit)
}

// ReadToolBlobCtx is the tenant-aware variant of ReadToolBlob.
func (s *Service) ReadToolBlobCtx(ctx context.Context, runID, toolUseID, kind string, offset, limit int64) ([]byte, int64, bool, error) {
	if err := validatePathComponent("run ID", runID); err != nil {
		return nil, 0, false, err
	}
	tbs := store.AsToolBlobStore(s.store)
	if tbs == nil {
		return nil, 0, false, fmt.Errorf("runview: tool blobs unavailable for this store")
	}
	return tbs.ReadToolBlob(ctx, runID, toolUseID, kind, offset, limit)
}
