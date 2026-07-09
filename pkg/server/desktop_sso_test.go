package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSafeDesktopRedirect(t *testing.T) {
	ok := []string{
		"http://127.0.0.1:53123/callback",
		"http://127.0.0.1:1/",
		"http://localhost:8080/desktop/cb?x=1",
		"http://[::1]:40000/cb",
	}
	for _, v := range ok {
		if safeDesktopRedirect(v) != v {
			t.Errorf("safeDesktopRedirect(%q) rejected a valid loopback URL", v)
		}
	}
	bad := []string{
		"",
		"http://evil.com:80/cb",             // non-loopback host
		"https://127.0.0.1:53123/cb",        // not http
		"http://127.0.0.1/cb",               // no explicit port
		"http://127.0.0.1:53123/cb#frag",    // fragment
		"http://user:pw@127.0.0.1:53123/cb", // embedded credentials
		"ftp://127.0.0.1:53123/cb",          // wrong scheme
		"//127.0.0.1:53123/cb",              // scheme-relative
		"http://169.254.169.254:80/latest",  // link-local metadata, not loopback
		"http://127.0.0.1.evil.com:80/cb",   // host that only prefixes loopback
	}
	for _, v := range bad {
		if got := safeDesktopRedirect(v); got != "" {
			t.Errorf("safeDesktopRedirect(%q) = %q, want rejected", v, got)
		}
	}
}

func TestHandleDesktopExchange_Unavailable(t *testing.T) {
	// No authSvc / no ticket store → 404 (feature off), never a panic.
	s := &Server{}
	r := httptest.NewRequest(http.MethodPost, "/api/auth/desktop/exchange", strings.NewReader(`{"ticket":"x"}`))
	w := httptest.NewRecorder()
	s.handleDesktopExchange(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when exchange unavailable", w.Code)
	}
}
