package server

import (
	"net/http"
	"os"
	"path/filepath"

	"github.com/SocialGouv/iterion/pkg/botimport"
)

// botImportRequest carries a Claude-Code workflow script to convert
// into a draft .bot. Source is the script text — the studio reads the
// operator's file client-side; the server never touches arbitrary
// paths.
type botImportRequest struct {
	Source string `json:"source"`
	// Filename labels report anchors (js:<line>) and the report header.
	Filename string `json:"filename,omitempty"`
	// Name overrides the workflow name (default meta.name, then the
	// filename stem).
	Name string `json:"name,omitempty"`
	// DryRun returns the draft + report without writing anything.
	DryRun bool `json:"dry_run,omitempty"`
}

type botImportReport struct {
	Mapped       []string `json:"mapped,omitempty"`
	Holes        []string `json:"holes,omitempty"`
	Placeholders []string `json:"placeholders,omitempty"`
	Dropped      []string `json:"dropped,omitempty"`
}

type botImportResponse struct {
	WorkflowName string `json:"workflow_name"`
	// Path is the workspace-relative file the draft landed in (write
	// mode only).
	Path           string          `json:"path,omitempty"`
	DryRun         bool            `json:"dry_run,omitempty"`
	NeedsAttention bool            `json:"needs_attention"`
	Report         botImportReport `json:"report"`
	// BotSource is always returned so the studio can preview the draft
	// (IMPORT REPORT header included).
	BotSource string `json:"bot_source"`
}

// handleBotImport converts a workflow script (.js) into a draft .bot —
// the HTTP twin of `iterion import` (pkg/botimport: goja AST walk,
// zero JS execution, lossy-by-contract with an embedded IMPORT
// REPORT). Local-mode only, like bot create: the draft lands in the
// workspace's bots/ directory.
func (s *Server) handleBotImport(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	if s.cfg.Mode == "cloud" {
		s.httpErrorFor(w, r, http.StatusForbidden, "bots: import is a local-mode operation")
		return
	}
	var req botImportRequest
	if err := readJSON(r, &req); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid request: %v", err)
		return
	}
	if req.Source == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "bots: import needs the script text in `source`")
		return
	}
	filename := req.Filename
	if filename == "" {
		filename = "import.js"
	}

	res, err := botimport.Import(filename, []byte(req.Source), botimport.Options{Name: req.Name})
	if err != nil {
		// Unparsable JS / no agent calls — the operator's input, 422.
		s.httpErrorFor(w, r, http.StatusUnprocessableEntity, "bots: import: %v", err)
		return
	}

	resp := botImportResponse{
		WorkflowName:   res.WorkflowName,
		DryRun:         req.DryRun,
		NeedsAttention: res.Report.NeedsAttention(),
		Report: botImportReport{
			Mapped:       botimport.FormatEntries(res.Report.Mapped),
			Holes:        botimport.FormatEntries(res.Report.Holes),
			Placeholders: botimport.FormatEntries(res.Report.Placeholders),
			Dropped:      botimport.FormatEntries(res.Report.Dropped),
		},
		BotSource: res.BotSource,
	}
	if req.DryRun {
		s.writeJSONFor(w, r, resp)
		return
	}

	if s.cfg.WorkDir == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "bots: server has no workdir to write the draft in")
		return
	}
	rel := filepath.Join("bots", res.WorkflowName+".bot")
	abs := filepath.Join(s.cfg.WorkDir, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "bots: %v", err)
		return
	}
	// A draft must never silently replace an existing workflow.
	f, err := os.OpenFile(abs, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if os.IsExist(err) {
			s.httpErrorFor(w, r, http.StatusConflict, "bots: %s already exists — pick another name", rel)
			return
		}
		s.httpErrorFor(w, r, http.StatusInternalServerError, "bots: %v", err)
		return
	}
	if _, err := f.WriteString(res.BotSource); err != nil {
		f.Close()
		s.httpErrorFor(w, r, http.StatusInternalServerError, "bots: write: %v", err)
		return
	}
	if err := f.Close(); err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "bots: write: %v", err)
		return
	}
	resp.Path = rel
	s.writeJSONFor(w, r, resp)
}
