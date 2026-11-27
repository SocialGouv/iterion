//go:build linux || darwin

package store

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

func birthTestContext(t *testing.T, id string) context.Context {
	t.Helper()
	ctx, err := WithRunValueFormat(t.Context(), RunValueFormat{RunID: id, Format: LegacyValueFormat})
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}
func birthTestStore(t *testing.T) *FilesystemRunStore {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func TestRunBirthUnpublishedRefusalLeavesNoRun(t *testing.T) {
	s := birthTestStore(t)
	ctx := birthTestContext(t, "child")
	b, err := s.BeginRunBirth(ctx, "child")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.BeginRunBirth(ctx, "child"); err == nil {
		t.Fatal("two reservations for same child")
	}
	if entries, e := os.ReadDir(filepath.Join(s.root, "runs")); e != nil || len(entries) != 0 {
		t.Fatalf("prebirth shell: %v %v", entries, e)
	}
	if err = b.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err = b.Unlock(); err != nil {
		t.Fatal("release not idempotent", err)
	}
	if _, err = b.Publish(ctx, RunCreateSpec{ID: "child", Format: LegacyValueFormat}); !errors.Is(err, ErrRunBirthState) {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(s.root, ".run-births"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("own unpublished stage not removed: %v %v", entries, err)
	}
}
func TestRunBirthOldAndNewOwnersRace(t *testing.T) {
	s := birthTestStore(t)
	for i := range 100 {
		id := fmt.Sprintf("race-%d", i)
		ctx := birthTestContext(t, id)
		b, err := s.BeginRunBirth(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		var old RunLock
		var oldErr, publishErr error
		var born *Run
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); <-start; old, oldErr = s.LockRun(ctx, id) }()
		go func() {
			defer wg.Done()
			<-start
			born, publishErr = b.Publish(ctx, RunCreateSpec{ID: id, Format: LegacyValueFormat})
		}()
		close(start)
		wg.Wait()
		// Both contenders retain their locks until both outcomes are observed.
		if (oldErr == nil) == (publishErr == nil) {
			t.Fatalf("double/neither owner: old=%v new=%v", oldErr, publishErr)
		}
		if publishErr != nil && !errors.Is(publishErr, fs.ErrExist) {
			t.Fatal(publishErr)
		}
		if born != nil {
			if born.BirthID == "" || born.ExecutionEpoch == "" {
				t.Fatal("incomplete published row")
			}
			if got, e := s.LoadRun(ctx, id); e != nil || got.BirthID != born.BirthID {
				t.Fatalf("wrong durable birth: %v", e)
			}
		}
		if old != nil {
			if err = old.Unlock(); err != nil {
				t.Fatal(err)
			}
		}
		if err = b.Unlock(); err != nil {
			t.Fatal(err)
		}
	}
}
func TestRunBirthPreservesEmptyCompetingDirectory(t *testing.T) {
	s := birthTestStore(t)
	ctx := birthTestContext(t, "child")
	b, err := s.BeginRunBirth(ctx, "child")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Unlock() }()
	if err = os.Mkdir(s.runDir("child"), dirPerm); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(s.runDir("child"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.Publish(ctx, RunCreateSpec{ID: "child", Format: LegacyValueFormat}); !errors.Is(err, fs.ErrExist) {
		t.Fatal(err)
	}
	after, err := os.Stat(s.runDir("child"))
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("replaced competitor", err)
	}
}

type cancelOnBirthContext struct {
	context.Context
	cancel context.CancelFunc
	path   string
}

func (c cancelOnBirthContext) Err() error {
	if _, err := os.Stat(c.path); err == nil {
		c.cancel()
	}
	return c.Context.Err()
}
func TestRunBirthCancellationAfterPublishRetainsReceipt(t *testing.T) {
	s := birthTestStore(t)
	ctx := birthTestContext(t, "child")
	b, err := s.BeginRunBirth(ctx, "child")
	if err != nil {
		t.Fatal(err)
	}
	active, cancel := context.WithCancel(ctx)
	defer cancel()
	// Trigger cancellation at the first post-publication context observation,
	// using the visible commit itself, rather than a timing-dependent sleep.
	boundary := cancelOnBirthContext{Context: active, cancel: cancel, path: s.runJSONPath("child")}
	born, err := b.Publish(boundary, RunCreateSpec{ID: "child", Format: LegacyValueFormat})
	if !errors.Is(err, context.Canceled) || born == nil || born.BirthID == "" {
		t.Fatalf("lost committed receipt: %v %v", born, err)
	}
	if err = b.Unlock(); err != nil {
		t.Fatal(err)
	}
	if got, e := s.LoadRun(ctx, "child"); e != nil || got.BirthID != born.BirthID {
		t.Fatal("release removed owned published birth", e)
	}
}
func TestRunBirthCrashHelper(t *testing.T) {
	root := os.Getenv("ITERION_TEST_BIRTH_CRASH_ROOT")
	if root == "" {
		return
	}
	s, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := birthTestContext(t, "child")
	b, err := s.BeginRunBirth(ctx, "child")
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("ITERION_TEST_BIRTH_CRASH_PUBLISHED") == "1" {
		if _, err = b.Publish(ctx, RunCreateSpec{ID: "child", Format: LegacyValueFormat}); err != nil {
			t.Fatal(err)
		}
	}
	fmt.Println("BIRTH_READY")
	for {
		time.Sleep(time.Hour)
	}
}
func TestRunBirthProcessCrash(t *testing.T) {
	for _, published := range []bool{false, true} {
		t.Run(fmt.Sprint(published), func(t *testing.T) {
			root := t.TempDir()
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, exe, "-test.run=^TestRunBirthCrashHelper$")
			cmd.Env = append(os.Environ(), childHomeEnv(t), "ITERION_TEST_BIRTH_CRASH_ROOT="+root)
			if published {
				cmd.Env = append(cmd.Env, "ITERION_TEST_BIRTH_CRASH_PUBLISHED=1")
			}
			out, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			cmd.Stderr = os.Stderr
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
			scan := bufio.NewScanner(out)
			ready := false
			for scan.Scan() {
				if scan.Text() == "BIRTH_READY" {
					ready = true
					break
				}
			}
			if !ready {
				t.Fatal("child never reached crash boundary", scan.Err())
			}
			if err = cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err = cmd.Wait(); err == nil {
				t.Fatal("child unexpectedly exited successfully")
			}
			s := Open(root)
			if !published {
				entries, e := os.ReadDir(filepath.Join(root, "runs"))
				if e != nil || len(entries) != 0 {
					t.Fatalf("crash exposed incomplete birth: %v %v", entries, e)
				}
				return
			}
			row, e := s.LoadRun(t.Context(), "child")
			if e != nil || row.BirthID == "" || row.ExecutionEpoch == "" {
				t.Fatalf("crash lost complete birth: %v %v", row, e)
			}
			lock, e := s.LockRun(t.Context(), "child")
			if e != nil {
				t.Fatal("crash retained published lock", e)
			}
			if e = lock.Unlock(); e != nil {
				t.Fatal(e)
			}
		})
	}
}

func TestRunBirthStrictSyncFailureBoundaries(t *testing.T) {
	for _, boundary := range []string{"stage", "root", "destination", "source"} {
		t.Run(boundary, func(t *testing.T) {
			s := birthTestStore(t)
			ctx := birthTestContext(t, "child")
			reservation, err := s.BeginRunBirth(ctx, "child")
			if err != nil {
				t.Fatal(err)
			}
			b := reservation.(*filesystemRunBirth)
			defer func() { _ = b.Unlock() }()
			paths := map[string]string{"stage": b.stage, "root": s.root, "destination": filepath.Join(s.root, "runs"), "source": filepath.Dir(b.stage)}
			b.syncDir = func(path string) error {
				if path == paths[boundary] {
					return syscall.ENOTSUP
				}
				return syncRunBirthDir(path)
			}
			born, err := b.Publish(ctx, RunCreateSpec{ID: "child", Format: LegacyValueFormat})
			if !errors.Is(err, syscall.ENOTSUP) {
				t.Fatalf("unsupported sync acknowledged: receipt=%v err=%v", born, err)
			}
			afterCommit := boundary == "destination" || boundary == "source"
			if !afterCommit {
				if born != nil {
					t.Fatal("unpublished failure returned receipt")
				}
				if _, e := os.Stat(s.runDir("child")); !os.IsNotExist(e) {
					t.Fatal("prepublication failure exposed run", e)
				}
				return
			}
			if born == nil || born.BirthID == "" {
				t.Fatal("lost committed receipt")
			}
			if peer, e := s.LockRun(ctx, "child"); e == nil {
				_ = peer.Unlock()
				t.Fatal("lost published execution lock")
			}
			if e := b.Unlock(); e != nil {
				t.Fatal(e)
			}
			got, e := s.LoadRun(ctx, "child")
			if e != nil || got.BirthID != born.BirthID {
				t.Fatal("release erased committed birth", e)
			}
		})
	}
}

func TestRunBirthSyncDoesNotSuppressUnsupported(t *testing.T) {
	// A non-syncable device exercises the real helper's error path, independent
	// of the reservation's boundary fault injection above.
	f, e := os.Open(os.DevNull)
	if e != nil {
		t.Fatal(e)
	}
	syncErr := f.Sync()
	_ = f.Close()
	if syncErr == nil {
		t.Skip("this platform syncs the null device")
	}
	if e = syncRunBirthDir(os.DevNull); e == nil {
		t.Fatal("strict sync suppressed device error")
	}
}
