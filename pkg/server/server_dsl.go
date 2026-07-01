package server

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/bots"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
	"github.com/SocialGouv/iterion/pkg/dsl/workflowfile"
)

// --- Request/Response types ---

type parseRequest struct {
	Source string `json:"source"`
}

type parseResponse struct {
	Document    json.RawMessage `json:"document"`
	Diagnostics []string        `json:"diagnostics,omitempty"`
	Issues      []DiagnosticDTO `json:"issues,omitempty"`
}

// DiagnosticDTO is the wire-safe shape of an ir.Diagnostic. It carries the
// structured fields (code, severity, attribution, hint) so the studio can
// render inline badges without resorting to string-matching the message.
type DiagnosticDTO struct {
	Code     string `json:"code,omitempty"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
	NodeID   string `json:"node_id,omitempty"`
	EdgeID   string `json:"edge_id,omitempty"`
	Hint     string `json:"hint,omitempty"`
}

func irDiagToDTO(d ir.Diagnostic) DiagnosticDTO {
	sev := "error"
	if d.Severity == ir.SeverityWarning {
		sev = "warning"
	}
	return DiagnosticDTO{
		Code:     string(d.Code),
		Severity: sev,
		Message:  d.Message,
		NodeID:   d.NodeID,
		EdgeID:   d.EdgeID,
		Hint:     d.Hint,
	}
}

type unparseRequest struct {
	Document json.RawMessage `json:"document"`
}

type unparseResponse struct {
	Source string `json:"source"`
}

type validateRequest struct {
	Document json.RawMessage `json:"document"`
}

type validateResponse struct {
	// Legacy string shape — preserved for any external consumer that already
	// reads it. New consumers should prefer Issues, which carries structured
	// attribution and hints.
	Diagnostics []string        `json:"diagnostics,omitempty"`
	Warnings    []string        `json:"warnings,omitempty"`
	Issues      []DiagnosticDTO `json:"issues,omitempty"`
	Valid       bool            `json:"valid"`
	NodeCount   int             `json:"node_count,omitempty"`
	EdgeCount   int             `json:"edge_count,omitempty"`
}

// --- Handlers ---

func (s *Server) handleParse(w http.ResponseWriter, r *http.Request) {
	var req parseRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	pr := parser.Parse("studio.bot", req.Source)

	var diags []string
	for _, d := range pr.Diagnostics {
		diags = append(diags, d.Error())
	}

	if pr.File == nil {
		writeJSON(w, parseResponse{Diagnostics: diags})
		return
	}

	docJSON, err := ast.MarshalFile(pr.File)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "marshal error: %v", err)
		return
	}

	writeJSON(w, parseResponse{
		Document:    json.RawMessage(docJSON),
		Diagnostics: diags,
	})
}

func (s *Server) handleUnparse(w http.ResponseWriter, r *http.Request) {
	var req unparseRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	f, err := ast.UnmarshalFile(req.Document)
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid document: %v", err)
		return
	}

	source := unparse.Unparse(f)
	writeJSON(w, unparseResponse{Source: source})
}

func (s *Server) handleValidate(w http.ResponseWriter, r *http.Request) {
	var req validateRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	f, err := ast.UnmarshalFile(req.Document)
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid document: %v", err)
		return
	}

	resp := validateResponse{Valid: true}

	// Parse diagnostics (re-validate via compiler).
	cr := ir.Compile(f)
	for _, d := range cr.Diagnostics {
		msg := d.Error()
		resp.Issues = append(resp.Issues, irDiagToDTO(d))
		if d.Severity == ir.SeverityError {
			resp.Diagnostics = append(resp.Diagnostics, msg)
			resp.Valid = false
		} else {
			resp.Warnings = append(resp.Warnings, msg)
		}
	}

	if cr.Workflow != nil {
		resp.NodeCount = len(cr.Workflow.Nodes)
		resp.EdgeCount = len(cr.Workflow.Edges)
	}

	writeJSON(w, resp)
}

func (s *Server) handleListExamples(w http.ResponseWriter, _ *http.Request) {
	// Two sources, merged + de-duplicated, surfaced as the studio Home's
	// "Bots" quick-open panel:
	//   1. <ExamplesDir>/<bot>/main.bot — first-class bots shipped on
	//      disk (e.g. <repo>/bots/ when the user opens an iterion repo),
	//      filtered to bundles whose manifest declares a display_name
	//      persona (see isFirstClassBot).
	//   2. The bot recipes embedded in the binary (bots/embed.go), so a
	//      fresh project that ships none of its own still gets the
	//      canonical built-ins (feature_dev, whole/branch_improve_loop).
	// On-disk wins on name collision: a project that overrides an
	// embedded recipe by placing one with the same relative name in its
	// bots/ dir gets to override what the SPA loads.
	seen := map[string]struct{}{}
	var entries []exampleEntry

	if dir := s.cfg.ExamplesDir; dir != "" {
		_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if path != dir && isSkippedDir(d.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			if !workflowfile.IsWorkflowFile(d.Name()) {
				return nil
			}
			// Only surface first-class bots — a bundle's main.bot whose
			// manifest.yaml declares a display_name persona. Drops loose
			// .bot files (smoke tests) and un-personified bundles so the
			// Home panel shows exactly the named team.
			persona, desc, ok := firstClassBot(path)
			if !ok {
				return nil
			}
			rel, err := filepath.Rel(dir, path)
			if err != nil {
				return nil
			}
			// Normalize to forward slashes so the name matches the
			// embed FS convention and the load endpoint.
			rel = filepath.ToSlash(rel)
			if _, dup := seen[rel]; dup {
				return nil
			}
			seen[rel] = struct{}{}
			entries = append(entries, exampleEntry{Name: rel, DisplayName: persona, Description: desc})
			return nil
		})
	}

	for _, p := range bots.List() {
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		// Embedded recipes carry no manifest in the binary, so no persona
		// here; the on-disk walk above already surfaced (with its persona)
		// any bundle the workspace actually ships.
		entries = append(entries, exampleEntry{Name: p})
	}

	// Present the bots in the curated team order (Nexie first), not the
	// filesystem walk order.
	sort.SliceStable(entries, func(i, j int) bool {
		ri, rj := botRosterRank(entries[i].Name), botRosterRank(entries[j].Name)
		if ri != rj {
			return ri < rj
		}
		return entries[i].Name < entries[j].Name
	})

	if entries == nil {
		entries = []exampleEntry{}
	}
	writeJSON(w, entries)
}

// exampleEntry is one bot surfaced by the studio Home "Bots" panel:
// its relative load name (e.g. "whats-next/main.bot") plus the manifest
// persona + a one-line description the SPA shows in place of the raw
// path. (The technical name is the first path segment, derived SPA-side.)
type exampleEntry struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name,omitempty"`
	Description string `json:"description,omitempty"`
}

// firstClassBot returns the persona + a one-line description of the bot
// bundle whose entry point is mainBotPath, and whether it is first-class
// — a "main.bot" whose sibling manifest.yaml declares a non-empty
// display_name. The studio Home lists only first-class bots, so loose
// .bot files (smoke tests) and un-personified bundles stay out.
func firstClassBot(mainBotPath string) (persona, description string, ok bool) {
	if filepath.Base(mainBotPath) != "main.bot" {
		return "", "", false
	}
	m, err := bundle.LoadManifest(filepath.Join(filepath.Dir(mainBotPath), "manifest.yaml"))
	if err != nil || m == nil {
		return "", "", false
	}
	persona = strings.TrimSpace(m.DisplayName)
	if persona == "" {
		return "", "", false
	}
	return persona, shortDescription(m.Description), true
}

// shortDescription condenses a multi-line manifest description into a
// single tidy line for the Home: whitespace collapsed, trimmed to the
// first sentence (or ~140 chars) so the panel rows stay compact.
func shortDescription(desc string) string {
	d := strings.Join(strings.Fields(desc), " ")
	if d == "" {
		return ""
	}
	if i := strings.Index(d, ". "); i > 0 && i < 160 {
		return d[:i+1]
	}
	const max = 140
	if len(d) > max {
		return strings.TrimSpace(d[:max-1]) + "…"
	}
	return d
}

// botRosterOrder is the curated team order the studio surfaces present
// bots in — Nexie first, then the build / improve / doc / review /
// security line (matches the README "Meet the legion" table). Bots
// outside the roster sort after it, alphabetically.
var botRosterOrder = []string{
	"whats-next",          // Nexie
	"feature-dev",         // Featurly
	"branch-improve-loop", // Billy
	"whole-improve-loop",  // Willy
	"docs-refresh",        // Doki
	"review-pr",           // Revi
	"sec-audit-source",    // Seki
	"sec-audit-deps",      // Depsy
	"secured-renovacy",    // Renovacy
}

// botRosterRank ranks an example name ("<bot-id>/main.bot") by its
// position in botRosterOrder; unknown bots rank last.
func botRosterRank(name string) int {
	id := name
	if i := strings.IndexByte(name, '/'); i >= 0 {
		id = name[:i]
	}
	for idx, b := range botRosterOrder {
		if b == id {
			return idx
		}
	}
	return len(botRosterOrder)
}

func (s *Server) handleLoadExample(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		httpError(w, http.StatusBadRequest, "missing example name")
		return
	}

	// Sanitize: allow forward-slash relative paths (e.g. "feature_dev/main.bot")
	// but reject backslashes, leading dots, parent traversal, and
	// absolute paths. Must end in an accepted workflow extension.
	if strings.Contains(name, "\\") || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "/") || !workflowfile.IsWorkflowFile(name) {
		httpError(w, http.StatusBadRequest, "invalid example name")
		return
	}
	cleaned := path.Clean(name)
	if cleaned != name || strings.HasPrefix(cleaned, "..") || strings.Contains(cleaned, "/../") {
		httpError(w, http.StatusBadRequest, "invalid example name")
		return
	}

	// Try on-disk first (lets a project's <ExamplesDir>/<name>
	// override an embedded recipe of the same basename), then fall
	// back to the binary-embedded recipe set (examples/embed.go).
	var data []byte
	if dir := s.cfg.ExamplesDir; dir != "" {
		if d, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name))); err == nil {
			data = d
		}
	}
	if data == nil {
		if d, ok := bots.Get(name); ok {
			data = d
		}
	}
	if data == nil {
		httpError(w, http.StatusNotFound, "example not found: %s", name)
		return
	}

	// Parse and return the document + source.
	pr := parser.Parse(name, string(data))
	var diags []string
	for _, d := range pr.Diagnostics {
		diags = append(diags, d.Error())
	}

	if pr.File == nil {
		writeJSON(w, parseResponse{Diagnostics: diags})
		return
	}

	docJSON, err := ast.MarshalFile(pr.File)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "marshal error: %v", err)
		return
	}

	writeJSON(w, struct {
		Source      string          `json:"source"`
		Document    json.RawMessage `json:"document"`
		Diagnostics []string        `json:"diagnostics,omitempty"`
	}{
		Source:      string(data),
		Document:    json.RawMessage(docJSON),
		Diagnostics: diags,
	})
}
