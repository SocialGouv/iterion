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

// PersistLaunchRun persists r through PersistLaunch with a callback a guard may
// replay, and is how a launch surface saves its run document under an
// admission.
//
// A run saver advances the run value's CAS version IN PLACE on a successful
// write (mongo.Store.SaveRun, FilesystemRunStore.writeRun), which makes
// "save this run value" a read-modify-write on the caller's own state rather
// than the idempotent operation a replay needs. A guard that runs the write in
// a Mongo transaction replays its callback on a TransientTransactionError, and
// the shape that bites is an abort AFTER the save's write succeeded: the insert
// is rolled back, the in-place version advance is not. Handed the same value a
// second time, the saver writes with its upsert switched off against a document
// that no longer exists, matches nothing, and refuses the launch with
// ErrRunConflict.
//
// In the guard that exists — cloudsched.MongoStore.WithSchedule — the run write
// is the transaction callback's last operation, so that abort is a commit-time
// TransientTransactionError: a primary stepdown between the write and the
// commit, a transaction killed by transactionLifetimeLimitSeconds. A write
// conflict, on the run write or on the lease $inc that precedes it, aborts
// before the version advance (mongo.Store.SaveRun increments only after its
// UpdateOne returned) and replays harmlessly even without this helper.
//
// Each attempt therefore saves a fresh copy of the run as it stood before the
// first one, and only a committed attempt publishes its post-save state back
// onto r. The copy is shallow by design: it isolates the fields a saver
// assigns on the run value, which is every mutation both savers make — they
// replace the pointer fields they clean rather than writing through them.
func PersistLaunchRun(ctx context.Context, r *Run, save func(context.Context, *Run) error) error {
	initial := *r
	committed := initial
	if err := PersistLaunch(ctx, func(ctx context.Context) error {
		attempt := initial
		if err := save(ctx, &attempt); err != nil {
			return err
		}
		committed = attempt
		return nil
	}); err != nil {
		return err
	}
	*r = committed
	return nil
}
