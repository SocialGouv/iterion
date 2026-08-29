package server

import (
	"net/http"
	"path/filepath"

	"github.com/SocialGouv/iterion/pkg/botlock"
	"github.com/SocialGouv/iterion/pkg/bundle"
)

type fileDependencyRequest struct {
	Path string `json:"path"`
}

type sharedBundleFileMetadata struct {
	Name         string `json:"name"`
	Version      string `json:"version,omitempty"`
	Workflow     string `json:"workflow,omitempty"`
	BundleSHA256 string `json:"bundle_sha256,omitempty"`
	Verified     bool   `json:"verified"`
}

type fileDependencyResponse struct {
	ReadOnly     bool                      `json:"read_only"`
	SharedBundle *sharedBundleFileMetadata `json:"shared_bundle,omitempty"`
}

// isMaterializedBotDependencyPath is the server-side write boundary for
// bundles installed by `iterion bots sync`. It compares canonical paths, so a
// workspace symlink pointing back into .botz cannot bypass the read-only rule.
func (s *Server) isMaterializedBotDependencyPath(absPath string) bool {
	s.stateMu.RLock()
	workDir := s.cfg.WorkDir
	s.stateMu.RUnlock()
	root, err := safePathWithin(workDir, ".botz")
	return err == nil && pathContains(root, absPath)
}

func (s *Server) handleFileDependency(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	var req fileDependencyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	absPath, err := s.safePath(req.Path)
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid path: %v", err)
		return
	}
	if !s.isMaterializedBotDependencyPath(absPath) {
		writeJSON(w, fileDependencyResponse{})
		return
	}

	s.stateMu.RLock()
	workDir := s.cfg.WorkDir
	s.stateMu.RUnlock()
	root, err := safePathWithin(workDir, ".botz")
	if err != nil {
		httpError(w, http.StatusInternalServerError, "resolve bot dependencies: %v", err)
		return
	}
	rel, err := filepath.Rel(root, absPath)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "resolve bot dependency path: %v", err)
		return
	}
	parts := splitPath(rel)
	if len(parts) < 2 {
		writeJSON(w, fileDependencyResponse{ReadOnly: true})
		return
	}
	name := parts[0]
	metadata := &sharedBundleFileMetadata{Name: name}
	response := fileDependencyResponse{ReadOnly: true, SharedBundle: metadata}

	lock, lockErr := botlock.Load(workDir)
	dep, locked := botlock.Dependency{}, false
	if lockErr == nil {
		dep, locked = lock.Dependencies[name]
		if locked {
			metadata.BundleSHA256 = dep.BundleSHA256
		}
	}
	bundleRoot := filepath.Join(root, name)
	b, bundleErr := bundle.OpenDir(bundleRoot)
	if bundleErr == nil && b.Manifest != nil {
		metadata.Version = b.Manifest.Version
		workflowPath := filepath.ToSlash(filepath.Join(parts[1:]...))
		for _, export := range b.Manifest.Exports.Workflows {
			if filepath.ToSlash(filepath.Clean(export.Path)) == workflowPath {
				metadata.Workflow = export.ID
				break
			}
		}
	}
	if locked && bundleErr == nil {
		if actual, hashErr := bundle.ContentHashDir(bundleRoot); hashErr == nil {
			metadata.Verified = actual == dep.BundleSHA256
		}
	}
	writeJSON(w, response)
}

func splitPath(path string) []string {
	var parts []string
	for path != "." && path != "" {
		dir, base := filepath.Split(path)
		parts = append([]string{base}, parts...)
		path = filepath.Clean(dir)
		if path == string(filepath.Separator) {
			break
		}
	}
	return parts
}
