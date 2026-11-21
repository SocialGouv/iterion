package runstream

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// streamEnd is how a subscription ended within a deadline: whether it ended,
// its first permanent error and how many warnings it published.
type streamEnd struct {
	ended bool
	fatal error
	warns []error
}

// awaitStreamEnd reads an events (logs false) or logs subscription of id until
// it ends or within elapses.
func awaitStreamEnd(t *testing.T, src *FileSource, id string, logs bool, within time.Duration) streamEnd {
	t.Helper()
	var data <-chan struct{}
	var errs <-chan error
	if logs {
		sub, err := src.SubscribeLogs(t.Context(), id, 0)
		if err != nil {
			return streamEnd{ended: true, fatal: err}
		}
		defer sub.Close()
		data, errs = forward(sub.Chunks()), sub.Errors()
	} else {
		sub, err := src.SubscribeEvents(t.Context(), id, 0)
		if err != nil {
			return streamEnd{ended: true, fatal: err}
		}
		defer sub.Close()
		data, errs = forward(sub.Events()), sub.Errors()
	}
	var out streamEnd
	note := func(err error) {
		if IsPermanentError(err) {
			if out.fatal == nil {
				out.fatal = err
			}
			return
		}
		out.warns = append(out.warns, err)
	}
	deadline := time.After(within)
	for {
		select {
		case _, open := <-data:
			if !open {
				if errs != nil {
					for err := range errs {
						note(err)
					}
				}
				out.ended = true
				return out
			}
		case err, open := <-errs:
			if !open {
				errs = nil
				continue
			}
			note(err)
		case <-deadline:
			return out
		}
	}
}

// forward turns a typed delivery channel into a signal channel closed with it.
func forward[T any](in <-chan T) <-chan struct{} {
	out := make(chan struct{}, 1)
	go func() {
		defer close(out)
		for range in {
			select {
			case out <- struct{}{}:
			default:
			}
		}
	}()
	return out
}

// requireRefused fails unless the stream ended with a permanent refusal
// matching target.
func requireRefused(t *testing.T, what string, end streamEnd, target error) {
	t.Helper()
	if !end.ended {
		t.Fatalf("%s: still retrying after the deadline (%d warnings, first %v)", what, len(end.warns), firstErr(end.warns))
	}
	if !errors.Is(end.fatal, target) {
		t.Fatalf("%s: ended with %v, want %v", what, end.fatal, target)
	}
}

func firstErr(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	return errs[0]
}

// A symlink where the run's metadata lock belongs is not a lock this reader
// may follow: the stream is refused as a replaced run, events and logs alike,
// instead of retrying the open forever.
func TestFileSourceSymlinkedLockIsRefused(t *testing.T) {
	src, st, dir := newFileSourceFixture(t)
	defer src.Close()
	seedHintRun(t, st, "lk")
	lockDir := filepath.Join(dir, ".run-write-locks")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(lockDir, "lk")
	if err := os.Remove(lock); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "elsewhere"), lock); err != nil {
		t.Fatal(err)
	}
	for _, logs := range []bool{false, true} {
		requireRefused(t, map[bool]string{false: "events", true: "logs"}[logs], awaitStreamEnd(t, src, "lk", logs, 1500*time.Millisecond), store.ErrRunDataIdentityChanged)
	}
}

// Open conditions no retry can heal end the subscription with a permanent
// refusal, events and logs alike: a run ID too long for a file name, a store
// root whose ancestor became a file, or a symlink looping on itself.
func TestFileSourceUnhealableOpenIsRefused(t *testing.T) {
	t.Run("run ID too long", func(t *testing.T) {
		src, _, _ := newFileSourceFixture(t)
		defer src.Close()
		id := strings.Repeat("a", 300)
		for _, logs := range []bool{false, true} {
			requireRefused(t, map[bool]string{false: "events", true: "logs"}[logs], awaitStreamEnd(t, src, id, logs, 1500*time.Millisecond), store.ErrInvalidRunDataIdentity)
		}
	})
	for _, tc := range []struct {
		name    string
		replace func(t *testing.T, ancestor string)
	}{
		{"store root ancestor is a file", func(t *testing.T, ancestor string) {
			if err := os.WriteFile(ancestor, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"store root ancestor is a symlink loop", func(t *testing.T, ancestor string) {
			if err := os.Symlink(filepath.Base(ancestor), ancestor); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			ancestor := filepath.Join(base, "a")
			root := filepath.Join(ancestor, "store")
			st, err := store.New(root)
			if err != nil {
				t.Fatal(err)
			}
			seedHintRun(t, st, "r1")
			if err := os.Rename(ancestor, filepath.Join(base, "a.moved")); err != nil {
				t.Fatal(err)
			}
			tc.replace(t, ancestor)
			src := NewFileSource(st, root, quietLogger())
			defer src.Close()
			for _, logs := range []bool{false, true} {
				requireRefused(t, map[bool]string{false: "events", true: "logs"}[logs], awaitStreamEnd(t, src, "r1", logs, 1500*time.Millisecond), store.ErrRunDataIdentityChanged)
			}
		})
	}
}

// FileSource retries name the file source, not the Mongo one.
func TestFileSourceRetryWarningNamesTheFileSource(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permissions do not bind root")
	}
	src, st, dir := newFileSourceFixture(t)
	defer src.Close()
	seedHintRun(t, st, "warned")
	runDir := filepath.Join(dir, "runs", "warned")
	if err := os.Chmod(runDir, 0); err != nil {
		t.Fatal(err)
	}
	restore := sync.OnceFunc(func() {
		if err := os.Chmod(runDir, 0o700); err != nil {
			t.Errorf("restore permissions: %v", err)
		}
	})
	t.Cleanup(restore)
	end := awaitStreamEnd(t, src, "warned", false, 600*time.Millisecond)
	restore()
	if len(end.warns) == 0 {
		t.Fatalf("no retry warning (ended %v, fatal %v)", end.ended, end.fatal)
	}
	if msg := end.warns[0].Error(); !strings.HasPrefix(msg, "runstream/file: ") {
		t.Fatalf("retry warning %q does not name the file source", msg)
	}
}

// A transient failure to open a log stream's selection, not only its log
// existence check, is retried: the bytes arrive once the run is readable.
func TestFileSourceTransientLogSelectionOpenIsRetried(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permissions do not bind root")
	}
	src, st, dir := newFileSourceFixture(t)
	defer src.Close()
	seedHintRun(t, st, "lo")
	runDir := filepath.Join(dir, "runs", "lo")
	if err := os.WriteFile(filepath.Join(runDir, "run.log"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(runDir, 0); err != nil {
		t.Fatal(err)
	}
	restore := sync.OnceFunc(func() {
		if err := os.Chmod(runDir, 0o700); err != nil {
			t.Errorf("restore permissions: %v", err)
		}
	})
	t.Cleanup(restore)
	sub, err := src.SubscribeLogs(t.Context(), "lo", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	select {
	case chunk, open := <-sub.Chunks():
		t.Fatalf("a transient open error ended or fed the log stream: %+v (open %v)", chunk, open)
	case <-time.After(600 * time.Millisecond):
	}
	restore()
	select {
	case chunk, open := <-sub.Chunks():
		if !open || string(chunk.Data) != "hello" {
			t.Fatalf("after access returned: %+v (open %v)", chunk, open)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("log bytes not delivered once readable")
	}
}

// A run resumed during the settle wait goes on as a new round, without a
// warning.
func TestFileSourceResumeDuringSettleIsNotAWarning(t *testing.T) {
	src, st := barrierFixture(t, 150*time.Millisecond, store.EventNodeStarted)
	sub, err := src.SubscribeEvents(t.Context(), "barrier", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	receiveSeq(t, sub, 0, 5*time.Second)
	setStatus(t, st, "barrier", store.RunStatusCancelled)
	time.Sleep(60 * time.Millisecond)
	setStatus(t, st, "barrier", store.RunStatusRunning)
	appendType(t, st, "barrier", store.EventRunResumed)
	receiveSeq(t, sub, 1, 5*time.Second)
	// The settle window elapses; the status read again is running.
	time.Sleep(400 * time.Millisecond)
	select {
	case err := <-sub.Errors():
		t.Fatalf("a resume reported as a warning: %v", err)
	default:
	}
	appendType(t, st, "barrier", store.EventNodeFinished)
	receiveSeq(t, sub, 2, 5*time.Second)
}

// secondCheckRunning reports a terminal status on every check but the second.
type secondCheckRunning struct {
	store.FilesystemStreamSelection
	dir    string
	checks atomic.Int64
}

func (s *secondCheckRunning) WatchHints() (string, string) { return s.dir, s.dir }
func (s *secondCheckRunning) Terminal(context.Context) (bool, error) {
	return s.checks.Add(1) != 2, nil
}

// The settle end drains once more before it reads the status: a terminal
// event that lands after the last tick ends the stream on that event, with no
// status read at all.
func TestFinalDrainSettleEndDrainsBeforeReadingStatus(t *testing.T) {
	s := &secondCheckRunning{dir: t.TempDir()}
	var drains atomic.Int64
	f := &FileSource{logger: quietLogger()}
	ended, err := f.finalDrain(t.Context(), s, fileRun{
		drain:       func(context.Context) error { drains.Add(1); return nil },
		ended:       func() bool { return drains.Load() >= 2 },
		hints:       FileStreamHints{TerminalPoll: time.Hour},
		settle:      50 * time.Millisecond,
		settleEvery: time.Hour,
		warn:        func(error) {},
		fail:        func(error) {},
	}, false)
	if !ended || err != nil || s.checks.Load() != 0 {
		t.Fatalf("finalDrain = %v, %v after %d status reads and %d drains: the settle end read the status before its last drain", ended, err, s.checks.Load(), drains.Load())
	}
}

// failingSecondCheck reports a terminal status, then a transient read error,
// then a running status.
type failingSecondCheck struct {
	store.FilesystemStreamSelection
	dir    string
	checks atomic.Int64
}

func (s *failingSecondCheck) WatchHints() (string, string) { return s.dir, s.dir }
func (s *failingSecondCheck) Terminal(context.Context) (bool, error) {
	switch s.checks.Add(1) {
	case 1:
		return true, nil
	case 2:
		return false, syscall.EACCES
	}
	return false, nil
}

// A transient status read error at the settle end is a warning and a new
// round, not the end of the stream.
func TestFileSourceSettleEndStatusErrorIsRetried(t *testing.T) {
	s := &failingSecondCheck{dir: t.TempDir()}
	var warns atomic.Int64
	f := &FileSource{logger: quietLogger()}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	f.runSelectedFile(ctx, s, fileRun{
		drain:       func(context.Context) error { return nil },
		ended:       func() bool { return false },
		hints:       FileStreamHints{TerminalPoll: time.Hour},
		settle:      50 * time.Millisecond,
		settleEvery: 10 * time.Millisecond,
		warn:        func(error) { warns.Add(1) },
		fail:        func(e error) { t.Errorf("fail: %v", e) },
	})
	if ctx.Err() == nil {
		t.Fatalf("the stream ended on a transient status read error (%d warnings)", warns.Load())
	}
	if warns.Load() == 0 {
		t.Fatal("no warning for the status read error")
	}
}
