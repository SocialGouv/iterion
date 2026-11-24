package runstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/store"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func fsRetainedBirth(t *testing.T, st *store.FilesystemRunStore, f store.ValueFormat, p store.RunDataProtocol) (context.Context, *store.Run) {
	t.Helper()
	ctx := t.Context()
	if e := store.RequireRunDataStorage(p); e != nil {
		if p == store.IncarnationRunData && errors.Is(e, store.ErrRunDataCapability) {
			t.Skip("strict storage admission disabled")
		}
		t.Fatal(e)
	}
	r, e := st.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: "r", WorkflowName: "wf", Format: f, DataProtocol: p})
	if e != nil {
		t.Fatal(e)
	}
	ctx, e = store.BindRunValueFormat(ctx, r)
	if e != nil {
		t.Fatal(e)
	}
	b, e := store.CaptureRunDataBinding(store.RunDataHeader{RunID: r.ID, Format: f, Protocol: p, BirthID: r.BirthID})
	if e != nil {
		t.Fatal(e)
	}
	ctx, e = store.WithRunDataBinding(ctx, b)
	if e != nil {
		t.Fatal(e)
	}
	return ctx, r
}
func fsRetainedEvent(t *testing.T, s EventSubscription) *store.Event {
	t.Helper()
	select {
	case b, ok := <-s.Events():
		if !ok || len(b) != 1 {
			t.Fatalf("events %+v open %v", b, ok)
		}
		return b[0]
	case <-time.After(3 * time.Second):
		t.Fatal("event timeout")
	}
	return nil
}
func fsRetainedLog(t *testing.T, s LogSubscription) LogChunk {
	t.Helper()
	select {
	case b, ok := <-s.Chunks():
		if !ok {
			t.Fatal("log closed")
		}
		return b
	case <-time.After(3 * time.Second):
		t.Fatal("log timeout")
	}
	return LogChunk{}
}
func TestFilesystemRetainedReplayAndTerminal(t *testing.T) {
	fastTerminalPoll(t)
	for _, p := range []store.RunDataProtocol{0, 1} {
		for _, f := range []store.ValueFormat{1, 2} {
			t.Run(fmt.Sprintf("p%d_f%d", p, f), func(t *testing.T) {
				src, st, _ := newFileSourceFixture(t)
				defer src.Close()
				ctx, r := fsRetainedBirth(t, st, f, p)
				data := map[string]any{"marker": "A"}
				if f == 2 {
					data["huge"] = json.Number("1e400")
				}
				if _, e := st.AppendEvent(ctx, r.ID, store.Event{RunID: r.ID, Data: data}); e != nil {
					t.Fatal(e)
				}
				if e := st.AppendRunLog(ctx, r.ID, 0, []byte("prefix-A")); e != nil {
					t.Fatal(e)
				}
				es, e := src.SubscribeEvents(ctx, r.ID, 0)
				if e != nil {
					t.Fatal(e)
				}
				defer es.Close()
				ls, e := src.SubscribeLogs(ctx, r.ID, 7)
				if e != nil {
					t.Fatal(e)
				}
				defer ls.Close()
				a := fsRetainedEvent(t, es)
				if a.Seq != 0 || a.Data["marker"] != "A" {
					t.Fatal(a)
				}
				if f == 2 && a.Data["huge"] != json.Number("1e400") {
					t.Fatalf("codec %T %v", a.Data["huge"], a.Data["huge"])
				}
				l := fsRetainedLog(t, ls)
				if l.Offset != 7 || string(l.Data) != "A" {
					t.Fatal(l)
				}
				if _, e = st.AppendEvent(ctx, r.ID, store.Event{Data: map[string]any{"marker": "last"}}); e != nil {
					t.Fatal(e)
				}
				if e = st.UpdateRunStatus(ctx, r.ID, store.RunStatusFinished, ""); e != nil {
					t.Fatal(e)
				}
				a = fsRetainedEvent(t, es)
				if a.Seq != 1 || a.Data["marker"] != "last" {
					t.Fatal(a)
				}
				select {
				case _, ok := <-es.Events():
					if ok {
						t.Fatal("duplicate")
					}
				case <-time.After(3 * time.Second):
					t.Fatal("terminal did not close")
				}
			})
		}
	}
}
func TestFilesystemRetainedOriginReplacement(t *testing.T) {
	fastTerminalPoll(t)
	src, st, root := newFileSourceFixture(t)
	defer src.Close()
	ctx, r := fsRetainedBirth(t, st, 1, 0)
	if _, e := st.AppendEvent(ctx, r.ID, store.Event{Data: map[string]any{"marker": "A"}}); e != nil {
		t.Fatal(e)
	}
	sub, e := src.SubscribeEvents(ctx, r.ID, 0)
	if e != nil {
		t.Fatal(e)
	}
	defer sub.Close()
	fsRetainedEvent(t, sub)
	if e = os.Rename(filepath.Join(root, "runs", "r"), filepath.Join(root, "runs", "old")); e != nil {
		t.Fatal(e)
	}
	if _, e = st.CreateRun(t.Context(), "r", "wf", nil); e != nil {
		t.Fatal(e)
	}
	if _, e = st.AppendEvent(t.Context(), "r", store.Event{Data: map[string]any{"marker": "B"}}); e != nil {
		t.Fatal(e)
	}
	select {
	case e := <-sub.Errors():
		if !errors.Is(e, store.ErrRunDataIdentityChanged) {
			t.Fatalf("refusal %v", e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no refusal")
	}
	for b := range sub.Events() {
		t.Fatalf("delivered successor %+v", b)
	}
}
func TestFilesystemRetainedHeaderlessOpaqueExact(t *testing.T) {
	src, _, root := newFileSourceFixture(t)
	defer src.Close()
	dir := filepath.Join(root, "runs", "r")
	if e := os.MkdirAll(dir, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(dir, "run.log"), []byte("opaque"), 0600); e != nil {
		t.Fatal(e)
	}
	ctx, e := store.WithRunValueFormat(t.Context(), store.RunValueFormat{RunID: "r", Format: 2})
	if e != nil {
		t.Fatal(e)
	}
	sub, e := src.SubscribeLogs(ctx, "r", 0)
	if e != nil {
		t.Fatal(e)
	}
	defer sub.Close()
	if c := fsRetainedLog(t, sub); string(c.Data) != "opaque" {
		t.Fatal(c)
	}
}
func TestFilesystemRetainedCloseJoinsInitialWait(t *testing.T) {
	src, _, _ := newFileSourceFixture(t)
	sub, e := src.SubscribeEvents(t.Context(), "never-created", 0)
	if e != nil {
		t.Fatal(e)
	}
	finished := make(chan struct{})
	go func() { _ = src.Close(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("source close did not join pending resolution")
	}
	if _, ok := <-sub.Events(); ok {
		t.Fatal("event after close")
	}
	if e = sub.Close(); e != nil {
		t.Fatal(e)
	}
	if _, e = src.SubscribeEvents(t.Context(), "later", 0); e == nil {
		t.Fatal("subscription accepted after source close")
	}
}
func TestFilesystemRetainedLogLeafRotation(t *testing.T) {
	src, st, root := newFileSourceFixture(t)
	defer src.Close()
	ctx, r := fsRetainedBirth(t, st, 1, 0)
	if e := st.AppendRunLog(ctx, r.ID, 0, []byte("AAAA")); e != nil {
		t.Fatal(e)
	}
	sub, e := src.SubscribeLogs(ctx, r.ID, 0)
	if e != nil {
		t.Fatal(e)
	}
	defer sub.Close()
	if c := fsRetainedLog(t, sub); string(c.Data) != "AAAA" {
		t.Fatal(c)
	}
	path := filepath.Join(root, "runs", r.ID, "run.log")
	if e = os.Rename(path, path+".old"); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(path, []byte("BB"), 0600); e != nil {
		t.Fatal(e)
	}
	if c := fsRetainedLog(t, sub); c.Offset != 0 || string(c.Data) != "BB" {
		t.Fatal(c)
	}
}
