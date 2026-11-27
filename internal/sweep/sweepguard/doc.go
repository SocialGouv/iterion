// Package sweepguard holds the one parallel-isolation guard over every
// package under internal/sweep: a test that calls t.Parallel() there may not
// reach the environment, the working directory, the goroutine census, a
// process-wide setter of another package, or a package variable it swaps.
//
// The sweeps came out of packages that each guard their own parallel tests
// (bots, pkg/server, pkg/backend/model), and those guards read only their own
// directory. One guard here, discovering the sweep directories instead of
// listing them, keeps every sweep package — today's and the next one — under
// the same rule without a copy of the guard in each.
package sweepguard
