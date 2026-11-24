package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// The browser approval `teams switch` runs through mints under the session
// the BROWSER holds, which is not necessarily the account the CLI is signed
// in as — the ordinary case on a machine where the operator's studio session
// is open while the CLI holds a service account's token. These witnesses
// drive the whole command against a real server and ask what the CLI does
// with a token it did not mint: it is read back from the server, refused
// without persisting when it answers for another account or another team,
// and revoked — so a refusal leaves no live token nobody can see.

// switchFixture is a real instance with two accounts in one shared team, the
// CLI's config holding the FIRST account's personal access token.
type switchFixture struct {
	base     string
	teamID   string
	emailA   string
	emailB   string
	patA     string
	sessionA string
	sessionB string
}

func newSwitchFixture(t *testing.T) switchFixture {
	t.Helper()
	const emailA, passA = "a@example.test", "password-forever-9"
	const emailB, passB = "b@example.test", "password-forever-8"
	const teamID = "11111111-2222-3333-4444-555555555555"
	base, store := realIterionServer(t, emailA, passA)
	ctx := context.Background()

	body, err := json.Marshal(map[string]string{"email": emailB, "password": passB})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(base+"/api/auth/register", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("register the second account: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("register the second account: %d %s", resp.StatusCode, raw)
	}

	sessionA := loginAccessToken(t, base, emailA, passA)
	sessionB := loginAccessToken(t, base, emailB, passB)
	meA, err := cli.NewRemoteClientFor(cli.RemoteConfig{BaseURL: base, Token: sessionA}).Me(ctx)
	if err != nil {
		t.Fatalf("read the first account: %v", err)
	}
	meB, err := cli.NewRemoteClientFor(cli.RemoteConfig{BaseURL: base, Token: sessionB}).Me(ctx)
	if err != nil {
		t.Fatalf("read the second account: %v", err)
	}
	if _, err := store.CreateOrg(ctx, identity.Org{ID: "o1", Name: "Org One", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("seed the org: %v", err)
	}
	if _, err := store.CreateTeam(ctx, identity.Team{ID: teamID, OrgID: "o1", Name: "Shared", Slug: "shared", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("seed the team: %v", err)
	}
	for _, u := range []string{meA.User.ID, meB.User.ID} {
		if err := store.UpsertOrgMembership(ctx, identity.OrgMembership{UserID: u, OrgID: "o1", Role: identity.OrgRoleMember, JoinedAt: time.Now()}); err != nil {
			t.Fatalf("seed the org membership: %v", err)
		}
		if err := store.UpsertMembership(ctx, identity.Membership{UserID: u, TeamID: teamID, Role: identity.RoleMember, JoinedAt: time.Now()}); err != nil {
			t.Fatalf("seed the team membership: %v", err)
		}
	}

	patA := mintTokenAs(t, base, sessionA, map[string]any{"name": "iterion-cli (A)"})
	t.Setenv("HOME", t.TempDir())
	for _, env := range []string{"ITERION_REMOTE_URL", "ITERION_REMOTE_TOKEN", "ITERION_REMOTE_TEAM", "ITERION_REMOTE_ORG"} {
		t.Setenv(env, "")
	}
	if err := cli.SaveRemoteConfig(cli.RemoteConfig{BaseURL: base, Token: patA}); err != nil {
		t.Fatal(err)
	}
	return switchFixture{base: base, teamID: teamID, emailA: emailA, emailB: emailB, patA: patA, sessionA: sessionA, sessionB: sessionB}
}

// approveAs plays the studio half of the approval: the page mints under
// session and answers the loopback. pinTeam decides whether it echoes the
// pin it was asked for — an instance whose /cli-auth predates ?team= mints
// without one and echoes nothing. It reports the token it minted.
func (f switchFixture) approveAs(t *testing.T, session string, pinTeam bool) *string {
	t.Helper()
	minted := new(string)
	prev := browserOpener
	t.Cleanup(func() { browserOpener = prev })
	browserOpener = func(u string) error {
		au, err := url.Parse(u)
		if err != nil {
			t.Errorf("the approval URL does not parse: %v", err)
			return nil
		}
		q := au.Query()
		body := map[string]any{"name": "iterion CLI"}
		if pinTeam {
			body["team_id"] = q.Get("team")
		}
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequest(http.MethodPost, f.base+"/api/me/tokens", bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+session)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("the approval's mint: %v", err)
		}
		defer resp.Body.Close()
		rb, _ := io.ReadAll(resp.Body)
		var out struct {
			Token string `json:"token"`
			OrgID string `json:"org_id"`
		}
		if err := json.Unmarshal(rb, &out); err != nil || out.Token == "" {
			t.Fatalf("the approval's mint answered %d %s", resp.StatusCode, rb)
		}
		*minted = out.Token
		cb := q.Get("redirect_uri") + "?token=" + url.QueryEscape(out.Token) +
			"&state=" + url.QueryEscape(q.Get("state")) +
			"&org_id=" + url.QueryEscape(out.OrgID)
		if pinTeam {
			cb += "&team_id=" + url.QueryEscape(q.Get("team"))
		}
		r, err := http.Get(cb)
		if err != nil {
			t.Errorf("the loopback callback: %v", err)
			return nil
		}
		r.Body.Close()
		return nil
	}
	return minted
}

// authenticates reports whether token still authenticates, and as whom.
func authenticates(ctx context.Context, base, token string) (string, bool) {
	me, err := cli.NewRemoteClientFor(cli.RemoteConfig{BaseURL: base, Token: token}).Me(ctx)
	if err != nil {
		return "", false
	}
	return me.User.Email, true
}

// TestRemoteTeamsSwitch_AnApprovalUnderAnotherAccountIsRefusedAndRevoked: the
// browser is signed in as B while the CLI holds A's token. The switch must
// not silently become B — every later command would run as B, and the
// revocation of "the previous CLI token" would look through B's tokens and
// find none of A's. Nothing is stored, the refusal names both accounts, and
// B's freshly minted token is revoked.
func TestRemoteTeamsSwitch_AnApprovalUnderAnotherAccountIsRefusedAndRevoked(t *testing.T) {
	f := newSwitchFixture(t)
	ctx := context.Background()
	minted := f.approveAs(t, f.sessionB, true)

	stored := cli.NewRemoteClientFor(cli.RemoteConfig{BaseURL: f.base, Token: f.patA})
	var switchErr error
	out := captureStdoutAllowingError(t, func() error {
		switchErr = cli.RemoteTeamsSwitch(ctx, stored, cli.NewPrinter(cli.OutputHuman), f.teamID, "iterion-cli (A)", cliTeamApproval(stored))
		return switchErr
	})
	if switchErr == nil {
		t.Fatalf("the switch accepted a token minted by another account: %s", out)
	}
	for _, want := range []string{f.emailA, f.emailB, "revoked"} {
		if !strings.Contains(switchErr.Error(), want) {
			t.Errorf("the refusal reads %q — it must name %q", switchErr, want)
		}
	}
	saved, err := cli.LoadRemoteConfig()
	if err != nil {
		t.Fatalf("read the saved config: %v", err)
	}
	if saved.Token != f.patA || saved.TeamID != "" {
		t.Fatalf("a refused switch persisted %+v, want the stored credential untouched", saved)
	}
	if who, ok := authenticates(ctx, f.base, *minted); ok {
		t.Errorf("the token the refused approval minted still authenticates (as %s): a refusal must leave none alive", who)
	}
	if who, ok := authenticates(ctx, f.base, f.patA); !ok || who != f.emailA {
		t.Errorf("the CLI's own token no longer authenticates as %s (got %q, ok=%v): a refused switch revokes nothing of the operator's", f.emailA, who, ok)
	}
}

// TestRemoteTeamsSwitch_AnApprovalWithoutThePinIsRefusedAndRevoked: an
// instance whose /cli-auth page predates ?team= mints an UNPINNED token and
// echoes no team. The CLI is distributed apart from the instance, so this is
// the ordinary version skew — and by the time the callback arrives the token
// exists. It is refused (the pin is the whole point of the command) and
// revoked, rather than abandoned alive and invisible outside the studio's
// token list.
func TestRemoteTeamsSwitch_AnApprovalWithoutThePinIsRefusedAndRevoked(t *testing.T) {
	f := newSwitchFixture(t)
	ctx := context.Background()
	minted := f.approveAs(t, f.sessionA, false)

	stored := cli.NewRemoteClientFor(cli.RemoteConfig{BaseURL: f.base, Token: f.patA})
	var switchErr error
	out := captureStdoutAllowingError(t, func() error {
		switchErr = cli.RemoteTeamsSwitch(ctx, stored, cli.NewPrinter(cli.OutputHuman), f.teamID, "iterion-cli (A)", cliTeamApproval(stored))
		return switchErr
	})
	if switchErr == nil {
		t.Fatalf("the switch accepted a token pinned to no team: %s", out)
	}
	if !strings.Contains(switchErr.Error(), "revoked") {
		t.Errorf("the refusal reads %q — it must say what became of the token it refused", switchErr)
	}
	saved, err := cli.LoadRemoteConfig()
	if err != nil {
		t.Fatalf("read the saved config: %v", err)
	}
	if saved.Token != f.patA || saved.TeamID != "" {
		t.Fatalf("a refused switch persisted %+v, want the stored credential untouched", saved)
	}
	if who, ok := authenticates(ctx, f.base, *minted); ok {
		t.Errorf("the unpinned token the approval minted still authenticates (as %s)", who)
	}
}

// TestRemoteTeamsSwitch_ASaveThatFailsRevokesTheApprovedToken drives the
// HAPPY approval — same account, the asked-for team pinned — and makes the
// one write the command does after its checks pass fail. The approval's
// token exists on the server and nothing holds it, so the branch that is
// NOT a verification refusal owes the same exit as the ones that are:
// nothing stored, the token revoked, its fate stated.
func TestRemoteTeamsSwitch_ASaveThatFailsRevokesTheApprovedToken(t *testing.T) {
	f := newSwitchFixture(t)
	ctx := context.Background()
	minted := f.approveAs(t, f.sessionA, true)

	p, err := cli.RemoteConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	// SaveRemoteConfig is a plain write; LoadRemoteConfig still reads.
	if err := os.Chmod(p, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o600) })

	stored := cli.NewRemoteClientFor(cli.RemoteConfig{BaseURL: f.base, Token: f.patA})
	var switchErr error
	_ = captureStdoutAllowingError(t, func() error {
		switchErr = cli.RemoteTeamsSwitch(ctx, stored, cli.NewPrinter(cli.OutputHuman), f.teamID, "iterion-cli (A)", cliTeamApproval(stored))
		return switchErr
	})
	if switchErr == nil {
		t.Fatal("the save was expected to fail")
	}
	for _, want := range []string{"save the CLI credentials", "revoked"} {
		if !strings.Contains(switchErr.Error(), want) {
			t.Errorf("the refusal reads %q — it must state %q", switchErr, want)
		}
	}
	saved, err := cli.LoadRemoteConfig()
	if err != nil {
		t.Fatalf("read the saved config: %v", err)
	}
	if saved.Token != f.patA {
		t.Errorf("a failed save persisted %+v, want the stored credential untouched", saved)
	}
	if who, ok := authenticates(ctx, f.base, *minted); ok {
		t.Errorf("the token the approval minted still authenticates (as %s): nothing holds it, so nothing can revoke it later", who)
	}
}

// TestRemoteLogin_ASaveThatFailsRevokesTheTokenItMinted is the SIBLING site:
// `remote login` through the browser receives a token minted by a session
// the CLI does not control, exactly like the switch, and its failures past
// that mint leave the same orphan. The control below proves the command
// leaves a token it did NOT mint alone.
func TestRemoteLogin_ASaveThatFailsRevokesTheTokenItMinted(t *testing.T) {
	f := newSwitchFixture(t)
	ctx := context.Background()
	minted := f.approveAs(t, f.sessionA, false)

	p, err := cli.RemoteConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o600) })

	remoteLoginCmd.SetContext(ctx)
	prevToken := remoteToken
	t.Cleanup(func() { remoteToken = prevToken })
	remoteToken = ""
	var loginErr error
	_ = captureStdoutAllowingError(t, func() error {
		loginErr = runRemoteLogin(remoteLoginCmd, []string{f.base})
		return loginErr
	})
	if loginErr == nil {
		t.Fatal("the save was expected to fail")
	}
	if !strings.Contains(loginErr.Error(), "revoked") {
		t.Errorf("the refusal reads %q — it must state the minted token's fate", loginErr)
	}
	if who, ok := authenticates(ctx, f.base, *minted); ok {
		t.Errorf("the token the browser login minted still authenticates (as %s): an orphan nobody holds", who)
	}
	// The control: a token the OPERATOR supplied has no row this command
	// created, and a login that refuses it must leave it alive.
	remoteToken = f.patA
	_ = captureStdoutAllowingError(t, func() error {
		loginErr = runRemoteLogin(remoteLoginCmd, []string{f.base})
		return loginErr
	})
	if loginErr == nil {
		t.Fatal("the save was expected to fail for the supplied token too")
	}
	if _, ok := authenticates(ctx, f.base, f.patA); !ok {
		t.Error("a login that failed revoked the token the OPERATOR supplied: it minted none, so it owns none")
	}
}
