package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
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
		"http://evil.com:80/cb",              // non-loopback host
		"https://127.0.0.1:53123/cb",         // not http
		"http://127.0.0.1/cb",                // no explicit port
		"http://127.0.0.1:53123/cb#frag",     // fragment
		"http://user:pw@127.0.0.1:53123/cb",  // embedded credentials
		"ftp://127.0.0.1:53123/cb",           // wrong scheme
		"//127.0.0.1:53123/cb",               // scheme-relative
		"http://169.254.169.254:80/latest",   // link-local metadata, not loopback
		"http://127.0.0.1.evil.com:80/cb",    // host that only prefixes loopback
	}
	for _, v := range bad {
		if got := safeDesktopRedirect(v); got != "" {
			t.Errorf("safeDesktopRedirect(%q) = %q, want rejected", v, got)
		}
	}
}

func TestDesktopTicketStore_SingleUse(t *testing.T) {
	store := newDesktopTicketStore(time.Minute)
	res := auth.LoginResult{AccessToken: "acc-1", RefreshToken: "ref-1"}

	ticket, err := store.mint(res)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if ticket == "" {
		t.Fatal("mint returned empty ticket")
	}

	got, ok := store.redeem(ticket)
	if !ok {
		t.Fatal("first redeem failed")
	}
	if got.AccessToken != "acc-1" || got.RefreshToken != "ref-1" {
		t.Errorf("redeemed result = %+v, want acc-1/ref-1", got)
	}

	// Second redeem must fail — single use.
	if _, ok := store.redeem(ticket); ok {
		t.Error("second redeem succeeded; ticket must be single-use")
	}
	// Unknown ticket fails.
	if _, ok := store.redeem("nope"); ok {
		t.Error("redeeming an unknown ticket succeeded")
	}
}

func TestDesktopTicketStore_Expiry(t *testing.T) {
	store := newDesktopTicketStore(-1 * time.Second) // already expired on mint
	ticket, err := store.mint(auth.LoginResult{AccessToken: "x"})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, ok := store.redeem(ticket); ok {
		t.Error("redeemed an expired ticket")
	}
}

func TestDesktopTicketStore_MintsDistinctTickets(t *testing.T) {
	store := newDesktopTicketStore(time.Minute)
	a, _ := store.mint(auth.LoginResult{AccessToken: "a"})
	b, _ := store.mint(auth.LoginResult{AccessToken: "b"})
	if a == b {
		t.Fatal("two mints produced the same ticket")
	}
	// Each redeems to its own result.
	ra, _ := store.redeem(a)
	rb, _ := store.redeem(b)
	if ra.AccessToken != "a" || rb.AccessToken != "b" {
		t.Errorf("tickets crossed: a=%q b=%q", ra.AccessToken, rb.AccessToken)
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
