package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func attachmentFormatRun(t *testing.T, s *FilesystemRunStore, id string, f ValueFormat) context.Context {
	t.Helper()
	r, err := CreateRunWithValueFormat(context.Background(), s, RunCreateSpec{ID: id, WorkflowName: "attachments", Format: f})
	if err != nil {
		t.Fatal(err)
	}
	b, err := CaptureRunValueFormat(r)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := WithRunValueFormat(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}
func attachmentFormatWrite(t *testing.T, s *FilesystemRunStore, ctx context.Context, id, body string) {
	t.Helper()
	if err := s.WriteAttachment(ctx, id, AttachmentRecord{Name: "doc", OriginalFilename: "input.txt", MIME: "text/plain"}, strings.NewReader(body)); err != nil {
		t.Fatal(err)
	}
}
func attachmentFormatCheck(t *testing.T, s *FilesystemRunStore, ctx context.Context, id string, want error) {
	t.Helper()
	rc, _, err := s.OpenAttachment(ctx, id, "doc")
	if rc != nil {
		_ = rc.Close()
	}
	if !errors.Is(err, want) {
		t.Errorf("OpenAttachment: %v, want %v", err, want)
	}
	if want != nil && rc != nil {
		t.Error("refused Open returned a reader")
	}
	rows, err := s.ListAttachments(ctx, id)
	if !errors.Is(err, want) {
		t.Errorf("ListAttachments: %v, want %v", err, want)
	}
	if want != nil && rows != nil {
		t.Error("refused List returned rows")
	}
	signed, err := s.PresignAttachment(ctx, id, "doc", time.Minute)
	if !errors.Is(err, want) {
		t.Errorf("PresignAttachment: %v, want %v", err, want)
	}
	if want != nil && signed != "" {
		t.Error("refused Presign returned URL")
	}
}

func TestAttachmentReadFormatMatrix(t *testing.T) {
	t.Parallel()
	for _, f := range []ValueFormat{1, 2} {
		t.Run(fmt.Sprint(f), func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			ctx := attachmentFormatRun(t, s, "r", f)
			attachmentFormatWrite(t, s, ctx, "r", "body")
			attachmentFormatCheck(t, s, ctx, "r", nil)
			attachmentFormatCheck(t, s, context.Background(), "r", nil)
			wrong, err := WithRunValueFormat(context.Background(), RunValueFormat{RunID: "other", Format: f})
			if err != nil {
				t.Fatal(err)
			}
			attachmentFormatCheck(t, s, wrong, "r", ErrValueFormatChanged)
			opposite, err := WithRunValueFormat(context.Background(), RunValueFormat{RunID: "r", Format: 3 - f})
			if err != nil {
				t.Fatal(err)
			}
			attachmentFormatCheck(t, s, opposite, "r", ErrValueFormatChanged)
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			attachmentFormatCheck(t, s, canceled, "r", context.Canceled)
			attachmentFormatCheck(t, s, nil, "r", ErrInvalidValueFormat)
		})
	}
}

func TestAttachmentReadRejectsMalformedHeader(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, header string
		want         error
	}{
		{"future", "99", ErrUnsupportedValueFormat}, {"null", "null", ErrInvalidValueFormat}, {"array", "[1]", ErrInvalidValueFormat}, {"float", "1.0", ErrInvalidValueFormat}, {"duplicate", "1,\"format_version\":1", ErrInvalidValueFormat},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			ctx := attachmentFormatRun(t, s, "r", 1)
			attachmentFormatWrite(t, s, ctx, "r", "body")
			raw := []byte(`{"id":"r","format_version":` + tc.header + `}`)
			if err := os.WriteFile(filepath.Join(s.Root(), "runs", "r", "run.json"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			attachmentFormatCheck(t, s, context.Background(), "r", tc.want)
		})
	}
}

func TestAttachmentReadLegacyAbsenceAndDeleted(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	for _, id := range []string{"absent", "orphan", "deleted"} {
		if id != "absent" {
			attachmentFormatWrite(t, s, ctx, id, "orphan")
		}
		if id == "deleted" {
			if err := s.DeleteRun(ctx, id); err != nil {
				t.Fatal(err)
			}
		}
		rows, err := s.ListAttachments(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		rc, _, err := s.OpenAttachment(ctx, id, "doc")
		if id == "orphan" {
			if err != nil || len(rows) != 1 {
				t.Fatalf("orphan: rows%d err%v", len(rows), err)
			}
			_ = rc.Close()
		} else if !errors.Is(err, ErrAttachmentNotFound) || len(rows) != 0 {
			t.Fatalf("%s: rows%d err%v", id, len(rows), err)
		}
		if signed, err := s.PresignAttachment(ctx, id, "doc", time.Minute); err != nil || signed == "" {
			t.Fatalf("%s URL: %q %v", id, signed, err)
		}
	}
	wrong, _ := WithRunValueFormat(ctx, RunValueFormat{RunID: "wrong", Format: 1})
	attachmentFormatCheck(t, s, wrong, "deleted", ErrValueFormatChanged)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	attachmentFormatCheck(t, s, canceled, "deleted", context.Canceled)
	bound2, _ := WithRunValueFormat(ctx, RunValueFormat{RunID: "deleted", Format: 2})
	if rows, err := s.ListAttachments(bound2, "deleted"); err != nil || len(rows) != 0 {
		t.Fatalf("headerless tombstone fabricated a format: %v %v", rows, err)
	}
}

func TestAttachmentReadPinsDescriptorAndRejectsRecreatedFormat(t *testing.T) {
	t.Parallel()
	for _, f := range []ValueFormat{1, 2} {
		t.Run(fmt.Sprint(f), func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			other, err := New(s.Root())
			if err != nil {
				t.Fatal(err)
			}
			old := attachmentFormatRun(t, s, "r", f)
			attachmentFormatWrite(t, s, old, "r", "original")
			signed, err := s.PresignAttachment(old, "r", "doc", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			fd, _, err := s.OpenAttachment(old, "r", "doc")
			if err != nil {
				t.Fatal(err)
			}
			defer fd.Close()
			// A live returned descriptor must not retain either store's run lock.
			deleted := make(chan error, 1)
			go func() { deleted <- other.DeleteRun(old, "r") }()
			select {
			case err = <-deleted:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("OpenAttachment retained the stable lock during transfer")
			}
			if n, e := other.PruneDeletionMarkers(context.Background(), time.Now().Add(time.Hour)); e != nil || n != 1 {
				t.Fatalf("prune %d %v", n, e)
			}
			next := attachmentFormatRun(t, other, "r", 3-f)
			attachmentFormatWrite(t, other, next, "r", "successor")
			raw, e := io.ReadAll(fd)
			if e != nil || string(raw) != "original" {
				t.Fatalf("old descriptor changed: %q %v", raw, e)
			}
			attachmentFormatCheck(t, s, old, "r", ErrValueFormatChanged)
			attachmentFormatCheck(t, other, next, "r", nil)
			u, e := url.Parse(signed)
			if e != nil {
				t.Fatal(e)
			}
			q := u.Query()
			rc, _, e := s.OpenPresignedAttachment(context.Background(), "r", "doc", q.Get("exp"), q.Get("sig"), q.Get("vf"))
			if rc != nil {
				_ = rc.Close()
			}
			if !errors.Is(e, ErrValueFormatChanged) || rc != nil {
				t.Fatalf("old signed URL opened successor: %v", e)
			}
		})
	}
}

func TestAttachmentPresignedFormatAndTampering(t *testing.T) {
	t.Parallel()
	for _, f := range []ValueFormat{1, 2} {
		t.Run(fmt.Sprint(f), func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			bound := attachmentFormatRun(t, s, "r", f)
			attachmentFormatWrite(t, s, bound, "r", "body")
			signed, err := s.PresignAttachment(bound, "r", "doc", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			u, err := url.Parse(signed)
			if err != nil {
				t.Fatal(err)
			}
			q := u.Query()
			if (q.Get("vf") == "2") != (f == 2) {
				t.Fatalf("wrong signed format: %s", signed)
			}
			read := func(ctx context.Context, name, exp, sig, vf string, want error) {
				t.Helper()
				rc, _, e := s.OpenPresignedAttachment(ctx, "r", name, exp, sig, vf)
				if rc != nil {
					defer rc.Close()
				}
				if !errors.Is(e, want) {
					t.Fatalf("signed read vf%q: %v want%v", vf, e, want)
				}
				if want != nil && rc != nil {
					t.Fatal("refused signature returned a reader")
				}
				if want == nil {
					b, e := io.ReadAll(rc)
					if e != nil || string(b) != "body" {
						t.Fatalf("body %q %v", b, e)
					}
				}
			}
			read(context.Background(), "doc", q.Get("exp"), q.Get("sig"), q.Get("vf"), nil)
			other, err := New(s.Root())
			if err != nil {
				t.Fatal(err)
			}
			if !other.VerifyAttachmentSignature("r", "doc", q.Get("exp"), q.Get("sig")) && f == 1 {
				t.Fatal("legacy signature lost restart compatibility")
			}
			read(bound, "other", q.Get("exp"), q.Get("sig"), q.Get("vf"), ErrAttachmentSignature)
			read(bound, "doc", "1", q.Get("sig"), q.Get("vf"), ErrAttachmentSignature)
			read(bound, "doc", q.Get("exp"), q.Get("sig"), "99", ErrAttachmentSignature)
			flipped := "2"
			if f == 2 {
				flipped = ""
			}
			read(context.Background(), "doc", q.Get("exp"), q.Get("sig"), flipped, ErrAttachmentSignature)
			opposite, _ := WithRunValueFormat(context.Background(), RunValueFormat{RunID: "r", Format: 3 - f})
			read(opposite, "doc", q.Get("exp"), q.Get("sig"), q.Get("vf"), ErrValueFormatChanged)
			wrong, _ := WithRunValueFormat(context.Background(), RunValueFormat{RunID: "wrong", Format: f})
			read(wrong, "doc", q.Get("exp"), q.Get("sig"), q.Get("vf"), ErrValueFormatChanged)
			canceled, cancel := context.WithCancel(bound)
			cancel()
			read(canceled, "doc", q.Get("exp"), q.Get("sig"), q.Get("vf"), context.Canceled)
		})
	}
}

type attachmentReaderDecorator struct{ RunStore }

func (d attachmentReaderDecorator) Unwrap() RunStore { return d.RunStore }
func TestPresignedAttachmentCapabilityThroughDecorator(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	reader := AsPresignedAttachmentReader(attachmentReaderDecorator{RunStore: s})
	if reader == nil {
		t.Fatal("decorator hid local signed-reader capability")
	}
	ctx := attachmentFormatRun(t, s, "r", 2)
	attachmentFormatWrite(t, s, ctx, "r", "body")
	signed, err := s.PresignAttachment(ctx, "r", "doc", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(signed)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	rc, _, err := reader.OpenPresignedAttachment(ctx, "r", "doc", q.Get("exp"), q.Get("sig"), q.Get("vf"))
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil || string(body) != "body" {
		t.Fatalf("body %q %v", body, err)
	}
}

func TestAttachmentDeletedCompatibilityDoesNotMaskRemainingHeader(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{`{"id":"r","format_version":99}`, `{"id":"r","format_version":null}`} {
		s := newTestStore(t)
		ctx := attachmentFormatRun(t, s, "r", 1)
		if err := s.DeleteRun(ctx, "r"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(s.Root(), "runs", "r", "run.json"), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		want := ErrUnsupportedValueFormat
		if strings.Contains(raw, "null") {
			want = ErrInvalidValueFormat
		}
		attachmentFormatCheck(t, s, context.Background(), "r", want)
	}
}

// The second lookup occurs after the MAC check and before its binding is
// installed. This context barrier controls the public method's interleaving;
// it never alters the context value or any store result.
type attachmentVerifiedBarrier struct {
	context.Context
	lookups  atomic.Int32
	verified chan struct{}
	proceed  chan struct{}
}

func (b *attachmentVerifiedBarrier) Value(key any) any {
	if _, ok := key.(runValueFormatKey); ok && b.lookups.Add(1) == 2 {
		close(b.verified)
		<-b.proceed
	}
	return b.Context.Value(key)
}
func TestAttachmentVerifiedURLRechecksBeforeOpen(t *testing.T) {
	t.Parallel()
	for _, f := range []ValueFormat{1, 2} {
		for _, replace := range []bool{false, true} {
			t.Run(fmt.Sprintf("format%d/replace%v", f, replace), func(t *testing.T) {
				t.Parallel()
				s := newTestStore(t)
				other, err := New(s.Root())
				if err != nil {
					t.Fatal(err)
				}
				old := attachmentFormatRun(t, s, "r", f)
				attachmentFormatWrite(t, s, old, "r", "original")
				signed, err := s.PresignAttachment(old, "r", "doc", time.Minute)
				if err != nil {
					t.Fatal(err)
				}
				u, _ := url.Parse(signed)
				q := u.Query()
				barrier := &attachmentVerifiedBarrier{Context: context.Background(), verified: make(chan struct{}), proceed: make(chan struct{})}
				type result struct {
					body string
					err  error
				}
				done := make(chan result, 1)
				go func() {
					rc, _, e := s.OpenPresignedAttachment(barrier, "r", "doc", q.Get("exp"), q.Get("sig"), q.Get("vf"))
					if e != nil {
						done <- result{err: e}
						return
					}
					defer rc.Close()
					raw, e := io.ReadAll(rc)
					done <- result{body: string(raw), err: e}
				}()
				select {
				case <-barrier.verified:
				case <-time.After(5 * time.Second):
					t.Fatal("verification did not reach pre-open barrier")
				}
				if replace {
					if err = other.DeleteRun(old, "r"); err != nil {
						t.Fatal(err)
					}
					if n, e := other.PruneDeletionMarkers(context.Background(), time.Now().Add(time.Hour)); e != nil || n != 1 {
						t.Fatalf("prune %d %v", n, e)
					}
					next := attachmentFormatRun(t, other, "r", 3-f)
					attachmentFormatWrite(t, other, next, "r", "successor")
				} else {
					r, e := other.LoadRun(old, "r")
					if e != nil {
						t.Fatal(e)
					}
					r.Name = "metadata changed"
					if e = other.SaveRun(old, r); e != nil {
						t.Fatal(e)
					}
				}
				close(barrier.proceed)
				select {
				case got := <-done:
					if replace {
						if !errors.Is(got.err, ErrValueFormatChanged) || got.body != "" {
							t.Fatalf("verified URL adopted successor: %q %v", got.body, got.err)
						}
					} else if got.err != nil || got.body != "original" {
						t.Fatalf("same-format metadata falsely refused: %q %v", got.body, got.err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("signed reader deadlocked")
				}
			})
		}
	}
}
