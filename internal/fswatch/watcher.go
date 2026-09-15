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
