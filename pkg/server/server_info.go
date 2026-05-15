package server

import (
	"net/http"
	"path/filepath"

	"github.com/SocialGouv/iterion/pkg/internal/appinfo"
)

// serverInfoResponse describes the running server to the SPA. Used by
// the Launch modal to render appropriate upload limits before any
// upload is attempted, and by the AuthProvider to decide whether to
// gate the editor on a sign-in flow.
type serverInfoResponse struct {
	Mode    string `json:"mode"`
	Version string `json:"version"`
	Commit  string `json:"commit,omitempty"`
	// AuthRequired is false in local / desktop mode (single-user TTY,
	// no JWT) and true in cloud mode (multitenant). The SPA short-
	// circuits its bootstrap when false and renders the editor as a
	// synthetic super-admin so the existing local UX is preserved.
	AuthRequired bool              `json:"auth_required"`
	Limits       serverLimitsBlock `json:"limits"`
	// WorkDir is the absolute working directory the server was launched
	// with (`iterion editor --dir`). Empty in cloud mode where there is
	// no per-server folder concept.
	WorkDir string `json:"work_dir,omitempty"`
	// ProjectName is a human-friendly label derived from WorkDir
	// (typically its basename). The SPA surfaces it in the Toolbar and
	// RunHeader so the user always sees which project they're editing.
	ProjectName string `json:"project_name,omitempty"`
	// CurrentProjectID matches the registry entry currently selected
	// (when the SPA wants to highlight it in the ProjectSwitcher).
	// Empty in cloud mode or when the registry has never been written.
	CurrentProjectID string `json:"current_project_id,omitempty"`
	// BrowseRoot is the absolute path under which the server-side
	// directory browser (/api/filesystem/list) is allowed to traverse,
	// or "" when the feature is disabled. The SPA shows the Browse
	// button in the AddProject dialog only when this is non-empty.
	BrowseRoot string `json:"browse_root,omitempty"`
}

type serverLimitsBlock struct {
	Upload uploadLimitsBlock `json:"upload"`
}

type uploadLimitsBlock struct {
	MaxFileSize    int64    `json:"max_file_size"`
	MaxTotalSize   int64    `json:"max_total_size"`
	MaxFilesPerRun int      `json:"max_files_per_run"`
	AllowedMIME    []string `json:"allowed_mime"`
}

// handleServerInfo answers GET /api/server/info. Public (no
// origin gate) because it returns inert metadata used by the SPA.
func (s *Server) handleServerInfo(w http.ResponseWriter, r *http.Request) {
	mode := s.cfg.Mode
	if mode == "" {
		mode = "local"
	}
	resp := serverInfoResponse{
		Mode:         mode,
		Version:      appinfo.Version,
		Commit:       appinfo.Commit,
		AuthRequired: s.authSvc != nil && !s.cfg.DisableAuth,
		Limits: serverLimitsBlock{
			Upload: uploadLimitsBlock{
				MaxFileSize:    s.cfg.MaxUploadSize,
				MaxTotalSize:   s.cfg.MaxTotalUploadSize,
				MaxFilesPerRun: s.cfg.MaxUploadsPerRun,
				AllowedMIME:    s.cfg.AllowedUploadMIMEs,
			},
		},
	}
	if mode == "local" {
		resp.WorkDir = s.cfg.WorkDir
		resp.ProjectName = deriveProjectName(s.cfg.WorkDir)
		resp.BrowseRoot = browseRoot()
		resp.CurrentProjectID = s.CurrentProjectID()
	}
	s.writeJSONFor(w, r, resp)
}

// deriveProjectName picks a human-friendly label from the working
// directory. Returns "" for empty / root-ish inputs so the SPA can fall
// back to no-label rendering.
func deriveProjectName(dir string) string {
	if dir == "" {
		return ""
	}
	base := filepath.Base(filepath.Clean(dir))
	if base == "." || base == "/" || base == string(filepath.Separator) {
		return ""
	}
	return base
}
