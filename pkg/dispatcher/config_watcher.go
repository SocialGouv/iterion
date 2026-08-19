package dispatcher

import (
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/SocialGouv/iterion/pkg/errtrack"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// configDebounce coalesces bursts of fsnotify events (most editors
// write+rename, so a single save fires several events).
const configDebounce = 200 * time.Millisecond

// ConfigWatcher tails a dispatcher config file. When the file changes,
// it reloads, validates, and fires onReload with the new *Config. If
// reload fails, it logs the error and keeps the previous config in
// effect (the caller's onReload is not called for invalid reloads).
type ConfigWatcher struct {
	path   string
	logger *iterlog.Logger

	mu        sync.Mutex
	debounce  *time.Timer
	stopOnce  sync.Once
	done      chan struct{}
	fsWatcher *fsnotify.Watcher
}

// NewConfigWatcher prepares a watcher for path. Call Start to begin.
func NewConfigWatcher(path string, logger *iterlog.Logger) (*ConfigWatcher, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	return &ConfigWatcher{
		path:      abs,
		logger:    logger,
		fsWatcher: fw,
		done:      make(chan struct{}),
	}, nil
}

// Start adds the config file's containing directory to the watch set
// and runs the event loop in a new goroutine. onReload is invoked from
// that goroutine; it must be safe for concurrent calls relative to any
// other shared state.
//
// We watch the directory (not the file directly) because most editors
// save by writing to a tmp file then renaming over the target — a
// pattern that fsnotify cannot follow on a per-file watch on Linux.
func (w *ConfigWatcher) Start(onReload func(*Config)) error {
	dir := filepath.Dir(w.path)
	if err := w.fsWatcher.Add(dir); err != nil {
		return err
	}
	errtrack.Go("dispatcher.configWatcher", func() { w.loop(onReload) })
	return nil
}

// Stop cancels the watch and releases fs resources. Safe to call more
// than once.
func (w *ConfigWatcher) Stop() {
	w.stopOnce.Do(func() {
		close(w.done)
		_ = w.fsWatcher.Close()
		w.mu.Lock()
		if w.debounce != nil {
			w.debounce.Stop()
		}
		w.mu.Unlock()
	})
}

func (w *ConfigWatcher) loop(onReload func(*Config)) {
	for {
		select {
		case <-w.done:
			return
		case ev, ok := <-w.fsWatcher.Events:
			if !ok {
				return
			}
			if !w.eventMatches(ev) {
				continue
			}
			w.scheduleReload(onReload)
		case err, ok := <-w.fsWatcher.Errors:
			if !ok {
				return
			}
			w.logger.Warn("dispatcher watcher: %v", err)
		}
	}
}

func (w *ConfigWatcher) eventMatches(ev fsnotify.Event) bool {
	clean, err := filepath.Abs(ev.Name)
	if err != nil {
		return false
	}
	if clean != w.path {
		return false
	}
	return ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Chmod) != 0
}

func (w *ConfigWatcher) scheduleReload(onReload func(*Config)) {
	w.mu.Lock()
	if w.debounce != nil {
		w.debounce.Stop()
	}
	w.debounce = time.AfterFunc(configDebounce, func() {
		cfg, err := Load(w.path)
		if err != nil {
			w.logger.Warn("dispatcher: config reload failed, keeping previous: %v", err)
			return
		}
		w.logger.Info("dispatcher: config reloaded (%s)", w.path)
		onReload(cfg)
	})
	w.mu.Unlock()
}
