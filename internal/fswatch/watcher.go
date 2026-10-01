// Package fswatch preserves filesystem watcher errors with resource evidence
// captured in the failing process, where the limits actually apply.
package fswatch

import "github.com/fsnotify/fsnotify"

func NewWatcher() (*fsnotify.Watcher, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, resourceError(err)
	}
	return w, nil
}

// Add is (*fsnotify.Watcher).Add with the same resource evidence NewWatcher
// attaches to a refused CONSTRUCTION. The two calls fail on different
// ceilings: inotify_init1 (behind fsnotify.NewWatcher) returns EMFILE /
// ENFILE / ENOMEM and never ENOSPC, while the call that reports a spent
// max_user_watches budget is inotify_add_watch — this one. Without the
// wrap a refused WATCH reached the log as a bare "no space left on device",
// indistinguishable from a full disk and silent about the limit actually
// hit (#1554); resourceError's ENOSPC arm only ever fires through here.
func Add(w *fsnotify.Watcher, path string) error {
	if err := w.Add(path); err != nil {
		return resourceError(err)
	}
	return nil
}
