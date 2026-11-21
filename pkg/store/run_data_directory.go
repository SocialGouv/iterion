package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// IncarnationDirectory owns independent descriptors for one namespace leaf and
// its immediate parent. Name is the single validated entry beneath Parent.
// Root is nil only when create=false and the leaf is absent beneath a retained
// existing parent. Callers close both handles. No pathname reopening is needed.
type IncarnationDirectory struct {
	Parent *os.Root
	Name   string
	Root   *os.Root
}

func (d *IncarnationDirectory) Close() error {
	var a, b error
	if d.Root != nil {
		a = d.Root.Close()
	}
	if d.Parent != nil {
		b = d.Parent.Close()
	}
	return errors.Join(a, b)
}

// OpenIncarnationDirectoryAt borrows base and returns owned handles. Creation
// syncs each parent even on retry; no shared ancestor is rolled back on error.
// The context controls cancellation only: scope already carries the captured
// identity. The store caller separately validates its current parent metadata.
func OpenIncarnationDirectoryAt(ctx context.Context, base *os.Root, scope IncarnationDataScope, create bool) (result *IncarnationDirectory, err error) {
	if ctx == nil {
		return nil, ErrRunDataBindingRequired
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if !runBirthAtomicPublicationSupported() || base == nil {
		return nil, ErrRunDataCapability
	}
	parts, err := scope.components()
	if err != nil {
		return nil, err
	}
	parts = append([]string{".run-data-v1"}, parts...)
	current, err := base.OpenRoot(".")
	if err != nil {
		return nil, err
	}
	defer func() {
		if current != nil {
			_ = current.Close()
		}
	}()
	for _, part := range parts[:len(parts)-1] {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		next, e := openIncarnationChildRoot(current, part, create)
		if e != nil {
			return nil, e
		}
		e = current.Close()
		current = next
		if e != nil {
			return nil, e
		}
	}
	name := parts[len(parts)-1]
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	leaf, err := openIncarnationChildRoot(current, name, create)
	if err != nil && (create || !errors.Is(err, os.ErrNotExist)) {
		return nil, err
	}
	if err != nil {
		leaf = nil
		err = nil
	}
	if create {
		if err = syncRunPayloadRoot(leaf); err != nil {
			leaf.Close()
			return nil, err
		}
	}
	if err = ctx.Err(); err != nil {
		if leaf != nil {
			leaf.Close()
		}
		return nil, err
	}
	result = &IncarnationDirectory{Parent: current, Name: name, Root: leaf}
	current = nil
	return result, nil
}

// OpenRunDataDirectory opens a configured physical path, retaining each parent
// before creating or opening its child. Resolve operator aliases before calling;
// namespace symlinks are refused. The returned root belongs to the caller.
func OpenRunDataDirectory(ctx context.Context, path string, create bool) (result *os.Root, err error) {
	if ctx == nil {
		return nil, ErrRunDataBindingRequired
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if !runBirthAtomicPublicationSupported() {
		return nil, ErrRunDataCapability
	}
	if path == "" {
		return nil, ErrInvalidRunDataIdentity
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	current, err := os.OpenRoot(string(filepath.Separator))
	if err != nil {
		return nil, err
	}
	defer func() {
		if current != nil {
			_ = current.Close()
		}
	}()
	for _, part := range strings.Split(strings.TrimPrefix(absolute, string(filepath.Separator)), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		next, e := openIncarnationChildRoot(current, part, create)
		if e != nil {
			return nil, e
		}
		e = current.Close()
		current = next
		if e != nil {
			return nil, e
		}
	}
	if create {
		if err = syncRunPayloadRoot(current); err != nil {
			return nil, err
		}
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	result = current
	current = nil
	return result, nil
}
