package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
)

func listRetainedRunFiles(ctx context.Context, root *runPayloadDirectory) ([]RunFileInfo, error) {
	var out []RunFileInfo
	var walk func(*runPayloadDirectory, string) error
	walk = func(dir *runPayloadDirectory, prefix string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := dir.ReadDir()
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return fs.SkipAll
			}
			return err
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			relative := path.Join(prefix, entry.Name())
			if entry.IsDir() {
				// Names come from ReadDir, not an ID validator: tool directory names
				// such as "part..name" remain legal, as in cleanRunFilePath.
				child, err := openIncarnationChildRoot(dir.parent.root, entry.Name(), false)
				if err != nil {
					if errors.Is(err, os.ErrNotExist) {
						return fs.SkipAll
					}
					return err
				}
				next := &runPayloadDirectory{parent: &runPayloadFile{root: child, strict: true, name: "."}}
				walkErr := walk(next, relative)
				closeErr := next.Close()
				if closeErr != nil {
					return errors.Join(walkErr, closeErr)
				}
				if walkErr != nil {
					return walkErr
				}
				continue
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			out = append(out, RunFileInfo{Path: relative, Size: info.Size(), ModifiedAt: info.ModTime().UTC()})
		}
		return nil
	}
	err := walk(root, "")
	if err != nil && err != fs.SkipAll {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: list run files: %w", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}
