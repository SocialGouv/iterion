package server

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// browseRootEnv names the env var that gates the server-side directory
// browser. Unset → endpoint returns 403. Set → its value is the root
// under which the browser is allowed to traverse; anything outside is
// rejected. The user controls exposure — no default; the feature is
// off until explicitly opted-in.
const browseRootEnv = "ITERION_BROWSE_ROOT"

type filesystemListResponse struct {
	// Cwd is the path the listing was performed against, relative to
	// the configured root. Always starts with "/" and uses forward
	// slashes regardless of host OS.
	Cwd string `json:"cwd"`
	// Root is the absolute, resolved browse root — surfaced so the
	// SPA can render a breadcrumb anchored on it.
	Root string `json:"root"`
	// Entries lists immediate subdirectories of Cwd. Files are
	// omitted: the picker only chooses folders.
	Entries []filesystemEntry `json:"entries"`
}

type filesystemEntry struct {
	Name string `json:"name"`
	// AbsDir is the absolute path the SPA passes back to /api/projects
	// when the user picks this entry.
	AbsDir string `json:"abs_dir"`
}

func browseRoot() string {
	return strings.TrimSpace(os.Getenv(browseRootEnv))
}

// resolveBrowsePath joins a request-supplied (relative) path onto the
// already-resolved root and refuses anything that escapes the root
// tree (via .. or symlinks pointing outside). Caller must pass the
// EvalSymlinks-resolved root.
func resolveBrowsePath(rootReal, requested string) (string, error) {
	cleaned := filepath.Clean("/" + strings.TrimPrefix(requested, "/"))
	abs := filepath.Join(rootReal, cleaned)
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("path does not exist: %s", cleaned)
		}
		return "", fmt.Errorf("resolve: %w", err)
	}
	rel, err := filepath.Rel(rootReal, resolved)
	if err != nil || strings.HasPrefix(rel, "..") || rel == ".." {
		return "", fmt.Errorf("path escapes browse root")
	}
	return resolved, nil
}

// handleFilesystemList answers GET /api/filesystem/list?path=<rel>.
// 403 when ITERION_BROWSE_ROOT is unset; otherwise lists immediate
// subdirectories of `path` (resolved relative to the root) so the
// AddProject dialog can render a folder picker without exposing
// arbitrary filesystem access.
func (s *Server) handleFilesystemList(w http.ResponseWriter, r *http.Request) {
	root := browseRoot()
	if root == "" {
		s.httpErrorFor(w, r, http.StatusForbidden, "filesystem browse disabled (set %s to enable)", browseRootEnv)
		return
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "browse root abs: %v", err)
		return
	}
	rootReal, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "browse root resolve: %v", err)
		return
	}
	requested := r.URL.Query().Get("path")
	if requested == "" {
		requested = "/"
	}
	resolved, err := resolveBrowsePath(rootReal, requested)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "%v", err)
		return
	}
	dir, err := os.Open(resolved)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "open: %v", err)
		return
	}
	defer dir.Close()
	names, err := dir.Readdirnames(-1)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "readdir: %v", err)
		return
	}
	entries := make([]filesystemEntry, 0, len(names))
	for _, name := range names {
		if strings.HasPrefix(name, ".") {
			continue
		}
		full := filepath.Join(resolved, name)
		info, err := os.Lstat(full)
		if err != nil || !info.IsDir() {
			continue
		}
		// Skip symlinked dirs that point outside the root — the
		// browser shouldn't even hint at paths the picker would
		// refuse to switch to.
		linkResolved, err := filepath.EvalSymlinks(full)
		if err != nil {
			continue
		}
		if rel, err := filepath.Rel(rootReal, linkResolved); err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		entries = append(entries, filesystemEntry{
			Name:   name,
			AbsDir: linkResolved,
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	// Re-derive the relative cwd from the resolved path so the SPA can
	// build a breadcrumb. Empty rel → root.
	rel, _ := filepath.Rel(rootReal, resolved)
	cwd := "/"
	if rel != "" && rel != "." {
		cwd = "/" + filepath.ToSlash(rel)
	}
	s.writeJSONFor(w, r, filesystemListResponse{
		Cwd:     cwd,
		Root:    rootReal,
		Entries: entries,
	})
}
