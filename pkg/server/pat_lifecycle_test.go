package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/audit"
	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/pat"
)

// revokeRefusingPATs is a PAT store whose revokes fail for the one token id
// it is told to refuse, and that can run a hook right before the next revoke
// (to land a concurrent act between a handler's read and its revoke) and
// hold the next creates at a barrier (to line two mints up). Everything else
// passes through.
type revokeRefusingPATs struct {
	pat.Store
	mu           sync.Mutex
	refuse       string
	beforeRevoke func(id string)
	barrier      *sync.WaitGroup
}

func (r *revokeRefusingPATs) refuseRevokeOf(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refuse = id
}

// onNextRevoke runs hook once, right before the next Revoke or RevokeIfLive.
func (r *revokeRefusingPATs) onNextRevoke(hook func(id string)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.beforeRevoke = hook
}

// holdCreates makes the next n creates wait, once their row is written,
// until all n are.
func (r *revokeRefusingPATs) holdCreates(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	wg := &sync.WaitGroup{}
	wg.Add(n)
	r.barrier = wg
}

func (r *revokeRefusingPATs) Create(ctx context.Context, tk pat.Token) error {
	if err := r.Store.Create(ctx, tk); err != nil {
		return err
	}
	r.mu.Lock()
	wg := r.barrier
	r.mu.Unlock()
	if wg != nil {
		wg.Done()
		wg.Wait()
	}
	return nil
}

// enter answers whether id's revoke is refused, after running the pending
// hook.
func (r *revokeRefusingPATs) enter(id string) bool {
	r.mu.Lock()
	hook := r.beforeRevoke
	r.beforeRevoke = nil
	refused := r.refuse == id
	r.mu.Unlock()
	if hook != nil {
		hook(id)
	}
	return refused
}

func (r *revokeRefusingPATs) Revoke(ctx context.Context, id string, at time.Time) error {
	if r.enter(id) {
		return errors.New("store unreachable")
	}
	return r.Store.Revoke(ctx, id, at)
}

func (r *revokeRefusingPATs) RevokeIfLive(ctx context.Context, id string, at time.Time) (bool, error) {
	if r.enter(id) {
		return false, errors.New("store unreachable")
	}
	return r.Store.RevokeIfLive(ctx, id, at)
}

// purgeUnder revokes and purges id straight in the store: a concurrent purge
// landing between a handler's read and its revoke.
func purgeUnder(t *testing.T, store pat.Store, id string) {
	t.Helper()
	if err := store.Revoke(context.Background(), id, time.Now().UTC()); err != nil {
		t.Errorf("revoke under the handler: %v", err)
	}
	if err := store.Purge(context.Background(), id); err != nil {
		t.Errorf("purge under the handler: %v", err)
	}
}

// patLifecycleFixture is invitationMailFixture with the PAT store wrapped so
// a scenario can refuse one revoke, and an audit store to read the trail.
func patLifecycleFixture(t *testing.T) (*derivationFixture, *revokeRefusingPATs, *recordingMailer) {
	t.Helper()
	mailer := &recordingMailer{}
	var wrapped *revokeRefusingPATs
	f := newDerivationFixtureWithAuth(t, func(c *auth.Config) {
		c.Mailer = mailer
		c.PublicURL = "https://iterion.example"
	}, func(c *Config) {
		wrapped = &revokeRefusingPATs{Store: c.PATs}
		c.PATs = wrapped
		c.Audit = audit.NewMemoryStore()
	})
	return f, wrapped, mailer
}

// lifecycleToken mints a token through POST /api/me/tokens with the session
// and answers its row and plaintext.
func lifecycleToken(t *testing.T, f *derivationFixture, body string) (pat.Token, string) {
	t.Helper()
	code, _, resp := f.call(t, http.MethodPost, "/api/me/tokens", f.user.AccessToken, body)
	var v struct {
		PAT   pat.Token `json:"pat"`
		Token string    `json:"token"`
	}
	if err := json.Unmarshal([]byte(resp), &v); err != nil || code != http.StatusCreated || v.Token == "" {
		t.Fatalf("mint %s: %d %s", body, code, resp)
	}
	return v.PAT, v.Token
}

// inviteWith has the token invite address into team ta, by email only, and
// answers the accept token the email carries.
func inviteWith(t *testing.T, f *derivationFixture, mailer *recordingMailer, bearer, address string) string {
	t.Helper()
	before := len(mailer.messages())
	code, _, body := f.call(t, http.MethodPost, "/api/teams/ta/invitations", bearer, `{"email":"`+address+`","role":"member"}`)
	if code != http.StatusOK {
		t.Fatalf("the PAT's invitation of %s: %d %s", address, code, body)
	}
	sent := mailer.messages()
	if len(sent) != before+1 {
		t.Fatalf("the invitation of %s sent %d emails, want 1", address, len(sent)-before)
	}
	return acceptTokenOf(t, sent[len(sent)-1])
}

func lookupStatus(t *testing.T, f *derivationFixture, inviteToken string) (int, string) {
	t.Helper()
	code, _, body := f.call(t, http.MethodGet, "/api/auth/invitations/lookup?token="+url.QueryEscape(inviteToken), "", "")
	return code, body
}

func listTokens(t *testing.T, f *derivationFixture, query string) map[string]pat.Token {
	t.Helper()
	code, _, body := f.call(t, http.MethodGet, "/api/me/tokens"+query, f.user.AccessToken, "")
	if code != http.StatusOK {
		t.Fatalf("GET /api/me/tokens%s: %d %s", query, code, body)
	}
	var v struct {
		Tokens []pat.Token `json:"tokens"`
	}
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("decode the list: %v", err)
	}
	out := map[string]pat.Token{}
	for _, tk := range v.Tokens {
		out[tk.ID] = tk
	}
	return out
}

// TestPATLifecycle_ADisableSuspendsTheTokenAndWhatItSentAndAnEnableRestoresBoth:
// a disabled token authenticates nothing and its emailed invitations are
// refused at lookup and accept — the rows stay pending, nothing is swept — and
// an enable gives both back: the token authenticates and the invitation is
// accepted.
func TestPATLifecycle_ADisableSuspendsTheTokenAndWhatItSentAndAnEnableRestoresBoth(t *testing.T) {
	f, _, mailer := patLifecycleFixture(t)
	tok, plaintext := lifecycleToken(t, f, `{"name":"cli","team_id":"ta"}`)
	invite := inviteWith(t, f, mailer, plaintext, "lucas@example.com")

	code, _, body := f.call(t, http.MethodPost, "/api/me/tokens/"+tok.ID+"/disable", f.user.AccessToken, "")
	if code != http.StatusOK || !strings.Contains(body, `"disabled_at":"`) {
		t.Fatalf("disable: %d %s, want 200 carrying disabled_at", code, body)
	}
	if got := f.meStatus(t, plaintext); got != http.StatusUnauthorized {
		t.Fatalf("a disabled token authenticates: /auth/me %d, want 401", got)
	}
	if code, body := lookupStatus(t, f, invite); code != http.StatusGone || !strings.Contains(body, "does not stand") {
		t.Fatalf("lookup of a disabled token's invitation: %d %s, want 410", code, body)
	}
	if n := len(invitationsOfTeam(t, f, "ta")); n != 1 {
		t.Fatalf("%d pending invitations after the disable, want the 1 it does not sweep", n)
	}
	lucas, err := f.svc.Register(context.Background(), "lucas@example.com", fixturePassword, "Lucas", "", "ua", "ip")
	if err != nil {
		t.Fatalf("register the invitee: %v", err)
	}
	if code, _, body := f.call(t, http.MethodPost, "/api/auth/invitations/accept", lucas.AccessToken, `{"token":"`+invite+`"}`); code != http.StatusGone {
		t.Fatalf("accepting a disabled token's invitation: %d %s, want 410", code, body)
	}

	code, _, body = f.call(t, http.MethodPost, "/api/me/tokens/"+tok.ID+"/enable", f.user.AccessToken, "")
	if code != http.StatusOK || strings.Contains(body, "disabled_at") {
		t.Fatalf("enable: %d %s, want 200 with no disabled_at", code, body)
	}
	if got := f.meStatus(t, plaintext); got != http.StatusOK {
		t.Fatalf("an enabled token: /auth/me %d, want 200", got)
	}
	if code, body := lookupStatus(t, f, invite); code != http.StatusOK {
		t.Fatalf("lookup once the token is enabled again: %d %s, want 200", code, body)
	}
	if code, _, body := f.call(t, http.MethodPost, "/api/auth/invitations/accept", lucas.AccessToken, `{"token":"`+invite+`"}`); code != http.StatusOK {
		t.Fatalf("accepting once the token is enabled again: %d %s, want 200", code, body)
	}
	if _, err := f.s.authStore().GetMembership(context.Background(), lucas.User.ID, "ta"); err != nil {
		t.Fatalf("the accepted invitation wrote no grant: %v", err)
	}
}

// TestPATLifecycle_ARevokedTokenIsNeitherEnabledNorDisabled: a revocation is
// final — the enable a disable allows is refused on it, 409 pat_revoked, and
// the token stays dead.
func TestPATLifecycle_ARevokedTokenIsNeitherEnabledNorDisabled(t *testing.T) {
	f, _, _ := patLifecycleFixture(t)
	tok, plaintext := lifecycleToken(t, f, `{"name":"cli","team_id":"ta"}`)
	if code, _, body := f.call(t, http.MethodPost, "/api/me/tokens/"+tok.ID+"/disable", f.user.AccessToken, ""); code != http.StatusOK {
		t.Fatalf("disable: %d %s", code, body)
	}
	if code, _, body := f.call(t, http.MethodDelete, "/api/me/tokens/"+tok.ID, f.user.AccessToken, ""); code != http.StatusNoContent {
		t.Fatalf("revoke the disabled token: %d %s", code, body)
	}
	for _, act := range []string{"enable", "disable"} {
		code, _, body := f.call(t, http.MethodPost, "/api/me/tokens/"+tok.ID+"/"+act, f.user.AccessToken, "")
		if code != http.StatusConflict || !strings.Contains(body, `"error_code":"pat_revoked"`) {
			t.Fatalf("%s a revoked token: %d %s, want 409 pat_revoked", act, code, body)
		}
	}
	if got := f.meStatus(t, plaintext); got != http.StatusUnauthorized {
		t.Fatalf("a revoked token after a refused enable: /auth/me %d, want 401", got)
	}
}

// TestPATLifecycle_OnlyASessionEnablesAToken: re-arming a credential is a
// mint (auth.MintPAT) — a personal access token is refused, as at POST
// /api/me/tokens — while disabling one is a credential change it may make.
func TestPATLifecycle_OnlyASessionEnablesAToken(t *testing.T) {
	f, _, _ := patLifecycleFixture(t)
	target, _ := lifecycleToken(t, f, `{"name":"target","team_id":"ta"}`)
	_, actor := lifecycleToken(t, f, `{"name":"actor","team_id":"ta"}`)
	if code, _, body := f.call(t, http.MethodPost, "/api/me/tokens/"+target.ID+"/disable", actor, ""); code != http.StatusOK {
		t.Fatalf("a PAT disabling another token of its account: %d %s, want 200", code, body)
	}
	if code, _, body := f.call(t, http.MethodPost, "/api/me/tokens/"+target.ID+"/enable", actor, ""); code != http.StatusForbidden {
		t.Fatalf("a PAT enabling a token: %d %s, want 403", code, body)
	}
	if row, _ := f.pats.Get(context.Background(), target.ID); row.DisabledAt == nil {
		t.Fatal("a refused enable lifted the disable")
	}
}

// TestPATLifecycle_PurgeTakesARevokedRowOnly: a live or disabled token is
// refused (409 pat_not_revoked) and kept; a revoked one's row goes, its audit
// lines stay, and a re-issued purge answers 204.
func TestPATLifecycle_PurgeTakesARevokedRowOnly(t *testing.T) {
	f, _, mailer := patLifecycleFixture(t)
	live, livePlain := lifecycleToken(t, f, `{"name":"live","team_id":"ta"}`)
	inviteWith(t, f, mailer, livePlain, "lucas@example.com")
	off, _ := lifecycleToken(t, f, `{"name":"off","team_id":"ta"}`)
	dead, _ := lifecycleToken(t, f, `{"name":"dead","team_id":"ta"}`)
	if code, _, body := f.call(t, http.MethodPost, "/api/me/tokens/"+off.ID+"/disable", f.user.AccessToken, ""); code != http.StatusOK {
		t.Fatalf("disable: %d %s", code, body)
	}
	if code, _, body := f.call(t, http.MethodDelete, "/api/me/tokens/"+dead.ID, f.user.AccessToken, ""); code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", code, body)
	}
	for _, tk := range []pat.Token{live, off} {
		code, _, body := f.call(t, http.MethodPost, "/api/me/tokens/"+tk.ID+"/purge", f.user.AccessToken, "")
		if code != http.StatusConflict || !strings.Contains(body, `"error_code":"pat_not_revoked"`) {
			t.Fatalf("purge the %s token: %d %s, want 409 pat_not_revoked", tk.Name, code, body)
		}
		if _, err := f.pats.Get(context.Background(), tk.ID); err != nil {
			t.Fatalf("a refused purge removed the %s token: %v", tk.Name, err)
		}
	}
	if n := len(invitationsOfTeam(t, f, "ta")); n != 1 {
		t.Fatalf("%d pending invitations after a refused purge of the live token that sent one, want it standing", n)
	}
	bob, err := f.svc.Register(context.Background(), "bob@example.com", fixturePassword, "Bob", "", "ua", "ip")
	if err != nil {
		t.Fatalf("register another account: %v", err)
	}
	if code, _, body := f.call(t, http.MethodPost, "/api/me/tokens/"+dead.ID+"/purge", bob.AccessToken, ""); code != http.StatusForbidden {
		t.Fatalf("another account purging the token: %d %s, want 403", code, body)
	}
	if code, _, body := f.call(t, http.MethodPost, "/api/me/tokens/"+dead.ID+"/purge", f.user.AccessToken, ""); code != http.StatusNoContent {
		t.Fatalf("purge the revoked token: %d %s, want 204", code, body)
	}
	if _, err := f.pats.Get(context.Background(), dead.ID); !errors.Is(err, pat.ErrNotFound) {
		t.Fatalf("the purged row reads err %v, want ErrNotFound", err)
	}
	if code, _, body := f.call(t, http.MethodPost, "/api/me/tokens/"+dead.ID+"/purge", f.user.AccessToken, ""); code != http.StatusNoContent {
		t.Fatalf("a re-issued purge: %d %s, want 204", code, body)
	}
	if _, ok := listTokens(t, f, "?include_revoked=1")[dead.ID]; ok {
		t.Fatal("the purged token is still listed with include_revoked")
	}
	tenant := f.user.User.DefaultTeamID
	waitForTenantAuditRows(t, f.s.auditStore, tenant, "pat.revoked", 1)
	purged := waitForTenantAuditRows(t, f.s.auditStore, tenant, "pat.purged", 1)
	if purged[0].TargetID != dead.ID {
		t.Fatalf("the pat.purged line names %q, want the purged token %s", purged[0].TargetID, dead.ID)
	}
}

// TestPATLifecycle_PurgeFinishesTheInvitationSweep: a revoke whose sweep left
// a pending invitation behind is finished by the purge, which names what it
// ended before the row goes.
func TestPATLifecycle_PurgeFinishesTheInvitationSweep(t *testing.T) {
	f, _, mailer := patLifecycleFixture(t)
	tok, plaintext := lifecycleToken(t, f, `{"name":"cli","team_id":"ta"}`)
	inviteWith(t, f, mailer, plaintext, "lucas@example.com")
	// A revoke that sweeps nothing: the store revocation alone.
	if err := f.pats.Revoke(context.Background(), tok.ID, time.Now().UTC()); err != nil {
		t.Fatalf("revoke in the store: %v", err)
	}
	code, _, body := f.call(t, http.MethodPost, "/api/me/tokens/"+tok.ID+"/purge", f.user.AccessToken, "")
	if code != http.StatusOK || !strings.Contains(body, "lucas@example.com") {
		t.Fatalf("purge a token whose invitation still stands: %d %s, want 200 naming it", code, body)
	}
	if n := len(invitationsOfTeam(t, f, "ta")); n != 0 {
		t.Fatalf("%d invitations pending after the purge, want 0", n)
	}
}

// TestPATLifecycle_TheListHidesTheDeadTokensUnlessAsked: revoked and expired
// tokens are listed with include_revoked only; a disabled one is listed by
// default, since an enable restores it.
func TestPATLifecycle_TheListHidesTheDeadTokensUnlessAsked(t *testing.T) {
	f, _, _ := patLifecycleFixture(t)
	live, _ := lifecycleToken(t, f, `{"name":"live","team_id":"ta"}`)
	off, _ := lifecycleToken(t, f, `{"name":"off","team_id":"ta"}`)
	dead, _ := lifecycleToken(t, f, `{"name":"dead","team_id":"ta"}`)
	past := time.Now().UTC().Add(-time.Hour)
	if err := f.pats.Create(context.Background(), pat.Token{
		ID: "expired-1", UserID: f.user.User.ID, Name: "old", TokenHash: "h-expired-1", TeamID: "ta",
		CreatedAt: past.Add(-time.Hour), ExpiresAt: &past,
	}); err != nil {
		t.Fatalf("seed an expired token: %v", err)
	}
	if code, _, body := f.call(t, http.MethodPost, "/api/me/tokens/"+off.ID+"/disable", f.user.AccessToken, ""); code != http.StatusOK {
		t.Fatalf("disable: %d %s", code, body)
	}
	if code, _, body := f.call(t, http.MethodDelete, "/api/me/tokens/"+dead.ID, f.user.AccessToken, ""); code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", code, body)
	}
	def := listTokens(t, f, "")
	for _, id := range []string{live.ID, off.ID} {
		if _, ok := def[id]; !ok {
			t.Fatalf("the default list misses %s: %v", id, def)
		}
	}
	for _, id := range []string{dead.ID, "expired-1"} {
		if _, ok := def[id]; ok {
			t.Fatalf("the default list shows the dead token %s", id)
		}
	}
	all := listTokens(t, f, "?include_revoked=1")
	for _, id := range []string{live.ID, off.ID, dead.ID, "expired-1"} {
		if _, ok := all[id]; !ok {
			t.Fatalf("include_revoked=1 misses %s: %v", id, all)
		}
	}
	if code, _, body := f.call(t, http.MethodGet, "/api/me/tokens?include_revoked=maybe", f.user.AccessToken, ""); code != http.StatusBadRequest {
		t.Fatalf("include_revoked=maybe: %d %s, want 400", code, body)
	}
}

// regenerated decodes a regenerate's answer.
type regenerated struct {
	PAT                         pat.Token               `json:"pat"`
	Token                       string                  `json:"token"`
	OrgID                       string                  `json:"org_id"`
	Replaced                    string                  `json:"replaced"`
	RevokeError                 string                  `json:"revoke_error"`
	InvitationsRevoked          []revokedInvitationView `json:"invitations_revoked"`
	InvitationsRevokedElsewhere int                     `json:"invitations_revoked_elsewhere"`
}

// TestPATLifecycle_ARegenerateReplacesTheTokenAndEndsWhatTheOldOneSent: the
// replacement keeps the name, the pin and the expiry INSTANT; its plaintext
// authenticates; the replaced token is revoked through the revoke's own path,
// so the invitation it emailed ends and is named.
func TestPATLifecycle_ARegenerateReplacesTheTokenAndEndsWhatTheOldOneSent(t *testing.T) {
	f, _, mailer := patLifecycleFixture(t)
	old, oldPlain := lifecycleToken(t, f, `{"name":"ci","team_id":"ta","expires_in_days":30}`)
	invite := inviteWith(t, f, mailer, oldPlain, "lucas@example.com")

	code, _, body := f.call(t, http.MethodPost, "/api/me/tokens/"+old.ID+"/regenerate", f.user.AccessToken, "")
	if code != http.StatusCreated {
		t.Fatalf("regenerate: %d %s, want 201", code, body)
	}
	var got regenerated
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode the regenerate: %v", err)
	}
	if got.Token == "" || got.Token == oldPlain || got.PAT.ID == old.ID || got.Replaced != old.ID {
		t.Fatalf("the regenerate answered %+v, want a new token replacing %s", got, old.ID)
	}
	if got.PAT.Name != old.Name || got.PAT.TeamID != "ta" || got.OrgID != "o1" {
		t.Fatalf("the replacement is %+v (org %q), want name %q pinned to ta in o1", got.PAT, got.OrgID, old.Name)
	}
	if got.PAT.ExpiresAt == nil || !got.PAT.ExpiresAt.Equal(*old.ExpiresAt) {
		t.Fatalf("the replacement expires at %v, want the replaced token's instant %v", got.PAT.ExpiresAt, old.ExpiresAt)
	}
	if f.meStatus(t, got.Token) != http.StatusOK {
		t.Fatal("the replacement does not authenticate")
	}
	if f.meStatus(t, oldPlain) != http.StatusUnauthorized {
		t.Fatal("the replaced token still authenticates")
	}
	if len(got.InvitationsRevoked) != 1 || got.InvitationsRevoked[0].Email != "lucas@example.com" {
		t.Fatalf("the regenerate named %+v, want the invitation the replaced token sent", got.InvitationsRevoked)
	}
	if n := len(invitationsOfTeam(t, f, "ta")); n != 0 {
		t.Fatalf("%d invitations pending after the regenerate, want 0", n)
	}
	if code, body := lookupStatus(t, f, invite); code != http.StatusNotFound {
		t.Fatalf("lookup of the replaced token's invitation: %d %s, want 404", code, body)
	}
	tenant := f.user.User.DefaultTeamID
	revoked := waitForTenantAuditRows(t, f.s.auditStore, tenant, "pat.revoked", 1)
	if revoked[0].Meta["reason"] != "regenerated" || revoked[0].Meta["replaced_by"] != got.PAT.ID {
		t.Fatalf("the pat.revoked line names %v, want reason regenerated and the replacement", revoked[0].Meta)
	}
}

// TestPATLifecycle_ARegenerateWhoseRevokeFailsStillHandsTheReplacementOver:
// the replacement is minted first, so a revoke that fails afterwards answers
// the new plaintext all the same, and says the replaced token still stands.
func TestPATLifecycle_ARegenerateWhoseRevokeFailsStillHandsTheReplacementOver(t *testing.T) {
	f, wrapped, _ := patLifecycleFixture(t)
	old, oldPlain := lifecycleToken(t, f, `{"name":"ci","team_id":"ta"}`)
	wrapped.refuseRevokeOf(old.ID)
	code, _, body := f.call(t, http.MethodPost, "/api/me/tokens/"+old.ID+"/regenerate", f.user.AccessToken, "")
	if code != http.StatusCreated {
		t.Fatalf("regenerate with a failing revoke: %d %s, want 201", code, body)
	}
	var got regenerated
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Token == "" || !strings.Contains(got.RevokeError, "still authenticates") {
		t.Fatalf("the regenerate answered %s, want the new plaintext and a revoke_error", body)
	}
	if f.meStatus(t, got.Token) != http.StatusOK || f.meStatus(t, oldPlain) != http.StatusOK {
		t.Fatal("after a failed revoke both tokens must stand, as the answer says")
	}
}

// TestPATLifecycle_ARegenerateIsRefusedWhereAMintWouldBe: from a personal
// access token (403, nothing minted), on another account's token (403), on a
// revoked (409 pat_revoked) or expired (409 pat_expired) token.
func TestPATLifecycle_ARegenerateIsRefusedWhereAMintWouldBe(t *testing.T) {
	f, _, _ := patLifecycleFixture(t)
	tok, plaintext := lifecycleToken(t, f, `{"name":"ci","team_id":"ta"}`)
	before, _ := f.tokensOf(t)
	if code, _, body := f.call(t, http.MethodPost, "/api/me/tokens/"+tok.ID+"/regenerate", plaintext, ""); code != http.StatusForbidden {
		t.Fatalf("a PAT regenerating: %d %s, want 403", code, body)
	}
	if after, _ := f.tokensOf(t); after != before {
		t.Fatalf("a refused regenerate minted: %d tokens, want %d", after, before)
	}
	admin := f.superAdmin(t)
	if code, _, body := f.call(t, http.MethodPost, "/api/me/tokens/"+tok.ID+"/regenerate", admin.AccessToken, ""); code != http.StatusForbidden {
		t.Fatalf("a super-admin regenerating another account's token: %d %s, want 403", code, body)
	}
	past := time.Now().UTC().Add(-time.Minute)
	if err := f.pats.Create(context.Background(), pat.Token{
		ID: "expired-1", UserID: f.user.User.ID, Name: "old", TokenHash: "h-expired-1", TeamID: "ta",
		CreatedAt: past.Add(-time.Hour), ExpiresAt: &past,
	}); err != nil {
		t.Fatalf("seed an expired token: %v", err)
	}
	if code, _, body := f.call(t, http.MethodPost, "/api/me/tokens/expired-1/regenerate", f.user.AccessToken, ""); code != http.StatusConflict || !strings.Contains(body, `"error_code":"pat_expired"`) {
		t.Fatalf("regenerate an expired token: %d %s, want 409 pat_expired", code, body)
	}
	if code, _, body := f.call(t, http.MethodDelete, "/api/me/tokens/"+tok.ID, f.user.AccessToken, ""); code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", code, body)
	}
	if code, _, body := f.call(t, http.MethodPost, "/api/me/tokens/"+tok.ID+"/regenerate", f.user.AccessToken, ""); code != http.StatusConflict || !strings.Contains(body, `"error_code":"pat_revoked"`) {
		t.Fatalf("regenerate a revoked token: %d %s, want 409 pat_revoked", code, body)
	}
	if code, _, body := f.call(t, http.MethodPost, "/api/me/tokens/ghost/regenerate", f.user.AccessToken, ""); code != http.StatusNotFound {
		t.Fatalf("regenerate a token that does not exist: %d %s, want 404", code, body)
	}
}

// TestPATLifecycle_ARegeneratePinnedToATeamItsOwnerLeftIsRefused: the
// replacement keeps the pin, so a pin its owner no longer reaches would mint
// a token that never authenticates — 409 pat_pin_unreachable, nothing minted.
func TestPATLifecycle_ARegeneratePinnedToATeamItsOwnerLeftIsRefused(t *testing.T) {
	f, _, _ := patLifecycleFixture(t)
	tok, _ := lifecycleToken(t, f, `{"name":"ci","team_id":"ta"}`)
	ctx := context.Background()
	if err := f.s.authStore().DeleteMembership(ctx, f.user.User.ID, "ta"); err != nil {
		t.Fatalf("remove the grant: %v", err)
	}
	if err := f.s.authStore().DeleteOrgMembership(ctx, f.user.User.ID, "o1"); err != nil {
		t.Fatalf("remove the org membership: %v", err)
	}
	before, _ := f.tokensOf(t)
	code, _, body := f.call(t, http.MethodPost, "/api/me/tokens/"+tok.ID+"/regenerate", f.user.AccessToken, "")
	if code != http.StatusConflict || !strings.Contains(body, `"error_code":"pat_pin_unreachable"`) {
		t.Fatalf("regenerate a token pinned to a team its owner left: %d %s, want 409 pat_pin_unreachable", code, body)
	}
	if after, _ := f.tokensOf(t); after != before {
		t.Fatalf("a refused regenerate minted: %d tokens, want %d", after, before)
	}
	if row, _ := f.pats.Get(ctx, tok.ID); row.RevokedAt != nil {
		t.Fatal("a refused regenerate revoked the token")
	}
}

// TestPATLifecycle_TwoRegeneratesOfOneTokenHandOverOneReplacement: both mint
// (lined up at the store), only the one whose compare-and-set retires the
// token answers 201; the other revokes its own replacement and answers 409
// pat_revoked — one usable token in the end, never two.
func TestPATLifecycle_TwoRegeneratesOfOneTokenHandOverOneReplacement(t *testing.T) {
	f, wrapped, _ := patLifecycleFixture(t)
	old, _ := lifecycleToken(t, f, `{"name":"ci","team_id":"ta"}`)
	wrapped.holdCreates(2)
	type answer struct {
		code int
		body string
	}
	answers := make(chan answer, 2)
	for range 2 {
		go func() {
			code, _, body := f.call(t, http.MethodPost, "/api/me/tokens/"+old.ID+"/regenerate", f.user.AccessToken, "")
			answers <- answer{code, body}
		}()
	}
	codes := map[int]int{}
	var refused string
	for range 2 {
		a := <-answers
		codes[a.code]++
		if a.code == http.StatusConflict {
			refused = a.body
		}
	}
	if codes[http.StatusCreated] != 1 || codes[http.StatusConflict] != 1 || !strings.Contains(refused, `"error_code":"pat_revoked"`) {
		t.Fatalf("two concurrent regenerates answered %v (refusal %s), want one 201 and one 409 pat_revoked", codes, refused)
	}
	if _, usable := f.tokensOf(t); usable != 1 {
		t.Fatalf("%d usable tokens after two concurrent regenerates, want the one replacement", usable)
	}
}

// TestPATLifecycle_ARegenerateOfATokenPurgedUnderItHandsNothingOver: the
// token is revoked and purged between the regenerate's read and its revoke —
// another call retired it, so the replacement goes and the answer is 409,
// never a 201 saying a gone token "still authenticates".
func TestPATLifecycle_ARegenerateOfATokenPurgedUnderItHandsNothingOver(t *testing.T) {
	f, wrapped, _ := patLifecycleFixture(t)
	old, _ := lifecycleToken(t, f, `{"name":"ci","team_id":"ta"}`)
	wrapped.onNextRevoke(func(id string) { purgeUnder(t, wrapped.Store, id) })
	code, _, body := f.call(t, http.MethodPost, "/api/me/tokens/"+old.ID+"/regenerate", f.user.AccessToken, "")
	if code != http.StatusConflict || !strings.Contains(body, `"error_code":"pat_revoked"`) {
		t.Fatalf("regenerate of a token purged under it: %d %s, want 409 pat_revoked", code, body)
	}
	if _, usable := f.tokensOf(t); usable != 0 {
		t.Fatalf("%d usable tokens after a regenerate that replaced nothing, want 0", usable)
	}
}

// TestPATLifecycle_ARevokeOfATokenPurgedUnderItIsDone: DELETE read the row,
// then it was purged: the token is gone, which is what the revoke asks for —
// 204, as for any token already gone.
func TestPATLifecycle_ARevokeOfATokenPurgedUnderItIsDone(t *testing.T) {
	f, wrapped, _ := patLifecycleFixture(t)
	tok, _ := lifecycleToken(t, f, `{"name":"ci","team_id":"ta"}`)
	wrapped.onNextRevoke(func(id string) { purgeUnder(t, wrapped.Store, id) })
	if code, _, body := f.call(t, http.MethodDelete, "/api/me/tokens/"+tok.ID, f.user.AccessToken, ""); code != http.StatusNoContent {
		t.Fatalf("revoke of a token purged under it: %d %s, want 204", code, body)
	}
}

// oneDeleteRefusingStore refuses the delete of one invitation.
type oneDeleteRefusingStore struct {
	identity.Store
	refuse string
}

func (s oneDeleteRefusingStore) DeleteInvitation(ctx context.Context, id string) error {
	if id == s.refuse {
		return errors.New("store unreachable")
	}
	return s.Store.DeleteInvitation(ctx, id)
}

// TestPATLifecycle_APurgeWhoseSweepStopsNamesWhatItEnded: one of the revoked
// token's two invitations cannot be taken out — 409 pat_sweep_unfinished
// naming the one that did end and why the other stands, the row kept; the
// re-issued purge finishes.
func TestPATLifecycle_APurgeWhoseSweepStopsNamesWhatItEnded(t *testing.T) {
	f, _, mailer := patLifecycleFixture(t)
	tok, plaintext := lifecycleToken(t, f, `{"name":"cli","team_id":"ta"}`)
	inviteWith(t, f, mailer, plaintext, "lucas@example.com")
	inviteWith(t, f, mailer, plaintext, "victor@example.com")
	var refuse string
	for _, inv := range invitationsOfTeam(t, f, "ta") {
		if inv.Email == "victor@example.com" {
			refuse = inv.ID
		}
	}
	if err := f.pats.Revoke(context.Background(), tok.ID, time.Now().UTC()); err != nil {
		t.Fatalf("revoke in the store: %v", err)
	}
	real := f.s.authStore()
	f.s.authSvc.SetStoreForTest(oneDeleteRefusingStore{Store: real, refuse: refuse})
	code, _, body := f.call(t, http.MethodPost, "/api/me/tokens/"+tok.ID+"/purge", f.user.AccessToken, "")
	f.s.authSvc.SetStoreForTest(real)
	var v struct {
		ErrorCode          string                  `json:"error_code"`
		InvitationsRevoked []revokedInvitationView `json:"invitations_revoked"`
		InvitationsError   string                  `json:"invitations_error"`
	}
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("decode the purge answer %s: %v", body, err)
	}
	if code != http.StatusConflict || v.ErrorCode != "pat_sweep_unfinished" ||
		len(v.InvitationsRevoked) != 1 || v.InvitationsRevoked[0].Email != "lucas@example.com" ||
		!strings.Contains(v.InvitationsError, refuse) {
		t.Fatalf("purge with a sweep that stopped: %d %s, want 409 pat_sweep_unfinished naming lucas and the invitation left", code, body)
	}
	if _, err := f.pats.Get(context.Background(), tok.ID); err != nil {
		t.Fatalf("a purge whose sweep stopped removed the row: %v", err)
	}
	code, _, body = f.call(t, http.MethodPost, "/api/me/tokens/"+tok.ID+"/purge", f.user.AccessToken, "")
	if code != http.StatusOK || !strings.Contains(body, "victor@example.com") {
		t.Fatalf("the re-issued purge: %d %s, want 200 naming the invitation it finished", code, body)
	}
}

// TestPATLifecycle_APinIsJudgedOnTheStoreNotOnTheSessionsClaim: a session
// switched to team ta carries ta and a member role in its access token; once
// its owner's membership in ta is removed, neither a mint nor a regenerate
// pinned there passes on that claim — the token would never authenticate.
func TestPATLifecycle_APinIsJudgedOnTheStoreNotOnTheSessionsClaim(t *testing.T) {
	f, _, _ := patLifecycleFixture(t)
	tok, _ := lifecycleToken(t, f, `{"name":"ci","team_id":"ta"}`)
	code, inTA, _ := switchOverHTTP(t, f.srv, "ta", f.user.AccessToken)
	if code != http.StatusOK || inTA == "" {
		t.Fatalf("switch to ta: %d", code)
	}
	ctx := context.Background()
	if err := f.s.authStore().DeleteMembership(ctx, f.user.User.ID, "ta"); err != nil {
		t.Fatalf("remove the grant: %v", err)
	}
	before, _ := f.tokensOf(t)
	code, _, body := f.call(t, http.MethodPost, "/api/me/tokens/"+tok.ID+"/regenerate", inTA, "")
	if code != http.StatusConflict || !strings.Contains(body, `"error_code":"pat_pin_unreachable"`) {
		t.Fatalf("regenerate on a stale team claim: %d %s, want 409 pat_pin_unreachable", code, body)
	}
	for _, req := range []string{`{"name":"explicit","team_id":"ta"}`, `{"name":"default-pin"}`} {
		if code, _, body := f.call(t, http.MethodPost, "/api/me/tokens", inTA, req); code != http.StatusForbidden {
			t.Fatalf("mint %s on a stale team claim: %d %s, want 403", req, code, body)
		}
	}
	if after, _ := f.tokensOf(t); after != before {
		t.Fatalf("a refused mint or regenerate wrote a token: %d, want %d", after, before)
	}
	if row, _ := f.pats.Get(ctx, tok.ID); row.RevokedAt != nil {
		t.Fatal("a refused regenerate revoked the live token")
	}
}
