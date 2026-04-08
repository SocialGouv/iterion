package server

import (
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

const (
	debounceDuration = 200 * time.Millisecond
	ignoreWindow     = 2 * time.Second
	ignoreCleanup    = 10 * time.Second

	EventFileCreated  = "file_created"
	EventFileModified = "file_modified"
	EventFileDeleted  = "file_deleted"
)

// Watcher watches WorkDir for .iter file changes and pushes events to a Hub.
type Watcher struct {
	workDir   string
	hub       *Hub
	fsWatcher *fsnotify.Watcher
	done      chan struct{}

	ignoreMu    sync.Mutex
	ignoreUntil map[string]time.Time

	debounceMu sync.Mutex
	debounce   map[string]*time.Timer
}

// NewWatcher creates a new file watcher for the given directory.
func NewWatcher(workDir string, hub *Hub) (*Watcher, error) {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	return &Watcher{
		workDir:     workDir,
		hub:         hub,
		fsWatcher:   fw,
		done:        make(chan struct{}),
		ignoreUntil: make(map[string]time.Time),
		debounce:    make(map[string]*time.Timer),
	}, nil
}

// Start walks the work directory, adds watches, and runs the event loop.
func (w *Watcher) Start() {
	filepath.WalkDir(w.workDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if isSkippedDir(d.Name()) {
				return filepath.SkipDir
			}
			if err := w.fsWatcher.Add(path); err != nil {
				log.Printf("watcher: cannot watch %s: %v", path, err)
			}
		}
		return nil
	})

	go w.cleanupIgnoreEntries()

	for {
		select {
		case <-w.done:
			return
		case ev, ok := <-w.fsWatcher.Events:
			if !ok {
				return
			}
			w.handleEvent(ev)
		case err, ok := <-w.fsWatcher.Errors:
			if !ok {
				return
			}
			log.Printf("watcher error: %v", err)
		}
	}
}

// Stop shuts down the watcher.
func (w *Watcher) Stop() {
	close(w.done)
	w.fsWatcher.Close()

	w.debounceMu.Lock()
	for _, t := range w.debounce {
		t.Stop()
	}
	w.debounceMu.Unlock()
}

// IgnorePath marks a path to be ignored for the next ignoreWindow.
func (w *Watcher) IgnorePath(absPath string) {
	w.ignoreMu.Lock()
	w.ignoreUntil[absPath] = time.Now().Add(ignoreWindow)
	w.ignoreMu.Unlock()
}

func (w *Watcher) cleanupIgnoreEntries() {
	ticker := time.NewTicker(ignoreCleanup)
	defer ticker.Stop()
	for {
		select {
		case <-w.done:
			return
		case <-ticker.C:
			w.ignoreMu.Lock()
			now := time.Now()
			for path, deadline := range w.ignoreUntil {
				if now.After(deadline) {
					delete(w.ignoreUntil, path)
				}
			}
			w.ignoreMu.Unlock()
		}
	}
}

func (w *Watcher) handleEvent(ev fsnotify.Event) {
	absPath := ev.Name

	if ev.Has(fsnotify.Create) {
		if info, err := os.Stat(absPath); err == nil && info.IsDir() {
			if !isSkippedDir(filepath.Base(absPath)) {
				w.fsWatcher.Add(absPath)
			}
			return
		}
	}

	if !isIterFile(absPath) {
		return
	}

	w.ignoreMu.Lock()
	if deadline, ok := w.ignoreUntil[absPath]; ok {
		if time.Now().Before(deadline) {
			delete(w.ignoreUntil, absPath)
			w.ignoreMu.Unlock()
			return
		}
		delete(w.ignoreUntil, absPath)
	}
	w.ignoreMu.Unlock()

	var eventType string
	switch {
	case ev.Has(fsnotify.Create):
		eventType = EventFileCreated
	case ev.Has(fsnotify.Write):
		eventType = EventFileModified
	case ev.Has(fsnotify.Remove), ev.Has(fsnotify.Rename):
		// Rename emits on the old path; the new path triggers a separate Create.
		eventType = EventFileDeleted
	default:
		return
	}

	// Debounce: if multiple events fire for the same path in quick succession,
	// only the last one's eventType is broadcast (each closure captures its own local).
	w.debounceMu.Lock()
	if t, ok := w.debounce[absPath]; ok {
		t.Stop()
	}
	w.debounce[absPath] = time.AfterFunc(debounceDuration, func() {
		w.debounceMu.Lock()
		delete(w.debounce, absPath)
		w.debounceMu.Unlock()

		rel, err := filepath.Rel(w.workDir, absPath)
		if err != nil {
			return
		}
		w.hub.Broadcast(FileEvent{
			Type: eventType,
			Path: filepath.ToSlash(rel),
		})
	})
	w.debounceMu.Unlock()
}
