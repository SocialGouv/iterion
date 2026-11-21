package store

import (
	"context"
	"errors"
	"os"
)

// RunFilesGrantStore exposes a retained directory for incarnation-scoped runs.
// The grant owns descriptors until Close; its path remains an ordinary host path.
type RunFilesGrantStore interface {
	OpenRunFilesGrant(context.Context, string) (RunFilesGrant, error)
}

// RunFilesGrant keeps the original data binding and directory together.
// Bind checks identity without granting execution authority. OpenRoot returns
// a separately owned descriptor; callers close that descriptor themselves.
type RunFilesGrant interface {
	HostPath() string
	Bind(context.Context, string) (context.Context, error)
	ValidateHostPath(context.Context) error
	OpenRoot(context.Context) (*os.Root, error)
	Close() error
}

func AsRunFilesGrantStore(s RunStore) RunFilesGrantStore { return capability[RunFilesGrantStore](s) }

type retainedRunFilesGrant struct {
	path    string
	root    *os.Root
	binding RunDataBinding
	check   func(context.Context) error
}

// NewRetainedRunFilesGrant takes ownership of root on success only. The store's
// check validates the bound original parent, never adopts a later observation.
func NewRetainedRunFilesGrant(path string, root *os.Root, binding RunDataBinding, check func(context.Context) error) (RunFilesGrant, error) {
	if root == nil || check == nil {
		return nil, ErrRunDataCapability
	}
	if _, err := WithRunDataBinding(context.Background(), binding); err != nil {
		return nil, err
	}
	if binding.Header().Protocol != IncarnationRunData {
		return nil, ErrRunDataCapability
	}
	return &retainedRunFilesGrant{path: path, root: root, binding: binding, check: check}, nil
}

func (g *retainedRunFilesGrant) HostPath() string { return g.path }
func (g *retainedRunFilesGrant) Close() error     { return g.root.Close() }
func (g *retainedRunFilesGrant) Bind(ctx context.Context, id string) (context.Context, error) {
	h := g.binding.Header()
	if id != h.RunID {
		return nil, ErrRunDataIdentityChanged
	}
	bound, err := WithRunDataBinding(ctx, g.binding)
	if err != nil {
		return nil, err
	}
	return WithRunValueFormat(bound, RunValueFormat{RunID: id, Format: h.Format})
}
func (g *retainedRunFilesGrant) checked(ctx context.Context) (context.Context, error) {
	bound, err := g.Bind(ctx, g.binding.Header().RunID)
	if err != nil {
		return nil, err
	}
	if err = g.check(bound); err != nil {
		return nil, err
	}
	return bound, bound.Err()
}
func (g *retainedRunFilesGrant) OpenRoot(ctx context.Context) (*os.Root, error) {
	bound, err := g.checked(ctx)
	if err != nil {
		return nil, err
	}
	root, err := g.root.OpenRoot(".")
	if err != nil {
		return nil, err
	}
	if err = bound.Err(); err != nil {
		return nil, errors.Join(err, root.Close())
	}
	return root, nil
}
func (g *retainedRunFilesGrant) ValidateHostPath(ctx context.Context) error {
	bound, err := g.checked(ctx)
	if err != nil {
		return err
	}
	original, err := g.root.Stat(".")
	if err != nil {
		return err
	}
	current, err := os.Lstat(g.path)
	if err != nil {
		return err
	}
	if !current.IsDir() || !os.SameFile(original, current) {
		return ErrRunDataIdentityChanged
	}
	return bound.Err()
}
