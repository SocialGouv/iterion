package mongo

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/store"
	"os"
)

type mongoRunFilesGrant struct {
	store.RunFilesGrant
	owner  *Store
	parent *os.Root
	root   *os.Root // borrowed from RunFilesGrant until Close
	name   string
}

func (g *mongoRunFilesGrant) Close() error {
	return errors.Join(g.RunFilesGrant.Close(), g.parent.Close())
}

// openRunFilesNamespace retains both the cleanup parent and the selected leaf.
// A missing read-only leaf leaves Root nil so cleanup can retry parent sync.
func (s *Store) openRunFilesNamespace(ctx context.Context, id string, format store.ValueFormat, create bool) (*store.IncarnationDirectory, string, error) {
	basePath, scope, err := s.runFilesScratchIncarnation(ctx, id, format)
	if err != nil {
		return nil, "", err
	}
	path, err := scope.FilesystemRoot(basePath)
	if err != nil {
		return nil, "", err
	}
	base, err := store.OpenRunDataDirectory(ctx, basePath, create)
	if err != nil {
		if !create && errors.Is(err, os.ErrNotExist) {
			return nil, path, nil
		}
		return nil, "", err
	}
	dir, err := store.OpenIncarnationDirectoryAt(ctx, base, scope, create)
	baseErr := base.Close()
	if err != nil {
		if !create && errors.Is(err, os.ErrNotExist) && baseErr == nil {
			return nil, path, nil
		}
		return nil, "", errors.Join(err, baseErr)
	}
	if baseErr != nil {
		dir.Close()
		return nil, "", baseErr
	}
	return dir, path, nil
}

func (s *Store) OpenRunFilesGrant(ctx context.Context, id string) (store.RunFilesGrant, error) {
	bound, _, err := s.captureRunObjects(ctx, id)
	if err != nil {
		return nil, err
	}
	binding, ok := store.RunDataBindingFrom(bound)
	if !ok || binding.Header().Protocol != store.IncarnationRunData {
		return nil, store.ErrRunDataCapability
	}
	format, err := store.ValueFormatForWrite(bound, id)
	if err != nil {
		return nil, err
	}
	dir, path, err := s.openRunFilesNamespace(bound, id, format, true)
	if err != nil {
		return nil, err
	}
	f, err := dir.Root.Open(".")
	if err != nil {
		dir.Close()
		return nil, err
	}
	modeErr := f.Chmod(0o775)
	syncErr := f.Sync()
	closeErr := f.Close()
	if err = errors.Join(modeErr, syncErr, closeErr, bound.Err()); err != nil {
		dir.Close()
		return nil, err
	}
	if err = s.checkRunObjects(bound, id); err != nil {
		dir.Close()
		return nil, err
	}
	g, err := store.NewRetainedRunFilesGrant(path, dir.Root, binding, func(ctx context.Context) error { return s.checkRunObjects(ctx, id) })
	if err != nil {
		dir.Close()
		return nil, err
	}
	return &mongoRunFilesGrant{RunFilesGrant: g, owner: s, parent: dir.Parent, root: dir.Root, name: dir.Name}, nil
}

func syncRunFilesRoot(root *os.Root) error {
	f, err := root.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

// removeRunFilesNamespace never reopens a pathname. The identity comparison
// detects replacement at this boundary, not an atomic inode CAS against a
// same-UID actor racing subsequent filesystem operations.
func removeRunFilesNamespace(ctx context.Context, dir *store.IncarnationDirectory) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if dir.Root == nil {
		return syncRunFilesRoot(dir.Parent)
	}
	original, err := dir.Root.Stat(".")
	if err != nil {
		return err
	}
	current, err := dir.Parent.Lstat(dir.Name)
	if errors.Is(err, os.ErrNotExist) {
		return syncRunFilesRoot(dir.Parent)
	}
	if err != nil {
		return err
	}
	if !current.IsDir() || !os.SameFile(original, current) {
		return store.ErrRunDataIdentityChanged
	}
	if err = dir.Parent.RemoveAll(dir.Name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncRunFilesRoot(dir.Parent)
}
