package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

// OpenDirectory selects a checked child from this retained directory. Its
// caller owns the returned capability independently of the parent capability.
func (d *runPayloadDirectory) OpenDirectory(ctx context.Context, name string, create bool) (result *runPayloadDirectory, err error) {
	if ctx == nil {
		return nil, ErrRunDataBindingRequired
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := sanitizePathComponent("payload directory", name); err != nil {
		return nil, err
	}
	defer func() {
		if err == nil {
			if e := ctx.Err(); e != nil {
				if result != nil {
					_ = result.Close()
				}
				result, err = nil, e
			}
		}
	}()
	file := &runPayloadFile{key: filepath.Join(d.parent.key, name), name: ".", strict: d.parent.strict}
	if !file.strict {
		if create {
			if err = os.MkdirAll(file.key, dirPerm); err != nil {
				return nil, err
			}
		}
		return &runPayloadDirectory{parent: file}, nil
	}
	if d.parent.root == nil {
		if create {
			return nil, os.ErrNotExist
		}
		return &runPayloadDirectory{parent: file}, nil
	}
	file.root, err = openIncarnationChildRoot(d.parent.root, name, create)
	if err != nil {
		if !create && errors.Is(err, os.ErrNotExist) {
			return &runPayloadDirectory{parent: file}, nil
		}
		return nil, err
	}
	if create {
		if err = syncRunPayloadRoot(file.root); err != nil {
			_ = file.Close()
			return nil, err
		}
	}
	return &runPayloadDirectory{parent: file}, nil
}
