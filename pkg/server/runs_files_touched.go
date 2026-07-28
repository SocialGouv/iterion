package server

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/store"
)

// This file implements GET /api/runs/{id}/files/touched — the list of
// workspace files the run's LLM nodes wrote or edited, derived from the
// persisted tool_started events rather than from git.
//
// The git-based /files views answer "what changed in this working
// directory", which is only equal to "what did the run produce" when the
// run executed in an isolated worktree. For an in-place run (worktree:
// none, or a non-git workspace) `git status` also reports the operator's
// own pre-existing dirty files, and files the run already committed drop
// out of it entirely. This endpoint answers the narrower, attribution-
// aware question: which paths did the nodes actually write, and which
// node wrote each one. The studio's pipeline-board "Produced elements"
// panel intersects/annotates the git view with it.
//
// Known under-report, by design: files created by Bash commands, by
// direct tool nodes (shell, no per-file tracking), or by CLI-agent
// backends that stream no tool events (kimi) are invisible here — for
// worktree runs the git channel still catches them; for in-place runs
// missing those beats listing the operator's whole git status.

// writeToolPathKeys maps each write-capable tool name to the ordered
// input keys that carry the target path (first non-empty string wins).
// Two namespaces coexist: CamelCase for the Claude Code SDK, snake_case
// for claw-code-go's built-ins — kept in sync with the canonical
// dispatch maps in pkg/backend/tooldisplay (CamelCaseKeys/SnakeCaseKeys),
// restricted to the tools that mutate files. Read-only tools (Read,
// read_file, Glob, …) and Bash are deliberately absent.
var writeToolPathKeys = map[string][]string{
	// Claude Code SDK tool names.
	"Write":        {"file_path"},
	"Edit":         {"file_path"},
	"MultiEdit":    {"file_path"},
	"NotebookEdit": {"notebook_path", "file_path"},
	// claw-code-go built-ins.
	"write_file":    {"path", "file_path"},
	"file_edit":     {"path", "file_path"},
	"notebook_edit": {"path", "file_path", "notebook_path"},
	// pi's built-ins: bare verbs taking `path`. Without them the studio's
	// "Produced elements" panel is EMPTY for every pi run that edits files —
	// the run looks like it changed nothing.
	"write": {"path", "file_path"},
	"edit":  {"path", "file_path"},
}

// touchedBlobReadCap bounds how much of a sidecar input blob we read when
// the inline 4 KB preview didn't carry the path key. Both SDKs emit the
// path as the first property, so this fallback is nearly never taken; the
// cap keeps a pathological multi-MB input from being slurped whole.
const touchedBlobReadCap = 1 << 20

// touchedFile is one file written/edited by the run's LLM nodes.
type touchedFile struct {
	// Path is workdir-relative when the tool wrote inside run.WorkDir
	// (matching the git /files listing), absolute otherwise.
	Path string `json:"path"`
	// NodeIDs lists the workflow nodes that wrote this path, in
	// first-write order.
	NodeIDs []string `json:"node_ids"`
	// Writes counts the write/edit tool calls that targeted this path.
	Writes int `json:"writes"`
	// LastSeq is the event seq of the most recent write — lets pollers
	// spot "new output since I last looked" cheaply.
	LastSeq int64 `json:"last_seq"`
}

// runTouchedFilesResponse is the wire shape of
// GET /api/runs/{id}/files/touched.
type runTouchedFilesResponse struct {
	WorkDir  string        `json:"work_dir,omitempty"`
	Worktree bool          `json:"worktree,omitempty"`
	Files    []touchedFile `json:"files"`
}

// handleListRunTouchedFiles scans the run's events for write-capable
// tool calls and aggregates the targeted paths per file with node
// attribution. A run with no events (or none matching) returns an empty
// list — that is a valid "the nodes wrote nothing" answer, not an error.
func (s *Server) handleListRunTouchedFiles(w http.ResponseWriter, r *http.Request) {
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
	rs := s.runs.RunStore()
	blobs := store.AsToolBlobStore(rs)

	agg := make(map[string]*touchedFile)
	scanErr := rs.ScanEvents(r.Context(), id, func(e *store.Event) bool {
		if e.Type != store.EventToolStarted || e.Data == nil {
			return true
		}
		tool, _ := e.Data["tool"].(string)
		keys, ok := writeToolPathKeys[tool]
		if !ok {
			return true
		}
		p := normalizeTouchedPath(run.WorkDir, pathFromToolInput(r, blobs, id, e, keys))
		// Re-check AFTER normalization: a whitespace-only tool path
		// normalizes to "", and a path equal to the workdir itself to "." —
		// neither is a file row (a degenerate Write's tool_started fires
		// before the tool errors, so such inputs do land in events).
		if p == "" || p == "." {
			return true
		}
		tf := agg[p]
		if tf == nil {
			tf = &touchedFile{Path: p}
			agg[p] = tf
		}
		tf.Writes++
		if e.Seq > tf.LastSeq {
			tf.LastSeq = e.Seq
		}
		if e.NodeID != "" && !slices.Contains(tf.NodeIDs, e.NodeID) {
			tf.NodeIDs = append(tf.NodeIDs, e.NodeID)
		}
		return true
	})
	if scanErr != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "scan events: %v", scanErr)
		return
	}

	files := make([]touchedFile, 0, len(agg))
	for _, tf := range agg {
		files = append(files, *tf)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	s.writeJSONFor(w, r, runTouchedFilesResponse{
		WorkDir:  run.WorkDir,
		Worktree: run.Worktree,
		Files:    files,
	})
}

// pathFromToolInput extracts the target path from a tool_started event's
// persisted input. Three storage shapes exist (see model.persistToolPayload):
// full inline (`data.input`), sidecar preview (`data.input_preview`, first
// 4 KB + `input_ref`), and capped inline fallback (`data.input` truncated,
// `input_size` present). The truncated shapes are handled by the lenient
// parser; when the preview didn't carry the key, the sidecar blob's head
// is read as a last resort.
func pathFromToolInput(r *http.Request, blobs store.ToolBlobStore, runID string, e *store.Event, keys []string) string {
	if raw, ok := e.Data["input"].(string); ok && raw != "" {
		if p := pathFromJSONObject(raw, keys); p != "" {
			return p
		}
	}
	if raw, ok := e.Data["input_preview"].(string); ok && raw != "" {
		if p := pathFromJSONObject(raw, keys); p != "" {
			return p
		}
	}
	if ref, ok := e.Data["input_ref"].(string); ok && ref != "" && blobs != nil {
		data, _, _, err := blobs.ReadToolBlob(r.Context(), runID, ref, "input", 0, touchedBlobReadCap)
		if err == nil {
			if p := pathFromJSONObject(string(data), keys); p != "" {
				return p
			}
		}
	}
	return ""
}

// pathFromJSONObject returns the first candidate key's top-level string
// value from raw. Complete JSON goes through json.Unmarshal; anything
// else (a preview cut mid-stream, a capped inline fallback) falls back to
// a token walk that keeps every top-level string seen before the cut —
// tool inputs put the target path in the leading bytes, so a truncated
// prefix nearly always still carries it.
func pathFromJSONObject(raw string, keys []string) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err == nil {
		for _, k := range keys {
			if s, ok := m[k].(string); ok && s != "" {
				return s
			}
		}
		return ""
	}

	found := make(map[string]string)
	dec := json.NewDecoder(strings.NewReader(raw))
	if tok, err := dec.Token(); err != nil {
		return ""
	} else if d, ok := tok.(json.Delim); !ok || d != '{' {
		return ""
	}
scan:
	for {
		keyTok, err := dec.Token()
		if err != nil {
			break
		}
		key, ok := keyTok.(string)
		if !ok {
			break // object close (or malformed) — done with top level
		}
		valTok, err := dec.Token()
		if err != nil {
			break
		}
		switch v := valTok.(type) {
		case string:
			found[key] = v
		case json.Delim:
			// Nested object/array value — drain its tokens so the walk
			// stays at the top level. A truncation inside the nested
			// value surfaces as a token error: stop, keep what we have.
			depth := 1
			for depth > 0 {
				t, err := dec.Token()
				if err != nil {
					break scan
				}
				if d, ok := t.(json.Delim); ok {
					switch d {
					case '{', '[':
						depth++
					case '}', ']':
						depth--
					}
				}
			}
		}
	}
	for _, k := range keys {
		if s := found[k]; s != "" {
			return s
		}
	}
	return ""
}

// normalizeTouchedPath maps a tool-input path onto the same namespace the
// git /files listing uses: workdir-relative for anything inside
// run.WorkDir, untouched (cleaned) otherwise. Tools receive absolute
// paths from claude_code and usually workdir-relative ones from claw, so
// both spellings of the same file must collapse to one key.
func normalizeTouchedPath(workDir, p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	p = filepath.Clean(p)
	if workDir != "" && filepath.IsAbs(p) {
		if rel, err := filepath.Rel(workDir, p); err == nil &&
			rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return rel
		}
	}
	return p
}
