package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"os"
)

func openReadBackDrainTree(ctx context.Context, dir sandbox.ReadBackDir) (*drainTree, error) {
	if dir.RootOpener == nil {
		if err := os.MkdirAll(dir.HostDir, 0775); err != nil {
			return nil, fmt.Errorf("create host dir %s: %w", dir.HostDir, err)
		}
		root, err := drainRoot(dir.HostDir)
		if err != nil {
			return nil, fmt.Errorf("resolve host dir %s: %w", dir.HostDir, err)
		}
		return openDrainTree(root)
	}
	root, err := dir.RootOpener.OpenRoot(ctx)
	if err != nil {
		return nil, err
	}
	if root == nil {
		return nil, fmt.Errorf("read-back root opener returned no root")
	}
	tree, openErr := openDrainTreeFromRoot(root, dir.HostDir)
	closeErr := root.Close()
	if err = errors.Join(openErr, closeErr, ctx.Err()); err != nil {
		if tree != nil {
			tree.close()
		}
		return nil, err
	}
	return tree, nil
}
func openDrainTreeFromRoot(root *os.Root, displayPath string) (*drainTree, error) {
	dir, err := openDrainRootFromRetained(root, displayPath)
	if err != nil {
		return nil, err
	}
	return &drainTree{root: dir}, nil
}
