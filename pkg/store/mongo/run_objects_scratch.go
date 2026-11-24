package mongo

import (
	"context"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/store"
	"os"
	"path/filepath"
	"strings"
)

// physicalScratchRoot resolves the existing prefix without creating anything.
// Trailing separators, relative paths and stable aliases must not put exact
// scratch underneath a legacy run. The configured filesystem is cooperative:
// changing symlinks after capture is not a revocable-handle guarantee.
func physicalScratchRoot(name string) (string, error) {
	root, err := filepath.Abs(name)
	if err != nil {
		return "", err
	}
	var tail []string
	for {
		resolved, e := filepath.EvalSymlinks(root)
		if e == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			return resolved, nil
		}
		if !os.IsNotExist(e) {
			return "", e
		}
		// A dangling symlink is not an absent directory suffix. Do not guess its
		// future physical target while claiming disjoint roots.
		if info, statErr := os.Lstat(root); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
			return "", e
		}

		parent := filepath.Dir(root)
		if parent == root {
			return "", e
		}
		tail = append(tail, filepath.Base(root))
		root = parent
	}
}
func scratchRootContains(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
func (s *Store) runFilesScratchDirForFormat(id string, format store.ValueFormat) (string, error) {
	if format != store.ExactValueFormat {
		return s.runFilesScratchDir(id), nil
	}
	if s.runFilesScratch == "" {
		return "", fmt.Errorf("%w: no run-files scratch root", store.ErrValueFormatCapability)
	}
	legacy, err := physicalScratchRoot(s.runFilesScratch)
	if err != nil {
		return "", err
	}
	exact, err := physicalScratchRoot(legacy + "-v2")
	if err != nil {
		return "", err
	}
	if scratchRootContains(legacy, exact) || scratchRootContains(exact, legacy) {
		return "", fmt.Errorf("%w: run-files scratch roots overlap", store.ErrValueFormatCapability)
	}
	return filepath.Join(exact, id), nil
}

// runFilesScratchForBinding preserves the original birth in the local namespace.
// Retained managed IO is a separate lifetime obligation.
func (s *Store) runFilesScratchForBinding(ctx context.Context, id string, format store.ValueFormat) (string, error) {
	binding, bound := store.RunDataBindingFrom(ctx)
	if !bound || binding.Header().Protocol != store.IncarnationRunData {
		return s.runFilesScratchDirForFormat(id, format)
	}
	strict, scope, err := s.runFilesScratchIncarnation(ctx, id, format)
	if err != nil {
		return "", err
	}
	return scope.FilesystemRoot(strict)
}
func (s *Store) runFilesScratchIncarnation(ctx context.Context, id string, format store.ValueFormat) (string, store.IncarnationDataScope, error) {
	binding, ok := store.RunDataBindingFrom(ctx)
	if !ok || binding.Header().Protocol != store.IncarnationRunData {
		return "", store.IncarnationDataScope{}, store.ErrRunDataBindingRequired
	}
	h := binding.Header()
	if h.RunID != id || h.Format != format {
		return "", store.IncarnationDataScope{}, store.ErrRunDataIdentityChanged
	}
	scope, err := store.ResolveIncarnationDataScope(ctx, h)
	if err != nil {
		return "", store.IncarnationDataScope{}, err
	}
	if s.runFilesScratch == "" {
		return "", store.IncarnationDataScope{}, store.ErrRunDataCapability
	}
	legacy, err := physicalScratchRoot(s.runFilesScratch)
	if err != nil {
		return "", store.IncarnationDataScope{}, err
	}
	exact, err := physicalScratchRoot(legacy + "-v2")
	if err != nil {
		return "", store.IncarnationDataScope{}, err
	}
	strict, err := physicalScratchRoot(legacy + "-incarnations")
	if err != nil {
		return "", store.IncarnationDataScope{}, err
	}
	for _, other := range []string{legacy, exact} {
		if scratchRootContains(other, strict) || scratchRootContains(strict, other) {
			return "", store.IncarnationDataScope{}, store.ErrRunDataCapability
		}
	}
	return strict, scope, nil
}
