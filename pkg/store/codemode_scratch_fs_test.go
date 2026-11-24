package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCodemodeScratchDeleteWaitLeavesStoreMutexFree(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const id = "busy-bank"
	if _, err := s.CreateRun(ctx, id, "test", nil); err != nil {
		t.Fatal(err)
	}
	lock, err := s.lockCodemodeScratch(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Unlock() }()
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		done <- s.DeleteRun(ctx, id)
	}()
	<-started
	blocked := false
	for deadline := time.Now().Add(100 * time.Millisecond); time.Now().Before(deadline); {
		if !s.mu.TryLock() {
			blocked = true
			break
		}
		s.mu.Unlock()
		runtime.Gosched()
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("deletion waiting for the stripe: %v", err)
	}
	if blocked {
		t.Fatal("deletion holds the store mutex while waiting for the bank stripe")
	}
}

func TestCodemodeScratchFilesystemLifecycle(t *testing.T) {
	ctx := t.Context()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	peer := Open(s.Root())
	const id = "scratch-file"
	path := filepath.Join(s.runDir(id), codemodeScratchFilename)
	const key = "k"
	put := func(ctx context.Context) error {
		return peer.PutCodemodeScratch(ctx, id, key, json.RawMessage(`true`), CodemodeScratchLimits{})
	}
	if err := put(ctx); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("put without run: %v", err)
	}
	if _, err := os.Stat(s.runDir(id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing-run put created a run directory: %v", err)
	}
	if _, err := s.CreateRun(ctx, id, "scratch", nil); err != nil {
		t.Fatal(err)
	}
	if err := put(ctx); err != nil {
		t.Fatal(err)
	}
	lock, err := s.lockCodemodeScratch(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	wait, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	err = put(wait)
	cancel()
	waitDelete, cancelDelete := context.WithTimeout(ctx, 30*time.Millisecond)
	deleteErr := peer.DeleteRun(waitDelete, id)
	cancelDelete()
	if unlockErr := lock.Unlock(); unlockErr != nil {
		t.Fatal(unlockErr)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("put waiting for stripe: %v", err)
	}
	if !errors.Is(deleteErr, context.DeadlineExceeded) {
		t.Fatalf("delete waiting for stripe: %v", deleteErr)
	}
	if value, ok, err := peer.LoadCodemodeScratch(ctx, id, key); err != nil || !ok || string(value) != `true` {
		t.Fatal("cancelled operations changed the bank")
	}
	const corrupt = `{"k":"private-value", "incomplete":}`
	if err := WriteFileAtomic(path, []byte(corrupt), filePerm); err != nil {
		t.Fatal(err)
	}
	for _, err := range []error{put(ctx), func() error {
		_, _, err := s.LoadCodemodeScratch(ctx, id, key)
		return err
	}()} {
		if err == nil || strings.Contains(err.Error(), "private-value") {
			t.Fatal("corrupt bank not refused without payload disclosure")
		}
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != corrupt {
		t.Fatal("corrupt bank was modified")
	}
	if err := s.DeleteRun(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("delete left the private file: %v", err)
	}
	if err := put(ctx); !errors.Is(err, ErrRunDeleted) {
		t.Fatalf("late put: %v", err)
	}
	if n, err := peer.PruneDeletionMarkers(ctx, time.Now().Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("prune deletion marker: count=%d err=%v", n, err)
	}
	if err := put(ctx); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("late put after prune: %v", err)
	}
	if _, err := os.Stat(s.runDir(id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("late put created a run directory: %v", err)
	}
}
