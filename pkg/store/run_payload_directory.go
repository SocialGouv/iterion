package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
)

// runPayloadDirectory owns the selected directory for a complete operation.
// Its borrowed file views never reacquire a pathname and must not be closed
// independently. Callers keep the directory alive through scans and writes.
type runPayloadDirectory struct{ parent *runPayloadFile }

func (s *FilesystemRunStore) openRunPayloadDirectory(ctx context.Context, id string, create bool, parts ...string) (result *runPayloadDirectory, err error) {
	defer func() {
		if err == nil {
			if e := ctx.Err(); e != nil {
				if result != nil {
					_ = result.Close()
				}
				result = nil
				err = e
			}
		}
	}()
	p, err := s.openRunPayloadPath(ctx, id, create, parts...)
	if err != nil {
		return nil, err
	}
	if p.strict {
		if p.root != nil {
			child, openErr := openIncarnationChildRoot(p.root, p.name, create)
			closeErr := p.Close()
			p.root = nil
			if openErr != nil {
				if errors.Is(openErr, os.ErrNotExist) && !create && closeErr == nil {
					p.name = "."
					return &runPayloadDirectory{parent: p}, nil
				}
				return nil, errors.Join(openErr, closeErr)
			}
			if closeErr != nil {
				_ = child.Close()
				return nil, closeErr
			}
			p.root = child
			if create {
				if e := syncRunPayloadRoot(child); e != nil {
					_ = p.Close()
					return nil, e
				}
			}
		}
		p.name = "."
	} else if create {
		if e := os.MkdirAll(p.key, dirPerm); e != nil {
			return nil, e
		}
	}
	return &runPayloadDirectory{parent: p}, nil
}

func (d *runPayloadDirectory) Close() error { return d.parent.Close() }

// file returns a borrowed view. The directory, not this view, owns the root.
func (d *runPayloadDirectory) file(name string) (*runPayloadFile, error) {
	if e := sanitizePathComponent("payload filename", name); e != nil {
		return nil, e
	}
	return &runPayloadFile{key: filepath.Join(d.parent.key, name), name: name, root: d.parent.root, strict: d.parent.strict}, nil
}

func (d *runPayloadDirectory) ReadDir() ([]os.DirEntry, error) {
	if !d.parent.strict {
		return os.ReadDir(d.parent.key)
	}
	f, err := d.parent.Open()
	if err != nil {
		return nil, err
	}
	entries, err := f.ReadDir(-1)
	err = errors.Join(err, f.Close())
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, err
}
func (d *runPayloadDirectory) ReadFile(name string) ([]byte, error) {
	f, e := d.file(name)
	if e != nil {
		return nil, e
	}
	return f.ReadFile()
}
func (d *runPayloadDirectory) WriteAtomic(ctx context.Context, name string, body []byte, perm os.FileMode) error {
	f, e := d.file(name)
	if e != nil {
		return e
	}
	return f.WriteAtomic(ctx, body, perm)
}
func (d *runPayloadDirectory) Remove(ctx context.Context, name string) error {
	if ctx == nil {
		return ErrRunDataBindingRequired
	}
	if e := ctx.Err(); e != nil {
		return e
	}
	f, e := d.file(name)
	if e != nil {
		return e
	}
	if !f.strict {
		return os.Remove(f.key)
	}
	if f.root == nil {
		return os.ErrNotExist
	}
	e = f.root.Remove(name)
	if e == nil || errors.Is(e, os.ErrNotExist) {
		// A retry after failed sync must acknowledge the earlier removal too.
		if syncErr := f.syncParent(); syncErr != nil {
			return syncErr
		}
	}
	return e
}
