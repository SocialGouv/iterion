package forge

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

// A caller-supplied part name or file name must not be able to smuggle a
// header into the part: DoMultipartFile is exported and general-purpose, and
// the explicit Content-Type it sets is the one an upload validator reads.
func TestDoMultipartFile_HeaderInjectionIsNeutralised(t *testing.T) {
	var raw []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	code, _, err := DoMultipartFile(context.Background(), srv.Client(), http.MethodPut, srv.URL, "t", nil,
		"avatar", "a.png\"\r\nX-Injected: yes\r\nContent-Type: text/plain", "image/png\r\nX-Also: yes", []byte("png"), nil)
	if err != nil || code != http.StatusNoContent {
		t.Fatalf("code=%d err=%v", code, err)
	}
	body := string(raw)
	if strings.Contains(body, "X-Injected") || strings.Contains(body, "X-Also") || strings.Contains(body, "text/plain\"\n") {
		t.Fatalf("a header was injected through a part parameter:\n%s", body)
	}
	if strings.Count(body, "Content-Type:") != 1 || !strings.Contains(body, "Content-Type: image/png") {
		t.Fatalf("the part must carry exactly one Content-Type, the caller's:\n%s", body)
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
