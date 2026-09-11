package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/SocialGouv/iterion/internal/httpx"
	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/bundle"
	gitlib "github.com/SocialGouv/iterion/pkg/git"
	"github.com/SocialGouv/iterion/pkg/store"
)

const botSourceEditorScheme = "botsource://"

type authoringSnapshotRequest struct {
	EditorPath string `json:"editor_path"`
}

type authoringFileSnapshot struct {
	Scope              string `json:"scope"`
	Path               string `json:"path"`
	Size               int    `json:"size"`
	SHA256             string `json:"sha256,omitempty"`
	Available          bool   `json:"available"`
	Readable           bool   `json:"readable"`
	GitCommittable     bool   `json:"git_committable"`
	IsManifestDeclared bool   `json:"is_manifest_declared"`
	Reason             string `json:"reason,omitempty"`
}

type authoringSnapshotResponse struct {
	EditorPath string                  `json:"editor_path"`
	Version    int                     `json:"version,omitempty"`
	Files      []authoringFileSnapshot `json:"files"`
	// ActiveFile is the host-attested bundle-relative identity of the editor
	// file when that exact file is in the bounded replacement perimeter. It
	// deliberately carries no absolute workspace path.
	ActiveFile *authoringActiveFile `json:"active_file,omitempty"`
}

type authoringActiveFile struct {
	Scope string `json:"scope"`
	Path  string `json:"path"`
}

type authoringReplacement struct {
	Before string `json:"before"`
	After  string `json:"after"`
}

type authoringCreate struct {
	Content string `json:"content"`
}

type authoringFileChange struct {
	Scope          string                 `json:"scope"`
	Path           string                 `json:"path"`
	ExpectedSHA256 string                 `json:"expected_sha256,omitempty"`
	Replacements   []authoringReplacement `json:"replacements,omitempty"`
	Create         *authoringCreate       `json:"create,omitempty"`
}

type authoringChangeRequest struct {
	EditorPath string                `json:"editor_path"`
	Version    int                   `json:"version,omitempty"`
	Changes    []authoringFileChange `json:"changes"`
}

type authoringPreviewFile struct {
	Scope     string `json:"scope"`
	Path      string `json:"path"`
	Operation string `json:"operation"`
	Before    string `json:"before"`
	After     string `json:"after"`
}

type authoringChangeResponse struct {
	Files      []authoringPreviewFile    `json:"files"`
	Version    int                       `json:"version,omitempty"`
	Saved      bool                      `json:"saved"`
	Validation authoringValidationResult `json:"validation"`
}

type authoringValidationCheck struct {
	Kind    string `json:"kind"`
	Path    string `json:"path"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

type authoringValidationResult struct {
	Checks          []authoringValidationCheck `json:"checks"`
	BehavioralTests string                     `json:"behavioral_tests"`
}

type authoringValidationFailure struct {
	Code    string
	Message string
}

func (e authoringValidationFailure) Error() string { return e.Message }

// authoringGitCommitRequest is intentionally not a general Git API. The
// browser derives every file's scope and expected hash from a current,
// host-attested authoring snapshot; the model can only select a subset of that
// declared perimeter and supply a bounded message.
type authoringGitCommitRequest struct {
	EditorPath string                   `json:"editor_path"`
	Files      []authoringGitCommitFile `json:"files"`
	Message    string                   `json:"message"`
}

type authoringGitCommitFile struct {
	Scope          string `json:"scope"`
	Path           string `json:"path"`
	ExpectedSHA256 string `json:"expected_sha256"`
}

type authoringGitCommitResponse struct {
	Commit string   `json:"commit"`
	Files  []string `json:"files"`
}

type authoringGitPublishRequest struct {
	EditorPath string `json:"editor_path"`
	Commit     string `json:"commit"`
	Branch     string `json:"branch"`
}

type authoringGitPublishResponse struct {
	Commit string `json:"commit"`
	Branch string `json:"branch"`
}

type authoringTarget struct {
	editorPath string
	// activeBundlePath is the normalized bundle-relative location of the
	// editor file. It is empty for a workspace file or an authoring bootstrap.
	activeBundlePath string
	manifest         *bundle.Manifest
	// bootstrapManifest is set only for a local bundle whose valid manifest
	// declares no authoring perimeter yet. It grants a single, deliberately
	// narrow first change: let the assistant propose the manifest that defines
	// its future companion-file perimeter. Once that manifest declares even one
	// editable file, normal explicit-scope rules take over on the next capture.
	bootstrapManifest string
	bundleDir         string
	workDir           string
	readOnlyRoot      string
	teamID            string
	slug              string
	version           int
	files             map[string]string // cloud bundle contents; nil for local
	userID            string
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
	active, hasActive := target.activeBundleFile()
	for _, replaceable := range target.replaceableFiles() {
		resolved, content, available, reason, err := target.readDeclared(replaceable)
		if err != nil {
			s.authoringError(w, r, err)
			return
		}
		item := authoringFileSnapshot{
			Scope:              replaceable.Scope,
			Path:               replaceable.Path,
			Available:          available,
			Readable:           target.files == nil && available,
			GitCommittable:     target.isManifestDeclared(replaceable.Scope, replaceable.Path),
			IsManifestDeclared: target.isManifestDeclared(replaceable.Scope, replaceable.Path),
			Reason:             reason,
		}
		if available {
			item.Size = len(content)
			item.SHA256 = contentSHA256(content)
			_ = resolved
		}
		out.Files = append(out.Files, item)
		if hasActive && available && sameAuthoringFile(replaceable, active) {
			out.ActiveFile = &authoringActiveFile{Scope: replaceable.Scope, Path: replaceable.Path}
		}
	}
	writeJSON(w, out)
}

func (s *Server) handleAuthoringPreview(w http.ResponseWriter, r *http.Request) {
	s.handleAuthoringChange(w, r, false)
}

func (s *Server) handleAuthoringCommit(w http.ResponseWriter, r *http.Request) {
	s.handleAuthoringChange(w, r, true)
}

func (s *Server) handleAuthoringGitCommit(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	var req authoringGitCommitRequest
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid body: %v", err)
		return
	}
	if err := validateAuthoringGitCommitShape(req); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "%v", err)
		return
	}
	target, err := s.resolveAuthoringTarget(r, req.EditorPath)
	if err != nil {
		s.authoringError(w, r, err)
		return
	}
	commit, files, err := authoringGitCommit(r, target, req)
	if err != nil {
		s.authoringError(w, r, err)
		return
	}
	writeJSON(w, authoringGitCommitResponse{Commit: commit, Files: files})
}

func (s *Server) handleAuthoringGitPublish(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	var req authoringGitPublishRequest
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid body: %v", err)
		return
	}
	if err := validateAuthoringGitPublishShape(req); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "%v", err)
		return
	}
	target, err := s.resolveAuthoringTarget(r, req.EditorPath)
	if err != nil {
		s.authoringError(w, r, err)
		return
	}
	commit, err := authoringGitPublish(r, target, req)
	if err != nil {
		s.authoringError(w, r, err)
		return
	}
	writeJSON(w, authoringGitPublishResponse{Commit: commit, Branch: req.Branch})
}

func validateAuthoringGitCommitShape(req authoringGitCommitRequest) error {
	if strings.TrimSpace(req.EditorPath) == "" {
		return errors.New("editor_path is required")
	}
	message := strings.TrimSpace(req.Message)
	if message == "" || len(message) > 240 || strings.ContainsAny(message, "\x00\r\n") {
		return errors.New("message must be one non-empty line of at most 240 characters")
	}
	if len(req.Files) == 0 || len(req.Files) > currentAuthoringLimits().maxFiles {
		return fmt.Errorf("files must contain between 1 and %d declared authoring files", currentAuthoringLimits().maxFiles)
	}
	seen := make(map[string]bool, len(req.Files))
	for i, file := range req.Files {
		key := strings.TrimSpace(file.Scope) + ":" + filepath.ToSlash(filepath.Clean(strings.TrimSpace(file.Path)))
		if file.Scope == "" || strings.TrimSpace(file.Path) == "" || key == ".:." || seen[key] {
			return fmt.Errorf("files[%d] must identify one unique declared file", i)
		}
		seen[key] = true
		if len(file.ExpectedSHA256) != sha256.Size*2 {
			return fmt.Errorf("files[%d].expected_sha256 is invalid", i)
		}
	}
	return nil
}

var authoringCommitPrefix = regexp.MustCompile(`^[0-9a-f]{12,40}$`)
var authoringBranch = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$`)

func validateAuthoringGitPublishShape(req authoringGitPublishRequest) error {
	if strings.TrimSpace(req.EditorPath) == "" {
		return errors.New("editor_path is required")
	}
	if !authoringCommitPrefix.MatchString(req.Commit) {
		return errors.New("commit must be a 12-to-40 character lowercase hexadecimal prefix")
	}
	if !authoringBranch.MatchString(req.Branch) || strings.Contains(req.Branch, "..") {
		return errors.New("branch is invalid")
	}
	return nil
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
	validation, err := target.validateChanges(r.Context(), previews)
	if err != nil {
		var failure authoringValidationFailure
		if errors.As(err, &failure) {
			s.writeAuthoringValidationError(w, r, failure)
			return
		}
		s.httpErrorFor(w, r, http.StatusBadRequest, "%v", err)
		return
	}
	if !commit {
		writeJSON(w, authoringChangeResponse{Files: previews, Version: target.version, Saved: false, Validation: validation})
		return
	}
	version, err := s.commitAuthoring(r, target, previews, resolved)
	if err != nil {
		s.authoringError(w, r, err)
		return
	}
	writeJSON(w, authoringChangeResponse{Files: previews, Version: version, Saved: true, Validation: validation})
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
		scope := strings.ToLower(strings.TrimSpace(change.Scope))
		path := filepath.ToSlash(filepath.Clean(strings.TrimSpace(change.Path)))
		if scope == "" || path == "" || path == "." {
			return fmt.Errorf("changes[%d] must identify one file", i)
		}
		key := scope + ":" + path
		if seen[key] {
			return fmt.Errorf("changes[%d] duplicates %s", i, key)
		}
		seen[key] = true
		if change.Create != nil {
			if len(change.Replacements) != 0 || change.ExpectedSHA256 != "" {
				return fmt.Errorf("changes[%d] must use either create or replacements, never both", i)
			}
			if len(change.Create.Content) > limits.maxBlockBytes {
				return fmt.Errorf("changes[%d].create.content exceeds the block limit (override with ITERION_ASSISTANT_AUTHORING_MAX_BLOCK_BYTES)", i)
			}
			totalBytes += len(change.Create.Content)
			continue
		}
		if len(change.Replacements) == 0 {
			return fmt.Errorf("changes[%d].replacements must not be empty when create is absent", i)
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
		teamID, slug, rel, err := parseBotSourceEditorPathServer(editorPath)
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
		if m == nil {
			return nil, errors.New("the bot source has no valid manifest")
		}
		activePath := filepath.ToSlash(filepath.Clean(rel))
		if activePath == "." || activePath == ".." || strings.HasPrefix(activePath, "../") {
			return nil, errors.New("active editor file is outside its bundle")
		}
		if _, ok := bs.Files[activePath]; !ok {
			return nil, errors.New("active editor file is absent from the bot source")
		}
		return &authoringTarget{editorPath: editorPath, activeBundlePath: activePath, manifest: m, teamID: teamID, slug: slug, version: bs.Version, files: bs.Files, userID: id.UserID}, nil
	}
	absEditor, err := s.safePath(editorPath)
	if err != nil {
		return nil, fmt.Errorf("invalid editor_path: %w", err)
	}
	if s.isMaterializedBotDependencyPath(absEditor) {
		return nil, authoringForbiddenError{"shared bot bundles under .botz are read-only; edit the source bundle instead"}
	}
	s.stateMu.RLock()
	workDir := s.cfg.WorkDir
	s.stateMu.RUnlock()
	bundleDir, manifestFile, m, err := findAuthoringManifest(absEditor, workDir)
	if err != nil {
		return nil, err
	}
	readOnlyRoot, err := s.materializedBotDependenciesRoot()
	if err != nil {
		return nil, fmt.Errorf("resolve read-only bot dependencies: %w", err)
	}
	rel, err := filepath.Rel(bundleDir, absEditor)
	if err != nil {
		return nil, fmt.Errorf("resolve active bundle path: %w", err)
	}
	rel = filepath.Clean(rel)
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, errors.New("active editor file is outside its bundle")
	}
	target := &authoringTarget{editorPath: editorPath, activeBundlePath: filepath.ToSlash(rel), manifest: m, bundleDir: bundleDir, workDir: workDir, readOnlyRoot: readOnlyRoot}
	if m.Authoring == nil || len(m.Authoring.EditableFiles) == 0 {
		// A new local bundle otherwise has no path through the assistant to
		// declare its first exact companion file. Do not treat this as a broad
		// fallback: the manifest is the sole bootstrap target and all normal
		// hash, replacement, parse, compile and write-race guards still apply.
		target.bootstrapManifest = manifestFile
	}
	return target, nil
}

func findAuthoringManifest(editorPath, workDir string) (string, string, *bundle.Manifest, error) {
	dir := filepath.Dir(editorPath)
	root, err := filepath.Abs(workDir)
	if err != nil {
		return "", "", nil, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", "", nil, err
	}
	for pathContains(root, dir) {
		for _, manifestFile := range []string{bundle.ManifestFile, bundle.ManifestFileAlt} {
			m, err := bundle.LoadManifest(filepath.Join(dir, manifestFile))
			if err != nil {
				return "", "", nil, err
			}
			if m != nil {
				return dir, manifestFile, m, nil
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", "", nil, errors.New("no bundle manifest found for the active editor file")
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
	for _, f := range t.editableFiles() {
		if f.Scope == scope && f.Path == path {
			return f, true
		}
	}
	return bundle.AuthoringEditableFile{}, false
}

func (t *authoringTarget) replaceable(scope, path string) (bundle.AuthoringEditableFile, bool) {
	scope = strings.ToLower(strings.TrimSpace(scope))
	path = filepath.ToSlash(filepath.Clean(strings.TrimSpace(path)))
	for _, f := range t.replaceableFiles() {
		if f.Scope == scope && f.Path == path {
			return f, true
		}
	}
	return bundle.AuthoringEditableFile{}, false
}

func (t *authoringTarget) activeBundleFile() (bundle.AuthoringEditableFile, bool) {
	if t.activeBundlePath == "" {
		return bundle.AuthoringEditableFile{}, false
	}
	return bundle.AuthoringEditableFile{Scope: bundle.AuthoringScopeBundle, Path: t.activeBundlePath}, true
}

func (t *authoringTarget) editableFiles() []bundle.AuthoringEditableFile {
	if t.bootstrapManifest != "" {
		return []bundle.AuthoringEditableFile{{
			Scope: bundle.AuthoringScopeBundle,
			Path:  t.bootstrapManifest,
		}}
	}
	if t.manifest == nil || t.manifest.Authoring == nil {
		return nil
	}
	return t.manifest.Authoring.EditableFiles
}

func (t *authoringTarget) replaceableFiles() []bundle.AuthoringEditableFile {
	files := append([]bundle.AuthoringEditableFile(nil), t.editableFiles()...)
	active, ok := t.activeBundleFile()
	if !ok {
		return files
	}
	for _, file := range files {
		if sameAuthoringFile(file, active) {
			return files
		}
	}
	return append(files, active)
}

func (t *authoringTarget) isManifestDeclared(scope, path string) bool {
	if t.bootstrapManifest != "" || t.manifest == nil || t.manifest.Authoring == nil {
		return false
	}
	scope = strings.ToLower(strings.TrimSpace(scope))
	path = filepath.ToSlash(filepath.Clean(strings.TrimSpace(path)))
	for _, file := range t.manifest.Authoring.EditableFiles {
		if file.Scope == scope && file.Path == path {
			return true
		}
	}
	return false
}

func sameAuthoringFile(a, b bundle.AuthoringEditableFile) bool {
	return strings.EqualFold(strings.TrimSpace(a.Scope), strings.TrimSpace(b.Scope)) &&
		filepath.ToSlash(filepath.Clean(strings.TrimSpace(a.Path))) == filepath.ToSlash(filepath.Clean(strings.TrimSpace(b.Path)))
}

func (t *authoringTarget) readDeclared(spec bundle.AuthoringEditableFile) (resolvedAuthoringFile, string, bool, string, error) {
	if t.files != nil {
		if spec.Scope == bundle.AuthoringScopeWorkspace {
			return resolvedAuthoringFile{spec: spec}, "", false, "cloud_workspace_unavailable", nil
		}
		content, ok := t.files[spec.Path]
		if !ok {
			return resolvedAuthoringFile{spec: spec}, "", false, "declared_missing_cloud_file", nil
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
	if t.readOnlyRoot != "" && pathContains(t.readOnlyRoot, abs) {
		return resolvedAuthoringFile{}, "", false, "", authoringForbiddenError{"shared bot bundles under .botz are read-only; edit the source bundle instead"}
	}
	body, err := os.ReadFile(abs) // #nosec G304 -- symlink-aware safePathWithin result
	if errors.Is(err, os.ErrNotExist) {
		return resolvedAuthoringFile{spec: spec, abs: abs}, "", false, "declared_missing_local_file", nil
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
		if change.Create != nil {
			if t.files != nil {
				return nil, nil, authoringForbiddenError{"file creation is unavailable for cloud bot sources"}
			}
			declared, ok := t.manifestDeclared(change.Scope, change.Path)
			if !ok {
				return nil, nil, fmt.Errorf("changes[%d]: %s:%s is not explicitly declared in authoring.editable_files", i, change.Scope, change.Path)
			}
			file, _, available, reason, err := t.readDeclared(declared)
			if err != nil {
				return nil, nil, err
			}
			if available {
				return nil, nil, authoringFileExistsError{fmt.Sprintf("%s:%s already exists", declared.Scope, declared.Path)}
			}
			if reason != "declared_missing_local_file" || t.files != nil {
				return nil, nil, fmt.Errorf("changes[%d]: %s:%s cannot be created from this authoring target: %s", i, declared.Scope, declared.Path, reason)
			}
			if err := t.validateCreateDestination(declared, file.abs); err != nil {
				return nil, nil, err
			}
			if !utf8.ValidString(change.Create.Content) {
				return nil, nil, fmt.Errorf("changes[%d].create.content is not UTF-8 text", i)
			}
			previews = append(previews, authoringPreviewFile{Scope: declared.Scope, Path: declared.Path, Operation: "create", Before: "", After: change.Create.Content})
			resolved = append(resolved, file)
			continue
		}
		declared, ok := t.replaceable(change.Scope, change.Path)
		if !ok {
			// Crucially before readDeclared: preview cannot be used as an oracle
			// for arbitrary workspace paths.
			return nil, nil, fmt.Errorf("changes[%d]: %s:%s is neither the active editor file nor in authoring.editable_files", i, change.Scope, change.Path)
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
		previews = append(previews, authoringPreviewFile{Scope: declared.Scope, Path: declared.Path, Operation: "replace", Before: content, After: next})
		resolved = append(resolved, file)
	}
	return previews, resolved, nil
}

func (t *authoringTarget) manifestDeclared(scope, path string) (bundle.AuthoringEditableFile, bool) {
	if !t.isManifestDeclared(scope, path) {
		return bundle.AuthoringEditableFile{}, false
	}
	scope = strings.ToLower(strings.TrimSpace(scope))
	path = filepath.ToSlash(filepath.Clean(strings.TrimSpace(path)))
	for _, file := range t.manifest.Authoring.EditableFiles {
		if file.Scope == scope && file.Path == path {
			return file, true
		}
	}
	return bundle.AuthoringEditableFile{}, false
}

func (t *authoringTarget) validateCreateDestination(spec bundle.AuthoringEditableFile, resolved string) error {
	if t.files != nil {
		return authoringForbiddenError{"file creation is unavailable for cloud bot sources"}
	}
	base := t.bundleDir
	if spec.Scope == bundle.AuthoringScopeWorkspace {
		base = t.workDir
	}
	base, err := filepath.EvalSymlinks(base)
	if err != nil {
		return err
	}
	clean := filepath.Clean(filepath.FromSlash(spec.Path))
	if filepath.IsAbs(clean) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return authoringForbiddenError{"declared creation path is invalid"}
	}
	current := base
	parts := strings.Split(clean, string(filepath.Separator))
	for _, part := range parts[:len(parts)-1] {
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				return fmt.Errorf("parent directory does not exist for %s:%s", spec.Scope, spec.Path)
			}
			return statErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return authoringForbiddenError{"declared creation path traverses a symbolic link"}
		}
		if !info.IsDir() {
			return fmt.Errorf("parent component is not a directory for %s:%s", spec.Scope, spec.Path)
		}
	}
	if filepath.Clean(filepath.Join(base, clean)) != filepath.Clean(resolved) {
		return authoringForbiddenError{"declared creation path resolves through a symbolic link"}
	}
	info, statErr := os.Lstat(resolved)
	if statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return authoringForbiddenError{"declared creation destination is a symbolic link"}
		}
		return authoringFileExistsError{fmt.Sprintf("%s:%s already exists", spec.Scope, spec.Path)}
	}
	if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	return nil
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

const (
	authoringPythonSyntaxTimeout = 3 * time.Second
	authoringPythonOutputLimit   = 8 << 10
)

// authoringPythonProgram is deliberately constant. Candidate source is sent
// over stdin and is never interpolated into argv or a shell command.
const authoringPythonProgram = `import ast
import sys

filename = sys.argv[1] if len(sys.argv) > 1 else "<authoring>"
source = sys.stdin.buffer.read()
try:
    ast.parse(source.decode("utf-8"), filename=filename, mode="exec")
except (SyntaxError, UnicodeDecodeError) as exc:
    line = getattr(exc, "lineno", 0) or 0
    column = getattr(exc, "offset", 0) or 0
    message = getattr(exc, "msg", "invalid Python source")
    sys.stderr.write(f"{line}:{column}:{message}")
    raise SystemExit(1)
`

type authoringBoundedBuffer struct {
	bytes.Buffer
	limit     int
	truncated bool
}

func (b *authoringBoundedBuffer) Write(p []byte) (int, error) {
	if b.limit <= b.Len() {
		b.truncated = true
		return len(p), nil
	}
	remaining := b.limit - b.Len()
	if len(p) > remaining {
		_, _ = b.Buffer.Write(p[:remaining])
		b.truncated = true
		return len(p), nil
	}
	_, _ = b.Buffer.Write(p)
	return len(p), nil
}

func (t *authoringTarget) validateChanges(ctx context.Context, previews []authoringPreviewFile) (authoringValidationResult, error) {
	result := authoringValidationResult{
		Checks:          make([]authoringValidationCheck, 0, len(previews)),
		BehavioralTests: "not_run",
	}
	if err := t.validateChangedBots(previews); err != nil {
		return result, authoringValidationFailure{Code: "bot_compile_failed", Message: fmt.Sprintf("bot does not compile: %v", err)}
	}
	for _, preview := range previews {
		lowerPath := strings.ToLower(preview.Path)
		switch {
		case strings.HasSuffix(lowerPath, ".bot"):
			result.Checks = append(result.Checks, authoringValidationCheck{Kind: "bot_compile", Path: preview.Path, Status: "passed"})
		case strings.HasSuffix(lowerPath, ".py"):
			if err := validateAuthoringPythonSyntax(ctx, preview.Path, preview.After); err != nil {
				var failure authoringValidationFailure
				if errors.As(err, &failure) {
					return result, failure
				}
				return result, authoringValidationFailure{Code: "python_syntax_failed", Message: err.Error()}
			}
			result.Checks = append(result.Checks, authoringValidationCheck{Kind: "python_syntax", Path: preview.Path, Status: "passed"})
		case strings.HasSuffix(lowerPath, ".json"):
			var value any
			if err := json.Unmarshal([]byte(preview.After), &value); err != nil {
				return result, authoringValidationFailure{Code: "json_syntax_failed", Message: fmt.Sprintf("JSON syntax validation failed for %s: %v", filepath.ToSlash(filepath.Clean(preview.Path)), err)}
			}
			result.Checks = append(result.Checks, authoringValidationCheck{Kind: "json_syntax", Path: preview.Path, Status: "passed"})
		}
	}
	return result, nil
}

func validateAuthoringPythonSyntax(parent context.Context, path, source string) error {
	interpreter := strings.TrimSpace(os.Getenv("ITERION_ASSISTANT_PYTHON"))
	if interpreter == "" {
		interpreter = "python3"
	}
	resolved, err := exec.LookPath(interpreter)
	if err != nil {
		return authoringValidationFailure{Code: "python_parser_unavailable", Message: "Python syntax parser is unavailable"}
	}
	ctx, cancel := context.WithTimeout(parent, authoringPythonSyntaxTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, resolved, "-I", "-S", "-B", "-c", authoringPythonProgram, filepath.ToSlash(filepath.Clean(path)))
	cmd.Env = []string{"LC_ALL=C", "LANG=C", "PYTHONDONTWRITEBYTECODE=1", "PYTHONNOUSERSITE=1"}
	cmd.Dir = os.TempDir()
	configureAuthoringPythonCommand(cmd)
	cmd.Stdin = strings.NewReader(source)
	var stdout, stderr authoringBoundedBuffer
	stdout.limit = authoringPythonOutputLimit
	stderr.limit = authoringPythonOutputLimit
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return authoringValidationFailure{Code: "python_syntax_timeout", Message: "Python syntax validation timed out"}
		}
		return authoringValidationFailure{Code: "python_syntax_cancelled", Message: "Python syntax validation was cancelled"}
	}
	if err == nil {
		return nil
	}
	diagnostic := strings.TrimSpace(stderr.String())
	if diagnostic == "" {
		diagnostic = "Python syntax validation failed"
	}
	diagnostic = strings.Join(strings.Fields(diagnostic), " ")
	if len(diagnostic) > 512 {
		diagnostic = diagnostic[:512] + "..."
	}
	return authoringValidationFailure{Code: "python_syntax_failed", Message: fmt.Sprintf("Python syntax validation failed for %s: %s", filepath.ToSlash(filepath.Clean(path)), diagnostic)}
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

func (s *Server) commitAuthoring(r *http.Request, target *authoringTarget, previews []authoringPreviewFile, _ []resolvedAuthoringFile) (int, error) {
	if target.files != nil {
		for _, preview := range previews {
			if preview.Operation == "create" {
				return 0, authoringForbiddenError{"file creation is unavailable for cloud bot sources"}
			}
		}
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
	// Resolve the local editor and live manifest again at commit time. Preview
	// validation can be slow enough for the authoring perimeter, workdir, or a
	// destination to change underneath it; never carry those stale path grants
	// into a write.
	freshTarget, err := s.resolveAuthoringTarget(r, target.editorPath)
	if err != nil {
		return 0, err
	}
	if freshTarget.files != nil {
		return 0, authoringForbiddenError{"local authoring target changed source kind before commit"}
	}
	freshResolved := make([]resolvedAuthoringFile, len(previews))
	for i, preview := range previews {
		var declared bundle.AuthoringEditableFile
		var ok bool
		if preview.Operation == "create" {
			declared, ok = freshTarget.manifestDeclared(preview.Scope, preview.Path)
		} else {
			declared, ok = freshTarget.replaceable(preview.Scope, preview.Path)
		}
		if !ok {
			return 0, authoringForbiddenError{fmt.Sprintf("%s:%s left the live authoring perimeter before commit", preview.Scope, preview.Path)}
		}
		file, content, available, reason, err := freshTarget.readDeclared(declared)
		if err != nil {
			return 0, err
		}
		if preview.Operation == "create" {
			if available {
				return 0, authoringFileExistsError{fmt.Sprintf("%s:%s already exists", preview.Scope, preview.Path)}
			}
			if reason != "declared_missing_local_file" {
				return 0, authoringForbiddenError{fmt.Sprintf("%s:%s is not a declared missing local file", preview.Scope, preview.Path)}
			}
			if err := freshTarget.validateCreateDestination(declared, file.abs); err != nil {
				return 0, err
			}
		} else {
			if !available || content != preview.Before {
				return 0, authoringConflictError{fmt.Sprintf("%s:%s changed before commit", preview.Scope, preview.Path)}
			}
		}
		freshResolved[i] = file
	}
	if _, err := freshTarget.validateChanges(r.Context(), previews); err != nil {
		return 0, err
	}
	// Re-read every destination once more immediately before the first write,
	// after compilation and syntax checks, to close that validation-time race.
	for i, preview := range previews {
		if preview.Operation == "create" {
			if err := freshTarget.validateCreateDestination(freshResolved[i].spec, freshResolved[i].abs); err != nil {
				return 0, err
			}
			continue
		}
		body, err := os.ReadFile(freshResolved[i].abs) // #nosec G304 -- resolved live manifest path
		if err != nil {
			return 0, err
		}
		if string(body) != preview.Before {
			return 0, authoringConflictError{fmt.Sprintf("%s:%s changed before commit", preview.Scope, preview.Path)}
		}
	}
	resolved := freshResolved
	attempted := make([]int, 0, len(previews))
	for i, p := range previews {
		if s.watcher != nil {
			s.watcher.IgnorePath(resolved[i].abs)
		}
		attempted = append(attempted, i)
		var err error
		if p.Operation == "create" {
			err = store.WriteFileAtomicNew(resolved[i].abs, []byte(p.After), 0o644)
			if errors.Is(err, os.ErrExist) {
				// The colliding inode belongs to another writer. It was never part
				// of this transaction, even if its bytes happen to match ours.
				attempted = attempted[:len(attempted)-1]
				err = authoringFileExistsError{fmt.Sprintf("%s:%s already exists", p.Scope, p.Path)}
			}
		} else {
			err = store.WriteFileAtomic(resolved[i].abs, []byte(p.After), 0o644)
		}
		if err != nil {
			rollbackErrs := s.rollbackAuthoring(previews, resolved, attempted)
			if len(rollbackErrs) > 0 {
				return 0, fmt.Errorf("write %s:%s failed: %w; rollback also failed: %s", p.Scope, p.Path, err, strings.Join(rollbackErrs, "; "))
			}
			return 0, fmt.Errorf("write %s:%s failed: %w; earlier files were rolled back", p.Scope, p.Path, err)
		}
	}
	return 0, nil
}

func (s *Server) rollbackAuthoring(previews []authoringPreviewFile, resolved []resolvedAuthoringFile, attempted []int) []string {
	rollbackErrs := make([]string, 0)
	for j := len(attempted) - 1; j >= 0; j-- {
		idx := attempted[j]
		preview := previews[idx]
		path := resolved[idx].abs
		body, err := os.ReadFile(path) // #nosec G304 -- resolved manifest path
		if errors.Is(err, os.ErrNotExist) && preview.Operation == "create" {
			continue
		}
		if err != nil {
			rollbackErrs = append(rollbackErrs, fmt.Sprintf("%s:%s: inspect rollback target: %v", preview.Scope, preview.Path, err))
			continue
		}
		if contentSHA256(string(body)) != contentSHA256(preview.After) {
			rollbackErrs = append(rollbackErrs, fmt.Sprintf("%s:%s: content changed after write; rollback refused", preview.Scope, preview.Path))
			continue
		}
		if s.watcher != nil {
			s.watcher.IgnorePath(path)
		}
		if preview.Operation == "create" {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				rollbackErrs = append(rollbackErrs, fmt.Sprintf("%s:%s: %v", preview.Scope, preview.Path, err))
			}
			continue
		}
		if err := store.WriteFileAtomic(path, []byte(preview.Before), 0o644); err != nil {
			rollbackErrs = append(rollbackErrs, fmt.Sprintf("%s:%s: %v", preview.Scope, preview.Path, err))
		}
	}
	return rollbackErrs
}

// authoringGitCommit stages and commits only the host-attested subset of an
// active local authoring bundle. It deliberately uses Git's argv API rather
// than a shell: the model cannot inject options, choose a repository, or run
// a second command. A dirty worktree is normal; a non-selected staged path is
// not, because it could otherwise ride into this commit.
func authoringGitCommit(r *http.Request, target *authoringTarget, req authoringGitCommitRequest) (string, []string, error) {
	if target.files != nil {
		return "", nil, errors.New("git commits are unavailable for cloud bot sources without a connected repository")
	}
	if target.bootstrapManifest != "" {
		return "", nil, errors.New("git commits require a declared authoring perimeter")
	}
	gitRootOut, err := runAuthoringGit(r.Context(), target.workDir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", nil, err
	}
	gitRoot, err := filepath.EvalSymlinks(strings.TrimSpace(gitRootOut))
	if err != nil {
		return "", nil, fmt.Errorf("resolve Git root: %w", err)
	}

	paths := make([]string, 0, len(req.Files))
	expectedHashes := make(map[string]string, len(req.Files))
	for i, requested := range req.Files {
		declared, ok := target.declared(requested.Scope, requested.Path)
		if !ok {
			return "", nil, fmt.Errorf("files[%d]: %s:%s is not manifest-declared in authoring.editable_files", i, requested.Scope, requested.Path)
		}
		resolved, content, available, reason, err := target.readDeclared(declared)
		if err != nil {
			return "", nil, err
		}
		if !available {
			return "", nil, fmt.Errorf("files[%d]: %s:%s is unavailable: %s", i, declared.Scope, declared.Path, reason)
		}
		if requested.ExpectedSHA256 != contentSHA256(content) {
			return "", nil, authoringConflictError{fmt.Sprintf("%s:%s changed since the host snapshot", declared.Scope, declared.Path)}
		}
		path, err := filepath.Rel(gitRoot, resolved.abs)
		if err != nil || path == "." || path == ".." || strings.HasPrefix(path, ".."+string(filepath.Separator)) {
			return "", nil, fmt.Errorf("files[%d]: declared file is outside the active Git repository", i)
		}
		path = filepath.ToSlash(path)
		paths = append(paths, path)
		expectedHashes[path] = requested.ExpectedSHA256
	}

	tracked, err := runAuthoringGit(r.Context(), gitRoot, append([]string{"ls-files", "--error-unmatch", "--"}, paths...)...)
	if err != nil {
		return "", nil, fmt.Errorf("selected authoring files must already be Git-tracked: %w", err)
	}
	if !sameGitPathSet(splitGitLines(tracked), paths) {
		return "", nil, errors.New("git did not resolve exactly the selected authoring files")
	}
	changed, err := runAuthoringGit(r.Context(), gitRoot, append([]string{"diff", "--name-only", "-z", "HEAD", "--"}, paths...)...)
	if err != nil {
		return "", nil, err
	}
	if !sameGitPathSet(splitGitPathList(changed), paths) {
		return "", nil, authoringConflictError{"selected files have no uncommitted change against HEAD (they may already be committed)"}
	}
	indexedBefore, err := runAuthoringGit(r.Context(), gitRoot, "diff", "--cached", "--name-only", "-z")
	if err != nil {
		return "", nil, err
	}
	selected := make(map[string]bool, len(paths))
	for _, path := range paths {
		selected[path] = true
	}
	for _, path := range splitGitPathList(indexedBefore) {
		if !selected[path] {
			return "", nil, fmt.Errorf("git index already contains non-selected path %q; leave it untouched and commit it separately", path)
		}
	}
	if _, err := runAuthoringGit(r.Context(), gitRoot, append([]string{"add", "--"}, paths...)...); err != nil {
		return "", nil, err
	}
	indexedAfter, err := runAuthoringGit(r.Context(), gitRoot, "diff", "--cached", "--name-only", "-z")
	if err != nil {
		return "", nil, err
	}
	if !sameGitPathSet(splitGitPathList(indexedAfter), paths) {
		return "", nil, errors.New("git index diverged while staging the selected authoring files")
	}
	for _, path := range paths {
		staged, err := runAuthoringGit(r.Context(), gitRoot, "show", ":"+path)
		if err != nil {
			return "", nil, err
		}
		if contentSHA256(staged) != expectedHashes[path] {
			return "", nil, authoringConflictError{fmt.Sprintf("staged content for %q differs from the host snapshot", path)}
		}
	}
	if _, err := runAuthoringGit(r.Context(), gitRoot, append([]string{"commit", "--only", "-m", strings.TrimSpace(req.Message), "--"}, paths...)...); err != nil {
		return "", nil, err
	}
	head, err := runAuthoringGit(r.Context(), gitRoot, "rev-parse", "HEAD")
	if err != nil {
		return "", nil, err
	}
	head = strings.TrimSpace(head)
	committed, err := runAuthoringGit(r.Context(), gitRoot, "diff-tree", "--no-commit-id", "--name-only", "-r", "-z", "HEAD")
	if err != nil {
		return head, paths, fmt.Errorf("commit %s exists but its changed paths could not be verified: %w", head, err)
	}
	if !sameGitPathSet(splitGitPathList(committed), paths) {
		return head, paths, fmt.Errorf("commit %s exists but changed paths diverged from the selected authoring files; no rollback was attempted", head)
	}
	for _, path := range paths {
		committedContent, err := runAuthoringGit(r.Context(), gitRoot, "show", "HEAD:"+path)
		if err != nil {
			return head, paths, fmt.Errorf("commit %s exists but %q could not be verified; no rollback was attempted: %w", head, path, err)
		}
		if contentSHA256(committedContent) != expectedHashes[path] {
			return head, paths, fmt.Errorf("commit %s exists but %q content diverged from the host snapshot; no rollback was attempted", head, path)
		}
	}
	return head, paths, nil
}

// authoringGitPublish exposes one deliberately narrow network mutation: push
// the current immutable HEAD of the active local authoring repository to a
// fresh origin branch. It neither chooses a remote nor accepts a refspec.
func authoringGitPublish(r *http.Request, target *authoringTarget, req authoringGitPublishRequest) (string, error) {
	if target.files != nil {
		return "", errors.New("git publication is unavailable for cloud bot sources without a connected repository")
	}
	if target.bootstrapManifest != "" {
		return "", errors.New("git publication requires a declared authoring perimeter")
	}
	rootOut, err := runAuthoringGit(r.Context(), target.workDir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	root, err := filepath.EvalSymlinks(strings.TrimSpace(rootOut))
	if err != nil {
		return "", fmt.Errorf("resolve Git root: %w", err)
	}
	headOut, err := runAuthoringGit(r.Context(), root, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	head := strings.TrimSpace(headOut)
	resolvedOut, err := runAuthoringGit(r.Context(), root, "rev-parse", "--verify", req.Commit+"^{commit}")
	if err != nil || strings.TrimSpace(resolvedOut) != head {
		return "", authoringConflictError{"requested commit is not the current repository HEAD"}
	}
	if _, err := runAuthoringGit(r.Context(), root, "check-ref-format", "--branch", req.Branch); err != nil {
		return "", fmt.Errorf("invalid branch: %w", err)
	}
	if _, err := runAuthoringGit(r.Context(), root, "remote", "get-url", "origin"); err != nil {
		return "", errors.New("origin remote is required for authoring publication")
	}
	dest := "refs/heads/" + req.Branch
	remoteBefore, err := runAuthoringGit(r.Context(), root, "ls-remote", "origin", dest)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(remoteBefore) != "" {
		return "", authoringConflictError{"remote branch already exists; publication never overwrites it"}
	}
	// A branch created between ls-remote and push is safe: non-force push
	// rejects it rather than overwriting it.
	if _, err := runAuthoringGit(r.Context(), root, "push", "--porcelain", "origin", head+":"+dest); err != nil {
		return "", err
	}
	remoteAfter, err := runAuthoringGit(r.Context(), root, "ls-remote", "origin", dest)
	if err != nil {
		return head, fmt.Errorf("published %s but could not verify remote branch; no rollback was attempted: %w", head, err)
	}
	fields := strings.Fields(remoteAfter)
	if len(fields) < 2 || fields[0] != head || fields[1] != dest {
		return head, fmt.Errorf("published %s but remote branch verification diverged; no rollback was attempted", head)
	}
	return head, nil
}

func runAuthoringGit(ctx context.Context, workDir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", workDir}, args...)...)
	cmd.Env = gitlib.SanitizeEnv(os.Environ())
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func splitGitPathList(value string) []string {
	parts := strings.Split(value, "\x00")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, filepath.ToSlash(part))
		}
	}
	return out
}

func splitGitLines(value string) []string {
	return splitGitPathList(strings.ReplaceAll(value, "\n", "\x00"))
}

func sameGitPathSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := make(map[string]bool, len(want))
	for _, path := range want {
		if seen[path] {
			return false
		}
		seen[path] = true
	}
	for _, path := range got {
		if !seen[path] {
			return false
		}
		delete(seen, path)
	}
	return len(seen) == 0
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

type authoringFileExistsError struct{ message string }

func (e authoringFileExistsError) Error() string { return e.message }

type authoringForbiddenError struct{ message string }

func (e authoringForbiddenError) Error() string { return e.message }

func (s *Server) authoringError(w http.ResponseWriter, r *http.Request, err error) {
	var conflict authoringConflictError
	var fileExists authoringFileExistsError
	var forbidden authoringForbiddenError
	switch {
	case errors.As(err, &fileExists):
		s.reflectAllowedOrigin(w, r)
		httpx.WriteJSON(w, http.StatusConflict, map[string]string{"error": fileExists.Error(), "error_code": "file_already_exists"})
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

func (s *Server) writeAuthoringValidationError(w http.ResponseWriter, r *http.Request, failure authoringValidationFailure) {
	s.reflectAllowedOrigin(w, r)
	httpx.WriteJSON(w, http.StatusBadRequest, map[string]string{
		"error":      failure.Message,
		"error_code": failure.Code,
	})
}
