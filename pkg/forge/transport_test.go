package forge

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
	"unicode/utf8"
)

// A caller-supplied part name or file name must not be able to smuggle a
// header into the part: DoMultipartFile is exported and general-purpose, and
// the explicit Content-Type it sets is the one an upload validator reads.
func TestDoMultipartFile_HeaderInjectionIsNeutralised(t *testing.T) {
	var (
		gotHeader textproto.MIMEHeader
		gotBytes  []byte
		parseErr  error
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mr, err := r.MultipartReader()
		if err != nil {
			parseErr = err
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		part, err := mr.NextPart()
		if err != nil {
			parseErr = err
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		gotHeader = part.Header
		gotBytes, _ = io.ReadAll(part)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	code, _, err := DoMultipartFile(context.Background(), srv.Client(), http.MethodPut, srv.URL, "t", nil,
		"avatar", "a.png\"\r\nX-Injected: yes\r\nContent-Type: text/plain", "image/png\r\nX-Also: yes", []byte("png"), nil)
	if err != nil || code != http.StatusNoContent || parseErr != nil {
		t.Fatalf("code=%d err=%v parse=%v", code, err, parseErr)
	}
	// The CR/LF are gone, so the smuggled text is inert: it stays INSIDE the
	// parameter value instead of becoming a header of its own.
	if v := gotHeader.Get("X-Injected"); v != "" {
		t.Errorf("a header was injected through the file name: X-Injected=%q", v)
	}
	if v := gotHeader.Get("X-Also"); v != "" {
		t.Errorf("a header was injected through the content type: X-Also=%q", v)
	}
	if cts := gotHeader.Values("Content-Type"); len(cts) != 1 || !strings.HasPrefix(cts[0], "image/png") {
		t.Errorf("the part must carry exactly one Content-Type, the caller's: %q", cts)
	}
	if string(gotBytes) != "png" {
		t.Errorf("body = %q", gotBytes)
	}
}

func TestTrimBody_CutsOnARuneBoundary(t *testing.T) {
	in := "x" + strings.Repeat("€", 200) // 601 bytes, every rune 3 bytes wide
	out := TrimBody([]byte(in))
	if !utf8.ValidString(out) {
		t.Fatalf("TrimBody produced invalid UTF-8: %q", out)
	}
	if !strings.HasSuffix(out, "…") || len(out) > 304 {
		t.Errorf("out = %q (%d bytes)", out, len(out))
	}
	if got := TrimBody([]byte("  short \n body ")); got != "short body" {
		t.Errorf("TrimBody(short) = %q", got)
	}
}

// A success is read whole: a GitLab whose answer outgrows a cap must not turn
// an upload that landed into a decode error stamped on the connection.
func TestDoMultipartFile_LargeSuccessBodyDecodes(t *testing.T) {
	pad := strings.Repeat("x", 20<<10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"avatar_url":"https://gl/u.png","padding":"` + pad + `"}`))
	}))
	defer srv.Close()
	var out struct {
		AvatarURL string `json:"avatar_url"`
	}
	code, _, err := DoMultipartFile(context.Background(), srv.Client(), http.MethodPut, srv.URL, "t", nil, "avatar", "a.png", "image/png", []byte("png"), &out)
	if err != nil || code != http.StatusOK || out.AvatarURL != "https://gl/u.png" {
		t.Fatalf("code=%d err=%v out=%+v", code, err, out)
	}
	refused := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(pad))
	}))
	defer refused.Close()
	if _, body, err := DoMultipartFile(context.Background(), refused.Client(), http.MethodPut, refused.URL, "t", nil, "avatar", "a.png", "image/png", []byte("png"), nil); err != nil || len(body) != 8<<10 {
		t.Fatalf("a refusal's body is capped at 8 KiB: len=%d err=%v", len(body), err)
	}
}
