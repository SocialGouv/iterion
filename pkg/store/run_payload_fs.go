package store

import (
	"context"
	"os"
	"path/filepath"
)

// runPayloadPath resolves only the original data namespace. It deliberately
// does not acquire the stable metadata lock: mutation callers already hold it.
// Readers may race replacement after this atomic metadata observation, but a
// protocol-one path remains in the original birth's disjoint subtree.
func (s *FilesystemRunStore) runPayloadPath(ctx context.Context, id string, parts ...string) (string, error) {
	if ctx == nil {
		return "", ErrRunDataBindingRequired
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := sanitizePathComponent("run ID", id); err != nil {
		return "", err
	}
	for _, part := range parts {
		if err := sanitizePathComponent("payload path component", part); err != nil {
			return "", err
		}
	}
	if binding, ok := RunDataBindingFrom(ctx); ok && binding.Header().RunID != id {
		return "", ErrRunDataIdentityChanged
	}
	root := s.runDir(id)
	raw, err := os.ReadFile(s.runJSONPath(id))
	if err != nil {
		if !os.IsNotExist(err) {
			return "", err
		}
		if err := CheckMissingRunDataParent(ctx, id); err != nil {
			return "", err
		}
	} else {
		header, err := RunDataHeaderFromJSON(ctx, raw)
		if err != nil {
			return "", err
		}
		if header.RunID != id {
			return "", ErrRunDataIdentityChanged
		}
		if err := RequireRunDataStorage(header.Protocol); err != nil {
			return "", err
		}
		if err := CheckRunDataBinding(ctx, header); err != nil {
			return "", err
		}
		if err := checkRunJSONObservation(ctx, raw); err != nil {
			return "", err
		}
		if header.Protocol == IncarnationRunData {
			if err := s.guardNotDeleted(id); err != nil {
				return "", err
			}
			scope, err := ResolveIncarnationDataScope(ctx, header)
			if err != nil {
				return "", err
			}
			root, err = scope.FilesystemRoot(s.root)
			if err != nil {
				return "", err
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return filepath.Join(append([]string{root}, parts...)...), nil
}
