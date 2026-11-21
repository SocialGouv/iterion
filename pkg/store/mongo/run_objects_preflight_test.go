package mongo

import (
	"bytes"
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/blob"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/event"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type runObjectsUnreadBody struct{ calls int }

func (r *runObjectsUnreadBody) Read([]byte) (int, error) { r.calls++; return 0, io.EOF }

func TestRunObjectsRefuseBeforeIO(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"future", "malformed", "missing-capability", "cancelled", "old-binding"} {
		t.Run(mode, func(t *testing.T) {
			s, ctx := valueFormatMongo(t, true)
			s.runFilesScratch = filepath.Join(t.TempDir(), "never-created")
			if _, e := s.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: "r", Format: 2}); e != nil {
				t.Fatal(e)
			}
			backing := s.blob.(*valueFormatTestBlob)
			var want error
			switch mode {
			case "future":
				_, e := s.runs.UpdateOne(ctx, bson.M{"_id": "r"}, bson.M{"$set": bson.M{"format_version": 99}})
				if e != nil {
					t.Fatal(e)
				}
				want = store.ErrUnsupportedValueFormat
			case "malformed":
				_, e := s.runs.UpdateOne(ctx, bson.M{"_id": "r"}, bson.M{"$set": bson.M{"format_version": nil}})
				if e != nil {
					t.Fatal(e)
				}
				want = store.ErrInvalidValueFormat
			case "missing-capability":
				s.blob = backing.inMemoryBlob
				want = store.ErrValueFormatCapability
			case "cancelled":
				c, cancel := context.WithCancel(ctx)
				cancel()
				ctx = c
				want = context.Canceled
			case "old-binding":
				ctx = mongoValueBind(t, ctx, "r", 1)
				want = store.ErrValueFormatChanged
			}
			// Snapshot with an uncancelled observer under the same tenant.
			observer := store.WithIdentity(t.Context(), "format-tenant", "tester")
			before, e := s.runs.FindOne(observer, bson.M{"_id": "r"}).Raw()
			if e != nil {
				t.Fatal(e)
			}
			beforeLegacy := len(backing.data)
			beforeExact := len(backing.exact.data)
			body := &runObjectsUnreadBody{}
			calls := map[string]func() error{
				"attachment":     func() error { return s.WriteAttachment(ctx, "r", store.AttachmentRecord{Name: "a"}, body) },
				"delete-one":     func() error { return s.RemoveAttachment(ctx, "r", "a") },
				"delete-all":     func() error { return s.DeleteRunAttachments(ctx, "r") },
				"attempt":        func() error { _, e := s.RecordAttempt(ctx, "r", &store.AttemptSnapshot{}); return e },
				"diff":           func() error { return s.PutRunDiffBlob(ctx, "r", "ref", []byte("x")) },
				"tool":           func() error { _, e := s.WriteToolBlob(ctx, "r", "u", "output", []byte("x")); return e },
				"session":        func() error { return s.PutBackendSession(ctx, "r", "ref", []byte("x")) },
				"delete-session": func() error { return s.DeleteBackendSession(ctx, "r", "ref") },
				"scratch":        func() error { return s.PutScratchBank(ctx, "r", body, 1) },
				"delete-scratch": func() error { return s.DeleteScratchBank(ctx, "r") },
				"path":           func() error { _, e := s.EnsureRunFilesDir(ctx, "r"); return e },
				"upload":         func() error { _, e := s.UploadRunFiles(ctx, "r"); return e },
				"delete-run":     func() error { return s.DeleteRun(ctx, "r") },
			}
			for name, call := range calls {
				if e := call(); !errors.Is(e, want) {
					t.Fatalf("%s err=%v want=%v", name, e, want)
				}
			}
			after, e := s.runs.FindOne(observer, bson.M{"_id": "r"}).Raw()
			if e != nil || !bytes.Equal(before, after) {
				t.Fatal("parent changed", e)
			}
			if body.calls != 0 || beforeLegacy != len(backing.data) || beforeExact != len(backing.exact.data) {
				t.Fatal("effects on refused operation", body.calls, backing.data, backing.exact.data)
			}
			if _, e = os.Stat(s.runFilesScratch); !os.IsNotExist(e) {
				t.Fatal("legacy scratch created", e)
			}
			if _, e = os.Stat(s.runFilesScratch + "-v2"); !os.IsNotExist(e) {
				t.Fatal("exact scratch created", e)
			}
		})
	}
}

func TestRunObjectsAttachmentTransactionRetry(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"write", "delete-one", "delete-all", "attempt"} {
		for _, mode := range []string{"changed", "same-format"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				s, base := valueFormatMongo(t, true)
				ctx, cancel := context.WithTimeout(base, 20*time.Second)
				defer cancel()
				if _, e := s.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: "r", Format: 2}); e != nil {
					t.Fatal(e)
				}
				if e := s.WriteAttachment(ctx, "r", store.AttachmentRecord{Name: "a"}, bytes.NewBufferString("old")); e != nil {
					t.Fatal(e)
				}
				started, release := make(chan struct{}), make(chan struct{})
				unblock := sync.OnceFunc(func() { close(release) })
				defer unblock()
				var armed atomic.Bool
				armed.Store(true)
				writer := valueMonitoredClient(t, s, &event.CommandMonitor{Started: func(_ context.Context, e *event.CommandStartedEvent) {
					if e.CommandName == "update" && e.Command.Lookup("update").StringValue() == colRuns && armed.CompareAndSwap(true, false) {
						close(started)
						select {
						case <-release:
						case <-ctx.Done():
						}
					}
				}})
				done := make(chan error, 1)
				go func() {
					switch kind {
					case "write":
						done <- writer.WriteAttachment(ctx, "r", store.AttachmentRecord{Name: "a"}, bytes.NewBufferString("new"))
					case "delete-one":
						done <- writer.RemoveAttachment(ctx, "r", "a")
					case "delete-all":
						done <- writer.DeleteRunAttachments(ctx, "r")
					case "attempt":
						_, e := writer.RecordAttempt(ctx, "r", &store.AttemptSnapshot{WorkflowHash: "new"})
						done <- e
					}
				}()
				select {
				case <-started:
				case e := <-done:
					t.Fatalf("no parent WRITE: %v", e)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				if mode == "changed" {
					if _, e := s.runs.UpdateOne(ctx, bson.M{"_id": "r"}, bson.M{"$set": bson.M{"format_version": 1}}); e != nil {
						t.Fatal(e)
					}
				} else {
					if e := s.SetRunnerVersion(ctx, "r", "concurrent"); e != nil {
						t.Fatal(e)
					}
				}
				before, e := s.runs.FindOne(ctx, bson.M{"_id": "r"}).Raw()
				if e != nil {
					t.Fatal(e)
				}
				unblock()
				select {
				case e = <-done:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				after, readErr := s.runs.FindOne(ctx, bson.M{"_id": "r"}).Raw()
				if readErr != nil {
					t.Fatal(readErr)
				}
				if mode == "changed" {
					if !errors.Is(e, store.ErrValueFormatChanged) || !bytes.Equal(before, after) {
						t.Fatal("transaction adopted successor", e, !bytes.Equal(before, after))
					}
				} else {
					if e != nil || reflect.DeepEqual(before, after) {
						t.Fatal("legitimate metadata retry refused", e)
					}
				}
			})
		}
	}
}

func TestRunObjectsDeleteRequiresAllExactNamespaces(t *testing.T) {
	t.Parallel()
	s, ctx := valueFormatMongo(t, true)
	if _, e := s.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: "r", Format: 2}); e != nil {
		t.Fatal(e)
	}
	capable := s.blob.(*valueFormatTestBlob)
	// Keeps RunObjects, deliberately hides ValueFormatArtifacts.
	s.blob = struct {
		blob.Client
		blob.ValueFormatRunObjects
	}{capable.inMemoryBlob, capable}
	before, e := s.runs.FindOne(ctx, bson.M{"_id": "r"}).Raw()
	if e != nil {
		t.Fatal(e)
	}
	e = s.DeleteRun(ctx, "r")
	after, readErr := s.runs.FindOne(ctx, bson.M{"_id": "r"}).Raw()
	if !errors.Is(e, store.ErrValueFormatCapability) || readErr != nil || !bytes.Equal(before, after) {
		t.Fatalf("missing namespace changed parent: err=%v read=%v changed=%t", e, readErr, !bytes.Equal(before, after))
	}
}
