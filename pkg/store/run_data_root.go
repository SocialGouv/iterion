package store

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// openIncarnationDataRoot retains a root for payload IO. Callers must use its
// methods, never reopen Name(). Each opened child is matched to the directory
// observed before opening, so replacement cannot redirect the retained root.
// Creation syncs every parent even when it existed: a previous failed attempt
// may have created an entry without acknowledging its durability.
func openIncarnationDataRoot(storeRoot string, scope IncarnationDataScope, create bool) (*os.Root, error) {
	if !runBirthAtomicPublicationSupported() {
		return nil, ErrRunDataCapability
	}
	if _, err := scope.components(); err != nil {
		return nil, err
	}
	base, err := openRunDataStoreRoot(storeRoot)
	if err != nil {
		return nil, err
	}
	dir, err := OpenIncarnationDirectoryAt(context.Background(), base, scope, create)
	baseErr := base.Close()
	if err != nil {
		return nil, errors.Join(err, baseErr)
	}
	if baseErr != nil {
		dir.Close()
		return nil, baseErr
	}
	if dir.Root == nil {
		return nil, errors.Join(ErrRunDataIdentityChanged, os.ErrNotExist, dir.Parent.Close())
	}
	if err = dir.Parent.Close(); err != nil {
		dir.Root.Close()
		return nil, err
	}
	return dir.Root, nil
}

func syncRunPayloadRoot(r *os.Root) error {
	f, e := r.Open(".")
	if e != nil {
		return e
	}
	return errors.Join(f.Sync(), f.Close())
}
func openIncarnationChildRoot(parent *os.Root, name string, create bool) (*os.Root, error) {
	if create {
		if e := parent.Mkdir(name, 0700); e != nil && !errors.Is(e, os.ErrExist) {
			return nil, e
		}
	}
	before, e := parent.Lstat(name)
	if e != nil || !before.IsDir() {
		return nil, errors.Join(ErrRunDataIdentityChanged, e)
	}
	child, e := parent.OpenRoot(name)
	if e != nil {
		return nil, e
	}
	after, e := child.Stat(".")
	if e != nil || !os.SameFile(before, after) {
		child.Close()
		return nil, errors.Join(ErrRunDataIdentityChanged, e)
	}
	if create {
		if e = syncRunPayloadRoot(parent); e != nil {
			child.Close()
			return nil, fmt.Errorf("store: persist incarnation ancestor: %w", e)
		}
	}
	return child, nil
}

// openRunDataStoreRoot retains the configured store directory before walking
// an incarnation namespace, including global maintenance of orphaned births.
func openRunDataStoreRoot(storeRoot string) (*os.Root, error) {
	if !runBirthAtomicPublicationSupported() {
		return nil, ErrRunDataCapability
	}
	if storeRoot == "" {
		return nil, ErrInvalidRunDataIdentity
	}
	info, err := os.Lstat(storeRoot)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, ErrRunDataIdentityChanged
	}
	current, err := os.OpenRoot(storeRoot)
	if err != nil {
		return nil, err
	}
	opened, err := current.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		_ = current.Close()
		return nil, errors.Join(ErrRunDataIdentityChanged, err)
	}
	return current, nil
}
