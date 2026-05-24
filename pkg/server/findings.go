// Package server — findings inbox surface.
//
// Findings are short markdown notes a bot run leaves in
// ${PROJECT_MEMORY_DIR}/findings/ when it discovers something the
// operator might want to act on but that doesn't fit the current
// roadmap. Today an operator must `ls` that directory to know what's
// waiting; this surface exposes the inbox to the studio so it can
// (a) show a badge on Home, (b) drive a dedicated /findings view,
// and (c) let the operator archive a finding without dropping to the
// shell.
//
// The directory layout matches the runtime's expectation
// (see pkg/runtime/engine.go's PROJECT_MEMORY_DIR resolution):
//
//	<iterion-home>/projects/<workdir-key>/memory/findings/<name>.md
//
// where workdir-key = store.EncodeWorkDirKey(cfg.WorkDir).
package server

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/store"
)

// Finding is the JSON shape returned by GET /api/v1/findings. The
// frontmatter fields are parsed permissively — a malformed finding
// still surfaces, just with empty parsed fields and the raw filename
// as the title fallback.
type Finding struct {
	// Filename is the basename relative to the findings directory.
	// Used by the DELETE endpoint and as a stable key in the UI.
	Filename string `json:"filename"`
	// Path is the absolute filesystem path. Lets the studio link to
	// it via vscode://file or print it for the operator.
	Path string `json:"path"`
	// SizeBytes is the size of the .md on disk, useful for sorting
	// when timestamps are unreliable.
	SizeBytes int64 `json:"size_bytes"`
	// ModifiedAt is the file's mtime, RFC3339. Used as the default
	// sort key (newest first).
	ModifiedAt string `json:"modified_at"`

	// Frontmatter parsed from the YAML header. Empty when the file
	// has no frontmatter or the parser couldn't make sense of it.
	Title       string   `json:"title,omitempty"`
	Description string   `json:"description,omitempty"`
	Kind        string   `json:"kind,omitempty"`
	SourceBot   string   `json:"source_bot,omitempty"`
	Tags        []string `json:"tags,omitempty"`

	// Body is the markdown after the closing `---`. Empty for
	// frontmatter-only files. Truncated past `findingBodyMaxBytes`
	// to avoid shipping huge findings through the list endpoint —
	// the caller can request the full file via the file_path link.
	Body string `json:"body,omitempty"`
	// BodyTruncated signals the operator's UI that what's shown is
	// a preview only.
	BodyTruncated bool `json:"body_truncated,omitempty"`
}

const findingBodyMaxBytes = 4096

// registerFindingsRoutes wires the /api/v1/findings endpoints onto
// mux. Mounted only when cfg.WorkDir is set — the resolver derives
// the project key from the workspace; without a workspace there's
// no inbox to talk about.
func (s *Server) registerFindingsRoutes() {
	if s.cfg.WorkDir == "" {
		return
	}
	prefix := "/api/v1/findings"
	s.mux.HandleFunc("GET "+prefix, s.handleListFindings)
	s.mux.HandleFunc("DELETE "+prefix+"/{name}", s.handleDeleteFinding)
}

func (s *Server) findingsDir() string {
	abs, err := filepath.Abs(s.cfg.WorkDir)
	if err != nil {
		abs = s.cfg.WorkDir
	}
	return filepath.Join(
		store.GlobalIterionDataDir(),
		"projects",
		store.EncodeWorkDirKey(abs),
		"memory",
		"findings",
	)
}

func (s *Server) handleListFindings(w http.ResponseWriter, _ *http.Request) {
	dir := s.findingsDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			// Empty inbox is a normal state, not an error — the
			// directory is created lazily by the first finding-writing
			// run. Return an empty list rather than 404.
			writeJSONResp(w, http.StatusOK, []Finding{})
			return
		}
		writeJSONErr(w, http.StatusInternalServerError, fmt.Errorf("findings dir: %w", err))
		return
	}
	out := make([]Finding, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		full := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		f := parseFinding(e.Name(), full, info, data)
		out = append(out, f)
	}
	// Newest first by mtime; falls back to filename for determinism
	// when two findings share an mtime to the second.
	sort.Slice(out, func(i, j int) bool {
		if out[i].ModifiedAt != out[j].ModifiedAt {
			return out[i].ModifiedAt > out[j].ModifiedAt
		}
		return out[i].Filename < out[j].Filename
	})
	writeJSONResp(w, http.StatusOK, out)
}

func (s *Server) handleDeleteFinding(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	// Defense in depth: refuse anything that contains a path
	// separator or `..`. The findings directory should be a flat
	// directory of .md files; sub-directories aren't a thing.
	if name == "" || strings.ContainsAny(name, "/\\") || name == ".." {
		writeJSONErr(w, http.StatusBadRequest, fmt.Errorf("invalid finding name"))
		return
	}
	if !strings.HasSuffix(strings.ToLower(name), ".md") {
		writeJSONErr(w, http.StatusBadRequest, fmt.Errorf("finding must be a .md file"))
		return
	}
	full := filepath.Join(s.findingsDir(), name)
	if err := os.Remove(full); err != nil {
		if os.IsNotExist(err) {
			writeJSONErr(w, http.StatusNotFound, fmt.Errorf("finding not found"))
			return
		}
		writeJSONErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSONResp(w, http.StatusOK, map[string]string{"deleted": name})
}

// parseFinding extracts YAML-ish frontmatter from the head of the
// file and returns the metadata + body preview. Handles the format
// the runtime's findings-handoff feature writes:
//
//	---
//	title: "..."
//	description: "..."
//	kind: "bug"|"drift"|"..."
//	source_bot: "..."
//	tags: ["...", "..."]
//	---
//
//	# Body
//	...
//
// Tolerates missing frontmatter (returns a Finding with only the
// filename + path + body populated) and unknown fields (silently
// dropped).
func parseFinding(filename, path string, info fs.FileInfo, data []byte) Finding {
	out := Finding{
		Filename:   filename,
		Path:       path,
		SizeBytes:  info.Size(),
		ModifiedAt: info.ModTime().UTC().Format("2006-01-02T15:04:05Z"),
		Title:      strings.TrimSuffix(filename, ".md"), // fallback
	}
	text := string(data)
	if !strings.HasPrefix(text, "---\n") {
		out.Body = trimBody(text)
		out.BodyTruncated = len(text) > findingBodyMaxBytes
		return out
	}
	end := strings.Index(text[4:], "\n---")
	if end < 0 {
		// Malformed frontmatter — bail out, treat the whole file as body.
		out.Body = trimBody(text)
		out.BodyTruncated = len(text) > findingBodyMaxBytes
		return out
	}
	header := text[4 : 4+end]
	body := strings.TrimLeft(text[4+end+4:], "\n")
	parseFrontmatter(&out, header)
	out.Body = trimBody(body)
	out.BodyTruncated = len(body) > findingBodyMaxBytes
	return out
}

func trimBody(s string) string {
	if len(s) <= findingBodyMaxBytes {
		return s
	}
	return s[:findingBodyMaxBytes]
}

// parseFrontmatter handles the limited YAML shape findings ship —
// scalar string values with optional quotes, plus a single
// inline-array field (`tags: ["a", "b"]`). Avoids pulling a full
// YAML parser dependency for what is by convention a 5-line header.
func parseFrontmatter(out *Finding, header string) {
	for _, line := range strings.Split(header, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		val = strings.Trim(val, `"'`)
		switch key {
		case "title":
			if val != "" {
				out.Title = val
			}
		case "description":
			out.Description = val
		case "kind":
			out.Kind = val
		case "source_bot":
			out.SourceBot = val
		case "tags":
			out.Tags = parseInlineArray(val)
		}
	}
}

// parseInlineArray turns `["a", "b", "c"]` into ["a","b","c"].
// Returns nil for empty / malformed input.
func parseInlineArray(raw string) []string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "[")
	raw = strings.TrimSuffix(raw, "]")
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		p = strings.Trim(p, `"'`)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Small helpers so this file doesn't depend on server.go's private
// JSON helpers — keeps the surface easy to lift out later if the
// findings inbox grows its own package.
func writeJSONResp(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONErr(w http.ResponseWriter, code int, err error) {
	writeJSONResp(w, code, map[string]string{"error": err.Error()})
}
