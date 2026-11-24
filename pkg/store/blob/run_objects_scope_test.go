package blob

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

func objectScope(t *testing.T, birth string, format store.ValueFormat) store.IncarnationDataScope {
	t.Helper()
	h := store.RunDataHeader{RunID: "run", BirthID: birth, Protocol: store.IncarnationRunData, Format: format}
	b, err := store.CaptureRunDataBinding(h)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := store.WithRunDataBinding(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	scope, err := store.ResolveIncarnationDataScope(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	return scope
}

func TestScopedRunObjectsLateUploadAndCleanup(t *testing.T) {
	t.Parallel()
	fake, _ := newFakeS3(t, "b")
	oldScope := objectScope(t, "birth-a", store.ExactValueFormat)
	newScope := objectScope(t, "birth-a-extra", store.ExactValueFormat)
	oldKey, err := AttachmentKeyForScope(oldScope, "run", "a", "f")
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var held atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, key := fake.splitPath(r.URL.Path)
		if r.Method == http.MethodPut && key == oldKey && held.CompareAndSwap(false, true) {
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		fake.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	c, err := NewS3(t.Context(), Config{Bucket: "b", Region: "us-east-1", Endpoint: srv.URL, UsePathStyle: true, AccessKeyID: "test", SecretAccessKey: "test"})
	if err != nil {
		t.Fatal(err)
	}
	old, err := SelectScopedRunObjects(c, oldScope)
	if err != nil {
		t.Fatal(err)
	}
	next, err := SelectScopedRunObjects(c, newScope)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- old.PutAttachment(ctx, "run", "a", "f", "text/plain", []byte("late-old")) }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("old PUT did not arrive")
	}
	released := false
	defer func() {
		if !released {
			close(release)
		}
		cancel()
	}()
	body := []byte("successor")
	for _, write := range []func() error{
		func() error { return next.PutAttachment(ctx, "run", "a", "f", "text/plain", body) },
		func() error { return next.PutToolBlob(ctx, "run", "tool", "output", body) },
		func() error { return next.PutBackendSession(ctx, "run", "session", body) },
		func() error {
			return next.(ScratchBanker).PutScratchBank(ctx, "run", bytes.NewReader(body), int64(len(body)))
		},
		func() error {
			return next.PutRunFile(ctx, "run", "nested/f", "text/plain", bytes.NewReader(body), int64(len(body)))
		},
	} {
		if err := write(); err != nil {
			t.Fatal(err)
		}
	}
	close(release)
	released = true
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// Both historical format sweeps and a late old-incarnation sweep must
	// leave all successor domains intact, including scratch under sessions.
	for _, format := range []int{1, 2} {
		legacy, err := SelectRunObjects(c, format)
		if err != nil {
			t.Fatal(err)
		}
		for _, cleanup := range []func(context.Context, string) error{legacy.DeleteRunAttachments, legacy.DeleteRunToolBlobs, legacy.DeleteRunBackendSessions, legacy.DeleteRunFiles} {
			if err := cleanup(ctx, "run"); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, cleanup := range []func(context.Context, string) error{old.DeleteRunAttachments, old.DeleteRunToolBlobs, old.DeleteRunBackendSessions, old.DeleteRunFiles} {
		if err := cleanup(ctx, "run"); err != nil {
			t.Fatal(err)
		}
	}
	prefix, err := newScope.BlobPrefix()
	if err != nil {
		t.Fatal(err)
	}
	keys := fake.keys()
	if len(keys) != 5 {
		t.Fatalf("cleanup changed successor domains: %v", keys)
	}
	for _, key := range keys {
		if !strings.HasPrefix(key, prefix) {
			t.Fatalf("wrong namespace: %s", key)
		}
		fake.mu.Lock()
		value := bytes.Clone(fake.objects[key])
		fake.mu.Unlock()
		if !bytes.Equal(value, body) {
			t.Fatalf("late writer changed %s: %s", key, value)
		}
	}
	rc, _, err := next.GetAttachment(ctx, "run", "a", "f")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || !bytes.Equal(got, body) {
		t.Fatal(string(got), err)
	}
	signed, err := next.PresignAttachment(ctx, "run", "a", "f", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(signed)
	if err != nil {
		t.Fatal(err)
	}
	key, err := AttachmentKeyForScope(newScope, "run", "a", "f")
	if err != nil {
		t.Fatal(err)
	}
	if u.Path != "/b/"+key {
		t.Fatalf("signed URL lost scope: %s", u.Path)
	}
	for _, method := range []string{"Close", "Ping", "PutUpload", "PutIRBlob", "PutArtifact", "RunObjectsForFormat", "RunObjectsForScope"} {
		if _, ok := reflect.TypeOf(next).MethodByName(method); ok {
			t.Fatalf("view exposes %s", method)
		}
	}
}

func TestScopedRunObjectsRefuseRebindingAndMissingCapability(t *testing.T) {
	t.Parallel()
	scope := objectScope(t, "birth", store.LegacyValueFormat)
	if _, err := SelectScopedRunObjects(struct{ Client }{&S3Client{}}, scope); !errors.Is(err, store.ErrRunDataCapability) {
		t.Fatal(err)
	}
	if _, err := SelectScopedRunObjects(&S3Client{}, store.IncarnationDataScope{}); !errors.Is(err, store.ErrRunDataBindingRequired) {
		t.Fatal(err)
	}
	view, err := SelectScopedRunObjects(&S3Client{}, scope)
	if err != nil {
		t.Fatal(err)
	}
	// No live transport: every cross-run operation must fail before AWS I/O.
	ctx := t.Context()
	id := "other"
	calls := []func() error{
		func() error { return view.PutAttachment(ctx, id, "a", "f", "text/plain", nil) },
		func() error { _, _, err := view.GetAttachment(ctx, id, "a", "f"); return err },
		func() error { _, err := view.PresignAttachment(ctx, id, "a", "f", time.Minute); return err },
		func() error { return view.DeleteAttachment(ctx, id, "a", "f") },
		func() error { return view.DeleteRunAttachments(ctx, id) },
		func() error { return view.PutToolBlob(ctx, id, "tool", "output", nil) },
		func() error { _, _, _, err := view.GetToolBlobRange(ctx, id, "tool", "output", 0, 1); return err },
		func() error { return view.DeleteRunToolBlobs(ctx, id) },
		func() error { return view.PutBackendSession(ctx, id, "session", nil) },
		func() error { _, err := view.GetBackendSession(ctx, id, "session"); return err },
		func() error { return view.DeleteBackendSession(ctx, id, "session") },
		func() error { return view.DeleteRunBackendSessions(ctx, id) },
		func() error { return view.PutRunFile(ctx, id, "f", "text/plain", bytes.NewReader(nil), 0) },
		func() error { _, err := view.ListRunFiles(ctx, id); return err },
		func() error { _, _, err := view.GetRunFile(ctx, id, "f"); return err },
		func() error { return view.DeleteRunFiles(ctx, id) },
		func() error { return view.(ScratchBanker).PutScratchBank(ctx, id, bytes.NewReader(nil), 0) },
		func() error { _, err := view.(ScratchBanker).OpenScratchBank(ctx, id); return err },
		func() error { return view.(ScratchBanker).DeleteScratchBank(ctx, id) },
	}
	for i, call := range calls {
		if err := call(); !errors.Is(err, store.ErrRunDataIdentityChanged) {
			t.Fatalf("operation %d: %v", i, err)
		}
	}
}
