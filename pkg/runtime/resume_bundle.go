package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ResumeBundleWorkflow is the exact workflow path a resume compiles and,
// aside, the refusal of a shared dependency whose identity changed since the
// run started (nil when it did not): the persisted shared-workflow identity is
// verified against the currently materialized bundle. The path is resolved
// either way, as --force resolves it — the scratch and the lineage are judged
// before the source, so a surface compiles through it and refuses the
// identity after them. Legacy/non-export bundle runs retain their prior
// entrypoint or persisted-path behaviour.
func ResumeBundleWorkflow(r *store.Run, b *bundle.Bundle, persistedPath string) (path string, identityErr, err error) {
	if b == nil {
		return persistedPath, nil, nil
	}
	path = b.IterPath
	if r == nil || r.BundleWorkflow == "" {
		if rel, err := filepath.Rel(b.Dir, persistedPath); err == nil && rel != "." && !filepath.IsAbs(rel) && rel != ".." && !strings.HasPrefix(filepath.ToSlash(rel), "../") {
			candidate := filepath.Join(b.Dir, rel)
			if info, statErr := osStatRegular(candidate); statErr == nil && info {
				path = candidate
			}
		} else if entry := bundle.EntryForCopy(persistedPath, b.Dir); entry != "" {
			// A studio launch records the store's materialised copy as the
			// FilePath — outside the bundle, so the rel above escapes. The
			// copy stands for the entry it was made of: resume THAT (the
			// sibling for a sibling launch), or the hash would refuse a
			// source that never changed and --force would run main.bot
			// against the sibling's checkpoint.
			path = entry
		}
		return path, nil, nil
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
	changed := ""
	switch {
	case b.Manifest == nil:
		changed = "manifest is missing"
	case r.BundleName != "" && b.Manifest.Name != r.BundleName:
		changed = fmt.Sprintf("bundle name changed from %q to %q", r.BundleName, b.Manifest.Name)
	case r.BundleVersion != "" && b.Manifest.Version != r.BundleVersion:
		changed = fmt.Sprintf("bundle version changed from %q to %q", r.BundleVersion, b.Manifest.Version)
	case selected == nil:
		changed = fmt.Sprintf("workflow export %q is missing", r.BundleWorkflow)
	}
	if changed == "" && r.BundleHash != "" {
		currentHash := b.Hash
		if currentHash == "" {
			var hashErr error
			currentHash, hashErr = bundle.ContentHashDir(b.Dir)
			if hashErr != nil {
				return "", nil, fmt.Errorf("resume bundle: hash current bundle: %w", hashErr)
			}
		}
		if currentHash != r.BundleHash {
			changed = fmt.Sprintf("bundle sha256 changed from %s to %s", shortWorkflowHash(r.BundleHash), shortWorkflowHash(currentHash))
		}
	}
	if changed != "" {
		identityErr = fmt.Errorf("%w: run %q shared dependency %s", ErrWorkflowSourceChanged, r.ID, changed)
	}
	if selected == nil {
		return path, identityErr, nil
	}
	return filepath.Join(b.Dir, filepath.FromSlash(selected.Path)), identityErr, nil
}

func osStatRegular(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	return info.Mode().IsRegular(), nil
}
