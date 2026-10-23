package store

import "context"

// LaunchPersistenceGuard atomically validates an admission and persists the
// run it authorizes. The write must contain database operations only: a cloud
// guard may retry its transaction, but queue publication must happen afterwards.
type LaunchPersistenceGuard func(context.Context, func(context.Context) error) error

type launchPersistenceKey struct{}

// WithLaunchPersistenceGuard carries a scheduler's fenced admission through
// compilation and credential resolution to the publisher's persistence boundary.
func WithLaunchPersistenceGuard(ctx context.Context, guard LaunchPersistenceGuard) context.Context {
	return context.WithValue(ctx, launchPersistenceKey{}, guard)
}

// PersistLaunch commits the run under its admission, when the launching surface
// supplied one. Ordinary launches retain their existing persistence path.
func PersistLaunch(ctx context.Context, write func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if guard, ok := ctx.Value(launchPersistenceKey{}).(LaunchPersistenceGuard); ok {
		return guard(ctx, write)
	}
	return write(ctx)
}
