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
	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/bundlelint"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
	"github.com/SocialGouv/iterion/pkg/dsl/workflowfile"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// --- Request/Response types ---

type parseRequest struct {
	Source string `json:"source"`
	// Files and Main describe a bot in several files — a cloud bundle's
	// files map, keyed by path from the bundle's root — parsed as its
	// unit: the document then carries each declaration's file, and the
	// response the unit's revision.
	Files map[string]string `json:"files,omitempty"`
	Main  string            `json:"main,omitempty"`
}

type parseResponse struct {
	Document    json.RawMessage `json:"document"`
	Diagnostics []string        `json:"diagnostics,omitempty"`
	Issues      []DiagnosticDTO `json:"issues,omitempty"`
	// Unit is set when the request parsed a bot in several files.
	Unit *unitInfo `json:"unit,omitempty"`
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
	// Files and Main, when given, are the bot in several files the
	// document was opened from: the response then holds every file the
	// document rewrites, by path, and only those. Revision is the unit's
	// revision the document was opened at, which must be the files' now.
	Files    map[string]string `json:"files,omitempty"`
	Main     string            `json:"main,omitempty"`
	Revision string            `json:"revision,omitempty"`
	// Flatten renders the merged program of a bot in several files as one
	// text for DISPLAY — the Source view — which is never written back:
	// without it a document whose declarations name their files is
	// refused here, since one file could only fold every file into it.
	Flatten bool `json:"flatten,omitempty"`
}

type unparseResponse struct {
	Source string `json:"source"`
	// Files holds the rewritten files of a bot in several files, by path
	// from the bundle's root; a file whose program did not change is
	// absent. Revision is the unit's revision once they are patched in.
	Files    map[string]string `json:"files,omitempty"`
	Revision string            `json:"revision,omitempty"`
}

type validateRequest struct {
	Document json.RawMessage `json:"document"`
	// Path is the workspace-relative file the document was opened from,
	// when the editor knows it. A main.bot whose parent is a bundle is
	// validated with that bundle's prompts/*.md in scope — the way a launch
	// compiles it — instead of refusing every `system: <prompt>` the
	// bundle ships as C003. A path with a scheme (a cloud `botsource://`
	// bot) or none at all validates the document alone, as before.
	Path string `json:"path,omitempty"`
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
	if len(req.Files) > 0 {
		s.parseUnitFiles(w, req)
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
	if len(req.Files) > 0 {
		s.unparseUnitFiles(w, req, f)
		return
	}
	if hasProvenance(f) && !req.Flatten {
		httpError(w, http.StatusUnprocessableEntity, "the document is a bot in several files (its declarations name their files): write it back with its files and revision, never as one file")
		return
	}

	source := unparse.Unparse(f)
	if err := unparse.Verify(f, source); err != nil {
		httpError(w, http.StatusUnprocessableEntity, "the document cannot be rendered as .bot source without changing it: %v", err)
		return
	}
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

	var unopenable error
	var notRead string
	// A bundle's prompts/*.md reach the compiler the way they do at a
	// launch when the editor says which file the document is. The path is
	// a hint: one the server cannot place (no workdir on a cloud server, an
	// example served from the embedded catalog, a sibling bundle that does
	// not open — a manifest mid-edit must not blind the editor, so this
	// falls through the way openBundleOrFile does on the CLI) validates the
	// document alone, as before the field.
	switch {
	case strings.HasPrefix(req.Path, botSourceScheme):
		// The cloud editor's bundle, `botsource://<team>/<slug>/<rel>`: its
		// files are in the tenant store, and the caller must be in that
		// team — any other team's path is a hint the server ignores.
		s.mergeBotSourcePrompts(r, f, req.Path)
	case req.Path != "" && !strings.Contains(req.Path, "://"):
		if abs, perr := s.safePath(req.Path); perr == nil {
			b, oerr := runview.ResolveBundleFromFilePath(abs)
			switch {
			case oerr != nil:
				// A sibling manifest.yaml that does not OPEN must not blind
				// the editor: LoadManifest is a strict unmarshal, and a
				// half-typed manifest is a normal state in a studio that
				// edits manifests too — a 422 for the whole request would
				// leave useAutoValidation's stale diagnostics on screen. The
				// document is validated alone, and the response SAYS so
				// (C222, below); the CLI refuses this same state outright.
				unopenable = oerr
			case b != nil:
				// A prompts merge that genuinely fails stays an error: the
				// bundle opened, so its prompts/*.md are in scope.
				if merr := runview.MergeBundlePrompts(f, b); merr != nil {
					httpError(w, http.StatusUnprocessableEntity, "bundle prompts: %v", merr)
					return
				}
			default:
				// A file named like a manifest beside the main.bot that did
				// NOT mark it — a typo in its only distinctive key, a file the
				// parser cannot read — leaves the document validated alone,
				// and the response says why (C223): the one outcome that
				// would otherwise be silent.
				if m, why := bundle.ForeignManifestBeside(abs); m != "" {
					notRead = filepath.Base(m) + " beside main.bot was not read as this bundle's manifest: it " + why
				}
			}
		}
	}

	resp := validateResponse{Valid: true}
	if notRead != "" {
		msg := notRead + " — the document was validated alone, without the prompts, presets and skills beside it"
		resp.Warnings = append(resp.Warnings, msg)
		resp.Issues = append(resp.Issues, DiagnosticDTO{
			Code:     string(bundlelint.DiagManifestNotRead),
			Severity: "warning",
			Message:  msg,
			Hint:     "if it is this bot's manifest, fix it (the reason names the keys); a manifest of another tool beside a loose main.bot needs nothing",
		})
	}
	if unopenable != nil {
		msg := "bundle does not open: " + unopenable.Error() + " — the document was validated alone, without the bundle's prompts, presets and skills; a reference to a bundle prompt reads as C003 until it opens"
		resp.Warnings = append(resp.Warnings, msg)
		resp.Issues = append(resp.Issues, DiagnosticDTO{
			Code:     string(bundlelint.DiagBundleUnopenable),
			Severity: "warning",
			Message:  msg,
			Hint:     "fix the manifest the message names (`iterion validate <bundle dir>` refuses with the same decode error)",
		})
	}

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

// botSourceScheme prefixes the studio editor's virtual path of a cloud
// bot, `botsource://<team>/<slug>/<rel>` (BOTSOURCE_SCHEME in the studio).
const botSourceScheme = "botsource://"

// mergeBotSourcePrompts declares a cloud bot's stored prompts/*.md on the
// document when the validate request names the bot the editor has open —
// through the one rule every surface merges bundle prompts by. The
// caller's active team must be the path's and the store must know the
// slug; otherwise the path is a hint the server ignores and the document
// is validated alone.
func (s *Server) mergeBotSourcePrompts(r *http.Request, f *ast.File, editorPath string) {
	if s.botSources == nil {
		return
	}
	parts := strings.SplitN(strings.TrimPrefix(editorPath, botSourceScheme), "/", 3)
	if len(parts) < 3 || parts[0] == "" || parts[1] == "" {
		return
	}
	// Only the bundle's entrypoint gets its prompts, as on disk
	// (bundle.DirForMainBot fires on main.bot alone): a child .bot the
	// bundle ships beside it compiles as a bare file at launch, so
	// validating it with the parent's prompts in scope would be a green
	// the run does not deliver.
	if parts[2] != "main.bot" {
		return
	}
	id, ok := auth.FromContext(r.Context())
	if !ok || id.TeamID != parts[0] {
		return
	}
	bs, err := s.botSources.GetBySlug(store.WithTenant(r.Context(), parts[0]), parts[0], parts[1])
	if err != nil {
		return
	}
	runview.MergePromptFiles(f, bs.Files, "")
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
	// back to the binary-embedded recipe set (bots/embed.go).
	if dir := s.cfg.ExamplesDir; dir != "" {
		abs := filepath.Join(dir, filepath.FromSlash(name))
		if data, err := os.ReadFile(abs); err == nil {
			s.serveDiskExample(w, name, abs, data)
			return
		}
	}
	src, ok, err := embeddedRecipe(name)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	if !ok {
		httpError(w, http.StatusNotFound, "example not found: %s", name)
		return
	}
	writeExample(w, name, src, "", "")
}

// writeExample answers with one program's document and text — a bot in
// one file, or the flat program an embedded bot or one outside the working
// directory is served as — and, for a file inside the working directory,
// the path the studio opens and saves it by with the disk path
// /api/files/open confirms for it (both empty otherwise, and omitted).
func writeExample(w http.ResponseWriter, name, source, rel, confirmed string) {
	pr := parser.Parse(name, source)
	var diags []string
	for _, d := range pr.Diagnostics {
		diags = append(diags, d.Error())
		// A file that does not parse is never bound to its path: the
		// document the parser salvaged is not the file, and a save of it
		// would replace what the author wrote with what the parser kept.
		if d.Severity == parser.SeverityError {
			rel, confirmed = "", ""
		}
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
		Source            string          `json:"source"`
		Document          json.RawMessage `json:"document"`
		Diagnostics       []string        `json:"diagnostics,omitempty"`
		Path              string          `json:"path,omitempty"`
		ConfirmedDiskPath string          `json:"confirmed_disk_path,omitempty"`
	}{
		Source:            source,
		Document:          json.RawMessage(docJSON),
		Diagnostics:       diags,
		Path:              rel,
		ConfirmedDiskPath: confirmed,
	})
}

// serveDiskExample answers for a bot found under ExamplesDir. A bot in one
// file is its text, with its path when it lies inside the working
// directory. A bot in several files INSIDE the working directory
// opens as its unit, the way /api/files/open opens it — the merged document
// with each declaration's file, the unit's files and revision, the path the
// studio opens and saves it by — since the studio binds the path it is
// handed and edits the files behind it. One that lives elsewhere (a catalog
// root outside the working directory, as in cloud mode) is served as one
// flat program, like an embedded bot: the text the studio launches inline
// and may save as a new file. A unit that does not load is told through its
// diagnostics, never served as its main alone.
func (s *Server) serveDiskExample(w http.ResponseWriter, name, abs string, data []byte) {
	u := unit.LoadDirWithMain(abs, abs, data)
	if len(u.Files) == 0 || u.Files[0].AST == nil || len(u.Files[0].AST.Imports) == 0 {
		// A bot in one file inside the working directory names its real
		// path too, so the studio opens and saves the file it was handed
		// rather than a copy under bots/.
		rel, confirmed, _ := s.workDirRelative(abs, name)
		writeExample(w, name, string(data), rel, confirmed)
		return
	}
	var diags []string
	for _, d := range u.Diagnostics {
		diags = append(diags, d.Error())
	}
	if u.Merged == nil {
		writeJSON(w, parseResponse{Diagnostics: diags})
		return
	}
	if u.HasErrors() {
		// A unit that does not load is never bound to its files: the studio
		// gets the program the loader salvaged, with its diagnostics, and no
		// path or unit — a save of it goes to a new file, never over the
		// files as the author wrote them.
		docJSON, err := ast.MarshalFile(u.Merged)
		if err != nil {
			httpError(w, http.StatusInternalServerError, "marshal error: %v", err)
			return
		}
		writeJSON(w, parseResponse{Document: json.RawMessage(docJSON), Diagnostics: diags})
		return
	}
	if rel, confirmed, ok := s.workDirRelative(abs, name); ok {
		docJSON, err := ast.MarshalFileWithProvenance(u.Merged, u.Root)
		if err != nil {
			httpError(w, http.StatusInternalServerError, "marshal error: %v", err)
			return
		}
		writeJSON(w, unitOpenResponse{Source: string(data), Document: json.RawMessage(docJSON), Diagnostics: diags, Path: rel, ConfirmedDiskPath: confirmed, Unit: unitInfoOf(u, rel)})
		return
	}
	flat, err := flatProgram(name, u.Merged)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	writeExample(w, name, flat, "", "")
}

// workDirRelative is abs — the file the example name designates under
// ExamplesDir — as the path the studio opens and saves it by, slash-
// separated and relative to the working directory, with the path
// /api/files/open confirms for it, when abs lies inside the working
// directory. ok is false when there is no working directory, when abs is
// outside it, or when abs resolves to another file than the one the name
// designates: a catalog file that is a symlink into the working directory
// would otherwise have the studio open and save that other file under the
// example's name.
func (s *Server) workDirRelative(abs, name string) (rel, confirmed string, ok bool) {
	s.stateMu.RLock()
	workDir, examplesDir := s.cfg.WorkDir, s.cfg.ExamplesDir
	s.stateMu.RUnlock()
	if workDir == "" || examplesDir == "" {
		return "", "", false
	}
	base, err := filepath.Abs(workDir)
	if err != nil {
		return "", "", false
	}
	baseReal, err := filepath.EvalSymlinks(base)
	if err != nil {
		return "", "", false
	}
	absReal, err := filepath.EvalSymlinks(abs)
	if err != nil || !pathContains(baseReal, absReal) {
		return "", "", false
	}
	examplesReal, err := filepath.EvalSymlinks(examplesDir)
	if err != nil || absReal != filepath.Join(examplesReal, filepath.FromSlash(name)) {
		return "", "", false
	}
	r, err := filepath.Rel(baseReal, absReal)
	if err != nil {
		return "", "", false
	}
	confirmed, err = s.safePath(r)
	if err != nil {
		return "", "", false
	}
	return filepath.ToSlash(r), confirmed, true
}
