package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// TestRemoteTeamsSwitch_TheBrowserApprovalMintsFromTheLiveSession is the
// #2674 end-to-end witness: the CLI's stored credential is a personal access
// token, which mints nothing, so `teams switch` re-opens the browser approval
// with ?team=<id> — the emulated studio half here mints through a FRESH
// session's access token, which is the only credential the real server's
// POST /api/me/tokens accepts the mint from. The approved token lands pinned
// to the target team, the CLI's config follows it (team AND org), and the
// replaced PAT is revoked.
func TestRemoteTeamsSwitch_TheBrowserApprovalMintsFromTheLiveSession(t *testing.T) {
	const email, password = "ops@example.test", "password-forever-9"
	base, store := realIterionServer(t, email, password)
	ctx := context.Background()

	session := loginAccessToken(t, base, email, password)
	sessionClient := cli.NewRemoteClientFor(cli.RemoteConfig{BaseURL: base, Token: session})
	me, err := sessionClient.Me(ctx)
	if err != nil {
		t.Fatalf("read the account: %v", err)
	}
	// Team B, joined by the account, addressable by its slug.
	if _, err := store.CreateOrg(ctx, identity.Org{ID: "o1", Name: "Org One", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("seed the org: %v", err)
	}
	if _, err := store.CreateTeam(ctx, identity.Team{ID: "team-b-uuid", OrgID: "o1", Name: "Team Bee", Slug: "team-bee", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("seed team B: %v", err)
	}
	if err := store.UpsertOrgMembership(ctx, identity.OrgMembership{UserID: me.User.ID, OrgID: "o1", Role: identity.OrgRoleAdmin, JoinedAt: time.Now()}); err != nil {
		t.Fatalf("seed the org membership: %v", err)
	}
	if err := store.UpsertMembership(ctx, identity.Membership{UserID: me.User.ID, TeamID: "team-b-uuid", Role: identity.RoleMember, JoinedAt: time.Now()}); err != nil {
		t.Fatalf("seed the team membership: %v", err)
	}

	// The CLI's stored credential: a personal access token (what `remote
	// login` saves), pinned to the account's personal team.
	storedPAT := mintTokenAs(t, base, session, map[string]any{"name": "iterion-cli (test)"})
	if !strings.HasPrefix(storedPAT, "iap_") {
		t.Fatalf("the stored credential is not a personal access token: %q", storedPAT)
	}
	t.Setenv("HOME", t.TempDir())
	for _, env := range []string{"ITERION_REMOTE_URL", "ITERION_REMOTE_TOKEN", "ITERION_REMOTE_TEAM", "ITERION_REMOTE_ORG"} {
		t.Setenv(env, "")
	}
	if err := cli.SaveRemoteConfig(cli.RemoteConfig{BaseURL: base, Token: storedPAT}); err != nil {
		t.Fatal(err)
	}

	// The studio's half of the approval: the page the CLI opened mints under
	// a LIVE SESSION — the real server refuses the mint to a PAT (403), so
	// this succeeding IS the witness that the stored token minted nothing.
	prevOpener := browserOpener
	t.Cleanup(func() { browserOpener = prevOpener })
	browserOpener = func(u string) error {
		authURL, err := url.Parse(u)
		if err != nil {
			t.Errorf("the approval URL does not parse: %v", err)
			return nil
		}
		q := authURL.Query()
		if !strings.HasPrefix(authURL.Path, "/cli-auth") {
			t.Errorf("the approval opened %q, want the /cli-auth page", authURL.Path)
		}
		if got := q.Get("team"); got != "team-b-uuid" {
			t.Errorf("the approval URL carries team=%q, want the resolved UUID", got)
		}
		approving := loginAccessToken(t, base, email, password)
		body := map[string]any{"name": "iterion CLI", "team_id": q.Get("team")}
		token, orgID := func() (string, string) {
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			req, err := http.NewRequest(http.MethodPost, base+"/api/me/tokens", bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+approving)
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("the approval's mint: %v", err)
			}
			defer resp.Body.Close()
			raw, _ = io.ReadAll(resp.Body)
			var out struct {
				Token string `json:"token"`
				OrgID string `json:"org_id"`
			}
			_ = json.Unmarshal(raw, &out)
			if resp.StatusCode != http.StatusCreated || out.Token == "" {
				t.Fatalf("the live session's mint answered %d %s", resp.StatusCode, raw)
			}
			return out.Token, out.OrgID
		}()
		cb := q.Get("redirect_uri") + "?token=" + url.QueryEscape(token) +
			"&state=" + url.QueryEscape(q.Get("state")) +
			"&team_id=" + url.QueryEscape(q.Get("team")) +
			"&org_id=" + url.QueryEscape(orgID)
		resp, err := http.Get(cb)
		if err != nil {
			t.Errorf("the loopback callback: %v", err)
			return nil
		}
		resp.Body.Close()
		return nil
	}

	stored := cli.NewRemoteClientFor(cli.RemoteConfig{BaseURL: base, Token: storedPAT})
	out := captureStdoutAllowingError(t, func() error {
		return cli.RemoteTeamsSwitch(ctx, stored, cli.NewPrinter(cli.OutputHuman), "team-bee", "iterion-cli (test)", cliTeamApproval(stored))
	})
	saved, err := cli.LoadRemoteConfig()
	if err != nil {
		t.Fatalf("read the saved config: %v (%s)", err, out)
	}
	if saved.TeamID != "team-b-uuid" {
		t.Fatalf("the CLI switched to %q, want team B's UUID (%s)", saved.TeamID, out)
	}
	if !strings.HasPrefix(saved.Token, "iap_") || saved.Token == storedPAT {
		t.Fatalf("the CLI stored %q — want the approval's fresh token", saved.Token)
	}
	if saved.OrgID != "o1" {
		t.Fatalf("the org scope did not follow the team: %q, want o1", saved.OrgID)
	}

	// The replaced CLI token is revoked: it authenticates nothing any more.
	oldClient := cli.NewRemoteClientFor(cli.RemoteConfig{BaseURL: base, Token: storedPAT})
	if _, err := oldClient.Me(ctx); err == nil {
		t.Fatal("the replaced personal access token still authenticates")
	}
	// The approved one does, pinned to team B.
	newMe, err := cli.NewRemoteClientFor(cli.RemoteConfig{BaseURL: base, Token: saved.Token}).Me(ctx)
	if err != nil {
		t.Fatalf("the approved token does not authenticate: %v", err)
	}
	if newMe.ActiveTeam != "team-b-uuid" {
		t.Fatalf("the approved token's pin is %q, want team-b-uuid", newMe.ActiveTeam)
	}
}

// mintTokenAs mints a personal access token for the account session belongs
// to, presenting the session's access token — the one credential the mint
// route accepts.
func mintTokenAs(t *testing.T, base, session string, body map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, base+"/api/me/tokens", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+session)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("mint the token: %v", err)
	}
	defer resp.Body.Close()
	raw, _ = io.ReadAll(resp.Body)
	var out struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(raw, &out)
	if resp.StatusCode != http.StatusCreated || out.Token == "" {
		t.Fatalf("the mint answered %d %s", resp.StatusCode, raw)
	}
	return out.Token
}
