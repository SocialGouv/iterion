package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/audit"
	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/auth/desktopsso"
	"github.com/SocialGouv/iterion/pkg/auth/oidc"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// The sign-in events of the audit log (ADR-157), one witness per site that
// mints or refuses a session. Every row is read back from the store — the
// insert is detached, so each assertion waits for the effect — and every
// row is checked for the cleartext identifier it must not carry.

// loginAuditFixture is the derivation fixture with a memory audit store and
// the loopback declared a trusted proxy, so X-Forwarded-For is the client
// IP the rows record (s.clientIP) and each scenario names its client. No
// deployment setting fills TrustedProxyCIDRs yet (#2673): what the shipped
// configuration records and bounds is witnessed by
// newShippedLoginAuditFixture.
type loginAuditFixture struct {
	*derivationFixture
	audit *audit.MemoryStore
}

func newLoginAuditFixture(t *testing.T) *loginAuditFixture {
	t.Helper()
	st := audit.NewMemoryStore()
	f := newDerivationFixtureWith(t, func(c *Config) {
		c.Audit = st
		c.TrustedProxyCIDRs = []string{"127.0.0.0/8", "::1/128"}
	})
	return &loginAuditFixture{derivationFixture: f, audit: st}
}

// clientUA and clientIP are what every request of these tests presents, and
// what every row must record.
const (
	clientUA = "audit-witness/1.0"
	clientIP = "203.0.113.9"
)

// send posts body to path as the client (UA + forwarded IP), with the bearer
// when set, and answers the status and the body.
func (f *loginAuditFixture) send(t *testing.T, method, path, bearer, body string, forwardedFor string) (int, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, f.srv.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("User-Agent", clientUA)
	req.Header.Set("X-Forwarded-For", forwardedFor)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// activeOrgOf reads the org a session answer is active in.
func activeOrgOf(t *testing.T, body string) string {
	t.Helper()
	var sess AuthSessionResponse
	if err := json.Unmarshal([]byte(body), &sess); err != nil {
		t.Fatalf("decode the session answer %q: %v", body, err)
	}
	if sess.ActiveOrg == "" {
		t.Fatalf("the session answer names no active org: %s", body)
	}
	return sess.ActiveOrg
}

func platformAuditRows(t *testing.T, st audit.Store, action string) []audit.Event {
	t.Helper()
	evs, err := st.ListPlatform(context.Background(), audit.Page{Action: action, Limit: 500})
	if err != nil {
		t.Fatalf("list platform audit: %v", err)
	}
	return evs
}

// waitForPlatformAuditRows polls the platform trail until want rows carry
// action, holds for the quiet window, and reports any extra.
func waitForPlatformAuditRows(t *testing.T, st audit.Store, action string, want int) []audit.Event {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(platformAuditRows(t, st, action)) < want {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of the %d platform %s rows this scenario accounts for landed",
				len(platformAuditRows(t, st, action)), want, action)
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(auditQuietWindow)
	hits := platformAuditRows(t, st, action)
	if len(hits) != want {
		t.Errorf("%d platform %s rows landed where this scenario accounts for %d: %+v", len(hits), action, want, hits)
	}
	return hits
}

// metaString reads a string of a row's meta.
func metaString(e audit.Event, key string) string {
	v, _ := e.Meta[key].(string)
	return v
}

// assertLoginRow checks what every sign-in row carries: the client's IP and
// user-agent, the method, and none of the cleartext values it must not.
func assertLoginRow(t *testing.T, e audit.Event, method string, cleartext ...string) {
	t.Helper()
	assertLoginRowFrom(t, e, clientIP, method, cleartext...)
}

// assertLoginRowFrom is assertLoginRow for a client at ip.
func assertLoginRowFrom(t *testing.T, e audit.Event, ip, method string, cleartext ...string) {
	t.Helper()
	if e.IP != ip {
		t.Errorf("%s: ip = %q, want the forwarded client address %q", e.Action, e.IP, ip)
	}
	if e.UserAgent != clientUA {
		t.Errorf("%s: user_agent = %q, want %q", e.Action, e.UserAgent, clientUA)
	}
	if got := metaString(e, "method"); got != method {
		t.Errorf("%s: meta.method = %q, want %q", e.Action, got, method)
	}
	if e.Target != "user" {
		t.Errorf("%s: target = %q, want user", e.Action, e.Target)
	}
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cleartext {
		if c != "" && strings.Contains(strings.ToLower(string(raw)), strings.ToLower(c)) {
			t.Errorf("%s: the row carries %q in clear: %s", e.Action, c, raw)
		}
	}
}

// assertFailedRow checks a refusal: platform scope, no tenant, the reason.
func assertFailedRow(t *testing.T, e audit.Event, method, reason string, cleartext ...string) {
	t.Helper()
	assertFailedRowFrom(t, e, clientIP, method, reason, cleartext...)
}

// assertFailedRowFrom is assertFailedRow for a client at ip.
func assertFailedRowFrom(t *testing.T, e audit.Event, ip, method, reason string, cleartext ...string) {
	t.Helper()
	assertLoginRowFrom(t, e, ip, method, cleartext...)
	if e.Scope != audit.ScopePlatform || e.TenantID != "" {
		t.Errorf("a refusal landed as scope=%q tenant=%q, want a platform row with no tenant", e.Scope, e.TenantID)
	}
	if got := metaString(e, "reason"); got != reason {
		t.Errorf("meta.reason = %q, want %q (%+v)", got, reason, e.Meta)
	}
}

// assertSucceededRow checks a success: a tenant row on the active org,
// attributed to the account.
func assertSucceededRow(t *testing.T, e audit.Event, method, orgID, userID string, cleartext ...string) {
	t.Helper()
	assertLoginRow(t, e, method, cleartext...)
	if e.Scope != audit.ScopeTenant || e.TenantID != orgID {
		t.Errorf("a success landed as scope=%q tenant=%q, want a tenant row on org %q", e.Scope, e.TenantID, orgID)
	}
	if e.ActorID != userID || e.ActorKind != "user" || e.TargetID != userID {
		t.Errorf("a success is attributed to actor=%q/%q target_id=%q, want the account %q", e.ActorID, e.ActorKind, e.TargetID, userID)
	}
}

var subjectDigestShape = regexp.MustCompile(`^[0-9a-f]{16}$`)

// subjectHashOf reads the peppered digest a refusal carries, and fails when
// the row carries none or one of another shape.
func subjectHashOf(t *testing.T, e audit.Event) string {
	t.Helper()
	h := metaString(e, "subject_hash")
	if !subjectDigestShape.MatchString(h) {
		t.Fatalf("meta.subject_hash = %q, want 16 hex characters (%+v)", h, e.Meta)
	}
	return h
}

// rowsByReason indexes refusals by their reason, for scenarios whose rows
// land in whichever order their goroutines finish.
func rowsByReason(rows []audit.Event) map[string][]audit.Event {
	out := map[string][]audit.Event{}
	for _, e := range rows {
		out[metaString(e, "reason")] = append(out[metaString(e, "reason")], e)
	}
	return out
}

func loginBody(email, password string) string {
	return fmt.Sprintf(`{"email":%q,"password":%q}`, email, password)
}

// TestAuditLogin_APasswordSignInIsAnOrgRowOfTheAccount: POST /api/auth/login
// accepted writes auth.login.succeeded on the org the session is active in,
// attributed to the account, with the forwarded IP and the user-agent, the
// method, and the e-mail nowhere.
func TestAuditLogin_APasswordSignInIsAnOrgRowOfTheAccount(t *testing.T) {
	f := newLoginAuditFixture(t)
	code, body := f.send(t, http.MethodPost, "/api/auth/login", "", loginBody(fixtureEmail, fixturePassword), clientIP)
	if code != http.StatusOK {
		t.Fatalf("login: %d %s", code, body)
	}
	org := activeOrgOf(t, body)
	rows := waitForTenantAuditRows(t, f.audit, org, actionLoginSucceeded, 1)
	assertSucceededRow(t, rows[0], loginMethodPassword, org, f.user.User.ID, fixtureEmail, fixturePassword)
	if _, flowed := rows[0].Meta["flow"]; flowed {
		t.Errorf("a plain sign-in carries a flow: %+v", rows[0].Meta)
	}
	if got := metaString(rows[0], "team_id"); got == "" {
		t.Errorf("the success names no active team: %+v", rows[0].Meta)
	}
	if n := len(platformAuditRows(t, f.audit, actionLoginFailed)); n != 0 {
		t.Errorf("an accepted sign-in wrote %d refusal(s)", n)
	}
}

// TestAuditLogin_RefusedPasswordSignInsArePlatformRowsWithTypedReasons: an
// unknown account, a wrong password, a locked account and a disabled one
// all answer the same 401, and each writes one platform row with its own
// typed reason, the attempted address only as a peppered digest — the same
// digest for the same address, another for another.
func TestAuditLogin_RefusedPasswordSignInsArePlatformRowsWithTypedReasons(t *testing.T) {
	f := newLoginAuditFixture(t)
	ctx := context.Background()
	disabledLogin, err := f.svc.Register(ctx, "off@example.com", fixturePassword, "", "", "ua", "ip")
	if err != nil {
		t.Fatal(err)
	}
	disabled := identity.UserStatusDisabled
	if _, err := f.s.authStore().PatchUser(ctx, disabledLogin.User.ID, identity.UserPatch{Status: &disabled}); err != nil {
		t.Fatal(err)
	}
	lockedLogin, err := f.svc.Register(ctx, "locked@example.com", fixturePassword, "", "", "ua", "ip")
	if err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Hour)
	if _, err := f.s.authStore().PatchUser(ctx, lockedLogin.User.ID, identity.UserPatch{Lockout: &identity.UserLockout{FailedLogins: auth.LockoutThreshold, LockedUntil: &until}}); err != nil {
		t.Fatal(err)
	}

	attempts := []struct{ email, password string }{
		{"nobody@example.com", fixturePassword},
		{fixtureEmail, "not-the-password"},
		{fixtureEmail, "still-not-the-password"},
		{"locked@example.com", fixturePassword},
		{"off@example.com", fixturePassword},
	}
	ips := map[string]bool{}
	for i, a := range attempts {
		// One client address per attempt: the per-IP limiter is not what
		// this scenario measures.
		ip := fmt.Sprintf("203.0.113.%d", 10+i)
		ips[ip] = true
		if code, body := f.send(t, http.MethodPost, "/api/auth/login", "", loginBody(a.email, a.password), ip); code != http.StatusUnauthorized {
			t.Fatalf("attempt %d answered %d (%s), want 401", i, code, body)
		}
	}
	rows := waitForPlatformAuditRows(t, f.audit, actionLoginFailed, len(attempts))
	byReason := rowsByReason(rows)
	for reason, want := range map[string]int{
		string(auth.ReasonUnknownAccount): 1,
		string(auth.ReasonBadPassword):    2,
		string(auth.ReasonLocked):         1,
		"disabled":                        1,
	} {
		if len(byReason[reason]) != want {
			t.Errorf("%d row(s) with reason %q, want %d (rows: %+v)", len(byReason[reason]), reason, want, rows)
		}
	}
	for _, e := range rows {
		if !ips[e.IP] {
			t.Errorf("a refusal records ip %q, none of the attempts' addresses", e.IP)
		}
		assertFailedRowFrom(t, e, e.IP, loginMethodPassword, metaString(e, "reason"), fixtureEmail, "nobody@example.com", "locked@example.com", "off@example.com", fixturePassword, "not-the-password")
		if e.ActorID != "" || e.ActorKind != "anonymous" || e.TargetID != "" {
			t.Errorf("an anonymous refusal by address is attributed to actor=%q/%q target_id=%q", e.ActorID, e.ActorKind, e.TargetID)
		}
	}
	// The digest correlates attempts on one address and separates addresses.
	bad := byReason[string(auth.ReasonBadPassword)]
	if len(bad) == 2 && subjectHashOf(t, bad[0]) != subjectHashOf(t, bad[1]) {
		t.Errorf("two attempts on %s digest differently: %q vs %q", fixtureEmail, subjectHashOf(t, bad[0]), subjectHashOf(t, bad[1]))
	}
	if u := byReason[string(auth.ReasonUnknownAccount)]; len(u) == 1 && len(bad) == 2 && subjectHashOf(t, u[0]) == subjectHashOf(t, bad[0]) {
		t.Errorf("two addresses digest alike: %q", subjectHashOf(t, u[0]))
	}
	// The digest is peppered: a bare SHA-256 of the address is not it.
	for _, a := range attempts {
		for _, e := range rows {
			if subjectHashOf(t, e) == bareSHA256Prefix(a.email) {
				t.Errorf("subject_hash of %s is the bare SHA-256 of the address: it is not peppered", a.email)
			}
		}
	}
}

// bareSHA256Prefix is the digest the audit log must NOT carry: the
// unkeyed SHA-256 of the normalised address, reversible by dictionary.
func bareSHA256Prefix(email string) string {
	sum := sha256.Sum256([]byte(identity.NormalizeEmail(email)))
	return hex.EncodeToString(sum[:])[:16]
}

// TestAuditLogin_TheSubjectDigestIsPepperedByTheSigningKey: the digest is
// an HMAC under a key derived from the signing key — two services with
// different keys digest one address differently, and neither digest is the
// address's bare SHA-256.
func TestAuditLogin_TheSubjectDigestIsPepperedByTheSigningKey(t *testing.T) {
	a := newLoginAuditFixture(t)
	b := newLoginAuditFixture(t)
	da, ok := a.svc.SubjectDigest(fixtureEmail)
	if !ok || !subjectDigestShape.MatchString(da) {
		t.Fatalf("SubjectDigest = %q, %v", da, ok)
	}
	db, _ := b.svc.SubjectDigest(fixtureEmail)
	if da == db {
		t.Errorf("two signing keys digest %s alike (%q): the digest is not peppered", fixtureEmail, da)
	}
	if da == bareSHA256Prefix(fixtureEmail) {
		t.Errorf("the digest is the bare SHA-256 of the address")
	}
	if again, _ := a.svc.SubjectDigest("  " + strings.ToUpper(fixtureEmail) + " "); again != da {
		t.Errorf("the digest does not normalise the address: %q vs %q", again, da)
	}
}

// newShippedLoginAuditFixture is the audit fixture in the configuration
// that ships: no trusted proxy, so a forwarded header is never read and
// the client is the TCP peer.
func newShippedLoginAuditFixture(t *testing.T) *loginAuditFixture {
	t.Helper()
	st := audit.NewMemoryStore()
	f := newDerivationFixtureWith(t, func(c *Config) { c.Audit = st })
	return &loginAuditFixture{derivationFixture: f, audit: st}
}

// TestAuditLogin_TheAddressTierBoundsAFloodOnOneAddressInTheShippedConfiguration:
// with no trusted proxy and a fresh TCP connection per attempt — a client
// cycling source ports, which the per-IP bucket keys on until #2673 — the
// login route's per-address tier is what bounds a flood on one address:
// every refusal that reaches the sign-in is one row, the attempt past the
// burst answers 429 and writes none.
func TestAuditLogin_TheAddressTierBoundsAFloodOnOneAddressInTheShippedConfiguration(t *testing.T) {
	f := newShippedLoginAuditFixture(t)
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	t.Cleanup(client.CloseIdleConnections)
	attempt := func() int {
		req, err := http.NewRequest(http.MethodPost, f.srv.URL+"/api/auth/login", strings.NewReader(loginBody("flood@example.com", "x-password-1")))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", clientUA)
		// Ignored: no proxy is trusted.
		req.Header.Set("X-Forwarded-For", clientIP)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	const burst = 5 // loginLimit's burst (auth_routes.go)
	for i := 0; i < burst; i++ {
		if code := attempt(); code != http.StatusUnauthorized {
			t.Fatalf("flood attempt %d answered %d, want 401", i, code)
		}
	}
	if code := attempt(); code != http.StatusTooManyRequests {
		t.Fatalf("the attempt past the per-address burst answered %d, want 429", code)
	}
	rows := waitForPlatformAuditRows(t, f.audit, actionLoginFailed, burst)
	for _, e := range rows {
		if got := metaString(e, "reason"); got != string(auth.ReasonUnknownAccount) {
			t.Errorf("meta.reason = %q, want %q", got, auth.ReasonUnknownAccount)
		}
		if e.IP == clientIP || e.IP == "" {
			t.Errorf("a row records ip %q: with no trusted proxy it is the TCP peer's, never a forwarded value", e.IP)
		}
		subjectHashOf(t, e)
	}
}

// TestAuditLogin_ARegistrationIsASignInUnderTheRegisterFlow: POST
// /api/auth/register mints the first session — audited as a success under
// flow=register — and a refused registration is a refusal with its reason.
func TestAuditLogin_ARegistrationIsASignInUnderTheRegisterFlow(t *testing.T) {
	f := newLoginAuditFixture(t)
	if code, body := f.send(t, http.MethodPost, "/api/auth/register", "", loginBody("new@example.com", "short"), clientIP); code != http.StatusBadRequest {
		t.Fatalf("a weak password answered %d (%s), want 400", code, body)
	}
	code, body := f.send(t, http.MethodPost, "/api/auth/register", "", loginBody("new@example.com", "a-long-enough-password-1"), clientIP)
	if code != http.StatusOK {
		t.Fatalf("register: %d %s", code, body)
	}
	org := activeOrgOf(t, body)
	var sess AuthSessionResponse
	_ = json.Unmarshal([]byte(body), &sess)
	ok := waitForTenantAuditRows(t, f.audit, org, actionLoginSucceeded, 1)
	assertSucceededRow(t, ok[0], loginMethodPassword, org, sess.User.ID, "new@example.com", "a-long-enough-password-1")
	if got := metaString(ok[0], "flow"); got != loginFlowRegister {
		t.Errorf("meta.flow = %q, want %q", got, loginFlowRegister)
	}
	failed := waitForPlatformAuditRows(t, f.audit, actionLoginFailed, 1)
	assertFailedRow(t, failed[0], loginMethodPassword, "password_weak", "new@example.com", "short")
	if got := metaString(failed[0], "flow"); got != loginFlowRegister {
		t.Errorf("meta.flow = %q, want %q", got, loginFlowRegister)
	}
	subjectHashOf(t, failed[0])
}

// TestAuditLogin_AForcedRotationIsASignInUnderThePasswordChangeFlow: POST
// /api/auth/password/change (the pending_password_change exit) refused on
// the temporary password is a bad_password refusal; accepted, it is a
// success under flow=password_change.
func TestAuditLogin_AForcedRotationIsASignInUnderThePasswordChangeFlow(t *testing.T) {
	f := newLoginAuditFixture(t)
	ctx := context.Background()
	reg, err := f.svc.Register(ctx, "pending@example.com", fixturePassword, "", "", "ua", "ip")
	if err != nil {
		t.Fatal(err)
	}
	pending := identity.UserStatusPendingPasswordChange
	if _, err := f.s.authStore().PatchUser(ctx, reg.User.ID, identity.UserPatch{Status: &pending}); err != nil {
		t.Fatal(err)
	}
	change := func(current string) (int, string) {
		return f.send(t, http.MethodPost, "/api/auth/password/change", "",
			fmt.Sprintf(`{"email":"pending@example.com","current_password":%q,"new_password":"rotated-password-forever-1"}`, current), clientIP)
	}
	if code, body := change("not-the-temporary-password"); code != http.StatusUnauthorized {
		t.Fatalf("a wrong temporary password answered %d (%s), want 401", code, body)
	}
	code, body := change(fixturePassword)
	if code != http.StatusOK {
		t.Fatalf("the rotation answered %d: %s", code, body)
	}
	org := activeOrgOf(t, body)
	ok := waitForTenantAuditRows(t, f.audit, org, actionLoginSucceeded, 1)
	assertSucceededRow(t, ok[0], loginMethodPassword, org, reg.User.ID, "pending@example.com", fixturePassword, "rotated-password")
	if got := metaString(ok[0], "flow"); got != loginFlowPasswordChange {
		t.Errorf("meta.flow = %q, want %q", got, loginFlowPasswordChange)
	}
	failed := waitForPlatformAuditRows(t, f.audit, actionLoginFailed, 1)
	assertFailedRow(t, failed[0], loginMethodPassword, string(auth.ReasonBadPassword), "pending@example.com", "not-the-temporary-password")
	subjectHashOf(t, failed[0])
}

// TestAuditLogin_AResetRedemptionIsASignInUnderThePasswordResetFlow: POST
// /api/auth/password/reset/confirm mints a session on a valid link — a
// success under flow=password_reset, beside the user.password_reset row —
// and an unknown link is a reset_token_invalid refusal.
func TestAuditLogin_AResetRedemptionIsASignInUnderThePasswordResetFlow(t *testing.T) {
	f := newLoginAuditFixture(t)
	now := time.Now().UTC()
	if err := f.resets.Create(context.Background(), auth.PasswordReset{
		ID: "reset-1", UserID: f.user.User.ID, TokenHash: auth.HashRefreshToken("iar_reset-token-1"),
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("seed reset: %v", err)
	}
	confirm := func(token string) (int, string) {
		return f.send(t, http.MethodPost, "/api/auth/password/reset/confirm", "",
			fmt.Sprintf(`{"token":%q,"password":"a-new-password-forever-3"}`, token), clientIP)
	}
	if code, body := confirm("iar_unknown"); code != http.StatusUnauthorized {
		t.Fatalf("an unknown link answered %d (%s), want 401", code, body)
	}
	code, body := confirm("iar_reset-token-1")
	if code != http.StatusOK {
		t.Fatalf("the redemption answered %d: %s", code, body)
	}
	org := activeOrgOf(t, body)
	ok := waitForTenantAuditRows(t, f.audit, org, actionLoginSucceeded, 1)
	assertSucceededRow(t, ok[0], loginMethodPassword, org, f.user.User.ID, fixtureEmail, "iar_reset-token-1", "a-new-password")
	if got := metaString(ok[0], "flow"); got != loginFlowPasswordReset {
		t.Errorf("meta.flow = %q, want %q", got, loginFlowPasswordReset)
	}
	failed := waitForPlatformAuditRows(t, f.audit, actionLoginFailed, 1)
	assertFailedRow(t, failed[0], loginMethodPassword, string(auth.ReasonResetTokenInvalid), "iar_unknown", "a-new-password")
	if _, hashed := failed[0].Meta["subject_hash"]; hashed {
		t.Errorf("a redemption names no address, yet the row carries a digest: %+v", failed[0].Meta)
	}
}

// TestAuditLogin_TheSelfServicePasswordChangeNamesItsActor: POST
// /api/me/password proves the current password from a live session, so
// both outcomes are attributed to the caller — the refusal too, unlike an
// anonymous one — under flow=password_change.
func TestAuditLogin_TheSelfServicePasswordChangeNamesItsActor(t *testing.T) {
	f := newLoginAuditFixture(t)
	if code, body := f.send(t, http.MethodPost, "/api/me/password", f.user.AccessToken,
		`{"current_password":"not-the-password","new_password":"new-password-forever-1"}`, clientIP); code != http.StatusUnauthorized {
		t.Fatalf("a wrong current password answered %d (%s), want 401", code, body)
	}
	code, body := f.send(t, http.MethodPost, "/api/me/password", f.user.AccessToken,
		`{"current_password":"`+fixturePassword+`","new_password":"new-password-forever-2"}`, clientIP)
	if code != http.StatusOK {
		t.Fatalf("the change answered %d: %s", code, body)
	}
	org := activeOrgOf(t, body)
	ok := waitForTenantAuditRows(t, f.audit, org, actionLoginSucceeded, 1)
	assertSucceededRow(t, ok[0], loginMethodPassword, org, f.user.User.ID, fixtureEmail, fixturePassword, "new-password-forever")
	if got := metaString(ok[0], "flow"); got != loginFlowPasswordChange {
		t.Errorf("meta.flow = %q, want %q", got, loginFlowPasswordChange)
	}
	failed := waitForPlatformAuditRows(t, f.audit, actionLoginFailed, 1)
	assertFailedRow(t, failed[0], loginMethodPassword, string(auth.ReasonBadPassword), fixtureEmail, "not-the-password")
	if failed[0].ActorID != f.user.User.ID || failed[0].ActorKind != "user" || failed[0].TargetID != f.user.User.ID {
		t.Errorf("the refusal of a signed-in caller is attributed to actor=%q/%q target_id=%q, want the caller %q",
			failed[0].ActorID, failed[0].ActorKind, failed[0].TargetID, f.user.User.ID)
	}
}

// ssoStart drives the browser SSO flow of the stub connector through the
// callback as the client, and answers the callback's redirect target.
func (f *loginAuditFixture) ssoStart(t *testing.T, query string) string {
	t.Helper()
	return f.ssoRoundTrip(t, query, nil)
}

// ssoRoundTrip is ssoStart with tamper applied to each cookie the start set
// before the callback presents it.
func (f *loginAuditFixture) ssoRoundTrip(t *testing.T, query string, tamper func(*http.Cookie)) string {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequest(http.MethodGet, f.srv.URL+"/api/auth/oidc/testsso/start"+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var state string
	if resp.StatusCode == http.StatusFound {
		authURL, err := url.Parse(resp.Header.Get("Location"))
		if err != nil {
			t.Fatal(err)
		}
		state = authURL.Query().Get("state")
	} else {
		var started struct {
			AuthorizeURL string `json:"authorize_url"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&started); err != nil {
			t.Fatalf("the start answered %d and no JSON: %v", resp.StatusCode, err)
		}
		authURL, err := url.Parse(started.AuthorizeURL)
		if err != nil {
			t.Fatal(err)
		}
		state = authURL.Query().Get("state")
	}
	cb, err := http.NewRequest(http.MethodGet, f.srv.URL+"/api/auth/oidc/testsso/callback?code=c&state="+url.QueryEscape(state), nil)
	if err != nil {
		t.Fatal(err)
	}
	cb.Header.Set("User-Agent", clientUA)
	cb.Header.Set("X-Forwarded-For", clientIP)
	for _, c := range resp.Cookies() {
		if tamper != nil {
			tamper(c)
		}
		cb.AddCookie(c)
	}
	out, err := client.Do(cb)
	if err != nil {
		t.Fatal(err)
	}
	_ = out.Body.Close()
	if out.StatusCode != http.StatusFound {
		t.Fatalf("the callback answered %d, want 302", out.StatusCode)
	}
	return out.Header.Get("Location")
}

// TestAuditLogin_AnSSOSignInCarriesItsProviderAndNoEmail: the browser
// callback of a generic connector is audited with method=oidc and the
// connector slug — a success on the org the session opened in, a refusal
// (an existing account that needs an explicit link) as a platform row with
// reason link_required and the IdP e-mail only as a digest.
func TestAuditLogin_AnSSOSignInCarriesItsProviderAndNoEmail(t *testing.T) {
	f := newLoginAuditFixture(t)
	// A new identity: the connector's e-mail answers no account, so the
	// sign-in provisions one and opens its session.
	f.withStubSSO(oidc.ExternalUser{Provider: "testsso", Subject: "sso-1", Email: "sso@example.org", Name: "Sso"})
	if loc := f.ssoStart(t, ""); strings.Contains(loc, "sso_error") {
		t.Fatalf("the sign-in of a new identity was refused: %s", loc)
	}
	// The account the IdP's e-mail matches is found by the store.
	u, err := f.s.authStore().GetUserByEmail(context.Background(), "sso@example.org")
	if err != nil {
		t.Fatalf("the SSO sign-in provisioned no account: %v", err)
	}
	// The org the session opened in is the account's one org, the personal
	// org the sign-in provisioned.
	mems, err := f.s.authStore().ListOrgMembershipsByUser(context.Background(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(mems) != 1 {
		t.Fatalf("the provisioned account holds %d org memberships, want its personal org alone: %+v", len(mems), mems)
	}
	ok := waitForTenantAuditRows(t, f.audit, mems[0].OrgID, actionLoginSucceeded, 1)
	assertSucceededRow(t, ok[0], loginMethodOIDC, mems[0].OrgID, u.ID, "sso@example.org", "sso-1")
	if got := metaString(ok[0], "provider"); got != "testsso" {
		t.Errorf("meta.provider = %q, want testsso", got)
	}

	// The fixture's own account exists with a password and no link: the
	// same e-mail from the IdP is refused until linked from settings.
	f.withStubSSO(oidc.ExternalUser{Provider: "testsso", Subject: "sso-2", Email: fixtureEmail})
	if loc := f.ssoStart(t, ""); !strings.Contains(loc, "sso_error="+ssoErrLinkRequired) {
		t.Fatalf("the unlinked identity was not refused with link_required: %s", loc)
	}
	failed := waitForPlatformAuditRows(t, f.audit, actionLoginFailed, 1)
	assertFailedRow(t, failed[0], loginMethodOIDC, "link_required", fixtureEmail, "sso-2")
	if got := metaString(failed[0], "provider"); got != "testsso" {
		t.Errorf("meta.provider = %q, want testsso", got)
	}
	subjectHashOf(t, failed[0])
}

// TestAuditLogin_ADesktopSignInIsAuditedWhereItsSessionIsMinted: a desktop
// SSO flow writes its success at the ticket's redemption (method=desktop),
// a spent ticket is a ticket_invalid refusal, and a sign-in the callback
// refuses (an account pending a password change) is a desktop refusal with
// the connector and the typed reason.
func TestAuditLogin_ADesktopSignInIsAuditedWhereItsSessionIsMinted(t *testing.T) {
	f := newLoginAuditFixture(t)
	f.s.desktopTickets = desktopsso.NewMemoryStore(time.Minute)
	f.withStubSSO(oidc.ExternalUser{Provider: "testsso", Subject: "desk-1", Email: "desk@example.org", Name: "Desk"})
	loc := f.ssoStart(t, "?desktop=1&format=json&next="+url.QueryEscape("http://127.0.0.1:41234/callback"))
	target, err := url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	ticket := target.Query().Get("ticket")
	if ticket == "" {
		t.Fatalf("the callback's redirect carries no ticket: %q", loc)
	}
	code, body := f.send(t, http.MethodPost, "/api/auth/desktop/exchange", "", `{"ticket":"`+ticket+`"}`, clientIP)
	if code != http.StatusOK {
		t.Fatalf("the exchange answered %d: %s", code, body)
	}
	org := activeOrgOf(t, body)
	var sess AuthSessionResponse
	_ = json.Unmarshal([]byte(body), &sess)
	ok := waitForTenantAuditRows(t, f.audit, org, actionLoginSucceeded, 1)
	assertSucceededRow(t, ok[0], loginMethodDesktop, org, sess.User.ID, "desk@example.org", ticket)
	// The connector rides the ticket from the callback to the redemption.
	if got := metaString(ok[0], "provider"); got != "testsso" {
		t.Errorf("the desktop success: meta.provider = %q, want testsso", got)
	}
	// Spent: the second redemption is refused, and audited as such.
	if code, _ := f.send(t, http.MethodPost, "/api/auth/desktop/exchange", "", `{"ticket":"`+ticket+`"}`, clientIP); code != http.StatusUnauthorized {
		t.Fatalf("the second redemption answered %d, want 401", code)
	}
	failed := waitForPlatformAuditRows(t, f.audit, actionLoginFailed, 1)
	assertFailedRow(t, failed[0], loginMethodDesktop, "ticket_invalid", ticket)
	if failed[0].TargetID != "" {
		t.Errorf("a ticket the store does not know names an account: target_id=%q", failed[0].TargetID)
	}

	// A callback refusal: the account is pending a password change.
	ctx := context.Background()
	reg, err := f.svc.Register(ctx, "pending@example.org", fixturePassword, "", "", "ua", "ip")
	if err != nil {
		t.Fatal(err)
	}
	pending := identity.UserStatusPendingPasswordChange
	if _, err := f.s.authStore().PatchUser(ctx, reg.User.ID, identity.UserPatch{Status: &pending}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.authStore().UpsertOIDCLink(ctx, identity.OIDCLink{
		Provider: "testsso", ProviderUserID: "desk-pending", UserID: reg.User.ID, Email: "pending@example.org",
	}); err != nil {
		t.Fatal(err)
	}
	f.withStubSSO(oidc.ExternalUser{Provider: "testsso", Subject: "desk-pending", Email: "pending@example.org"})
	if loc := f.ssoStart(t, "?desktop=1&format=json&next="+url.QueryEscape("http://127.0.0.1:41234/callback")); !strings.Contains(loc, "sso_error="+ssoErrPasswordChangeRequired) {
		t.Fatalf("the pending account was not refused at the callback: %s", loc)
	}
	failed = waitForPlatformAuditRows(t, f.audit, actionLoginFailed, 2)
	byReason := rowsByReason(failed)
	refusal := byReason["password_change_required"]
	if len(refusal) != 1 {
		t.Fatalf("%d password_change_required row(s), want 1: %+v", len(refusal), failed)
	}
	assertFailedRow(t, refusal[0], loginMethodDesktop, "password_change_required", "pending@example.org", "desk-pending")
	if got := metaString(refusal[0], "provider"); got != "testsso" {
		t.Errorf("meta.provider = %q, want testsso", got)
	}
	subjectHashOf(t, refusal[0])
}

// TestAuditLogin_ADesktopRedemptionRefusalNamesTheTicketsAccount: a ticket
// whose login a "sign out everywhere" revoked between the callback and the
// redemption is refused at the redemption, and the refusal names what the
// ticket carried — the account (target_id) and the connector.
func TestAuditLogin_ADesktopRedemptionRefusalNamesTheTicketsAccount(t *testing.T) {
	f := newLoginAuditFixture(t)
	f.s.desktopTickets = desktopsso.NewMemoryStore(time.Minute)
	f.withStubSSO(oidc.ExternalUser{Provider: "testsso", Subject: "desk-r", Email: "desk-r@example.org", Name: "Desk"})
	loc := f.ssoStart(t, "?desktop=1&format=json&next="+url.QueryEscape("http://127.0.0.1:41234/callback"))
	target, err := url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	ticket := target.Query().Get("ticket")
	if ticket == "" {
		t.Fatalf("the callback's redirect carries no ticket: %q", loc)
	}
	u, err := f.s.authStore().GetUserByEmail(context.Background(), "desk-r@example.org")
	if err != nil {
		t.Fatalf("the desktop sign-in provisioned no account: %v", err)
	}
	if err := f.svc.RevokeUserSessions(context.Background(), u.ID); err != nil {
		t.Fatal(err)
	}
	if code, body := f.send(t, http.MethodPost, "/api/auth/desktop/exchange", "", `{"ticket":"`+ticket+`"}`, clientIP); code != http.StatusUnauthorized {
		t.Fatalf("the redemption of a revoked login answered %d (%s), want 401", code, body)
	}
	failed := waitForPlatformAuditRows(t, f.audit, actionLoginFailed, 1)
	assertFailedRow(t, failed[0], loginMethodDesktop, "session_revoked", "desk-r@example.org", ticket)
	if failed[0].TargetID != u.ID || failed[0].ActorKind != "anonymous" {
		t.Errorf("the refusal is attributed to actor=%q/%q target_id=%q, want anonymous on the ticket's account %q",
			failed[0].ActorID, failed[0].ActorKind, failed[0].TargetID, u.ID)
	}
	if got := metaString(failed[0], "provider"); got != "testsso" {
		t.Errorf("meta.provider = %q, want testsso", got)
	}
}

// TestAuditLogin_TheLoginCSRFGuardFiringIsARefusal: an OIDC callback whose
// user-agent binding cookie disagrees with the flow's is refused before any
// identity resolves, and audited as agent_binding with the connector — no
// subject, no account.
func TestAuditLogin_TheLoginCSRFGuardFiringIsARefusal(t *testing.T) {
	f := newLoginAuditFixture(t)
	f.withStubSSO(oidc.ExternalUser{Provider: "testsso", Subject: "csrf-1", Email: "csrf@example.org"})
	tampered := false
	loc := f.ssoRoundTrip(t, "", func(c *http.Cookie) {
		if strings.HasSuffix(c.Name, oidcAgentBindingCookie) {
			c.Value = "a-binding-another-browser-holds"
			tampered = true
		}
	})
	if !tampered {
		t.Fatal("the start set no agent-binding cookie to tamper with")
	}
	if !strings.Contains(loc, "sso_error="+ssoErrAgentBinding) {
		t.Fatalf("the mismatched binding was not refused with %s: %s", ssoErrAgentBinding, loc)
	}
	failed := waitForPlatformAuditRows(t, f.audit, actionLoginFailed, 1)
	assertFailedRow(t, failed[0], loginMethodOIDC, "agent_binding", "csrf@example.org", "csrf-1")
	if got := metaString(failed[0], "provider"); got != "testsso" {
		t.Errorf("meta.provider = %q, want testsso", got)
	}
	if _, hashed := failed[0].Meta["subject_hash"]; hashed || failed[0].TargetID != "" {
		t.Errorf("a refusal before any identity resolved names a subject: %+v target_id=%q", failed[0].Meta, failed[0].TargetID)
	}
	if n := len(platformAuditRows(t, f.audit, actionLoginSucceeded)); n != 0 {
		t.Errorf("a refused callback wrote %d success row(s)", n)
	}
}

// TestAuditLogin_ACommittedChangeWithoutASessionIsAttributed: a credential
// change whose new password is committed but whose session cannot be
// opened answers 409 password_changed_sign_in — and the audit log names
// the account it changed: the refusal's target_id, and for a reset the
// user.password_reset row the change owes.
func TestAuditLogin_ACommittedChangeWithoutASessionIsAttributed(t *testing.T) {
	t.Run("reset confirm", func(t *testing.T) {
		f := newLoginAuditFixture(t)
		now := time.Now().UTC()
		if err := f.resets.Create(context.Background(), auth.PasswordReset{
			ID: "reset-c", UserID: f.user.User.ID, TokenHash: auth.HashRefreshToken("iar_reset-token-c"),
			CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		}); err != nil {
			t.Fatalf("seed reset: %v", err)
		}
		f.sessions.failCreateArmed.Store(true)
		if code, body := f.send(t, http.MethodPost, "/api/auth/password/reset/confirm", "",
			`{"token":"iar_reset-token-c","password":"a-new-password-forever-4"}`, clientIP); code != http.StatusConflict {
			t.Fatalf("a reset committed without a session answered %d (%s), want 409", code, body)
		}
		failed := waitForPlatformAuditRows(t, f.audit, actionLoginFailed, 1)
		assertFailedRow(t, failed[0], loginMethodPassword, "password_changed_sign_in", "iar_reset-token-c", "a-new-password")
		if failed[0].TargetID != f.user.User.ID {
			t.Errorf("the refusal names target_id=%q, want the account whose password changed %q", failed[0].TargetID, f.user.User.ID)
		}
		reset := waitForPlatformAuditRows(t, f.audit, "user.password_reset", 1)
		if reset[0].TargetID != f.user.User.ID {
			t.Errorf("user.password_reset names target_id=%q, want %q", reset[0].TargetID, f.user.User.ID)
		}
	})
	t.Run("forced rotation", func(t *testing.T) {
		f := newLoginAuditFixture(t)
		ctx := context.Background()
		reg, err := f.svc.Register(ctx, "pending-c@example.com", fixturePassword, "", "", "ua", "ip")
		if err != nil {
			t.Fatal(err)
		}
		pending := identity.UserStatusPendingPasswordChange
		if _, err := f.s.authStore().PatchUser(ctx, reg.User.ID, identity.UserPatch{Status: &pending}); err != nil {
			t.Fatal(err)
		}
		f.sessions.failCreateArmed.Store(true)
		if code, body := f.send(t, http.MethodPost, "/api/auth/password/change", "",
			`{"email":"pending-c@example.com","current_password":"`+fixturePassword+`","new_password":"rotated-password-forever-2"}`, clientIP); code != http.StatusConflict {
			t.Fatalf("a rotation committed without a session answered %d (%s), want 409", code, body)
		}
		failed := waitForPlatformAuditRows(t, f.audit, actionLoginFailed, 1)
		assertFailedRow(t, failed[0], loginMethodPassword, "password_changed_sign_in", "pending-c@example.com", "rotated-password")
		if failed[0].TargetID != reg.User.ID {
			t.Errorf("the refusal names target_id=%q, want the account whose password changed %q", failed[0].TargetID, reg.User.ID)
		}
	})
}

// TestSSOLoginMethod_ClassifiesEveryConnector: the built-in providers by
// name, an org's own IdP as org_sso whatever its slug, and every other
// deployment-wide connector — the one named "sso" included — as oidc.
func TestSSOLoginMethod_ClassifiesEveryConnector(t *testing.T) {
	for _, tc := range []struct {
		connector string
		org       bool
		want      string
	}{
		{"github", false, loginMethodGitHub},
		{"google", false, loginMethodGoogle},
		{"sso", false, loginMethodOIDC},
		{"testsso", false, loginMethodOIDC},
		{"oidc-org-o1", true, loginMethodOrgSSO},
		{"github", true, loginMethodOrgSSO},
	} {
		if got := ssoLoginMethod(tc.connector, tc.org); got != tc.want {
			t.Errorf("ssoLoginMethod(%q, org=%t) = %q, want %q", tc.connector, tc.org, got, tc.want)
		}
	}
}

// TestLoginFailureReason_NamesTheRefusalsNoAnnotationCarries: the sentinels
// the IdP side and the callback answer with are named, not collapsed into
// "error" (which is a server failure's), and a committed change without a
// session wins over the reason of the cause that stopped its session.
func TestLoginFailureReason_NamesTheRefusalsNoAnnotationCarries(t *testing.T) {
	// An annotated refusal as pkg/auth answers it: the cause a committed
	// change can wrap (its session's stamp refused).
	_, annotated := newLoginAuditFixture(t).svc.Login(context.Background(), "nobody@example.com", fixturePassword, "ua", "ip")
	if r, ok := auth.ReasonOf(annotated); !ok || r != auth.ReasonUnknownAccount {
		t.Fatalf("the probe refusal carries reason %q, %v", r, ok)
	}
	for _, tc := range []struct {
		err  error
		want string
	}{
		{oidc.ErrEmailMissing, "email_missing"},
		{oidc.ErrUnknownProvider, "provider_unknown"},
		{errAgentBindingMismatch, "agent_binding"},
		{fmt.Errorf("%w: %w", auth.ErrPasswordChangedSignIn, annotated), "password_changed_sign_in"},
		{annotated, string(auth.ReasonUnknownAccount)},
		{fmt.Errorf("store down"), "error"},
	} {
		if got := loginFailureReason(tc.err); got != tc.want {
			t.Errorf("loginFailureReason(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}
