package server

import (
	"net/http"

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
	s.writeJSONFor(w, r, resp)
}
