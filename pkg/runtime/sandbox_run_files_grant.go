package runtime

import (
	"context"
	"github.com/SocialGouv/iterion/pkg/store"
)

type runFilesGrantProvider interface{ RunFilesGrant() store.RunFilesGrant }

// runFilesForSandbox returns a borrowed provider grant or an Engine-owned
// fallback. Its closer never closes the executor and is a no-op for a borrow.
func (e *Engine) runFilesForSandbox(ctx context.Context, id string) (string, store.RunFilesGrant, func(), error) {
	noop := func() {}
	if provider, ok := e.executor.(runFilesGrantProvider); ok {
		if g := provider.RunFilesGrant(); g != nil {
			bound, err := g.Bind(ctx, id)
			if err != nil {
				return "", nil, noop, err
			}
			if err = g.ValidateHostPath(bound); err != nil {
				return "", nil, noop, err
			}
			return g.HostPath(), g, noop, nil
		}
	}
	if binding, ok := store.RunDataBindingFrom(ctx); ok && binding.Header().Protocol == store.IncarnationRunData {
		grants := store.AsRunFilesGrantStore(e.store)
		if grants == nil {
			return "", nil, noop, store.ErrRunDataCapability
		}
		g, err := grants.OpenRunFilesGrant(ctx, id)
		if err != nil {
			return "", nil, noop, err
		}
		if g == nil {
			return "", nil, noop, store.ErrRunDataCapability
		}
		closeGrant := func() {
			if err := g.Close(); err != nil && e.logger != nil {
				e.logger.Warn("runtime: close run-files grant: %v", err)
			}
		}
		bound, err := g.Bind(ctx, id)
		if err == nil {
			err = g.ValidateHostPath(bound)
		}
		if err != nil {
			closeGrant()
			return "", nil, noop, err
		}
		return g.HostPath(), g, closeGrant, nil
	}
	if files := store.AsRunFilesStore(e.store); files != nil {
		path, err := files.EnsureRunFilesDir(ctx, id)
		if err != nil {
			if e.logger != nil {
				e.logger.Warn("runtime: ensure run-files dir failed for run %s: %v", id, err)
			}
		} else {
			return path, nil, noop, nil
		}
	}
	return "", nil, noop, nil
}
