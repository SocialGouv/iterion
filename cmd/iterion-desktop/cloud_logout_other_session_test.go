package main

import (
	"io"
	"net/http"
	"net/url"
	"testing"

	iserver "github.com/SocialGouv/iterion/pkg/server"
)

// paneLogoutOf sends a pane's sign-out through the tunnel with the pane's own
// query (the studio appends ?lineage=<the session its mark names>), as the
// demux proxy forwards it: no cookie, the jar's access token as the Bearer.
func paneLogoutOf(t *testing.T, jar *cloudTokenJar, cloudURL, query string) (int, string, http.Header) {
	t.Helper()
	rt := &cloudRoundTripper{base: http.DefaultTransport, jar: jar, onExpired: func(string) {}}
	target := cloudURL + "/api/auth/logout"
	if query != "" {
		target += "?" + query
	}
	req, err := http.NewRequest(http.MethodPost, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tok := jar.AccessToken(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(body), resp.Header
}

// TestCloudRoundTripper_AnOtherSessionAnswerSignsNothingOut: a pane's
// sign-out carries the lineage its mark names, and the cloud answers a token
// of ANOTHER lineage with 204 + X-Iterion-Logout: other-session, revoking
// NOTHING. Read as a confirmed sign-out, that 204 deletes the jar's token —
// the only credential that can end a session the cloud keeps live — and ends
// the session the operator just opened. So the tunnel keeps the jar, holds no
// sign-out, and hands the answer to the pane as it came: the pane holds the
// fact that decides. A sign-out the user asks for NOW names no lineage, and
// that presentation does end the jar's session.
func TestCloudRoundTripper_AnOtherSessionAnswerSignsNothingOut(t *testing.T) {
	key := cloudRefreshKeyPrefix + "conn-1"

	t.Run("a pane sign-out naming another session keeps the jar and the cloud session", func(t *testing.T) {
		c := newRealCloud(t)
		jar, store, _ := c.seedJar(t)
		stored := jar.storedRefresh()
		code, body, header := paneLogoutOf(t, jar, c.url, "lineage="+url.QueryEscape("a-lineage-this-connection-never-held"))
		if code != http.StatusNoContent {
			t.Fatalf("the pane's sign-out naming another session was answered %d %s, want 204", code, body)
		}
		if got := header.Get(iserver.LogoutOutcomeHeader); got != iserver.LogoutOtherSession {
			t.Fatalf("the answer carries %s=%q, want %q: the fact the tunnel reads is not there", iserver.LogoutOutcomeHeader, got, iserver.LogoutOtherSession)
		}
		if !c.sessionLive(t, stored) {
			t.Fatal("the cloud ended a session it answered other-session for")
		}
		if jar.storedRefresh() != stored || store.m[key] != stored {
			t.Fatalf("the tunnel dropped the token of a session the cloud kept live (jar %q, keychain %q)", jar.storedRefresh(), store.m[key])
		}
		if jar.SignOutPending() {
			t.Fatal("the tunnel held a sign-out of the jar's session: nothing was asked to end it")
		}
		if jar.AccessToken() == "" || !jar.LoggedIn() {
			t.Fatalf("the tunnel stopped using a session nothing signed out (access %q, logged in %t)", jar.AccessToken(), jar.LoggedIn())
		}
		// The session the operator holds keeps working.
		if code, body, expired := paneGET(t, jar, c.url+"/api/auth/me"); code != http.StatusOK {
			t.Fatalf("the pane's next request was answered %d %s (re-login asked: %v), want 200", code, body, expired)
		}
	})

	t.Run("a pane sign-out naming no session ends the jar's session", func(t *testing.T) {
		c := newRealCloud(t)
		jar, store, other := c.seedJar(t)
		stored := jar.storedRefresh()
		code, body, header := paneLogoutOf(t, jar, c.url, "")
		if code != http.StatusNoContent {
			t.Fatalf("the pane's sign-out was answered %d %s, want 204", code, body)
		}
		if got := header.Get(iserver.LogoutOutcomeHeader); got != "" {
			t.Fatalf("the answer carries %s=%q: a sign-out that named nothing ended the presented session", iserver.LogoutOutcomeHeader, got)
		}
		if c.sessionLive(t, stored) {
			t.Fatal("the cloud kept the session a sign-out naming nothing presented")
		}
		if jar.AccessToken() != "" || jar.storedRefresh() != "" {
			t.Fatal("the jar kept the session the pane signed out of")
		}
		if v, ok := store.m[key]; ok {
			t.Fatalf("the keychain kept the session the pane signed out of: %q", v)
		}
		if !c.sessionLive(t, other.RefreshToken) {
			t.Fatal("the pane's sign-out ended the account's other device too")
		}
	})
}
