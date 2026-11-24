package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// runPayloadFile keeps the selected identity separate from the retained IO root.
// key is only for cache identity; strict IO never reopens that pathname.
type runPayloadFile struct {
	key, name string
	root      *os.Root
	strict    bool
}

func (s *FilesystemRunStore) openRunPayloadFile(ctx context.Context, id, name string, create bool) (*runPayloadFile, error) {
	return s.openRunPayloadPath(ctx, id, create, name)
}
func (s *FilesystemRunStore) openRunPayloadPath(ctx context.Context, id string, create bool, parts ...string) (result *runPayloadFile, err error) {
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
	if len(parts) == 0 {
		return nil, ErrInvalidRunDataIdentity
	}
	key, err := s.runPayloadPath(ctx, id, parts...)
	if err != nil {
		return nil, err
	}
	p := &runPayloadFile{key: key, name: parts[len(parts)-1]}
	binding, bound := RunDataBindingFrom(ctx)
	if bound && binding.Header().Protocol == IncarnationRunData {
		p.strict = true
		scope, e := ResolveIncarnationDataScope(ctx, binding.Header())
		if e != nil {
			return nil, e
		}
		p.root, err = openIncarnationDataRoot(s.root, scope, create)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) && !create {
				return p, nil
			}
			return nil, err
		}
		for _, part := range parts[:len(parts)-1] {
			next, e := openIncarnationChildRoot(p.root, part, create)
			closeErr := p.root.Close()
			p.root = nil
			if e != nil {
				if errors.Is(e, os.ErrNotExist) && !create && closeErr == nil {
					return p, nil
				}
				return nil, errors.Join(e, closeErr)
			}
			if closeErr != nil {
				next.Close()
				return nil, closeErr
			}
			p.root = next
		}
		if create {
			if e := syncRunPayloadRoot(p.root); e != nil {
				p.root.Close()
				return nil, e
			}
		}
	} else if create {
		if err = os.MkdirAll(filepath.Dir(key), dirPerm); err != nil {
			return nil, err
		}
	}
	return p, nil
}
func (p *runPayloadFile) Close() error {
	if p.root != nil {
		return p.root.Close()
	}
	return nil
}
func (p *runPayloadFile) Open() (*os.File, error) { return p.OpenFile(os.O_RDONLY, 0) }
func (p *runPayloadFile) OpenFile(flag int, perm os.FileMode) (*os.File, error) {
	if !p.strict {
		return os.OpenFile(p.key, flag, perm)
	}
	if p.root == nil {
		return nil, os.ErrNotExist
	}
	return p.root.OpenFile(p.name, flag, perm)
}
func (p *runPayloadFile) Stat() (os.FileInfo, error) {
	if !p.strict {
		return os.Stat(p.key)
	}
	if p.root == nil {
		return nil, os.ErrNotExist
	}
	return p.root.Stat(p.name)
}
func (p *runPayloadFile) syncParent() error {
	if !p.strict {
		return nil
	}
	if p.root == nil {
		return os.ErrNotExist
	}
	return syncRunPayloadRoot(p.root)
}

func (p *runPayloadFile) ReadFile() ([]byte, error) {
	f, e := p.Open()
	if e != nil {
		return nil, e
	}
	defer f.Close()
	return io.ReadAll(f)
}
func (p *runPayloadFile) WriteAtomic(ctx context.Context, data []byte, perm os.FileMode) error {
	if !p.strict {
		return WriteFileAtomic(p.key, data, perm)
	}
	return p.writeReaderAtomic(ctx, bytes.NewReader(data), perm)
}

// writeReaderAtomic publishes an owned reader through the retained strict root.
// Callers must not pass an arbitrary external reader while holding metadata locks.
func (p *runPayloadFile) writeReaderAtomic(ctx context.Context, source io.Reader, perm os.FileMode) error {
	if !p.strict {
		return ErrRunDataCapability
	}
	if p.root == nil {
		return os.ErrNotExist
	}
	var f *os.File
	var tmp string
	for {
		if e := ctx.Err(); e != nil {
			return e
		}
		tmp = "." + p.name + ".atomic-" + rand.Text()
		var e error
		f, e = p.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
		if errors.Is(e, os.ErrExist) {
			continue
		}
		if e != nil {
			return e
		}
		break
	}
	defer func() { _ = p.root.Remove(tmp) }()
	if _, e := io.Copy(f, runMetadataBodyReader{ctx: ctx, source: source}); e != nil {
		f.Close()
		return e
	}
	if e := f.Sync(); e != nil {
		f.Close()
		return e
	}
	if e := f.Close(); e != nil {
		return e
	}
	if e := ctx.Err(); e != nil {
		return e
	}
	if e := p.root.Rename(tmp, p.name); e != nil {
		return e
	}
	return p.syncParent()
}
