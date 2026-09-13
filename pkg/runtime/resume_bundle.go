package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ResolveResumeBundleWorkflow verifies a persisted shared-workflow identity
// against the currently materialized bundle and returns the exact workflow
// path to compile. Legacy/non-export bundle runs retain their prior entrypoint
// or persisted-path behaviour.
func ResolveResumeBundleWorkflow(r *store.Run, b *bundle.Bundle, persistedPath string, force bool) (string, error) {
	if b == nil {
		return persistedPath, nil
	}
	path := b.IterPath
	if r == nil || r.BundleWorkflow == "" {
		if rel, err := filepath.Rel(b.Dir, persistedPath); err == nil && rel != "." && !filepath.IsAbs(rel) && rel != ".." && !strings.HasPrefix(filepath.ToSlash(rel), "../") {
			candidate := filepath.Join(b.Dir, rel)
			if info, statErr := osStatRegular(candidate); statErr == nil && info {
				path = candidate
			}
		}
		return path, nil
	}

	var selected *bundle.WorkflowExport
	if b.Manifest != nil {
		for i := range b.Manifest.Exports.Workflows {
			if b.Manifest.Exports.Workflows[i].ID == r.BundleWorkflow {
				selected = &b.Manifest.Exports.Workflows[i]
				break
			}
		}
	}
	identityErr := ""
	switch {
	case b.Manifest == nil:
		identityErr = "manifest is missing"
	case r.BundleName != "" && b.Manifest.Name != r.BundleName:
		identityErr = fmt.Sprintf("bundle name changed from %q to %q", r.BundleName, b.Manifest.Name)
	case r.BundleVersion != "" && b.Manifest.Version != r.BundleVersion:
		identityErr = fmt.Sprintf("bundle version changed from %q to %q", r.BundleVersion, b.Manifest.Version)
	case selected == nil:
		identityErr = fmt.Sprintf("workflow export %q is missing", r.BundleWorkflow)
	}
	if identityErr == "" && r.BundleHash != "" {
		currentHash := b.Hash
		if currentHash == "" {
			var hashErr error
			currentHash, hashErr = bundle.ContentHashDir(b.Dir)
			if hashErr != nil {
				return "", fmt.Errorf("resume bundle: hash current bundle: %w", hashErr)
			}
		}
		if currentHash != r.BundleHash {
			identityErr = fmt.Sprintf("bundle sha256 changed from %s to %s", shortWorkflowHash(r.BundleHash), shortWorkflowHash(currentHash))
		}
	}
	if identityErr != "" && !force {
		return "", fmt.Errorf("%w: run %q shared dependency %s", ErrWorkflowSourceChanged, r.ID, identityErr)
	}
	if selected == nil {
		return path, nil
	}
	return filepath.Join(b.Dir, filepath.FromSlash(selected.Path)), nil
}

func osStatRegular(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	return info.Mode().IsRegular(), nil
}
