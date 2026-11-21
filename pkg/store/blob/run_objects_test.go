package blob

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/store"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRunObjectsS3AllOperations(t *testing.T) {
	t.Parallel()
	fake, _ := newFakeS3(t, "b")
	// Add HEAD and Range semantics to the existing deterministic HTTP fixture.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, key := fake.splitPath(r.URL.Path)
		if r.Method == http.MethodHead && key != "" || r.Method == http.MethodGet && r.Header.Get("Range") != "" {
			fake.mu.Lock()
			p, ok := fake.objects[key]
			p = bytes.Clone(p)
			fake.mu.Unlock()
			if !ok {
				writeS3Error(w, 404, "NoSuchKey")
				return
			}
			if r.Method == http.MethodHead {
				w.Header().Set("Content-Length", strconv.Itoa(len(p)))
				return
			}
			var from, to int
			if _, e := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &from, &to); e != nil || from < 0 || to >= len(p) || to < from {
				w.WriteHeader(416)
				return
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, to, len(p)))
			w.WriteHeader(206)
			_, _ = w.Write(p[from : to+1])
			return
		}
		fake.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	c, e := NewS3(t.Context(), Config{Bucket: "b", Region: "us-east-1", Endpoint: srv.URL, UsePathStyle: true, AccessKeyID: "test", SecretAccessKey: "test"})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = c.Close() })
	views := make(map[int]RunObjects)
	for _, format := range []int{1, 2} {
		view, e := SelectRunObjects(c, format)
		if e != nil {
			t.Fatal(e)
		}
		views[format] = view
		for _, name := range []string{"Close", "Ping", "PutUpload", "PutIRBlob", "PutArtifact", "RunObjectsForFormat"} {
			if _, ok := reflect.TypeOf(view).MethodByName(name); ok {
				t.Fatalf("view exposes %s", name)
			}
		}
		scratch, ok := view.(ScratchBanker)
		if !ok {
			t.Fatal("S3 lost scratch capability")
		}
		body := []byte(fmt.Sprintf("value-%d", format))
		for _, r := range []string{"r", "neighbor"} {
			for _, write := range []func() error{
				func() error { return view.PutAttachment(t.Context(), r, "a", "f.txt", "text/plain", body) },
				func() error { return view.PutToolBlob(t.Context(), r, "u", "output", body) },
				func() error { return view.PutBackendSession(t.Context(), r, "ref", body) },
				func() error { return scratch.PutScratchBank(t.Context(), r, bytes.NewReader(body), int64(len(body))) },
				func() error {
					return view.PutRunFile(t.Context(), r, "nested/f.txt", "text/plain", bytes.NewReader(body), int64(len(body)))
				},
			} {
				if e := write(); e != nil {
					t.Fatal(e)
				}
			}
			rc, _, e := view.GetAttachment(t.Context(), r, "a", "f.txt")
			if e != nil {
				t.Fatal(e)
			}
			p, e := io.ReadAll(rc)
			_ = rc.Close()
			if e != nil || !bytes.Equal(p, body) {
				t.Fatal(string(p), e)
			}
			signed, e := view.PresignAttachment(t.Context(), r, "a", "f.txt", time.Minute)
			if e != nil {
				t.Fatal(e)
			}
			u, e := url.Parse(signed)
			if e != nil {
				t.Fatal(e)
			}
			key, e := AttachmentKeyForFormat(format, r, "a", "f.txt")
			if e != nil || u.Path != "/b/"+key {
				t.Fatal(signed, key, e)
			}
			response, e := http.Get(signed)
			if e != nil {
				t.Fatal(e)
			}
			p, e = io.ReadAll(response.Body)
			_ = response.Body.Close()
			if e != nil || !bytes.Equal(p, body) {
				t.Fatal(string(p), e)
			}
			p, total, eof, e := view.GetToolBlobRange(t.Context(), r, "u", "output", 1, 3)
			if e != nil || total != 7 || eof || string(p) != "alu" {
				t.Fatal(string(p), total, eof, e)
			}
			p, e = view.GetBackendSession(t.Context(), r, "ref")
			if e != nil || !bytes.Equal(p, body) {
				t.Fatal(string(p), e)
			}
			rc, e = scratch.OpenScratchBank(t.Context(), r)
			if e != nil {
				t.Fatal(e)
			}
			p, e = io.ReadAll(rc)
			_ = rc.Close()
			if e != nil || !bytes.Equal(p, body) {
				t.Fatal(string(p), e)
			}
			rc, obj, e := view.GetRunFile(t.Context(), r, "nested/f.txt")
			if e != nil {
				t.Fatal(e)
			}
			p, e = io.ReadAll(rc)
			_ = rc.Close()
			if e != nil || !bytes.Equal(p, body) || obj.Path != "nested/f.txt" {
				t.Fatal(string(p), obj, e)
			}
			files, e := view.ListRunFiles(t.Context(), r)
			if e != nil || len(files) != 1 || files[0].Path != "nested/f.txt" {
				t.Fatal(files, e)
			}
		}
	}
	// Old public legacy client remains legacy after creation/use of format2 views.
	if e = c.PutAttachment(t.Context(), "r", "legacy", "f", "", []byte("legacy")); e != nil {
		t.Fatal(e)
	}
	for _, format := range []int{1, 2} {
		v := views[format]
		scratch := v.(ScratchBanker)
		for _, del := range []func() error{func() error { return v.DeleteAttachment(t.Context(), "r", "a", "f.txt") }, func() error { return v.DeleteBackendSession(t.Context(), "r", "ref") }, func() error { return scratch.DeleteScratchBank(t.Context(), "r") }, func() error { return v.DeleteRunAttachments(t.Context(), "r") }, func() error { return v.DeleteRunToolBlobs(t.Context(), "r") }, func() error { return v.DeleteRunBackendSessions(t.Context(), "r") }, func() error { return v.DeleteRunFiles(t.Context(), "r") }} {
			if e = del(); e != nil {
				t.Fatal(e)
			}
		}
		for _, key := range fake.keys() {
			root, rest, _ := strings.Cut(key, "/")
			if strings.HasPrefix(rest, "r/") && strings.HasSuffix(root, "-v2") == (format == 2) {
				t.Fatal("sweep missed", key)
			}
		}
		if _, e := views[2].GetBackendSession(t.Context(), "neighbor", "ref"); e != nil {
			t.Fatal("neighbor erased", e)
		}
		if format == 1 {
			if p, e := views[2].GetBackendSession(t.Context(), "r", "ref"); e != nil || string(p) != "value-2" {
				t.Fatal("legacy sweep crossed root", string(p), e)
			}
		}
	}
	before := fake.keys()
	if _, e = SelectRunObjects(c, 99); !errors.Is(e, store.ErrUnsupportedValueFormat) {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(before, fake.keys()) {
		t.Fatal("future changed objects")
	}
}

func TestRunObjectsOptionalCapabilities(t *testing.T) {
	t.Parallel()
	// The wrapper's method set intentionally hides S3's optional capabilities.
	raw := struct{ Client }{&S3Client{}}
	v, e := SelectRunObjects(raw, 1)
	if e != nil {
		t.Fatal(e)
	}
	if _, ok := v.(ScratchBanker); ok {
		t.Fatal("invented scratch capability")
	}
	if _, ok := v.(interface{ Close() error }); ok {
		t.Fatal("leaked lifecycle")
	}
	if _, e = SelectRunObjects(raw, 2); !errors.Is(e, store.ErrValueFormatCapability) {
		t.Fatal(e)
	}
	withScratch := struct {
		Client
		ScratchBanker
	}{raw, &S3Client{}}
	v, e = SelectRunObjects(withScratch, 0)
	if e != nil {
		t.Fatal(e)
	}
	if _, ok := v.(ScratchBanker); !ok {
		t.Fatal("lost genuine legacy scratch")
	}
}
