package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/store"
)

const botSourceEditorScheme = "botsource://"

type authoringSnapshotRequest struct {
	EditorPath string `json:"editor_path"`
}

type authoringFileSnapshot struct {
	Scope     string `json:"scope"`
	Path      string `json:"path"`
	Size      int    `json:"size"`
	SHA256    string `json:"sha256,omitempty"`
	Available bool   `json:"available"`
	Readable  bool   `json:"readable"`
	Reason    string `json:"reason,omitempty"`
}

type authoringSnapshotResponse struct {
	EditorPath string                  `json:"editor_path"`
	Version    int                     `json:"version,omitempty"`
	Files      []authoringFileSnapshot `json:"files"`
}

type authoringReplacement struct {
	Before string `json:"before"`
	After  string `json:"after"`
}

type authoringFileChange struct {
	Scope          string                 `json:"scope"`
	Path           string                 `json:"path"`
	ExpectedSHA256 string                 `json:"expected_sha256"`
	Replacements   []authoringReplacement `json:"replacements"`
}

type authoringChangeRequest struct {
	EditorPath string                `json:"editor_path"`
	Version    int                   `json:"version,omitempty"`
	Changes    []authoringFileChange `json:"changes"`
}

type authoringPreviewFile struct {
	Scope  string `json:"scope"`
	Path   string `json:"path"`
	Before string `json:"before"`
	After  string `json:"after"`
}

type authoringChangeResponse struct {
	Files   []authoringPreviewFile `json:"files"`
	Version int                    `json:"version,omitempty"`
	Saved   bool                   `json:"saved"`
}

type authoringTarget struct {
	editorPath string
	manifest   *bundle.Manifest
	bundleDir  string
	workDir    string
	teamID     string
	slug       string
	version    int
	files      map[string]string // cloud bundle contents; nil for local
	userID     string
}

type resolvedAuthoringFile struct {
	spec bundle.AuthoringEditableFile
	abs  string
}

type authoringLimits struct {
	maxFiles        int
	maxPerFile      int
	maxReplacements int
	maxBlockBytes   int
	maxTotalBytes   int
}

func currentAuthoringLimits() authoringLimits {
	return authoringLimits{
		maxFiles:        envPositiveInt("ITERION_ASSISTANT_AUTHORING_MAX_FILES", 8),
		maxPerFile:      envPositiveInt("ITERION_ASSISTANT_AUTHORING_MAX_REPLACEMENTS_PER_FILE", 16),
		maxReplacements: envPositiveInt("ITERION_ASSISTANT_AUTHORING_MAX_REPLACEMENTS", 64),
		maxBlockBytes:   envPositiveInt("ITERION_ASSISTANT_AUTHORING_MAX_BLOCK_BYTES", 32<<10),
		maxTotalBytes:   envPositiveInt("ITERION_ASSISTANT_AUTHORING_MAX_TOTAL_BYTES", 256<<10),
	}
}

func envPositiveInt(name string, fallback int) int {
	v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if err != nil || v <= 0 {
		return fallback
	}
	return v
}

func (s *Server) handleAuthoringSnapshot(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	var req authoringSnapshotRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	target, err := s.resolveAuthoringTarget(r, req.EditorPath)
	if err != nil {
		s.authoringError(w, r, err)
		return
	}
	out := authoringSnapshotResponse{EditorPath: target.editorPath, Version: target.version, Files: []authoringFileSnapshot{}}
	for _, declared := range target.manifest.Authoring.EditableFiles {
		resolved, content, available, reason, err := target.readDeclared(declared)
		if err != nil {
			s.authoringError(w, r, err)
			return
		}
		item := authoringFileSnapshot{Scope: declared.Scope, Path: declared.Path, Available: available, Readable: target.files == nil && available, Reason: reason}
		if available {
			item.Size = len(content)
			item.SHA256 = contentSHA256(content)
			_ = resolved
		}
		out.Files = append(out.Files, item)
	}
	writeJSON(w, out)
}

func (s *Server) handleAuthoringPreview(w http.ResponseWriter, r *http.Request) {
	s.handleAuthoringChange(w, r, false)
}

func (s *Server) handleAuthoringCommit(w http.ResponseWriter, r *http.Request) {
	s.handleAuthoringChange(w, r, true)
}

func (s *Server) handleAuthoringChange(w http.ResponseWriter, r *http.Request, commit bool) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	var req authoringChangeRequest
	r.Body = http.MaxBytesReader(w, r.Body, int64(currentAuthoringLimits().maxTotalBytes*8+(64<<10)))
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid body: %v", err)
		return
	}
	if err := validateAuthoringChangesShape(req.Changes, currentAuthoringLimits()); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "%v", err)
		return
	}
	target, err := s.resolveAuthoringTarget(r, req.EditorPath)
	if err != nil {
		s.authoringError(w, r, err)
		return
	}
	// Cloud CAS is checked before reading any proposed path. Local files use
	// one hash per file below, because the workspace has no aggregate version.
	if target.files != nil && req.Version != target.version {
		s.httpErrorFor(w, r, http.StatusConflict, "bot source version changed (expected %d, current %d)", req.Version, target.version)
		return
	}
	previews, resolved, err := target.preview(req.Changes)
	if err != nil {
		s.authoringError(w, r, err)
		return
	}
	if err := target.validateChangedBots(previews); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "bot does not compile: %v", err)
		return
	}
	if !commit {
		writeJSON(w, authoringChangeResponse{Files: previews, Version: target.version, Saved: false})
		return
	}
	version, err := s.commitAuthoring(r, target, previews, resolved)
	if err != nil {
		s.authoringError(w, r, err)
		return
	}
	writeJSON(w, authoringChangeResponse{Files: previews, Version: version, Saved: true})
}

func validateAuthoringChangesShape(changes []authoringFileChange, limits authoringLimits) error {
	if len(changes) == 0 {
		return errors.New("changes must not be empty")
	}
	if len(changes) > limits.maxFiles {
		return fmt.Errorf("changes has %d files, over the %d-file limit (override with ITERION_ASSISTANT_AUTHORING_MAX_FILES)", len(changes), limits.maxFiles)
	}
	seen := map[string]bool{}
	totalReplacements, totalBytes := 0, 0
	for i, change := range changes {
		key := strings.TrimSpace(change.Scope) + ":" + strings.TrimSpace(change.Path)
		if seen[key] {
			return fmt.Errorf("changes[%d] duplicates %s", i, key)
		}
		seen[key] = true
		if len(change.Replacements) == 0 {
			return fmt.Errorf("changes[%d].replacements must not be empty", i)
		}
		if len(change.Replacements) > limits.maxPerFile {
			return fmt.Errorf("changes[%d] has too many replacements (override with ITERION_ASSISTANT_AUTHORING_MAX_REPLACEMENTS_PER_FILE)", i)
		}
		totalReplacements += len(change.Replacements)
		for j, replacement := range change.Replacements {
			if replacement.Before == "" {
				return fmt.Errorf("changes[%d].replacements[%d].before must not be empty (file creation is not supported in v1)", i, j)
			}
			if len(replacement.Before) > limits.maxBlockBytes || len(replacement.After) > limits.maxBlockBytes {
				return fmt.Errorf("changes[%d].replacements[%d] exceeds the block limit (override with ITERION_ASSISTANT_AUTHORING_MAX_BLOCK_BYTES)", i, j)
			}
			totalBytes += len(replacement.Before) + len(replacement.After)
		}
	}
	if totalReplacements > limits.maxReplacements {
		return fmt.Errorf("changes have too many replacements (override with ITERION_ASSISTANT_AUTHORING_MAX_REPLACEMENTS)")
	}
	if totalBytes > limits.maxTotalBytes {
		return fmt.Errorf("changes exceed the cumulative byte limit (override with ITERION_ASSISTANT_AUTHORING_MAX_TOTAL_BYTES)")
	}
	return nil
}

func (s *Server) resolveAuthoringTarget(r *http.Request, editorPath string) (*authoringTarget, error) {
	editorPath = strings.TrimSpace(editorPath)
	if editorPath == "" {
		return nil, errors.New("editor_path is required")
	}
	if strings.HasPrefix(editorPath, botSourceEditorScheme) {
		if s.botSources == nil {
			return nil, errors.New("bot editing is not enabled on this server")
		}
		teamID, slug, _, err := parseBotSourceEditorPathServer(editorPath)
		if err != nil {
			return nil, err
		}
		id, ok := auth.FromContext(r.Context())
		if !ok || !s.canEditBots(r.Context(), id, teamID) {
			return nil, authoringForbiddenError{"bot editor, team admin, or owner required"}
		}
		bs, err := s.botSources.GetBySlug(store.WithTenant(r.Context(), teamID), teamID, slug)
		if err != nil {
			return nil, err
		}
		m := bs.Manifest()
		if m == nil || m.Authoring == nil || len(m.Authoring.EditableFiles) == 0 {
			return nil, errors.New("the bot manifest declares no authoring.editable_files")
		}
		return &authoringTarget{editorPath: editorPath, manifest: m, teamID: teamID, slug: slug, version: bs.Version, files: bs.Files, userID: id.UserID}, nil
	}
	absEditor, err := s.safePath(editorPath)
	if err != nil {
		return nil, fmt.Errorf("invalid editor_path: %w", err)
	}
	s.stateMu.RLock()
	workDir := s.cfg.WorkDir
	s.stateMu.RUnlock()
	bundleDir, m, err := findAuthoringManifest(absEditor, workDir)
	if err != nil {
		return nil, err
	}
	return &authoringTarget{editorPath: editorPath, manifest: m, bundleDir: bundleDir, workDir: workDir}, nil
}

func findAuthoringManifest(editorPath, workDir string) (string, *bundle.Manifest, error) {
	dir := filepath.Dir(editorPath)
	root, err := filepath.Abs(workDir)
	if err != nil {
		return "", nil, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", nil, err
	}
	for pathContains(root, dir) {
		m, err := bundle.LoadManifest(filepath.Join(dir, bundle.ManifestFile))
		if err != nil {
			return "", nil, err
		}
		if m != nil {
			if m.Authoring == nil || len(m.Authoring.EditableFiles) == 0 {
				return "", nil, errors.New("the bot manifest declares no authoring.editable_files")
			}
			return dir, m, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", nil, errors.New("no bundle manifest found for the active editor file")
}

func parseBotSourceEditorPathServer(value string) (teamID, slug, rel string, err error) {
	parts := strings.Split(strings.TrimPrefix(value, botSourceEditorScheme), "/")
	if len(parts) < 3 || parts[0] == "" || parts[1] == "" {
		return "", "", "", errors.New("invalid botsource editor path")
	}
	return parts[0], parts[1], strings.Join(parts[2:], "/"), nil
}

func (t *authoringTarget) declared(scope, path string) (bundle.AuthoringEditableFile, bool) {
	scope = strings.ToLower(strings.TrimSpace(scope))
	path = filepath.ToSlash(filepath.Clean(strings.TrimSpace(path)))
	for _, f := range t.manifest.Authoring.EditableFiles {
		if f.Scope == scope && f.Path == path {
			return f, true
		}
	}
	return bundle.AuthoringEditableFile{}, false
}

func (t *authoringTarget) readDeclared(spec bundle.AuthoringEditableFile) (resolvedAuthoringFile, string, bool, string, error) {
	if t.files != nil {
		if spec.Scope == bundle.AuthoringScopeWorkspace {
			return resolvedAuthoringFile{spec: spec}, "", false, "workspace files are unavailable without a connected repository", nil
		}
		content, ok := t.files[spec.Path]
		if !ok {
			return resolvedAuthoringFile{spec: spec}, "", false, "declared file does not exist", nil
		}
		return resolvedAuthoringFile{spec: spec}, content, true, "", nil
	}
	base := t.bundleDir
	if spec.Scope == bundle.AuthoringScopeWorkspace {
		base = t.workDir
	}
	abs, err := safePathWithin(base, spec.Path)
	if err != nil {
		return resolvedAuthoringFile{}, "", false, "", fmt.Errorf("%s:%s: %w", spec.Scope, spec.Path, err)
	}
	body, err := os.ReadFile(abs) // #nosec G304 -- symlink-aware safePathWithin result
	if errors.Is(err, os.ErrNotExist) {
		return resolvedAuthoringFile{spec: spec, abs: abs}, "", false, "declared file does not exist", nil
	}
	if err != nil {
		return resolvedAuthoringFile{}, "", false, "", err
	}
	if !utf8.Valid(body) {
		return resolvedAuthoringFile{}, "", false, "", fmt.Errorf("%s:%s is not UTF-8 text", spec.Scope, spec.Path)
	}
	return resolvedAuthoringFile{spec: spec, abs: abs}, string(body), true, "", nil
}

func (t *authoringTarget) preview(changes []authoringFileChange) ([]authoringPreviewFile, []resolvedAuthoringFile, error) {
	previews := make([]authoringPreviewFile, 0, len(changes))
	resolved := make([]resolvedAuthoringFile, 0, len(changes))
	for i, change := range changes {
		declared, ok := t.declared(change.Scope, change.Path)
		if !ok {
			// Crucially before readDeclared: preview cannot be used as an oracle
			// for arbitrary workspace paths.
			return nil, nil, fmt.Errorf("changes[%d]: %s:%s is outside authoring.editable_files", i, change.Scope, change.Path)
		}
		file, content, available, reason, err := t.readDeclared(declared)
		if err != nil {
			return nil, nil, err
		}
		if !available {
			return nil, nil, fmt.Errorf("changes[%d]: %s:%s is unavailable: %s", i, declared.Scope, declared.Path, reason)
		}
		if change.ExpectedSHA256 == "" || change.ExpectedSHA256 != contentSHA256(content) {
			return nil, nil, authoringConflictError{fmt.Sprintf("%s:%s changed since the assistant snapshot", declared.Scope, declared.Path)}
		}
		next := content
		for j, replacement := range change.Replacements {
			matches := strings.Count(next, replacement.Before)
			if matches != 1 {
				return nil, nil, fmt.Errorf("changes[%d].replacements[%d]: before text matched %d times, want exactly once", i, j, matches)
			}
			next = strings.Replace(next, replacement.Before, replacement.After, 1)
		}
		if !utf8.ValidString(next) {
			return nil, nil, fmt.Errorf("changes[%d] result is not UTF-8 text", i)
		}
		previews = append(previews, authoringPreviewFile{Scope: declared.Scope, Path: declared.Path, Before: content, After: next})
		resolved = append(resolved, file)
	}
	return previews, resolved, nil
}

func (t *authoringTarget) validateChangedBots(previews []authoringPreviewFile) error {
	var modified []string
	if t.files != nil {
		files := cloneAuthoringStringMap(t.files)
		for _, p := range previews {
			if p.Scope == bundle.AuthoringScopeBundle {
				files[p.Path] = p.After
				if strings.HasSuffix(strings.ToLower(p.Path), ".bot") {
					modified = append(modified, p.Path)
				}
			}
		}
		if err := validateChangedAuthoringManifest(files, previews); err != nil {
			return err
		}
		if len(modified) == 0 {
			return nil
		}
		if diags := validateBundleCompileSelected(files, modified); len(diags) > 0 {
			return errors.New(strings.Join(diags, "; "))
		}
		return nil
	}
	files, err := botsource.ReadBundleDir(t.bundleDir)
	if err != nil {
		return err
	}
	for i, p := range previews {
		if p.Scope == bundle.AuthoringScopeBundle {
			files[p.Path] = p.After
			if strings.HasSuffix(strings.ToLower(p.Path), ".bot") {
				modified = append(modified, p.Path)
			}
		} else if strings.HasSuffix(strings.ToLower(p.Path), ".bot") {
			// A workspace .bot is compiled in the same materialized bundle so
			// declared prompts are available, but under a synthetic safe path —
			// it is never persisted into the bundle.
			rel := fmt.Sprintf("__authoring_workspace/%d-%s", i, filepath.Base(p.Path))
			files[rel] = p.After
			modified = append(modified, rel)
		}
	}
	if err := validateChangedAuthoringManifest(files, previews); err != nil {
		return err
	}
	if len(modified) == 0 {
		return nil
	}
	if diags := validateBundleCompileSelected(files, modified); len(diags) > 0 {
		return errors.New(strings.Join(diags, "; "))
	}
	return nil
}

func validateChangedAuthoringManifest(files map[string]string, previews []authoringPreviewFile) error {
	for _, p := range previews {
		if p.Scope != bundle.AuthoringScopeBundle || (p.Path != bundle.ManifestFile && p.Path != bundle.ManifestFileAlt) {
			continue
		}
		if _, err := bundle.DecodeManifest([]byte(files[p.Path]), p.Path); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) commitAuthoring(r *http.Request, target *authoringTarget, previews []authoringPreviewFile, resolved []resolvedAuthoringFile) (int, error) {
	if target.files != nil {
		bs, err := s.botSources.GetBySlug(store.WithTenant(r.Context(), target.teamID), target.teamID, target.slug)
		if err != nil {
			return 0, err
		}
		if bs.Version != target.version {
			return 0, authoringConflictError{"bot source changed before commit"}
		}
		files := cloneAuthoringStringMap(bs.Files)
		for _, p := range previews {
			if p.Scope != bundle.AuthoringScopeBundle {
				return 0, errors.New("cloud workspace files are not writable without a connected repository")
			}
			files[p.Path] = p.After
		}
		bs.Files = files
		bs.Version = target.version
		bs.UpdatedBy = target.userID
		if err := bs.Validate(); err != nil {
			return 0, err
		}
		out, err := s.botSources.Update(store.WithTenant(r.Context(), target.teamID), bs)
		if err != nil {
			return 0, err
		}
		s.auditBotSource(r, target.teamID, "updated", out)
		return out.Version, nil
	}
	// Re-read every destination immediately before the first write. The
	// preview above already checked hashes, but bot compilation can take long
	// enough for an IDE save to race it; fail the whole batch before touching
	// disk instead of overwriting that newer content.
	for i := range previews {
		body, err := os.ReadFile(resolved[i].abs) // #nosec G304 -- resolved manifest path
		if err != nil {
			return 0, err
		}
		if string(body) != previews[i].Before {
			return 0, authoringConflictError{fmt.Sprintf("%s:%s changed before commit", previews[i].Scope, previews[i].Path)}
		}
	}
	written := make([]int, 0, len(previews))
	for i, p := range previews {
		if s.watcher != nil {
			s.watcher.IgnorePath(resolved[i].abs)
		}
		if err := store.WriteFileAtomic(resolved[i].abs, []byte(p.After), 0o644); err != nil {
			rollbackErrs := make([]string, 0, len(written))
			for j := len(written) - 1; j >= 0; j-- {
				idx := written[j]
				if rollbackErr := store.WriteFileAtomic(resolved[idx].abs, []byte(previews[idx].Before), 0o644); rollbackErr != nil {
					rollbackErrs = append(rollbackErrs, fmt.Sprintf("%s:%s: %v", previews[idx].Scope, previews[idx].Path, rollbackErr))
				}
			}
			if len(rollbackErrs) > 0 {
				return 0, fmt.Errorf("write %s:%s failed: %w; rollback also failed: %s", p.Scope, p.Path, err, strings.Join(rollbackErrs, "; "))
			}
			return 0, fmt.Errorf("write %s:%s failed: %w; earlier files were rolled back", p.Scope, p.Path, err)
		}
		written = append(written, i)
	}
	return 0, nil
}

func contentSHA256(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func cloneAuthoringStringMap(src map[string]string) map[string]string {
	out := make(map[string]string, len(src))
	for key, value := range src {
		out[key] = value
	}
	return out
}

type authoringConflictError struct{ message string }

func (e authoringConflictError) Error() string { return e.message }

type authoringForbiddenError struct{ message string }

func (e authoringForbiddenError) Error() string { return e.message }

func (s *Server) authoringError(w http.ResponseWriter, r *http.Request, err error) {
	var conflict authoringConflictError
	var forbidden authoringForbiddenError
	switch {
	case errors.As(err, &conflict), errors.Is(err, botsource.ErrVersionConflict):
		s.httpErrorFor(w, r, http.StatusConflict, "%v", err)
	case errors.As(err, &forbidden):
		s.httpErrorFor(w, r, http.StatusForbidden, "%v", err)
	case errors.Is(err, botsource.ErrNotFound), errors.Is(err, os.ErrNotExist):
		s.httpErrorFor(w, r, http.StatusNotFound, "%v", err)
	default:
		s.httpErrorFor(w, r, http.StatusBadRequest, "%v", err)
	}
}
