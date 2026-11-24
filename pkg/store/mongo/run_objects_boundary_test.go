package mongo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/blob"
	"go.mongodb.org/mongo-driver/v2/bson"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type m2DelayedKey struct{}
type m2DelayedBlob struct {
	*valueFormatTestBlob
	entered, release chan struct{}
	once             sync.Once
}

func (b *m2DelayedBlob) hold(c context.Context) error {
	if c.Value(m2DelayedKey{}) == true {
		b.once.Do(func() { close(b.entered) })
		select {
		case <-b.release:
		case <-c.Done():
			return c.Err()
		}
	}
	return nil
}
func (b *m2DelayedBlob) RunObjectsForFormat(f int) (blob.RunObjects, error) {
	v, e := b.valueFormatTestBlob.RunObjectsForFormat(f)
	if e != nil {
		return nil, e
	}
	return &m2DelayedObjects{RunObjects: v, owner: b}, nil
}

type m2DelayedObjects struct {
	blob.RunObjects
	owner *m2DelayedBlob
}

func (v *m2DelayedObjects) PutAttachment(c context.Context, r, n, f, ct string, p []byte) error {
	if e := v.owner.hold(c); e != nil {
		return e
	}
	return v.RunObjects.PutAttachment(c, r, n, f, ct, p)
}
func (v *m2DelayedObjects) PutBackendSession(c context.Context, r, ref string, p []byte) error {
	if e := v.owner.hold(c); e != nil {
		return e
	}
	return v.RunObjects.PutBackendSession(c, r, ref, p)
}
func (v *m2DelayedObjects) PutToolBlob(c context.Context, r, u, k string, p []byte) error {
	if e := v.owner.hold(c); e != nil {
		return e
	}
	return v.RunObjects.PutToolBlob(c, r, u, k, p)
}
func (v *m2DelayedObjects) PutScratchBank(c context.Context, r string, body io.Reader, size int64) error {
	p, e := io.ReadAll(body)
	if e != nil {
		return e
	}
	if e = v.owner.hold(c); e != nil {
		return e
	}
	return v.RunObjects.(blob.ScratchBanker).PutScratchBank(c, r, bytes.NewReader(p), size)
}
func (v *m2DelayedObjects) OpenScratchBank(c context.Context, r string) (io.ReadCloser, error) {
	return v.RunObjects.(blob.ScratchBanker).OpenScratchBank(c, r)
}
func (v *m2DelayedObjects) DeleteScratchBank(c context.Context, r string) error {
	if e := v.owner.hold(c); e != nil {
		return e
	}
	return v.RunObjects.(blob.ScratchBanker).DeleteScratchBank(c, r)
}
func (v *m2DelayedObjects) PutRunFile(c context.Context, r, f, ct string, body io.Reader, size int64) error {
	p, e := io.ReadAll(body)
	if e != nil {
		return e
	}
	if e = v.owner.hold(c); e != nil {
		return e
	}
	return v.RunObjects.PutRunFile(c, r, f, ct, bytes.NewReader(p), size)
}
func (v *m2DelayedObjects) DeleteAttachment(c context.Context, r, n, f string) error {
	if e := v.owner.hold(c); e != nil {
		return e
	}
	return v.RunObjects.DeleteAttachment(c, r, n, f)
}
func (v *m2DelayedObjects) DeleteRunAttachments(c context.Context, r string) error {
	if e := v.owner.hold(c); e != nil {
		return e
	}
	return v.RunObjects.DeleteRunAttachments(c, r)
}
func TestRunObjectsPublicLatePUTKeepsOriginalFormat(t *testing.T) {
	for _, from := range []store.ValueFormat{1, 2} {
		for _, kind := range []string{"attachment", "diff", "session", "scratch", "tool", "runfiles"} {
			t.Run(fmt.Sprintf("%d-to-%d/%s", from, 3-from, kind), func(t *testing.T) {
				s, base := valueFormatMongo(t, true)
				s.runFilesScratch = filepath.Join(t.TempDir(), "scratch")
				ctx, cancel := context.WithTimeout(base, 20*time.Second)
				defer cancel()
				b := &m2DelayedBlob{valueFormatTestBlob: s.blob.(*valueFormatTestBlob), entered: make(chan struct{}), release: make(chan struct{})}
				s.blob = b
				unblock := sync.OnceFunc(func() { close(b.release) })
				defer unblock()
				if _, err := s.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: "r", Format: from}); err != nil {
					t.Fatal(err)
				}
				write := func(c context.Context, p []byte) error {
					switch kind {
					case "attachment":
						return s.WriteAttachment(c, "r", store.AttachmentRecord{Name: "a", OriginalFilename: "a.txt"}, bytes.NewReader(p))
					case "diff":
						return s.PutRunDiffBlob(c, "r", "ref", p)
					case "session":
						return s.PutBackendSession(c, "r", "ref", p)
					case "scratch":
						return s.PutScratchBank(c, "r", bytes.NewReader(p), int64(len(p)))
					case "tool":
						_, e := s.WriteToolBlob(c, "r", "use", "output", p)
						return e
					case "runfiles":
						d, e := s.EnsureRunFilesDir(c, "r")
						if e != nil {
							return e
						}
						if e = os.WriteFile(filepath.Join(d, "a.txt"), p, 0600); e != nil {
							return e
						}
						_, e = s.UploadRunFiles(c, "r")
						return e
					}
					panic(kind)
				}
				read := func() ([]byte, error) {
					var rc io.ReadCloser
					var e error
					switch kind {
					case "attachment":
						rc, _, e = s.OpenAttachment(ctx, "r", "a")
					case "diff":
						return s.GetRunDiffBlob(ctx, "r", "ref")
					case "session":
						return s.GetBackendSession(ctx, "r", "ref")
					case "scratch":
						rc, e = s.OpenScratchBank(ctx, "r")
					case "tool":
						p, _, _, e := s.ReadToolBlob(ctx, "r", "use", "output", 0, 0)
						return p, e
					case "runfiles":
						rc, _, e = s.OpenRunFile(ctx, "r", "a.txt")
					}
					if e != nil {
						return nil, e
					}
					defer rc.Close()
					return io.ReadAll(rc)
				}
				// No caller binding: the public operation must capture its own original.
				old := context.WithValue(ctx, m2DelayedKey{}, true)
				var err error
				done := make(chan error, 1)
				go func() { done <- write(old, []byte("old")) }()
				select {
				case <-b.entered:
				case e := <-done:
					t.Fatalf("no PUT barrier: %v", e)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				if err = s.DeleteRun(ctx, "r"); err != nil {
					t.Fatal(err)
				}
				if n, e := s.PruneDeletionMarkers(ctx, time.Now().Add(time.Second)); e != nil || n != 1 {
					t.Fatal(n, e)
				}
				if _, err = s.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: "r", Format: 3 - from}); err != nil {
					t.Fatal(err)
				}
				next, e := store.WithRunValueFormat(ctx, store.RunValueFormat{RunID: "r", Format: 3 - from})
				if e != nil {
					t.Fatal(e)
				}
				if e = write(next, []byte("new")); e != nil {
					t.Fatal(e)
				}
				if p, e := read(); e != nil || string(p) != "new" {
					t.Fatalf("positive control read=%q err=%v", p, e)
				}
				unblock()
				select {
				case err = <-done:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				got, e := read()
				if e != nil {
					t.Fatal(e)
				}
				if string(got) != "new" || !errors.Is(err, store.ErrValueFormatChanged) {
					t.Fatalf("late write crossed original: got=%q lateerr=%v", got, err)
				}
				t.Logf("public lifecycle %d→DeleteRun→Prune→Create%d: current bytes=%s; old writer err=%v", from, 3-from, got, err)
			})
		}
	}

}

func TestRunObjectsLateAttachmentDeleteAndAttempt(t *testing.T) {
	for _, from := range []store.ValueFormat{1, 2} {
		for _, kind := range []string{"delete-one", "delete-all", "attempt"} {
			t.Run(fmt.Sprintf("%d/%s", from, kind), func(t *testing.T) {
				s, base := valueFormatMongo(t, true)
				ctx, cancel := context.WithTimeout(base, 20*time.Second)
				defer cancel()
				b := &m2DelayedBlob{valueFormatTestBlob: s.blob.(*valueFormatTestBlob), entered: make(chan struct{}), release: make(chan struct{})}
				s.blob = b
				release := sync.OnceFunc(func() { close(b.release) })
				defer release()
				if _, e := s.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: "r", Format: from}); e != nil {
					t.Fatal(e)
				}
				rec := store.AttachmentRecord{Name: "a", OriginalFilename: "a.txt"}
				if e := s.WriteAttachment(ctx, "r", rec, bytes.NewBufferString("old")); e != nil {
					t.Fatal(e)
				}
				old := context.WithValue(ctx, m2DelayedKey{}, true)
				done := make(chan error, 1)
				go func() {
					switch kind {
					case "delete-one":
						done <- s.RemoveAttachment(old, "r", "a")
					case "delete-all":
						done <- s.DeleteRunAttachments(old, "r")
					case "attempt":
						_, e := s.RecordAttempt(old, "r", &store.AttemptSnapshot{WorkflowHash: "old"})
						done <- e
					}
				}()
				select {
				case <-b.entered:
				case e := <-done:
					t.Fatalf("no barrier: %v", e)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				if e := s.DeleteRun(ctx, "r"); e != nil {
					t.Fatal(e)
				}
				if n, e := s.PruneDeletionMarkers(ctx, time.Now().Add(time.Second)); e != nil || n != 1 {
					t.Fatal(n, e)
				}
				if _, e := s.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: "r", Format: 3 - from}); e != nil {
					t.Fatal(e)
				}
				fresh := mongoValueBind(t, ctx, "r", 3-from)
				if e := s.WriteAttachment(fresh, "r", rec, bytes.NewBufferString("new")); e != nil {
					t.Fatal(e)
				}
				meta, e := s.RecordAttempt(fresh, "r", &store.AttemptSnapshot{WorkflowHash: "new"})
				if e != nil {
					t.Fatal(e)
				}
				before, e := s.LoadRun(fresh, "r")
				if e != nil {
					t.Fatal(e)
				}
				release()
				select {
				case e = <-done:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				if !errors.Is(e, store.ErrValueFormatChanged) {
					t.Fatal("old operation must refuse", e)
				}
				after, e := s.LoadRun(fresh, "r")
				if e != nil || after.CASVersion != before.CASVersion || len(after.Attachments) != 1 || len(after.Attempts) != 1 || after.Attempts[0].ID != meta.ID {
					t.Fatal(after, e)
				}
				rc, record, e := s.OpenAttachment(fresh, "r", "a")
				if e != nil {
					t.Fatal(e)
				}
				p, e := io.ReadAll(rc)
				_ = rc.Close()
				if e != nil || string(p) != "new" {
					t.Fatal(string(p), e)
				}
				key, e := blob.AttachmentKeyForFormat(int(3-from), "r", "a", "a.txt")
				if e != nil || record.StorageRef != key {
					t.Fatal(record.StorageRef, key, e)
				}
				snap, e := s.LoadAttempt(fresh, "r", meta.ID)
				if e != nil || snap.WorkflowHash != "new" {
					t.Fatal(snap, e)
				}
			})
		}
	}
}

func TestRunObjectsDelayedPUTRecheckAndMetadataControl(t *testing.T) {
	for _, future := range []bool{false, true} {
		t.Run(fmt.Sprint(future), func(t *testing.T) {
			s, base := valueFormatMongo(t, true)
			ctx, cancel := context.WithTimeout(base, 20*time.Second)
			defer cancel()
			b := &m2DelayedBlob{valueFormatTestBlob: s.blob.(*valueFormatTestBlob), entered: make(chan struct{}), release: make(chan struct{})}
			s.blob = b
			release := sync.OnceFunc(func() { close(b.release) })
			defer release()
			if _, e := s.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: "r", Format: 2}); e != nil {
				t.Fatal(e)
			}
			bound := context.WithValue(mongoValueBind(t, ctx, "r", 2), m2DelayedKey{}, true)
			done := make(chan error, 1)
			go func() { done <- s.PutBackendSession(bound, "r", "ref", []byte("data")) }()
			select {
			case <-b.entered:
			case e := <-done:
				t.Fatal(e)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if future {
				if _, e := s.runs.UpdateOne(ctx, bson.M{"_id": "r"}, bson.M{"$set": bson.M{"format_version": 99}}); e != nil {
					t.Fatal(e)
				}
			} else {
				if e := s.SetRunnerVersion(ctx, "r", "v-new"); e != nil {
					t.Fatal(e)
				}
			}
			release()
			var e error
			select {
			case e = <-done:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if future {
				if !errors.Is(e, store.ErrUnsupportedValueFormat) {
					t.Fatal(e)
				}
			} else {
				if e != nil {
					t.Fatal(e)
				}
				p, e := s.GetBackendSession(ctx, "r", "ref")
				if e != nil || string(p) != "data" {
					t.Fatal(string(p), e)
				}
			}
		})
	}
}
