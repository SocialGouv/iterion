package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// globalActiveRun is the wire shape of GET /api/runs/global-active.
//
// Mirrors runview.RunSummary's most-relevant fields, with two extras:
//   - StorePath: the iterion store root the run lives in. Lets the
//     desktop app surface "this run is in project X" without picking
//     the project itself open.
//   - WorkspaceDir: best-effort guess at the project workdir, derived
//     from the store path. Empty for the global ~/.iterion/runs/ slot.
//
// Status is always one of the active values (running, queued, paused).
// Inactive runs are filtered out at scan time so the response stays
// small even with thousands of historical runs on disk.
type globalActiveRun struct {
	ID                string          `json:"id"`
	Name              string          `json:"name,omitempty"`
	ParentRunID       string          `json:"parent_run_id,omitempty"`
	WorkflowName      string          `json:"workflow_name"`
	BundleName        string          `json:"bundle_name,omitempty"`
	BundleDisplayName string          `json:"bundle_display_name,omitempty"`
	InputPath         string          `json:"input_path,omitempty"`
	Status            store.RunStatus `json:"status"`
	CreatedAt         time.Time       `json:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at"`
	StorePath         string          `json:"store_path"`
	WorkspaceDir      string          `json:"workspace_dir,omitempty"`
}

// handleListGlobalActiveRuns serves GET /api/runs/global-active. It
// scans every iterion store the daemon can see on the local
// filesystem (the global ~/.iterion/runs/ slot, every per-project
// slot under ~/.iterion/projects/*/runs/) and returns the runs whose
// status is currently active.
//
// Read-only on JSON files; no locking required because run.json is
// rewritten atomically by the producing process. A partially-written
// or corrupt file is skipped silently — better to under-report one
// run than to fail the whole listing.
//
// Concurrency: serial filepath.Walk inside a single request. Sub-
// second on the typical ~10-50 stored runs. Wire an in-process LRU
// + inotify watcher if this grows past a few hundred runs.
func (s *Server) handleListGlobalActiveRuns(w http.ResponseWriter, r *http.Request) {
	// This endpoint is a desktop-daemon affordance: it walks the local
	// $HOME/.iterion/** filesystem looking for active runs across every
	// project the user has on this machine. In cloud mode the server
	// pod's $HOME is shared infrastructure that may contain runs from
	// other tenants (or the cloud store's local mirror), so we refuse
	// here rather than risk a cross-tenant leak.
	if s.cfg.Mode == "cloud" {
		s.writeJSONFor(w, r, map[string]any{"runs": []globalActiveRun{}})
		return
	}
	roots, err := globalStoreRoots()
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "resolve global stores: %v", err)
		return
	}

	out := make([]globalActiveRun, 0)
	for _, root := range roots {
		runsDir := filepath.Join(root, "runs")
		entries, readErr := os.ReadDir(runsDir)
		if readErr != nil {
			// Missing or unreadable: skip (the slot might exist
			// without any runs yet, or the user removed it).
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			runJSON := filepath.Join(runsDir, e.Name(), "run.json")
			data, rdErr := os.ReadFile(runJSON)
			if rdErr != nil {
				continue
			}
			var rec runJSONShape
			if jerr := json.Unmarshal(data, &rec); jerr != nil {
				continue
			}
			if !isGlobalDiscoverableStatus(rec.Status) {
				continue
			}
			// Stale-orphan filter: a run.json with status=running whose
			// events.jsonl hasn't been touched in staleRunCutoff is
			// almost certainly an orphan
			// — the owning test/CLI process died without
			// UpdateRunStatus ever firing, so the status is frozen
			// even though nothing is driving it. Showing these in the
			// banner is noise (operator clicks → cross-store load →
			// "running" snapshot that never advances) and the user
			// flagged it as confusing. We filter them out without
			// mutating run.json so the orphans remain visible to
			// `iterion inspect --store-dir <path>` for forensics.
			//
			// Heartbeat = events.jsonl mtime. The engine appends
			// events continuously while alive (tool_started /
			// tool_called / llm_request / …); once dead, mtime
			// freezes. run.json's updated_at fires only on
			// UpdateRunStatus (sparse — mostly terminal transitions)
			// so it's not a reliable liveness signal.
			if globalRunNeedsHeartbeat(rec.Status) {
				evPath := runEventsFilenameForStore(root, rec.ID)
				if st, statErr := os.Stat(evPath); statErr == nil {
					if time.Since(st.ModTime()) > staleRunCutoff {
						continue
					}
				}
			}
			out = append(out, globalActiveRun{
				ID:                rec.ID,
				Name:              rec.Name,
				ParentRunID:       rec.ParentRunID,
				WorkflowName:      rec.WorkflowName,
				BundleName:        rec.BundleName,
				BundleDisplayName: rec.BundleDisplayName,
				InputPath:         runJSONInputString(rec.Inputs, "input_path"),
				Status:            rec.Status,
				CreatedAt:         rec.CreatedAt,
				UpdatedAt:         rec.UpdatedAt,
				StorePath:         root,
				WorkspaceDir:      workspaceDirForGlobalRun(root, rec),
			})
		}
	}

	// Newest first so the operator's most recent activity is at the
	// top of the list — typically what they want to jump to.
	sort.Slice(out, func(i, j int) bool {
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})

	s.writeJSONFor(w, r, map[string]any{"runs": out})
}

// runJSONShape is the subset of run.json this endpoint reads. Defined
// locally rather than importing pkg/store's Run struct to avoid
// pulling its lifecycle helpers into a read-only path; if the JSON
// shape evolves, the worst case is a field falling back to its zero
// value here.
type runJSONShape struct {
	ID                string          `json:"id"`
	Name              string          `json:"name"`
	ParentRunID       string          `json:"parent_run_id"`
	WorkflowName      string          `json:"workflow_name"`
	BundleName        string          `json:"bundle_name"`
	BundleDisplayName string          `json:"bundle_display_name"`
	Inputs            map[string]any  `json:"inputs"`
	Status            store.RunStatus `json:"status"`
	CreatedAt         time.Time       `json:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at"`
	WorkDir           string          `json:"work_dir"`
	RepoRoot          string          `json:"repo_root"`
}

func runJSONInputString(inputs map[string]any, key string) string {
	if inputs == nil {
		return ""
	}
	value, _ := inputs[key].(string)
	return strings.TrimSpace(value)
}

// isGlobalDiscoverableStatus includes both executing work and durable states
// an operator can resume or answer from another store. Individual surfaces
// deliberately apply narrower filters (for example, Home only highlights
// running work and /pipelines excludes resumable failures).
func isGlobalDiscoverableStatus(s store.RunStatus) bool {
	switch s {
	case store.RunStatusRunning,
		store.RunStatusPausedWaitingHuman,
		store.RunStatusPausedOperator,
		store.RunStatusQueued,
		store.RunStatusFailedResumable:
		return true
	}
	return false
}

// workspaceDirForGlobalRun prefers the workspace identity persisted by the
// runtime over the lossy reverse of an encoded store key. Keys replace both
// path separators and literal hyphens with "-", so decoding them cannot
// faithfully recover paths such as "town-vertical-pipeline". Worktree runs
// point back to their source repository through RepoRoot; ordinary runs use
// WorkDir. Legacy records fall back to the historical best-effort decoder.
func workspaceDirForGlobalRun(storePath string, rec runJSONShape) string {
	if root := strings.TrimSpace(rec.RepoRoot); root != "" {
		return filepath.Clean(root)
	}
	if dir := strings.TrimSpace(rec.WorkDir); dir != "" {
		return filepath.Clean(dir)
	}
	return workspaceDirForStore(storePath)
}

// staleRunCutoff is the maximum gap between events.jsonl mtime and "now"
// before an executing run is treated as an orphan. Live runs flush events at
// minute-scale during normal work (every tool_started / tool_called /
// llm_request). Paused and queued runs are intentionally quiet and must remain
// discoverable regardless of age.
const staleRunCutoff = 15 * time.Minute

func globalRunNeedsHeartbeat(status store.RunStatus) bool {
	return status == store.RunStatusRunning
}

// runEventsFilenameForStore mirrors store.FilesystemRunStore's events
// path layout WITHOUT importing the store package's internals.
func runEventsFilenameForStore(root, runID string) string {
	return filepath.Join(root, "runs", runID, "events.jsonl")
}

// globalStoreRoots returns every iterion store directory the daemon
// should scan for cross-folder runs. Always includes:
//
//   - $HOME/.iterion        (the "loose" / no-project slot)
//   - $HOME/.iterion/projects/<key>/  (every per-project store)
//
// Missing directories produce no error — the deeper read picks them
// up empty.
func globalStoreRoots() ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil, nil
	}
	iterionHome := filepath.Join(home, ".iterion")
	roots := []string{iterionHome}

	projectsDir := filepath.Join(iterionHome, "projects")
	entries, err := os.ReadDir(projectsDir)
	if err == nil {
		for _, e := range entries {
			if e.IsDir() {
				roots = append(roots, filepath.Join(projectsDir, e.Name()))
			}
		}
	}
	return roots, nil
}

// workspaceDirForStore turns a per-project store path back into the
// original workspace dir (best-effort, for display only). The
// encoding in pkg/store/storedir.go replaces "/" with "-", so we
// reverse that here. Empty string when the path isn't a per-project
// slot (e.g. the global ~/.iterion root).
func workspaceDirForStore(storePath string) string {
	key := filepath.Base(storePath)
	parent := filepath.Base(filepath.Dir(storePath))
	if parent != "projects" {
		return ""
	}
	// Encoding is leading "-" then "/" → "-". Defensive reverse:
	// trim the leading marker, then replace "-" with "/".
	if !strings.HasPrefix(key, "-") {
		return ""
	}
	return "/" + strings.ReplaceAll(strings.TrimPrefix(key, "-"), "-", "/")
}
