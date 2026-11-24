package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

func attachmentFormatServer(t *testing.T) (*Server, *store.FilesystemRunStore) {
	t.Helper()
	srv := uploadTestServer(t)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		srv.runs.Stop(ctx)
	})
	st, err := store.New(srv.runs.StoreRoot())
	if err != nil {
		t.Fatal(err)
	}
	return srv, st
}
func attachmentHTTPBirth(t *testing.T, st *store.FilesystemRunStore, id string, f store.ValueFormat, body string) context.Context {
	t.Helper()
	r, err := store.CreateRunWithValueFormat(context.Background(), st, store.RunCreateSpec{ID: id, WorkflowName: "attachment", Format: f})
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.CaptureRunValueFormat(r)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := store.WithRunValueFormat(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	if err = st.WriteAttachment(ctx, id, store.AttachmentRecord{Name: "doc", OriginalFilename: "input.txt", MIME: "text/plain"}, strings.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	return ctx
}
func attachmentHTTPGet(srv *Server, ctx context.Context, target, id string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, target, nil).WithContext(ctx)
	r.SetPathValue("id", id)
	r.SetPathValue("name", "doc")
	w := httptest.NewRecorder()
	srv.handleServeAttachment(w, r)
	return w
}
func attachmentHTTPPresign(t *testing.T, srv *Server, id string) string {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/runs/"+id+"/attachments/doc/url", nil)
	r.SetPathValue("id", id)
	r.SetPathValue("name", "doc")
	w := httptest.NewRecorder()
	srv.handlePresignAttachment(w, r)
	if w.Code != 200 {
		t.Fatalf("presign handler: %d %s", w.Code, w.Body.String())
	}
	var v struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v.URL
}
func TestAttachmentHTTPFormatsAndOriginalSignedURL(t *testing.T) {
	for _, f := range []store.ValueFormat{1, 2} {
		t.Run(fmt.Sprint(f), func(t *testing.T) {
			srv, st := attachmentFormatServer(t)
			old := attachmentHTTPBirth(t, st, "r", f, "original")
			signed := attachmentHTTPPresign(t, srv, "r")
			get := func(ctx context.Context, u string) *httptest.ResponseRecorder {
				return attachmentHTTPGet(srv, ctx, "http://foreign.example"+u, "r")
			}
			if w := get(context.Background(), signed); w.Code != 200 || w.Body.String() != "original" {
				t.Fatalf("signed control: %d %q", w.Code, w.Body.String())
			}
			if w := attachmentHTTPGet(srv, context.Background(), "http://127.0.0.1/api/runs/r/attachments/doc", "r"); w.Code != 200 || w.Body.String() != "original" {
				t.Fatalf("unsigned control: %d %q", w.Code, w.Body.String())
			}
			wrong, _ := store.WithRunValueFormat(context.Background(), store.RunValueFormat{RunID: "wrong", Format: f})
			if w := get(wrong, signed); w.Code != 409 {
				t.Fatalf("signed URL replaced caller binding: %d %s", w.Code, w.Body.String())
			}
			if err := st.DeleteRun(old, "r"); err != nil {
				t.Fatal(err)
			}
			if n, e := st.PruneDeletionMarkers(context.Background(), time.Now().Add(time.Hour)); e != nil || n != 1 {
				t.Fatalf("prune %d %v", n, e)
			}
			next := attachmentHTTPBirth(t, st, "r", 3-f, "successor")
			if w := get(context.Background(), signed); w.Code != 409 || strings.Contains(w.Body.String(), "successor") {
				t.Fatalf("old signed URL served successor: %d %q", w.Code, w.Body.String())
			}
			fresh := attachmentHTTPPresign(t, srv, "r")
			if w := get(next, fresh); w.Code != 200 || w.Body.String() != "successor" {
				t.Fatalf("successor control %d %q", w.Code, w.Body.String())
			}
			if err := os.WriteFile(filepath.Join(st.Root(), "runs", "r", "run.json"), []byte(`{"id":"r","format_version":99}`), 0600); err != nil {
				t.Fatal(err)
			}
			if w := get(context.Background(), fresh); w.Code != 500 || strings.Contains(w.Body.String(), "successor") {
				t.Fatalf("future header exposed bytes: %d %q", w.Code, w.Body.String())
			}
		})
	}
}

func TestAttachmentHTTPNoSignatureDowngrade(t *testing.T) {
	srv, st := attachmentFormatServer(t)
	attachmentHTTPBirth(t, st, "r", 2, "secret-body")
	signed := attachmentHTTPPresign(t, srv, "r")
	u, err := url.Parse(signed)
	if err != nil {
		t.Fatal(err)
	}
	original := u.Query()
	for _, tc := range []struct {
		name   string
		change func(url.Values)
	}{
		{"strip-format", func(q url.Values) { q.Del("vf") }},
		{"future-format", func(q url.Values) { q.Set("vf", "99") }},
		{"empty-format", func(q url.Values) { q.Set("vf", "") }},
		{"strip-signature", func(q url.Values) { q.Del("sig") }},
		{"strip-expiry", func(q url.Values) { q.Del("exp") }},
		{"empty-signature", func(q url.Values) { q.Set("sig", "") }},
		{"duplicate-format", func(q url.Values) { q.Add("vf", "2") }},
		{"duplicate-expiry", func(q url.Values) { q.Add("exp", q.Get("exp")) }},
		{"duplicate-signature", func(q url.Values) { q.Add("sig", q.Get("sig")) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := url.Values{}
			for k, v := range original {
				q[k] = append([]string(nil), v...)
			}
			tc.change(q)
			// Safe origin is intentional: malformed signed URLs cannot silently
			// downgrade to that otherwise legitimate unsigned reading.
			target := "http://127.0.0.1" + u.Path + "?" + q.Encode()
			w := attachmentHTTPGet(srv, context.Background(), target, "r")
			if w.Code != 403 || strings.Contains(w.Body.String(), "secret-body") {
				t.Fatalf("signature downgrade: %d %q", w.Code, w.Body.String())
			}
		})
	}
}

func TestAttachmentHTTPLegacyAbsentOrphanDeleted(t *testing.T) {
	srv, st := attachmentFormatServer(t)
	ctx := context.Background()
	for _, id := range []string{"absent", "orphan", "deleted"} {
		t.Run(id, func(t *testing.T) {
			if id != "absent" {
				if err := st.WriteAttachment(ctx, id, store.AttachmentRecord{Name: "doc", OriginalFilename: "input.txt", MIME: "text/plain"}, strings.NewReader("legacy")); err != nil {
					t.Fatal(err)
				}
			}
			if id == "deleted" {
				if err := st.DeleteRun(ctx, id); err != nil {
					t.Fatal(err)
				}
			}
			signed := attachmentHTTPPresign(t, srv, id)
			w := attachmentHTTPGet(srv, ctx, "http://foreign.example"+signed, id)
			want := 404
			if id == "orphan" {
				want = 200
			}
			if w.Code != want {
				t.Fatalf("%s handler %d %s", id, w.Code, w.Body.String())
			}
		})
	}
}

func TestAttachmentHTTPRejectsMalformedQueryInsteadOfIgnoringIt(t *testing.T) {
	srv, st := attachmentFormatServer(t)
	attachmentHTTPBirth(t, st, "r", 1, "body")
	signed := attachmentHTTPPresign(t, srv, "r")
	for _, extra := range []string{"&vf=%zz", "&sig=%zz", "&exp=%zz", "&vf=2;sig=ignored", "&%76%66=2&vf=2"} {
		w := attachmentHTTPGet(srv, context.Background(), "http://127.0.0.1"+signed+extra, "r")
		if w.Code != 403 {
			t.Fatalf("bad query accepted: %q -> %d %s", extra, w.Code, w.Body.String())
		}
	}
}
