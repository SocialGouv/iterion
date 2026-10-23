package main

// cloud_roundtrip.go carries NO build tag, like cloud_client.go: the
// authenticating tunnel's retry is plain net/http over the token jar, so it
// is unit-tested without the Wails/webkit toolchain. asset_proxy.go
// (//go:build desktop) installs it on a cloud connection's reverse proxy.

import (
	"bytes"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	iserver "github.com/SocialGouv/iterion/pkg/server"
)

// sessionRenewalUnavailable is the body of the answer the tunnel gives a
// request whose access token expired and whose renewal the cloud could not
// serve right now: the session is still the desktop's, nothing signed it out.
const sessionRenewalUnavailable = `{"error":"session renewal unavailable"}`

// keychainLocked is the body of the answer the tunnel gives a request it
// cannot authenticate because the SYSTEM KEYCHAIN could not be read — a
// locked login keyring, a Secret Service not up. Nothing about the cloud
// failed, and the operator has the remedy, so the answer says which one: the
// error_code lets the pane render its own state and offer a retry
// (cloudKeychainRetryHeader) instead of blaming the server.
const keychainLocked = `{"error":"keychain locked: unlock your system keychain to use this connection","error_code":"keychain_locked"}`

// cloudKeychainRetryHeader on a pane request is a USER ACTION: the operator
// asked this connection to read the keychain again (the pane's retry control),
// so the jar reads it now whatever its back-off, and the header is not
// forwarded to the cloud.
const cloudKeychainRetryHeader = "X-Iterion-Keychain-Retry"

// signOutUnavailable is the body of the answer the tunnel gives a pane's
// sign-out when the keychain could not be read: nothing was sent to the
// cloud, and the sign-out is held — this connection presents the session no
// more, and the sign-out is presented once the keychain answers.
const signOutUnavailable = `{"error":"sign-out unavailable: the stored session could not be read; this connection uses it no more and the sign-out is sent once the keychain answers"}`

// cloudRoundTripper wraps the base transport for a CLOUD connection with a
// single refresh-and-retry on 401. The background refresh loop is the
// primary defense (it refreshes before expiry); this is the safety net for
// the race where a token lapses between refreshes. Only a refresh the cloud
// REFUSES ends the session — a 401/403, and a jar with nothing left to
// present, which is the same dead end: onExpired is called and the original
// 401 passes through, so the pane signs out and the shell asks for a new
// sign-in. A refresh it could not serve (a 5xx, a rate limit, no answer) is
// answered 503 instead — the pane keeps its session, and the jar its token,
// for the next attempt, which this tunnel asks the loop to make at once
// rather than at its own next tick. A renewal the KEYCHAIN kept from being
// attempted is a 503 of its own (keychainLocked): the remedy is the
// operator's. The pane's sign-out is the jar's (logout).
type cloudRoundTripper struct {
	base      http.RoundTripper
	jar       *cloudTokenJar
	onExpired func(connID string)
}

func (t *cloudRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get(cloudKeychainRetryHeader) != "" {
		t.jar.allowKeychainRead()
		req = req.Clone(req.Context())
		req.Header.Del(cloudKeychainRetryHeader)
	}
	if isCloudLogout(req) {
		return t.logout(req)
	}
	// Buffer the body so the request can be replayed after a refresh.
	var body []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return nil, err
		}
		body = b
		req.Body = io.NopCloser(bytes.NewReader(body))
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	// 401 → refresh once, then replay the request with the new token.
	if rerr := t.jar.refreshNow(req.Context()); rerr != nil {
		if cloudRefreshRefused(rerr) {
			if tok := t.jar.AccessToken(); tok != "" {
				// The refusal named the token the renewal presented — a
				// session a rotation or a sign-in replaced while it was in
				// flight (clearIfStillHolds kept the jar). The jar holds a
				// live one: answer the request with it instead of asking the
				// shell for a sign-in (F13).
				resp.Body.Close()
				req2 := req.Clone(req.Context())
				if body != nil {
					req2.Body = io.NopCloser(bytes.NewReader(body))
				}
				req2.Header.Set("Authorization", "Bearer "+tok)
				return t.base.RoundTrip(req2)
			}
			if t.onExpired != nil {
				t.onExpired(t.jar.connID)
			}
			return resp, nil // pass the original 401 through
		}
		// The renewal was not served, and the token the jar still holds may
		// have been rotated by it. Only the background loop schedules
		// anything, and its timer is set from the access expiry it saw last:
		// wake it, so the retries that can still be served the rotation's
		// successor follow this failure instead of that tick.
		t.jar.kickRefresh()
		resp.Body.Close()
		if errors.Is(rerr, errCloudSessionUnread) {
			// The cloud was never asked: the credential is in a keychain this
			// computer could not read, and the pane is told so — a retry of
			// its own is the remedy, not waiting for a server.
			return jsonResponse(req, http.StatusServiceUnavailable, keychainLocked), nil
		}
		return renewalUnavailable(req), nil
	}
	resp.Body.Close()
	req2 := req.Clone(req.Context())
	if body != nil {
		req2.Body = io.NopCloser(bytes.NewReader(body))
	}
	if tok := t.jar.AccessToken(); tok != "" {
		req2.Header.Set("Authorization", "Bearer "+tok)
	}
	return t.base.RoundTrip(req2)
}

// isCloudLogout reports whether req is a pane's sign-out (POST
// /api/auth/logout, under whatever base path the cloud URL carries).
func isCloudLogout(req *http.Request) bool {
	return req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/api/auth/logout")
}

// logout forwards a pane's sign-out with the jar's refresh token: the tunnel
// strips the webview's cookies and the pane holds no token of its own, so
// without it the cloud would revoke nothing while the jar kept signing the
// pane's requests. The jar is cleared — memory and keychain — only once the
// cloud answered 2xx for the session it holds.
//
// The mark is written to the entry BEFORE the wire (the machine's SIGN-OUT
// REQUESTED): a crash between the POST leaving and the answer marking would
// otherwise adopt the session as signed-in — the entry says the sign-out is
// owed, and a restart presents it instead of renewing it. On the answer: a
// 2xx clears the jar (the mark goes with it); other-session lifts the mark —
// the answer says the named ask was not this jar's session, and a mark kept
// would end the KEPT session at the next presentation; anything else keeps
// the mark (the loop presents until the cloud confirms it).
//
// A 2xx carrying iserver.LogoutOutcomeHeader: other-session signed NOTHING
// out: the pane named a lineage (?lineage=) the jar's token does not belong
// to, and the cloud kept both that session and its cookies. So the jar keeps
// its token, lifts the mark the wire was preceded by, and hands the answer to
// the pane unchanged — the pane is what holds the fact that decides. A mark
// its studio replays names a session the desktop signed out of and replaced,
// and ending the jar's session over it would end the one the operator just
// opened; a sign-out the user asks for NOW is re-presented naming no lineage,
// and that presentation ends the jar's session here.
//
// Any other answer, or none, ended nothing on the cloud, and the desktop owns
// that session: the mark stands (written before the wire), so the token stops
// being presented and renewed and the background loop presents the sign-out
// until the cloud confirms it. Without that mark the pane's banner ("you may
// not be signed out yet") was the only trace, and the next pane load — a
// reload, a restart — was served the session the operator ended.
func (t *cloudRoundTripper) logout(req *http.Request) (*http.Response, error) {
	if err := t.jar.readIfUnread(); err != nil {
		log.Printf("desktop: sign-out of %s not sent: %v (held: the connection presents the session no more)", t.jar.connID, err)
		t.holdSignOut()
		if req.Body != nil {
			req.Body.Close()
		}
		return jsonResponse(req, http.StatusServiceUnavailable, signOutUnavailable), nil
	}
	wasPending := t.jar.SignOutPending()
	if refresh := t.jar.storedRefresh(); refresh != "" {
		// The mark is written BEFORE the wire: once the POST leaves, the
		// answer alone decides the session's end, and a crash in between must
		// not adopt the session — the entry already says its sign-out is owed.
		t.holdSignOut()
		req = req.Clone(req.Context())
		req.Header.Set("X-Iterion-Refresh", refresh)
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		// No answer: the cloud may have revoked the session or not, and the
		// desktop must not use it either way. The mark already stands; this
		// re-write names a hold the keychain may not carry yet.
		t.holdSignOut()
		return nil, err
	}
	if resp.StatusCode/100 == 2 {
		if resp.Header.Get(iserver.LogoutOutcomeHeader) == iserver.LogoutOtherSession {
			log.Printf("desktop: the sign-out a pane of %s sent revoked nothing — it named another session than this connection's; the session is kept", t.jar.connID)
			// The answer says the NAMED ask was not this jar's session. The
			// mark THIS ask wrote before the wire is lifted — memory and
			// entry — so no presentation ends the KEPT session (I3). A mark
			// that PREDATED the ask is the jar's own owed sign-out: this
			// answer says nothing about it, and it stands.
			if !wasPending {
				if uerr := t.jar.clearSignOutPending(); uerr != nil {
					log.Printf("desktop: the kept session of %s is still marked sign-out-pending in its keychain entry (%v) — the next renewal rewrites the entry without the mark; a restart before that presents the kept session's sign-out", t.jar.connID, uerr)
					t.jar.kickRefresh()
				}
			}
			return resp, nil
		}
		if cerr := t.jar.clear(); cerr != nil {
			log.Printf("desktop: signed %s out, but deleting its keychain entry failed: %v", t.jar.connID, cerr)
		}
		return resp, nil
	}
	t.holdSignOut()
	return resp, nil
}

// holdSignOut marks the jar's sign-out pending and reports a hold the store
// does not carry yet — the keyring that could not be read cannot be written
// either, so the hold lives in this process until a presentation whose read
// answers writes it (cloudTokenJar.retrySignOut). It runs BEFORE the wire
// too: the mark is written to the entry before the sign-out is sent.
func (t *cloudRoundTripper) holdSignOut() {
	if err := t.jar.markSignOutPending(); err != nil {
		log.Printf("desktop: the sign-out of %s is held in this app's memory only: %v (it is written to the keychain at the first presentation the keychain answers; until then a restart loses it, and closing the pane is refused so the hold is not dropped with it)", t.jar.connID, err)
	}
}

// renewalUnavailable is the 503 the tunnel answers req with when the
// session's renewal could not be served.
func renewalUnavailable(req *http.Request) *http.Response {
	return jsonResponse(req, http.StatusServiceUnavailable, sessionRenewalUnavailable)
}

// jsonResponse is an answer the tunnel gives req itself, without the cloud.
func jsonResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		Status:        strconv.Itoa(status) + " " + http.StatusText(status),
		StatusCode:    status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": {"application/json"}, "Content-Length": {strconv.Itoa(len(body))}},
		Body:          io.NopCloser(bytes.NewReader([]byte(body))),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}
