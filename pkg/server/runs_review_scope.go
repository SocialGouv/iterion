package server

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	gitlib "github.com/SocialGouv/iterion/pkg/git"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/workspacetrack"
)

// reviewScopeResponse is what a human gate shows the operator: every file
// changed since the previous gate, grouped by the node that changed it.
//
// The GROUP is presentation; the FILE LIST is the contract. Grouping uses
// per-node boundary refs, which only some node kinds record — a subbot, a
// fan-out branch or a compute node has none. So anything that cannot be
// attributed lands in the trailing group with an empty NodeID rather than
// being dropped: a reviewer approving this must never be shown less than
// what changed.
type reviewScopeResponse struct {
	RunID   string `json:"run_id"`
	GateSeq int    `json:"gate_seq"`
	// BaseRef/HeadRef bracket the range. For a worktree run they are git
	// refs; for an in-place run they are workspacetrack snapshot ids
	// (resolved server-side — never taken from the client).
	BaseRef string `json:"base_ref"`
	HeadRef string `json:"head_ref"`
	// Backend is "git" or "workspace" so the UI/diff path know which
	// loader to use. Omitted on unavailable responses.
	Backend string `json:"backend,omitempty"`
	// Available is false when no range could be resolved; Reason then says
	// why, in the operator's terms.
	Available bool               `json:"available"`
	Reason    string             `json:"reason,omitempty"`
	Groups    []reviewScopeGroup `json:"groups"`
	// TotalFiles is the size of the whole range, so the UI can show the
	// count without summing groups (they partition it).
	TotalFiles int `json:"total_files"`
}

// reviewScopeGroup is one node's contribution to the range.
type reviewScopeGroup struct {
	// NodeID is empty for the catch-all group.
	NodeID    string              `json:"node_id"`
	Label     string              `json:"label"`
	Iteration int                 `json:"iteration,omitempty"`
	Files     []gitlib.FileStatus `json:"files"`
}

// handleGetRunReviewScope serves GET /api/runs/{id}/review/scope.
//
// Optional ?gate=<n> selects a specific gate; the default is the latest,
// which is the one the operator is paused on.
func (s *Server) handleGetRunReviewScope(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	run, err := s.runs.LoadRunCtx(r.Context(), id)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "run not found: %v", err)
		return
	}
	gate := -1
	if raw := r.URL.Query().Get("gate"); raw != "" {
		n, cerr := strconv.Atoi(raw)
		if cerr != nil || n < 0 {
			s.httpErrorFor(w, r, http.StatusBadRequest, "invalid gate: %q", raw)
			return
		}
		gate = n
	}
	s.writeJSONFor(w, r, buildReviewScope(run, gate, s.workspaceTracker()))
}

// handleGetRunReviewDiff serves GET /api/runs/{id}/review/diff.
//
// The refs are resolved server-side from the gate number, never taken
// from the caller: they end up as arguments to git (or as snapshot ids),
// and a client-supplied ref is an injection surface for no benefit.
func (s *Server) handleGetRunReviewDiff(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	path := r.URL.Query().Get("path")
	if err := gitlib.ValidateRelPath(path); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid path: %v", err)
		return
	}
	run, err := s.runs.LoadRunCtx(r.Context(), id)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "run not found: %v", err)
		return
	}
	gate := -1
	if raw := r.URL.Query().Get("gate"); raw != "" {
		n, cerr := strconv.Atoi(raw)
		if cerr != nil || n < 0 {
			s.httpErrorFor(w, r, http.StatusBadRequest, "invalid gate: %q", raw)
			return
		}
		gate = n
	}
	tracker := s.workspaceTracker()
	scope := buildReviewScope(run, gate, tracker)
	if !scope.Available {
		s.httpErrorFor(w, r, http.StatusConflict, "no review range: %s", scope.Reason)
		return
	}
	if scope.Backend == "workspace" {
		payload, derr := diffReviewScopeWorkspace(tracker, run.ID, scope.BaseRef, scope.HeadRef, path)
		if derr != nil {
			s.httpErrorFor(w, r, http.StatusInternalServerError, "workspace diff: %v", derr)
			return
		}
		s.writeJSONFor(w, r, payload)
		return
	}
	payload, derr := gitlib.DiffBetween(run.WorkDir, scope.BaseRef, scope.HeadRef, path)
	if derr != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "git diff: %v", derr)
		return
	}
	s.writeJSONFor(w, r, payload)
}

// handleGetRunWorkspaceFile streams one path from the run's live workspace
// so the review panel can play audio/video and show images without going
// through the text-only /files/content or the 5 MiB review/diff cap.
//
// When the live path is missing (deleted file, finalized worktree), it
// falls back to the workspacetrack object for the HEAD of the current
// review gate so a paused review still has a player for versioned media.
func (s *Server) handleGetRunWorkspaceFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	relPath := r.PathValue("path")
	if id == "" || relPath == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id or file path")
		return
	}
	if err := gitlib.ValidateRelPath(relPath); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid path: %v", err)
		return
	}
	run, err := s.runs.LoadRunCtx(r.Context(), id)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "run not found: %v", err)
		return
	}

	// Prefer the live workspace — the operator is reviewing the state
	// they can still open on disk.
	if run.WorkDir != "" {
		abs, joinErr := safeJoinUnder(run.WorkDir, relPath)
		if joinErr == nil {
			f, openErr := os.Open(abs)
			if openErr == nil {
				defer f.Close()
				info, statErr := f.Stat()
				if statErr == nil && !info.IsDir() {
					serveWorkspaceFile(w, r, relPath, info, f)
					return
				}
				_ = f.Close()
			}
		}
	}

	// Fallback: content-addressed object from the latest (or requested)
	// review gate's head snapshot.
	tracker := s.workspaceTracker()
	if tracker == nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "file not found")
		return
	}
	gate := -1
	if raw := r.URL.Query().Get("gate"); raw != "" {
		n, cerr := strconv.Atoi(raw)
		if cerr != nil || n < 0 {
			s.httpErrorFor(w, r, http.StatusBadRequest, "invalid gate: %q", raw)
			return
		}
		gate = n
	}
	scope := buildReviewScope(run, gate, tracker)
	if !scope.Available || scope.HeadRef == "" {
		s.httpErrorFor(w, r, http.StatusNotFound, "file not found")
		return
	}
	headSnap, loadErr := tracker.Load(run.ID, scope.HeadRef)
	if loadErr != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "file not found")
		return
	}
	var hash string
	var size int64
	for _, e := range headSnap.Entries {
		if e.Path == relPath {
			hash, size = e.Hash, e.Size
			break
		}
	}
	if hash == "" {
		s.httpErrorFor(w, r, http.StatusNotFound, "file not found")
		return
	}
	// Stream when the tracker can (the filesystem one can): buffering the
	// blob would hold a multi-hundred-MB media export in the server heap
	// once per concurrent request, on the endpoint whose entire purpose is
	// playback. Fall back to the byte slice for trackers that cannot.
	setWorkspaceFileHeaders(w, r, relPath)
	if opener, ok := tracker.(interface {
		OpenObject(string) (*os.File, error)
	}); ok {
		if f, oerr := opener.OpenObject(hash); oerr == nil {
			defer f.Close()
			_ = size
			http.ServeContent(w, r, filepath.Base(relPath), headSnap.CreatedAt, f)
			return
		}
	}
	body, objErr := tracker.Object(hash)
	if objErr != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "file not found")
		return
	}
	// ServeContent, not a bare Write: this endpoint exists for media
	// playback, and a <video> seek issues Range requests. Writing the body
	// answers each with a 200 and the WHOLE file, so every scrub re-sends
	// it. ServeContent honours Range, sets Content-Length itself, and 304s
	// a repeat fetch.
	_ = size
	http.ServeContent(w, r, filepath.Base(relPath), headSnap.CreatedAt, bytes.NewReader(body))
}

func serveWorkspaceFile(w http.ResponseWriter, r *http.Request, relPath string, info os.FileInfo, f *os.File) {
	setWorkspaceFileHeaders(w, r, relPath)
	// ServeContent handles Range requests so large media can seek.
	http.ServeContent(w, r, filepath.Base(relPath), info.ModTime(), f)
}

// inlineWorkspaceTypes are the content types this endpoint will render
// in the browser. It exists to play back media in the review panel, so
// that is the whole list.
//
// Anything else — above all `.html` and `.svg`, which
// artifactFileContentType maps to text/html and image/svg+xml — is forced
// to a download. The handler accepts ANY path under the run workspace,
// not just the media the panel links, so serving script-bearing content
// inline would let a file an agent wrote (or one that ships in the repo
// under review) execute on the studio's own origin, where it can drive
// every unauthenticated local /api/... endpoint.
// Deliberately an allow-list of concrete types rather than an
// "image/*, audio/*, video/*" prefix rule: image/svg+xml is an image that
// executes script.
var inlineWorkspaceTypes = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/gif": true,
	"image/webp": true, "image/avif": true, "image/bmp": true,
	"audio/mpeg": true, "audio/ogg": true, "audio/wav": true,
	"audio/webm": true, "audio/flac": true, "audio/aac": true,
	"audio/mp4": true, "audio/x-wav": true,
	"video/mp4": true, "video/webm": true, "video/ogg": true,
	"video/quicktime": true,
	"text/plain":      true,
}

// setWorkspaceFileHeaders writes the content type and disposition shared
// by the live-file and snapshot-object paths.
func setWorkspaceFileHeaders(w http.ResponseWriter, r *http.Request, relPath string) {
	ct := artifactFileContentType(relPath)
	w.Header().Set("Content-Type", ct)
	// nosniff so a downloaded file cannot be re-interpreted as markup by
	// content sniffing regardless of the type we declare.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Compare on the media type alone — artifactFileContentType appends
	// `; charset=utf-8` to the textual ones.
	base := strings.ToLower(strings.TrimSpace(strings.SplitN(ct, ";", 2)[0]))
	disposition := "attachment"
	if inlineWorkspaceTypes[base] && r.URL.Query().Get("download") != "1" {
		disposition = "inline"
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`%s; filename=%q`, disposition, filepath.Base(relPath)))
}

// safeJoinUnder resolves rel under root and rejects any escape —
// including an escape THROUGH a symlink inside the workspace.
//
// The lexical check this replaces (Abs + string prefix) passed for a
// symlink whose own path sits under the workspace but whose target does
// not, and os.Open then followed it: `GET
// /api/runs/{id}/workspace-files/<link>` streamed back /etc/passwd,
// ~/.aws/credentials or ~/.iterion/secrets.json. Agents write freely into
// a run workspace, and the workspace may be a checkout of an untrusted
// repo, so planting that link is inside the threat model.
//
// safePathWithin (pkg/server/server_files.go) is the repo's audited
// boundary for exactly this class — introduced by the c9e18195 hardening
// and already used by every other run-file endpoint. This was the only
// one reimplementing containment.
func safeJoinUnder(root, rel string) (string, error) {
	if err := gitlib.ValidateRelPath(rel); err != nil {
		return "", err
	}
	return safePathWithin(root, rel)
}

// workspaceTracker returns the runview service's workspace tracker, or a
// freshly constructed one from the store dir when the service has none
// (tests, CLI). nil when versioning is disabled.
func (s *Server) workspaceTracker() workspacetrack.Tracker {
	if s == nil || s.runs == nil {
		return nil
	}
	if tr := s.runs.WorkspaceTracker(); tr != nil {
		return tr
	}
	return runviewWorkspaceTracker(s.runs.StoreDir())
}

// runviewWorkspaceTracker is a thin alias so tests can inject without
// importing runview's package-level helper through a cycle. The real
// construction lives in runview.WorkspaceTrackerFor.
var runviewWorkspaceTracker = func(storeDir string) workspacetrack.Tracker {
	if storeDir == "" {
		return nil
	}
	return workspacetrack.NewNative(storeDir)
}

// buildReviewScope resolves the gate range and partitions it by node.
//
// tracker is required for in-place runs (and is ignored for worktree
// runs, which use git refs). Passing nil for an in-place run yields an
// unavailable scope with a reason.
func buildReviewScope(run *store.Run, gate int, tracker workspacetrack.Tracker) *reviewScopeResponse {
	out := &reviewScopeResponse{RunID: run.ID, Groups: []reviewScopeGroup{}}
	if run.WorkDir == "" || !dirExists(run.WorkDir) {
		out.Reason = "this run has no workspace on this host — review ranges are recorded next to the run"
		return out
	}
	if run.Worktree {
		return buildReviewScopeGit(run, gate, out)
	}
	return buildReviewScopeWorkspace(run, gate, tracker, out)
}

func buildReviewScopeGit(run *store.Run, gate int, out *reviewScopeResponse) *reviewScopeResponse {
	gates := listReviewGates(run.WorkDir, run.ID)
	if len(gates) == 0 {
		out.Reason = "no review gate has been reached in this run yet"
		return out
	}
	if gate < 0 {
		gate = gates[len(gates)-1]
	}
	if !containsInt(gates, gate) {
		out.Reason = fmt.Sprintf("gate %d was never reached (recorded: %v)", gate, gates)
		return out
	}
	out.GateSeq = gate
	out.Backend = "git"
	out.HeadRef = store.ReviewGateRef(run.ID, gate)
	if gate > 0 {
		out.BaseRef = store.ReviewGateRef(run.ID, gate-1)
	} else {
		// First gate: the range starts where the run did.
		out.BaseRef = run.BaseCommit
	}
	if out.BaseRef == "" {
		out.Reason = "the run recorded no base commit, so the first gate has no range to compare against"
		return out
	}

	files, err := gitlib.StatusBetween(run.WorkDir, out.BaseRef, out.HeadRef)
	if err != nil {
		out.Reason = fmt.Sprintf("could not read the range: %v", err)
		return out
	}
	out.Available = true
	out.TotalFiles = len(files)
	out.Groups = groupByNodeGit(run, files, out.BaseRef, out.HeadRef)
	return out
}

func buildReviewScopeWorkspace(run *store.Run, gate int, tracker workspacetrack.Tracker, out *reviewScopeResponse) *reviewScopeResponse {
	if tracker == nil {
		out.Reason = "workspace versioning is disabled on this host, so in-place review ranges cannot be recorded"
		return out
	}
	gates := workspacetrack.ListGates(tracker, run.ID)
	var baseID, headID string
	switch {
	case len(gates) > 0:
		if gate < 0 {
			gate = gates[len(gates)-1]
		}
		if !containsInt(gates, gate) {
			out.Reason = fmt.Sprintf("gate %d was never reached (recorded: %v)", gate, gates)
			return out
		}
		out.GateSeq = gate
		headLabel := workspacetrack.GateLabel(gate)
		var ok bool
		headID, ok = tracker.Resolve(run.ID, headLabel)
		if !ok {
			out.Reason = fmt.Sprintf("gate %d is labelled but its snapshot is missing", gate)
			return out
		}
		if gate > 0 {
			baseID, ok = tracker.Resolve(run.ID, workspacetrack.GateLabel(gate-1))
			if !ok {
				out.Reason = fmt.Sprintf("previous gate %d has no snapshot", gate-1)
				return out
			}
		} else {
			baseID, ok = workspacetrack.Root(tracker, run.ID)
			if !ok {
				out.Reason = "the run has no initial workspace snapshot to compare the first gate against"
				return out
			}
		}
	default:
		// No explicit gate labels yet. For a run that is currently paused
		// waiting on a human — the only caller of this panel — fall back
		// to "everything since the run's first capture". That covers runs
		// that reached a human gate before markReviewGate started writing
		// gate:N labels, without inventing a range for a still-running
		// or finished run that never paused.
		if !run.Status.IsPaused() {
			out.Reason = "no review gate has been reached in this run yet"
			return out
		}
		headID = tracker.Head(run.ID)
		if headID == "" {
			out.Reason = "this run captured no workspace snapshots at all"
			return out
		}
		var ok bool
		baseID, ok = workspacetrack.Root(tracker, run.ID)
		if !ok {
			out.Reason = "this run captured no workspace snapshots at all"
			return out
		}
		out.GateSeq = 0
	}
	out.Backend = "workspace"
	out.BaseRef = baseID
	out.HeadRef = headID

	baseSnap, err := tracker.Load(run.ID, baseID)
	if err != nil {
		out.Reason = fmt.Sprintf("could not load base snapshot: %v", err)
		return out
	}
	headSnap, err := tracker.Load(run.ID, headID)
	if err != nil {
		out.Reason = fmt.Sprintf("could not load head snapshot: %v", err)
		return out
	}
	// When base and head are the same snapshot (first gate taken before
	// any node ran, or fallback with a single capture), the range is empty
	// rather than "every file in the workspace as an addition".
	var changes []workspacetrack.FileChange
	if baseID == headID {
		changes = nil
	} else {
		changes = workspacetrack.StatusBetween(baseSnap, headSnap)
	}
	files := workspaceChangesToFileStatus(changes)
	out.Available = true
	out.TotalFiles = len(files)
	out.Groups = groupByNodeWorkspace(tracker, run.ID, files, baseSnap, headSnap)
	return out
}

func workspaceChangesToFileStatus(changes []workspacetrack.FileChange) []gitlib.FileStatus {
	out := make([]gitlib.FileStatus, 0, len(changes))
	for _, c := range changes {
		fs := gitlib.FileStatus{Path: c.Path, Status: c.Status, Binary: c.Binary}
		switch {
		case c.Uncaptured:
			// Nothing was stored for this path, so there is no diff to
			// render and no counts to give. Listing it is the point: the
			// file most likely to exceed the capture cap is the media
			// export the run exists to produce.
			fs.CountsUnknown = true
			fs.Uncaptured = true
		case c.Binary:
			fs.Added, fs.Deleted = -1, -1
		default:
			// The tracker stores CONTENT, not diffs, and there is no git
			// here to ask for a numstat — so line counts are genuinely
			// unknown rather than zero. Saying so is the difference
			// between "no lines changed" and "we did not measure".
			fs.CountsUnknown = true
		}
		out = append(out, fs)
	}
	return out
}

func diffReviewScopeWorkspace(tracker workspacetrack.Tracker, runID, baseID, headID, path string) (gitlib.DiffPayload, error) {
	if tracker == nil {
		return gitlib.DiffPayload{}, fmt.Errorf("no workspace tracker")
	}
	var baseSnap, headSnap *workspacetrack.Snapshot
	var err error
	if baseID != "" {
		baseSnap, err = tracker.Load(runID, baseID)
		if err != nil {
			return gitlib.DiffPayload{}, err
		}
	}
	if headID != "" {
		headSnap, err = tracker.Load(runID, headID)
		if err != nil {
			return gitlib.DiffPayload{}, err
		}
	}
	d, err := workspacetrack.DiffBetween(tracker, baseSnap, headSnap, path)
	if err != nil {
		return gitlib.DiffPayload{}, err
	}
	return gitlib.DiffPayload{
		Path:      d.Path,
		Before:    d.Before,
		After:     d.After,
		Binary:    d.Binary,
		Oversized: d.Oversized,
	}, nil
}

// groupByNodeGit attributes each file in the range to the node that last
// changed it, using per-node git boundary refs.
//
// baseRef/headRef bound WHICH boundaries may claim a file. Without that
// bound, every boundary of the whole run competes: a file that `implement`
// wrote before gate/1 — already approved — and that a boundary-less writer
// (a subbot, a fan-out branch, a compute node) rewrote between gate/1 and
// gate/2 was still attributed to `implement` in the gate/2 panel, because
// its pre..post set contains that path. The reviewer is then told a
// specific node made a change it did not make in this range, and the
// change is hidden from *Other changes*, where the design says
// unattributable work belongs. Completeness held; attribution did not.
func groupByNodeGit(run *store.Run, files []gitlib.FileStatus, baseRef, headRef string) []reviewScopeGroup {
	lo, hi := refCommitTimes(run.WorkDir, baseRef, headRef)
	var ranges []nodeRange
	for _, b := range listNodeBoundaries(run.WorkDir, run.ID) {
		if !withinGateWindow(b.when, lo, hi) {
			continue
		}
		changed, err := gitlib.StatusBetween(run.WorkDir, b.preRef, b.postRef)
		if err != nil {
			continue
		}
		set := make(map[string]bool, len(changed))
		for _, f := range changed {
			set[f.Path] = true
		}
		ranges = append(ranges, nodeRange{node: b.node, loopIter: b.loopIter, when: b.when, set: set})
	}
	// Latest boundary wins: when two nodes touched the same file, the
	// reviewer cares about who left it in the state under review.
	//
	// SliceStable, not Slice: `when` is committerdate at ONE-SECOND
	// resolution and node boundaries routinely land in the same second, so
	// an unstable sort permutes the tied ones. partitionFilesByRanges then
	// awards a shared file to whichever came last, and with the panel
	// refetching the operator watches files hop between node groups with
	// nothing having changed. Stable sorting lets for-each-ref's own
	// deterministic order break the tie.
	sort.SliceStable(ranges, func(i, j int) bool { return ranges[i].when < ranges[j].when })
	return partitionFilesByRanges(files, ranges)
}

// groupByNodeWorkspace attributes files using workspacetrack pre:/post:
// labels. Same partition contract as the git path.
// baseSnap/headSnap bound WHICH boundaries may claim a file, for the same
// reason as the git path: without it, a node that touched a path in an
// already-approved range still out-competes *Other changes* for it here.
func groupByNodeWorkspace(tracker workspacetrack.Tracker, runID string, files []gitlib.FileStatus, baseSnap, headSnap *workspacetrack.Snapshot) []reviewScopeGroup {
	// Bound on snapshot IDs, not CreatedAt: newSnapshotID is
	// "<unixnano>-<counter>", monotonic by construction, so the ordering
	// is exact. CreatedAt is truncated to seconds and several boundaries
	// routinely share one, which makes a timestamp window either drop
	// legitimate boundaries or keep the ones it exists to exclude.
	baseID, headSnapID := "", ""
	if baseSnap != nil {
		baseID = baseSnap.ID
	}
	if headSnap != nil {
		headSnapID = headSnap.ID
	}
	labels := tracker.Labels(runID)
	// Collect nodes that have BOTH pre and post labels.
	type bound struct {
		node     string
		loopIter int
		preID    string
		postID   string
	}
	var bounds []bound
	for label, postID := range labels {
		// post:<node>:<iter>
		if !strings.HasPrefix(label, workspacetrack.PhasePost+":") {
			continue
		}
		rest := strings.TrimPrefix(label, workspacetrack.PhasePost+":")
		slash := strings.LastIndex(rest, ":")
		if slash < 0 {
			continue
		}
		node := rest[:slash]
		loopIter, err := strconv.Atoi(rest[slash+1:])
		if err != nil {
			continue
		}
		preID, ok := labels[workspacetrack.Label(workspacetrack.PhasePre, node, loopIter)]
		if !ok {
			continue
		}
		bounds = append(bounds, bound{node: node, loopIter: loopIter, preID: preID, postID: postID})
	}
	var ranges []nodeRange
	for _, b := range bounds {
		// Filter BEFORE the two manifest loads and the diff, as the git
		// twin already does: bounds holds every pre:/post: pair the run
		// ever recorded, each manifest lists the whole versioned workspace
		// (up to MaxFiles), and on a multi-gate run most pairs are outside
		// the range being shown. The predicate needs only the ids.
		if !snapshotWithinGate(b.postID, baseID, headSnapID) {
			continue
		}
		preSnap, err := tracker.Load(runID, b.preID)
		if err != nil {
			continue
		}
		postSnap, err := tracker.Load(runID, b.postID)
		if err != nil {
			continue
		}
		changed := workspacetrack.StatusBetween(preSnap, postSnap)
		set := make(map[string]bool, len(changed))
		for _, f := range changed {
			set[f.Path] = true
		}
		when := int64(0)
		if !postSnap.CreatedAt.IsZero() {
			when = postSnap.CreatedAt.Unix()
		}
		ranges = append(ranges, nodeRange{node: b.node, loopIter: b.loopIter, when: when, set: set})
	}
	// SliceStable for the same reason as the git path above: CreatedAt is
	// truncated to seconds here too.
	sort.SliceStable(ranges, func(i, j int) bool { return ranges[i].when < ranges[j].when })
	return partitionFilesByRanges(files, ranges)
}

type nodeRange struct {
	node     string
	loopIter int
	when     int64
	set      map[string]bool
}

func partitionFilesByRanges(files []gitlib.FileStatus, ranges []nodeRange) []reviewScopeGroup {
	byNode := map[string]*reviewScopeGroup{}
	order := []string{}
	var unattributed []gitlib.FileStatus
	for _, f := range files {
		owner, loopIter := "", 0
		for _, rg := range ranges {
			if rg.set[f.Path] {
				owner, loopIter = rg.node, rg.loopIter
			}
		}
		if owner == "" {
			unattributed = append(unattributed, f)
			continue
		}
		key := fmt.Sprintf("%s@%d", owner, loopIter)
		g, ok := byNode[key]
		if !ok {
			g = &reviewScopeGroup{NodeID: owner, Label: owner, Iteration: loopIter}
			byNode[key] = g
			order = append(order, key)
		}
		g.Files = append(g.Files, f)
	}

	groups := make([]reviewScopeGroup, 0, len(order)+1)
	for _, k := range order {
		groups = append(groups, *byNode[k])
	}
	if len(unattributed) > 0 {
		// Never dropped. A subbot, a fan-out branch or a compute node
		// records no boundary, so its work can only appear here — and a
		// reviewer approving the range must see it.
		groups = append(groups, reviewScopeGroup{
			Label: "Other changes (no per-node boundary recorded)",
			Files: unattributed,
		})
	}
	return groups
}

type nodeBoundary struct {
	node     string
	loopIter int
	preRef   string
	postRef  string
	when     int64
}

// refCommitTimes reads the commit timestamps bracketing the gate range.
// A zero bound means "unknown", which withinGateWindow treats as open —
// grouping is presentation, so a missing timestamp must not drop a file
// from its group, only widen who may claim it.
func refCommitTimes(workDir, baseRef, headRef string) (lo, hi int64) {
	read := func(ref string) int64 {
		if ref == "" {
			return 0
		}
		out, err := gitlib.ForEachRef(workDir, "%(committerdate:unix)", ref)
		if err != nil {
			return 0
		}
		n, _ := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
		return n
	}
	return read(baseRef), read(headRef)
}

// withinGateWindow reports whether a node boundary closed inside the gate
// range. Half-open at the base (a boundary AT the previous gate belongs to
// the range that gate closed) and inclusive at the head.
func withinGateWindow(when, lo, hi int64) bool {
	if when == 0 {
		return true // unknown: let it compete rather than drop the group
	}
	// Inclusive at the base: committerdate has one-second resolution, and
	// a boundary sharing the gate commit's second is genuinely ambiguous.
	// Erring toward attribution is the right error — the case this window
	// exists for (a boundary from an already-approved range) is many
	// seconds away, while excluding a same-second boundary would silently
	// dump real work into *Other changes*.
	if lo > 0 && when < lo {
		return false
	}
	if hi > 0 && when > hi {
		return false
	}
	return true
}

// snapshotWithinGate reports whether a node's closing snapshot falls in
// the gate range, comparing the monotonic "<unixnano>-<counter>" ids
// rather than their second-truncated timestamps. An unparseable or empty
// bound is treated as open, so a malformed id widens attribution rather
// than dropping a group.
func snapshotWithinGate(postID, baseID, headID string) bool {
	nano := func(id string) (int64, bool) {
		dash := strings.IndexByte(id, '-')
		if dash <= 0 {
			return 0, false
		}
		n, err := strconv.ParseInt(id[:dash], 10, 64)
		return n, err == nil
	}
	p, ok := nano(postID)
	if !ok {
		return true
	}
	if b, ok := nano(baseID); ok && p <= b {
		return false
	}
	if h, ok := nano(headID); ok && p > h {
		return false
	}
	return true
}

// listNodeBoundaries enumerates the nodes that recorded BOTH boundaries,
// with the commit time of the post ref for ordering.
func listNodeBoundaries(workDir, runID string) []nodeBoundary {
	prefix := fmt.Sprintf("refs/iterion/runs/%s/nodes/", runID)
	out, err := gitlib.ForEachRef(workDir, "%(refname) %(committerdate:unix)", prefix)
	if err != nil {
		return nil
	}
	var boundaries []nodeBoundary
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 2 {
			continue
		}
		rest := strings.TrimPrefix(fields[0], prefix)
		slash := strings.LastIndex(rest, "/")
		if slash < 0 {
			continue
		}
		node := rest[:slash]
		loopIter, cerr := strconv.Atoi(rest[slash+1:])
		if cerr != nil {
			continue
		}
		when, _ := strconv.ParseInt(fields[1], 10, 64)
		boundaries = append(boundaries, nodeBoundary{
			node:     node,
			loopIter: loopIter,
			preRef:   store.NodePreSnapshotRef(runID, node, loopIter),
			postRef:  store.NodeSnapshotRef(runID, node, loopIter),
			when:     when,
		})
	}
	return boundaries
}

// listReviewGates returns the gate numbers recorded for a run, ascending.
func listReviewGates(workDir, runID string) []int {
	prefix := strings.TrimSuffix(store.ReviewGateRef(runID, 0), "0")
	out, err := gitlib.ForEachRef(workDir, "%(refname)", prefix)
	if err != nil {
		return nil
	}
	var seqs []int
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if n, cerr := strconv.Atoi(line[strings.LastIndex(line, "/")+1:]); cerr == nil {
			seqs = append(seqs, n)
		}
	}
	sort.Ints(seqs)
	return seqs
}

func containsInt(xs []int, want int) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
