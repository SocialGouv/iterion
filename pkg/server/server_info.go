package server

import (
	"net/http"

	"github.com/SocialGouv/iterion/pkg/internal/appinfo"
)

// serverInfoResponse describes the running server to the SPA. Used by
// the Launch modal to render appropriate upload limits before any
// upload is attempted.
type serverInfoResponse struct {
	Mode    string            `json:"mode"`
	Version string            `json:"version"`
	Commit  string            `json:"commit,omitempty"`
	Limits  serverLimitsBlock `json:"limits"`
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
		Mode:    mode,
		Version: appinfo.Version,
		Commit:  appinfo.Commit,
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
