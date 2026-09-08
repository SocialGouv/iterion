//go:build !linux

package proctest

// NoProcessLeaks runs the suite. Orphan adoption requires Linux subreaper
// support; portable fixture cancellation still runs on other platforms.
func NoProcessLeaks(run func() int) int { return run() }
