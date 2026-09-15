package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/botregistry"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/fsnotify/fsnotify"
)

func TestAuthoringLocalDelayedWatcherEventsKeepCurrentSourceObservable(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "file.bot")
	before := "workflow original:\n  entry: done\n"
	after := "workflow changed:\n  entry: done\n"
	if err := os.WriteFile(path, []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}
	logger := iterlog.New(iterlog.LevelError, os.Stderr)
	hub := NewHub(logger)
	watcher, err := NewWatcher(root, hub, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Stop()
	locks, err := acquireAuthoringLocalLocks(t.Context(), []string{path})
	if err != nil {
		t.Fatal(err)
	}
	defer closeAuthoringLocalLocks(locks)
	tx, err := prepareAuthoringLocal(locks[0], authoringPreviewFile{Operation: "replace", Before: before, After: after}, watcher.IgnorePath, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.close()
	checkNotification := func(wantName string) {
		t.Helper()
		payload := awaitWatchSignal(t, hub.broadcast)
		var event FileEvent
		if err := json.Unmarshal(payload, &event); err != nil {
			t.Fatal(err)
		}
		if event.Path != "file.bot" || event.Type != EventFileModified {
			t.Fatalf("latest event=%+v", event)
		}
		entries, _, err := botregistry.ListWithDiagnostics(botregistry.ListOptions{Paths: []string{root}})
		if err != nil || len(entries) != 1 || entries[0].MainFile() != path {
			t.Fatalf("discovery did not resolve the live path: %+v %v", entries, err)
		}
		authoringBytes(t, entries[0].MainFile(), "workflow "+wantName+":\n  entry: done\n")
	}
	delayedEvents := func() {
		// Both IgnorePath calls happened before any queued event was consumed.
		// Only the first is suppressed; subsequent events must reconcile correctly.
		watcher.handleEvent(fsnotify.Event{Name: path, Op: fsnotify.Rename})
		watcher.handleEvent(fsnotify.Event{Name: path, Op: fsnotify.Create})
		watcher.handleEvent(fsnotify.Event{Name: path, Op: fsnotify.Write})
	}
	if err := tx.publish(); err != nil {
		t.Fatal(err)
	}
	delayedEvents()
	checkNotification("changed")
	if err := tx.rollback(); err != nil {
		t.Fatal(err)
	}
	delayedEvents()
	checkNotification("original")
	if err := os.WriteFile(path, []byte("workflow independent:\n  entry: done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	watcher.handleEvent(fsnotify.Event{Name: path, Op: fsnotify.Write})
	checkNotification("independent")
}
