package cli

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/pkg/connector/identity"
	"github.com/SocialGouv/iterion/pkg/connector/overlay"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
	"github.com/SocialGouv/iterion/pkg/store"
)

func lockConnectorGeneration(dir string) (store.RunLock, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	parent := filepath.Dir(abs)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, err
	}
	parent, err = filepath.EvalSymlinks(parent)
	if err != nil {
		return nil, err
	}
	lock, err := store.AcquireFileLock(filepath.Join(parent, "."+filepath.Base(abs)+".connector-generation.lock"), "connector generation")
	if err != nil {
		return nil, err
	}
	// A crash between the directory renames must not silently seed a fresh
	// identity history over the previous package. Require explicit recovery.
	entries, err := os.ReadDir(parent)
	if err == nil {
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "."+filepath.Base(abs)+".connector-backup-") {
				err = fmt.Errorf("connectors: interrupted replacement; recover the previous package at %s before generating", filepath.Join(parent, entry.Name()))
				break
			}
		}
	}
	if err != nil {
		_ = lock.Unlock()
		return nil, err
	}
	return lock, nil
}

func connectorIdentity(dir string, pkg *spec.Package, ov *overlay.Overlay) (*identity.Lock, *spec.Package, error) {
	lock, err := identity.Load(dir)
	if err != nil {
		return nil, nil, err
	}
	if lock == nil {
		lock = identity.New(pkg.Connector.ID)
		if _, err := os.Stat(filepath.Join(dir, spec.ConnectorFile)); err == nil {
			previous, err := spec.LoadGenerated(dir)
			if err != nil {
				return nil, nil, err
			}
			if err := lock.Reconcile(previous); err != nil {
				return nil, nil, err
			}
			if ov != nil {
				if err := overlay.Apply(previous, ov); err != nil {
					return nil, nil, err
				}
			}
			if err := lock.ObservePublic(previous); err != nil {
				return nil, nil, err
			}
		} else if !os.IsNotExist(err) {
			return nil, nil, err
		}
	}
	if err := lock.Reconcile(pkg); err != nil {
		return nil, nil, err
	}
	// An overlay must never be baked into the generated files. Keep one
	// independent copy for public-name checks and the completeness report.
	body, err := json.Marshal(pkg)
	if err != nil {
		return nil, nil, err
	}
	var merged spec.Package
	if err := json.Unmarshal(body, &merged); err != nil {
		return nil, nil, err
	}
	if ov != nil {
		if err := overlay.Apply(&merged, ov); err != nil {
			return nil, nil, fmt.Errorf("the existing overlay no longer applies to the regenerated package: %w", err)
		}
	}
	if err := lock.ObservePublic(&merged); err != nil {
		return nil, nil, err
	}
	return lock, &merged, nil
}

// Write the complete candidate beside the destination before replacing it.
// Validation and staging failures leave the previous package byte-identical.
// The two renames are rollback-protected, not a claim of crash-atomic exchange.
func writeLockedConnector(dir string, pkg *spec.Package, lock *identity.Lock) (spec.Size, error) {
	var size spec.Size
	dir = filepath.Clean(dir)
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return size, err
	}
	stage, err := os.MkdirTemp(parent, "."+filepath.Base(dir)+".connector-stage-")
	if err != nil {
		return size, err
	}
	defer os.RemoveAll(stage)
	exists := false
	if info, err := os.Lstat(dir); err == nil {
		if !info.IsDir() {
			return size, fmt.Errorf("connectors: destination must be a directory, not a symlink or file")
		}
		exists = true
		// CopyFS follows symlinks, replacing authored links by the contents of
		// their targets. Refuse them before reading any target or staging bytes.
		if err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.IsDir() && !entry.Type().IsRegular() {
				return fmt.Errorf("connectors: cannot stage non-regular authored file %s", path)
			}
			return nil
		}); err != nil {
			return size, err
		}
		if err := os.CopyFS(stage, os.DirFS(dir)); err != nil {
			return size, err
		}
		// CopyFS preserves content and executable bits, not private modes.
		// Preserve the authored files' modes before publishing the candidate.
		if err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(dir, path)
			if err != nil {
				return err
			}
			return os.Chmod(filepath.Join(stage, rel), info.Mode().Perm())
		}); err != nil {
			return size, err
		}
	} else if !os.IsNotExist(err) {
		return size, err
	} else if err := os.Chmod(stage, 0o755); err != nil {
		return size, err
	}
	if err := writePackage(stage, pkg); err != nil {
		return size, err
	}
	if err := lock.Write(stage); err != nil {
		return size, err
	}
	size, err = spec.Measure(stage)
	if err != nil {
		return size, err
	}
	if !exists {
		return size, os.Rename(stage, dir)
	}
	backup, err := os.MkdirTemp(parent, "."+filepath.Base(dir)+".connector-backup-")
	if err != nil {
		return size, err
	}
	if err := os.Remove(backup); err != nil {
		return size, err
	}
	if err := os.Rename(dir, backup); err != nil {
		return size, err
	}
	if err := os.Rename(stage, dir); err != nil {
		if restoreErr := os.Rename(backup, dir); restoreErr != nil {
			return size, fmt.Errorf("connectors: install: %w; restore failed: %v (previous package remains at %s)", err, restoreErr, backup)
		}
		return size, err
	}
	// The package is committed. A backup cleanup error must not report the
	// generation as failed after the old package has already been replaced.
	_ = os.RemoveAll(backup)
	return size, nil
}
