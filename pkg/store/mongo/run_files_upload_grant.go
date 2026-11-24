package mongo

import (
	"context"
	"errors"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/blob"
	"os"
	"path"
	"sort"
)

// UploadRunFilesFrom consumes the originally issued local grant, never a new
// pathname observation. The grant remains owned by its caller after upload.
func (s *Store) UploadRunFilesFrom(ctx context.Context, id string, grant store.RunFilesGrant) (int, error) {
	g, ok := grant.(*mongoRunFilesGrant)
	if !ok || g.owner != s {
		return 0, store.ErrRunDataCapability
	}
	bound, err := g.Bind(ctx, id)
	if err != nil {
		return 0, err
	}
	bound, objects, err := s.captureRunObjects(bound, id)
	if err != nil {
		return 0, err
	}
	root, err := g.OpenRoot(bound)
	if err != nil {
		return 0, err
	}
	defer root.Close()
	uploaded, err := s.uploadRetainedRunFiles(bound, id, objects, root)
	if err != nil {
		return uploaded, err
	}
	if err = s.checkRunObjects(bound, id); err != nil {
		return uploaded, err
	}
	err = removeRunFilesNamespace(bound, &store.IncarnationDirectory{Parent: g.parent, Name: g.name, Root: g.root})
	return uploaded, err
}
func (s *Store) uploadRetainedRunFiles(ctx context.Context, id string, objects blob.RunObjects, root *os.Root) (uploaded int, err error) {
	var walk func(*os.Root, string) error
	walk = func(dir *os.Root, prefix string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		f, err := dir.Open(".")
		if err != nil {
			return err
		}
		entries, readErr := f.ReadDir(-1)
		if err = errors.Join(readErr, f.Close()); err != nil {
			return err
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			relative := path.Join(prefix, entry.Name())
			if entry.IsDir() {
				child, err := store.OpenRunFileDirectoryChild(ctx, dir, entry.Name())
				if err != nil {
					return err
				}
				walkErr := walk(child, relative)
				if err = errors.Join(walkErr, child.Close()); err != nil {
					return err
				}
				continue
			}
			if !entry.Type().IsRegular() {
				continue
			}
			if err = func() (result error) {
				file, info, e := store.OpenRegularRunFileRoot(ctx, dir, entry.Name())
				if e != nil {
					return e
				}
				defer func() {
					result = errors.Join(result, file.Close())
					if result == nil {
						uploaded++
					}
				}()
				if e = objects.PutRunFile(ctx, id, relative, "", newRunFileUploadReader(file, info.Size()), info.Size()); e != nil {
					return fmt.Errorf("put %s: %w", relative, e)
				}
				return s.checkRunObjects(ctx, id)
			}(); err != nil {
				return err
			}
		}
		return nil
	}
	err = walk(root, "")
	return uploaded, err
}
