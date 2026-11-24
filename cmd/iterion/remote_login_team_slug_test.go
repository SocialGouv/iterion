package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
)

// loginAccessToken signs in over the real server's HTTP surface and returns
// the access JWT, the credential a session holds.
func loginAccessToken(t *testing.T, base, email, password string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"email": email, "password": password})
	resp, err := http.Post(base+"/api/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var v struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.Unmarshal(raw, &v)
	if v.AccessToken == "" {
		t.Fatalf("the login answered %d %s", resp.StatusCode, raw)
	}
	return v.AccessToken
}

// TestRemoteLogin_TakesTheTeamSlugItsOwnAdviceCarries: `teams switch <slug>`
// with no browser approval wired (a caller that cannot browse passes a nil
// seam) refuses before sending anything and names the login that does pin a
// team, carrying the argument as TYPED. A
// slug is what an operator types (team ids are UUIDs nobody remembers), so
// the login has to resolve it the way `teams switch` would — the mint pins
// `team_id` verbatim, and an unresolved slug comes back 403 "not a member of
// team <slug>" on a team the account IS a member of.
func TestRemoteLogin_TakesTheTeamSlugItsOwnAdviceCarries(t *testing.T) {
	const email, password = "ops@example.test", "password-forever-9"
	base, _ := realIterionServer(t, email, password)
	ctx := context.Background()

	me, err := cli.NewRemoteClientFor(cli.RemoteConfig{BaseURL: base, Token: loginAccessToken(t, base, email, password)}).Me(ctx)
	if err != nil {
		t.Fatalf("read the account's org tree: %v", err)
	}
	var slug, teamID string
	for _, org := range me.Orgs {
		for _, team := range org.Teams {
			if slug == "" {
				slug, teamID = team.TeamSlug, team.TeamID
			}
		}
	}
	if slug == "" || teamID == "" || slug == teamID {
		t.Fatalf("the account has no team addressable by a slug distinct from its id (slug %q, id %q)", slug, teamID)
	}

	// The refusal an operator reads, and the command line it hands them.
	stored := cli.NewRemoteClientFor(cli.RemoteConfig{BaseURL: base, Token: "iap_stored_cli_token"})
	switchErr := cli.RemoteTeamsSwitch(ctx, stored, cli.NewPrinter(cli.OutputHuman), slug, "iterion-cli (host)", nil)
	if switchErr == nil {
		t.Fatal("`teams switch` from a stored personal access token reported success")
	}
	advice := "iterion remote login " + base + " --email <you> --team " + slug
	if !strings.Contains(switchErr.Error(), advice) {
		t.Fatalf("the refusal does not carry %q: %v", advice, switchErr)
	}

	// Following it: the slug resolves to the team's UUID, the mint pins it,
	// and the stored scope is the team the operator asked for.
	out, saved, lerr := runRemoteLoginWithTeam(t, ctx, base, email, "", slug)
	if lerr != nil {
		t.Fatalf("the login the refusal advises refuses the slug it carries: %v (stdout %q)", lerr, out)
	}
	if saved.TeamID != teamID {
		t.Fatalf("the login stored team %q, want the UUID %q the slug names", saved.TeamID, teamID)
	}
	if saved.Token == "" {
		t.Fatal("the login saved no token")
	}

	// A slug the account has no team for is refused by name, with nothing
	// minted: the CLI resolves against the account's own tree.
	out, saved, lerr = runRemoteLoginWithTeam(t, ctx, base, email, "", "no-such-team-slug")
	if lerr == nil {
		t.Fatalf("a login pinned to a team the account has none of succeeded: %s", out)
	}
	if !strings.Contains(lerr.Error(), "no-such-team-slug") {
		t.Fatalf("the refusal does not name the team asked for: %v", lerr)
	}
	if saved.Token != "" {
		t.Fatalf("the refused login stored %+v", saved)
	}
}
