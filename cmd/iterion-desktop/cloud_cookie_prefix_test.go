package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	iserver "github.com/SocialGouv/iterion/pkg/server"
)

// TestCloudLoginHarvestsEitherCookieSpelling is the regression guard for the
// worst defect this whole change could have shipped.
//
// The desktop harvests the refresh token out of Set-Cookie. It recognised the
// cookie by a private copy of the literal `iterion_refresh`, so when the
// server began emitting `__Host-iterion_refresh` the harvest matched nothing
// and returned "" — which seed() reads as "not rotated this hop" and answers
// by KEEPING the previous token. The next hop replays a token the server has
// already rotated; the server correctly reads a replay as theft and revokes
// EVERY session that user holds, on every device. On a ~13-minute refresh
// loop, and with no `cloud:auth-expired` emitted because the resulting error
// is not a *cloudAuthError, so the user just gets silent 401s.
//
// Both spellings are asserted because the deployment may emit either: the
// prefix is withheld where its terms cannot be met (no TLS, or an explicit
// cookie domain).
func TestCloudLoginHarvestsEitherCookieSpelling(t *testing.T) {
	for _, prefix := range []string{"", iserver.HostCookiePrefix} {
		name := "legacy"
		if prefix != "" {
			name = "host-prefixed (what production emits)"
		}
		t.Run(name, func(t *testing.T) {
			f := &fakeCloud{cookiePrefix: prefix}
			srv := httptest.NewServer(f.handler())
			defer srv.Close()

			res, err := cloudLogin(context.Background(), newCloudHTTPClient(), srv.URL, "a@b.io", "pw")
			if err != nil {
				t.Fatalf("cloudLogin: %v", err)
			}
			if res.RefreshToken == "" {
				t.Fatalf("no refresh token harvested from %q — the next refresh will replay a rotated token and revoke every session this user holds",
					prefix+cloudRefreshCookieName)
			}
			if res.RefreshToken != "refresh-tok-1" {
				t.Errorf("refresh = %q, want refresh-tok-1", res.RefreshToken)
			}
		})
	}
}

// TestHarvestRefreshCookieMatchesBothSpellings pins the matcher directly, so a
// failure names the cause rather than a downstream symptom.
func TestHarvestRefreshCookieMatchesBothSpellings(t *testing.T) {
	for _, name := range []string{
		cloudRefreshCookieName,
		iserver.HostCookiePrefix + cloudRefreshCookieName,
	} {
		got := harvestRefreshCookie([]*http.Cookie{
			{Name: "unrelated", Value: "x"},
			{Name: name, Value: "tok"},
		})
		if got != "tok" {
			t.Errorf("harvestRefreshCookie(%q) = %q; want tok", name, got)
		}
	}
}

// TestProxyStripsBothSpellingsFromTheWebview guards the boundary "the cloud's
// session cookies never reach the wails webview". The strip was keyed on the
// bare names AND gated behind a successful harvest, so when the server started
// emitting `__Host-` the guard silently went false and forwarded both cookies
// into the webview — where `wails.localhost` counts as potentially-trustworthy,
// so even Secure does not keep them out.
func TestProxyStripsBothSpellingsFromTheWebview(t *testing.T) {
	for _, prefix := range []string{"", iserver.HostCookiePrefix} {
		resp := &http.Response{Header: http.Header{}}
		resp.Header.Add("Set-Cookie", prefix+cloudAuthCookieName+"=jwt; Path=/; HttpOnly; Secure")
		resp.Header.Add("Set-Cookie", prefix+cloudRefreshCookieName+"=ref; Path=/; HttpOnly; Secure")
		resp.Header.Add("Set-Cookie", "unrelated=keepme; Path=/")

		stripSetCookies(resp, append(
			iserver.SessionCookieSpellings(cloudAuthCookieName),
			iserver.SessionCookieSpellings(cloudRefreshCookieName)...,
		)...)

		got := resp.Header.Values("Set-Cookie")
		if len(got) != 1 || got[0] != "unrelated=keepme; Path=/" {
			t.Errorf("prefix %q: surviving Set-Cookie = %q; want only the unrelated one", prefix, got)
		}
	}
}
