package store

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
)

// IncarnationDataScope is an immutable physical namespace derived from an
// original binding and a matching header observed under the store's lock/CAS.
// Resolving it performs no IO and does not enable protocol-one admission.
// Callers retain the scope for uploads, granted paths and delayed cleanup.
type IncarnationDataScope struct{ binding RunDataBinding }

func ResolveIncarnationDataScope(ctx context.Context, current RunDataHeader) (IncarnationDataScope, error) {
	if err := CheckRunDataBinding(ctx, current); err != nil {
		return IncarnationDataScope{}, err
	}
	if current.Protocol != IncarnationRunData {
		return IncarnationDataScope{}, ErrRunDataCapability
	}
	binding, ok := RunDataBindingFrom(ctx)
	if !ok {
		return IncarnationDataScope{}, ErrRunDataBindingRequired
	}
	scope := IncarnationDataScope{binding: binding}
	if _, err := scope.components(); err != nil {
		return IncarnationDataScope{}, err
	}
	return scope, nil
}

// Header returns a copy; changing it cannot redirect delayed IO.
func (s IncarnationDataScope) Header() RunDataHeader { return s.binding.Header() }

func (s IncarnationDataScope) components() ([]string, error) {
	if !s.binding.observed {
		return nil, ErrRunDataBindingRequired
	}
	h, err := s.binding.header.validate()
	if err != nil {
		return nil, err
	}
	if h.Protocol != IncarnationRunData {
		return nil, ErrRunDataCapability
	}
	if err := SanitizePathComponent("run ID", h.RunID); err != nil {
		return nil, err
	}
	if err := SanitizePathComponent("birth ID", h.BirthID); err != nil {
		return nil, err
	}
	return []string{fmt.Sprintf("f%d", h.Format), h.RunID, h.BirthID}, nil
}

// FilesystemRoot is outside runs/, including every historical per-run cleanup
// subtree. The caller still owes symlink-safe creation/open and metadata guards.
func (s IncarnationDataScope) FilesystemRoot(storeRoot string) (string, error) {
	parts, err := s.components()
	if err != nil {
		return "", err
	}
	if storeRoot == "" {
		return "", ErrInvalidRunDataIdentity
	}
	return filepath.Join(append([]string{storeRoot, ".run-data-v1"}, parts...)...), nil
}

// BlobPrefix is outside the historical domain prefixes. Its trailing slash
// makes a listing or deletion prefix exact at the incarnation boundary.
func (s IncarnationDataScope) BlobPrefix() (string, error) {
	parts, err := s.components()
	if err != nil {
		return "", err
	}
	return path.Join(append([]string{"run-data-v1"}, parts...)...) + "/", nil
}
