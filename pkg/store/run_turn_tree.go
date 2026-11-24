package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// turnPayloadPaths owns one retained tree for an entire turn operation. Path
// strings are logical keys only; selected node/iteration roots are cached and
// never reopened during that operation.
type turnPayloadPaths struct {
	root           string
	ctx            context.Context
	create, strict bool
	dirs           map[string]*runPayloadDirectory
}

func (s *FilesystemRunStore) turnPayloadPaths(ctx context.Context, runID string, create bool) (turnPayloadPaths, error) {
	d, err := s.openRunPayloadDirectory(ctx, runID, create, "turns")
	if err != nil {
		return turnPayloadPaths{}, err
	}
	return turnPayloadPaths{root: d.parent.key, ctx: ctx, create: create, strict: d.parent.strict, dirs: map[string]*runPayloadDirectory{".": d}}, nil
}
func (p turnPayloadPaths) Close() error {
	var errs []error
	for _, dir := range p.dirs {
		errs = append(errs, dir.Close())
	}
	return errors.Join(errs...)
}
func (p turnPayloadPaths) directory(path string) (*runPayloadDirectory, error) {
	rel, err := filepath.Rel(p.root, path)
	if err != nil {
		return nil, err
	}
	if rel == "." {
		return p.dirs["."], nil
	}
	parts := strings.Split(rel, string(filepath.Separator))
	for _, part := range parts {
		if err := sanitizePathComponent("turn directory", part); err != nil {
			return nil, err
		}
	}
	current := p.dirs["."]
	key := ""
	for _, part := range parts {
		key = filepath.Join(key, part)
		if cached, ok := p.dirs[key]; ok {
			current = cached
			continue
		}
		child, err := current.OpenDirectory(p.ctx, part, p.create)
		if err != nil {
			return nil, err
		}
		p.dirs[key] = child
		current = child
	}
	return current, nil
}
func (p turnPayloadPaths) ReadFile(path string) ([]byte, error) {
	d, err := p.directory(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	return d.ReadFile(filepath.Base(path))
}
func (p turnPayloadPaths) WriteFile(path string, data []byte, perm os.FileMode) error {
	d, err := p.directory(filepath.Dir(path))
	if err != nil {
		return err
	}
	return d.WriteAtomic(p.ctx, filepath.Base(path), data, perm)
}
func (p turnPayloadPaths) ReadDir(path string) ([]os.DirEntry, error) {
	d, err := p.directory(path)
	if err != nil {
		return nil, err
	}
	return d.ReadDir()
}
func (p turnPayloadPaths) Remove(path string) error {
	d, err := p.directory(filepath.Dir(path))
	if err != nil {
		return err
	}
	return d.Remove(p.ctx, filepath.Base(path))
}
