package store

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"
)

func TestRunWriteLockAcrossProcess(t *testing.T) {
	const envRoot = "ITERION_TEST_RUN_WRITE_LOCK_ROOT"
	const envMode = "ITERION_TEST_RUN_WRITE_LOCK_MODE"
	if root := os.Getenv(envRoot); root != "" {
		s, err := New(root)
		if err != nil {
			t.Fatal(err)
		}
		if os.Getenv(envMode) == "held" {
			lock, err := AcquireFileLock(filepath.Join(root, ".run-write-locks", "child"), "child probe")
			if err == nil {
				_ = lock.Unlock()
				t.Fatal("child acquired parent's held mutation lock")
			}
			return
		}
		lock, err := s.acquireRunWriteLock("child")
		if err != nil {
			t.Fatal(err)
		}
		if err := lock.Unlock(); err != nil {
			t.Fatal(err)
		}
		return
	}
	s := tmpStore(t)
	lock, err := s.acquireRunWriteLock("child")
	if err != nil {
		t.Fatal(err)
	}
	held := true
	defer func() {
		if held {
			_ = lock.Unlock()
		}
	}()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	probe := func(mode string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), exe, "-test.run=^TestRunWriteLockAcrossProcess$")
		cmd.Env = append(os.Environ(), childHomeEnv(t), envRoot+"="+s.root, envMode+"="+mode)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("child %s: %v\n%s", mode, err, out)
		}
	}
	probe("held")
	if err := lock.Unlock(); err != nil {
		t.Fatal(err)
	}
	held = false
	probe("released")
}

// The fake clock advances only when all bubble goroutines are blocked. Wait
// observes the retry waiting for our real file lock, without a scheduling sleep.
func TestRunWriteLockSerializesLifecycle(t *testing.T) {
	for _, operation := range []string{"create", "save", "delete", "prune"} {
		t.Run(operation, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := tmpStore(t)
				other, err := New(s.root)
				if err != nil {
					t.Fatal(err)
				}
				const id = "serialized"
				var r *Run
				if operation != "create" {
					r, err = s.CreateRun(t.Context(), id, "old", nil)
					if err != nil {
						t.Fatal(err)
					}
				}
				if operation == "prune" {
					if err := s.DeleteRun(t.Context(), id); err != nil {
						t.Fatal(err)
					}
				}
				lock, err := s.acquireRunWriteLock(id)
				if err != nil {
					t.Fatal(err)
				}
				held := true
				defer func() {
					if held {
						_ = lock.Unlock()
					}
				}()
				done := make(chan error, 1)
				go func() {
					switch operation {
					case "create":
						_, err = other.CreateRun(t.Context(), id, "new", nil)
					case "save":
						r.Name = "metadata"
						err = other.SaveRun(t.Context(), r)
					case "delete":
						err = other.DeleteRun(t.Context(), id)
					case "prune":
						var n int
						n, err = other.PruneDeletionMarkers(t.Context(), time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
						if err == nil && n != 1 {
							err = errors.New("prune did not remove one marker")
						}
					}
					done <- err
				}()
				synctest.Wait()
				select {
				case err := <-done:
					t.Fatalf("%s completed while stable lock held: %v", operation, err)
				default:
				}
				// A different run remains writable: this is not a global process lock.
				if _, err := s.CreateRun(t.Context(), "independent", "wf", nil); err != nil {
					t.Fatal(err)
				}
				if err := lock.Unlock(); err != nil {
					t.Fatal(err)
				}
				held = false
				// Receiving lets the fake clock advance to the next lock retry.
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				if operation == "delete" || operation == "prune" {
					if operation == "delete" {
						if _, err := other.PruneDeletionMarkers(t.Context(), time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)); err != nil {
							t.Fatal(err)
						}
					}
					next, err := other.CreateRun(t.Context(), id, "successor", nil)
					if err != nil {
						t.Fatal(err)
					}
					if next.ExecutionEpoch == "" || next.ExecutionEpoch == r.ExecutionEpoch {
						t.Fatal("recreated run reused birth epoch")
					}
					old := RunExecutionExpectation{Version: r.CASVersion, Epoch: r.ExecutionEpoch, Status: r.Status, QueuedAt: r.QueuedAt}
					if _, ok, err := other.AcquireRunExecutionIf(t.Context(), id, old, ExecutionStart); ok || err != nil {
						t.Fatalf("stale claim=%v err=%v", ok, err)
					}
					final, err := other.LoadRun(t.Context(), id)
					if err != nil {
						t.Fatal(err)
					}
					if final.ExecutionEpoch != next.ExecutionEpoch || final.WorkflowName != "successor" || len(final.OutputCorrections) != 0 {
						t.Fatalf("successor corrupted: %+v", final)
					}
				}
			})
		})
	}
}

func TestPruneDeletionMarkerRechecksUnderStableLock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := tmpStore(t)
		other, err := New(s.root)
		if err != nil {
			t.Fatal(err)
		}
		const id = "marker"
		if err := s.DeleteRun(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		lock, err := s.acquireRunWriteLock(id)
		if err != nil {
			t.Fatal(err)
		}
		held := true
		defer func() {
			if held {
				_ = lock.Unlock()
			}
		}()
		done := make(chan int, 1)
		errs := make(chan error, 1)
		go func() {
			n, err := other.PruneDeletionMarkers(t.Context(), time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
			done <- n
			errs <- err
		}()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("prune completed while stable lock held")
		default:
		}
		// The lock owner renews the tombstone after the prune scanned the directory.
		// This fixture writes only a marker, never bypasses an authority check.
		marker := filepath.Join(s.runDir(id), deletionMarkerName)
		newer := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
		if err := os.Chtimes(marker, newer, newer); err != nil {
			t.Fatal(err)
		}
		if err := lock.Unlock(); err != nil {
			t.Fatal(err)
		}
		held = false
		synctest.Wait()
		if n := <-done; n != 0 {
			t.Fatalf("prune used stale marker observation: %d", n)
		}
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		if !s.runDeleted(id) {
			t.Fatal("renewed tombstone lost")
		}
	})
}

// The lock's lifetime must not depend on any object in the purged tree. This
// deliberately removes that tree as a fixture while its lock owner is alive;
// the lifecycle test above separately proves Delete/Prune wait for the owner.
func TestRunWriteLockOutlivesRemovedRunTree(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := tmpStore(t)
		other, err := New(s.root)
		if err != nil {
			t.Fatal(err)
		}
		lock, err := s.acquireRunWriteLock("removed")
		if err != nil {
			t.Fatal(err)
		}
		held := true
		defer func() {
			if held {
				_ = lock.Unlock()
			}
		}()
		if err := os.RemoveAll(s.runDir("removed")); err != nil {
			t.Fatal(err)
		}
		done := make(chan RunLock, 1)
		errs := make(chan error, 1)
		go func() { next, err := other.acquireRunWriteLock("removed"); done <- next; errs <- err }()
		synctest.Wait()
		select {
		case next := <-done:
			if next != nil {
				_ = next.Unlock()
			}
			t.Fatalf("removed run tree replaced a held lock: %v", <-errs)
		default:
		}
		if err := lock.Unlock(); err != nil {
			t.Fatal(err)
		}
		held = false
		next := <-done
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		if err := next.Unlock(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestExecutionBirthIdentityAllFilesystemCreators(t *testing.T) {
	for _, kind := range []string{"run", "child", "queued", "legacy-save"} {
		t.Run(kind, func(t *testing.T) {
			s := tmpStore(t)
			if _, err := s.CreateRun(t.Context(), "parent", "wf", nil); err != nil {
				t.Fatal(err)
			}
			create := func() (*Run, error) {
				switch kind {
				case "child":
					return s.CreateChildRun(t.Context(), "birth", "wf", "parent", nil)
				case "queued":
					return s.CreateQueuedRun(t.Context(), "birth", "wf", "wf.bot", "bot", nil)
				case "legacy-save":
					r := &Run{ID: "birth", Status: RunStatusRunning}
					err := s.SaveRun(t.Context(), r)
					if err != nil {
						return nil, err
					}
					return s.LoadRun(t.Context(), "birth")
				default:
					return s.CreateRun(t.Context(), "birth", "wf", nil)
				}
			}
			a, err := create()
			if err != nil {
				t.Fatal(err)
			}
			if a.ExecutionEpoch == "" {
				t.Fatal("birth has no epoch")
			}
			if err := s.DeleteRun(t.Context(), a.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.PruneDeletionMarkers(t.Context(), time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			b, err := create()
			if err != nil {
				t.Fatal(err)
			}
			if b.ExecutionEpoch == "" || b.ExecutionEpoch == a.ExecutionEpoch {
				t.Fatal("birth epoch reused")
			}
			stale := RunExecutionExpectation{Version: a.CASVersion, Epoch: a.ExecutionEpoch, Status: a.Status, QueuedAt: a.QueuedAt}
			if _, ok, err := s.AcquireRunExecutionIf(t.Context(), a.ID, stale, ExecutionStart); ok || err != nil {
				t.Fatalf("old birth acquired successor: %v %v", ok, err)
			}
		})
	}
}
