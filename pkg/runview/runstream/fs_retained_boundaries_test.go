package runstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/store"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func retainedStreamEvent(t *testing.T, s EventSubscription) *store.Event {
	t.Helper()
	select {
	case rows, ok := <-s.Events():
		if !ok || len(rows) == 0 {
			t.Fatalf("event closed/empty; errors %v", retainedStreamErrors(s.Errors()))
		}
		return rows[0]
	case <-time.After(3 * time.Second):
		t.Fatal("event timeout")
	}
	return nil
}
func retainedStreamErrors(c <-chan error) []error {
	var out []error
	for {
		select {
		case e, ok := <-c:
			if !ok {
				return out
			}
			out = append(out, e)
		default:
			return out
		}
	}
}
func retainedStreamLog(t *testing.T, s LogSubscription) LogChunk {
	t.Helper()
	select {
	case v, ok := <-s.Chunks():
		if !ok {
			t.Fatalf("log closed; errors %v", retainedStreamErrors(s.Errors()))
		}
		return v
	case <-time.After(3 * time.Second):
		t.Fatal("log timeout")
	}
	return LogChunk{}
}
func retainedStreamBirth(t *testing.T, st *store.FilesystemRunStore, ctx context.Context, f store.ValueFormat) (context.Context, *store.Run) {
	t.Helper()
	r, e := st.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: "r", WorkflowName: "wf", Format: f})
	if e != nil {
		t.Fatal(e)
	}
	ctx, e = store.BindRunValueFormat(ctx, r)
	if e != nil {
		t.Fatal(e)
	}
	b, e := store.CaptureRunDataBinding(store.RunDataHeader{RunID: r.ID, Format: f, Protocol: r.DataProtocol, BirthID: r.BirthID})
	if e != nil {
		t.Fatal(e)
	}
	ctx, e = store.WithRunDataBinding(ctx, b)
	if e != nil {
		t.Fatal(e)
	}
	return ctx, r
}

type retainedHeaderCancelContext struct {
	context.Context
	cancel context.CancelFunc
}

func (c retainedHeaderCancelContext) Err() error {
	var pcs [16]uintptr
	n := runtime.Callers(2, pcs[:])
	frames := runtime.CallersFrames(pcs[:n])
	for {
		f, more := frames.Next()
		if strings.HasSuffix(f.Function, "store.RunDataHeaderFromJSON") {
			c.cancel()
			break
		}
		if !more {
			break
		}
	}
	return c.Context.Err()
}
func TestFileSourceBoundaryHeaderPureCancellationIsNotPermanent(t *testing.T) {
	t.Parallel()
	_, st, _ := newFileSourceFixture(t)
	ctx, r := retainedStreamBirth(t, st, t.Context(), 1)
	selected, ok, e := store.AsFilesystemStreamStore(st).OpenFilesystemStream(ctx, r.ID, store.FilesystemStreamOpenOptions{})
	if e != nil || !ok {
		t.Fatal(ok, e)
	}
	defer selected.Close()
	base, cancel := context.WithCancel(ctx)
	defer cancel()
	e = selected.Validate(retainedHeaderCancelContext{Context: base, cancel: cancel})
	if !errors.Is(e, context.Canceled) || IsPermanentError(e) {
		t.Fatalf("pure header cancellation became permanent=%v: %v", IsPermanentError(e), e)
	}
}

type retainedCodecCancelContext struct {
	context.Context
	cancel context.CancelFunc
}

func (c retainedCodecCancelContext) Err() error {
	var pcs [16]uintptr
	n := runtime.Callers(2, pcs[:])
	frames := runtime.CallersFrames(pcs[:n])
	for {
		f, more := frames.Next()
		if strings.HasSuffix(f.Function, "store.DecodeValueRecordJSON") {
			c.cancel()
			break
		}
		if !more {
			break
		}
	}
	return c.Context.Err()
}
func TestFileSourceBoundaryExactEventPureCancellationIsNotPermanent(t *testing.T) {
	t.Parallel()
	_, st, _ := newFileSourceFixture(t)
	ctx, r := retainedStreamBirth(t, st, t.Context(), 2)
	selected, ok, e := store.AsFilesystemStreamStore(st).OpenFilesystemStream(ctx, r.ID, store.FilesystemStreamOpenOptions{})
	if e != nil || !ok {
		t.Fatal(ok, e)
	}
	defer selected.Close()
	base, cancel := context.WithCancel(ctx)
	defer cancel()
	_, e = selected.DecodeEvent(retainedCodecCancelContext{Context: base, cancel: cancel}, []byte(`{"run_id":"r","seq":0}`))
	if !errors.Is(e, context.Canceled) || IsPermanentError(e) {
		t.Fatalf("pure event cancellation became permanent=%v: %v", IsPermanentError(e), e)
	}
}

type retainedDecodeCancelContext struct {
	context.Context
	cancel context.CancelFunc
}

func (c retainedDecodeCancelContext) Err() error {
	err := c.Context.Err()
	var pcs [12]uintptr
	n := runtime.Callers(2, pcs[:])
	frames := runtime.CallersFrames(pcs[:n])
	for {
		f, more := frames.Next()
		if strings.HasSuffix(f.Function, "store.DecodeValueRecordJSON") {
			c.cancel()
			break
		}
		if !more {
			break
		}
	}
	return err
}
func TestFileSourceBoundaryTerminalDecodeCancellationKeepsCause(t *testing.T) {
	t.Parallel()
	_, st, root := newFileSourceFixture(t)
	ctx, r := retainedStreamBirth(t, st, t.Context(), 1)
	selected, ok, e := store.AsFilesystemStreamStore(st).OpenFilesystemStream(ctx, r.ID, store.FilesystemStreamOpenOptions{})
	if e != nil || !ok {
		t.Fatal(ok, e)
	}
	defer selected.Close()
	path := filepath.Join(root, "runs", r.ID, "run.json")
	raw, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	var doc map[string]any
	if e = json.Unmarshal(raw, &doc); e != nil {
		t.Fatal(e)
	}
	doc["status"] = map[string]any{"bad": true}
	raw, e = json.Marshal(doc)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(path, raw, 0600); e != nil {
		t.Fatal(e)
	}
	base, cancel := context.WithCancel(ctx)
	defer cancel()
	_, e = selected.Terminal(retainedDecodeCancelContext{Context: base, cancel: cancel})
	if !errors.Is(e, store.ErrEventsCorrupted) || !errors.Is(e, context.Canceled) {
		t.Fatalf("decode failure lost after cancellation: %T %v", e, e)
	}
}
func TestFileSourceBoundaryInitiallyDanglingConfiguredAlias(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	target := filepath.Join(parent, "target")
	alias := filepath.Join(parent, "configured-alias")
	if e := os.Symlink(target, alias); e != nil {
		t.Skip(e)
	}
	st := store.Open(alias)
	src := NewFileSource(st, alias, quietLogger())
	defer src.Close()
	sub, e := src.SubscribeEvents(t.Context(), "r", 0)
	if e != nil {
		t.Fatal(e)
	}
	defer sub.Close()
	time.Sleep(30 * time.Millisecond)
	if e = os.Mkdir(target, 0700); e != nil {
		t.Fatal(e)
	}
	if _, e = st.CreateRun(t.Context(), "r", "wf", nil); e != nil {
		t.Fatal(e)
	}
	if _, e = st.AppendEvent(t.Context(), "r", store.Event{Data: map[string]any{"marker": "trusted-config"}}); e != nil {
		t.Fatal(e)
	}
	if ev := retainedStreamEvent(t, sub); ev.Data["marker"] != "trusted-config" {
		t.Fatal(ev)
	}
}
func TestFileSourceBoundaryLiveNonregularMustStop(t *testing.T) {
	t.Parallel()
	for _, logs := range []bool{false, true} {
		t.Run(fmt.Sprint(logs), func(t *testing.T) {
			src, st, root := newFileSourceFixture(t)
			defer src.Close()
			ctx, r := retainedStreamBirth(t, st, t.Context(), 1)
			var es EventSubscription
			var ls LogSubscription
			var errs <-chan error
			name := "events.jsonl"
			if logs {
				if e := st.AppendRunLog(ctx, r.ID, 0, []byte("seed")); e != nil {
					t.Fatal(e)
				}
				var e error
				ls, e = src.SubscribeLogs(ctx, r.ID, 0)
				if e != nil {
					t.Fatal(e)
				}
				defer ls.Close()
				retainedStreamLog(t, ls)
				errs = ls.Errors()
				name = "run.log"
			} else {
				if _, e := st.AppendEvent(ctx, r.ID, store.Event{}); e != nil {
					t.Fatal(e)
				}
				var e error
				es, e = src.SubscribeEvents(ctx, r.ID, 0)
				if e != nil {
					t.Fatal(e)
				}
				defer es.Close()
				retainedStreamEvent(t, es)
				errs = es.Errors()
			}
			path := filepath.Join(root, "runs", r.ID, name)
			if e := os.Rename(path, path+".saved"); e != nil {
				t.Fatal(e)
			}
			if e := os.Mkdir(path, 0700); e != nil {
				t.Fatal(e)
			}
			select {
			case first, ok := <-errs:
				if !ok {
					t.Fatal("no refusal")
				}
				if !IsPermanentError(first) {
					select {
					case second := <-errs:
						t.Fatalf("nonregular payload retried twice: first=%v second=%v", first, second)
					case <-time.After(time.Second):
						t.Fatalf("nonregular error not typed permanent: %v", first)
					}
				}
			case <-time.After(2 * time.Second):
				t.Fatal("no nonregular refusal")
			}
			if logs {
				select {
				case _, ok := <-ls.Chunks():
					if ok {
						t.Fatal("unexpected log")
					}
				case <-time.After(time.Second):
					t.Fatal("log not closed")
				}
			} else {
				select {
				case _, ok := <-es.Events():
					if ok {
						t.Fatal("unexpected event")
					}
				case <-time.After(time.Second):
					t.Fatal("events not closed")
				}
			}
		})
	}
}
