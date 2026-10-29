package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// Every route that mints a team token refuses a personal access token (403,
// auth.MintTeamToken), and a personal access token is the only credential
// the CLI ever stores. So each first-hand client of those routes states the
// refusal itself, with the studio named — the advice a login would give is a
// dead end, since the login stores another token that mints no more.
//
// The witness drives the COMMANDS, against a server that fails the test on
// any request: "the refusal happens before anything is sent" is the claim
// docs/cloud-cli.md makes, and the only way to hold it is to send nothing —
// not even the /api/auth/me the team and org scope resolution would read.
func TestRemoteTeamTokenMints_EveryFirstHandCommandRefusesAStoredTokenBeforeSendingAnything(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("a command that mints a team token sent %s %s with a personal access token stored: the refusal must come first", r.Method, r.URL.Path)
		http.Error(w, `{"error":"a personal access token cannot mint another credential: sign in to mint one"}`, http.StatusForbidden)
	}))
	defer srv.Close()

	t.Setenv("HOME", t.TempDir())
	t.Setenv("ITERION_REMOTE_URL", srv.URL)
	t.Setenv("ITERION_REMOTE_TOKEN", "iap_stored_token")
	t.Setenv("ITERION_REMOTE_TEAM", "11111111-2222-3333-4444-555555555555")
	t.Setenv("ITERION_REMOTE_ORG", "o1")
	ctx := context.Background()

	// The data flags the create/update commands read, so the refusal is
	// reached rather than an argument error.
	remoteWebhooksData = `{"name":"hook","wildcard_bots":true}`
	remoteForgeData = `{"connection_id":"c1","repo":"group/api","bot_ids":["review-pr"]}`
	t.Cleanup(func() { remoteWebhooksData, remoteForgeData = "", "" })

	for _, tc := range []struct {
		name string
		cmd  *cobra.Command
		args []string
	}{
		{"webhooks create", remoteWebhooksCreateCmd, nil},
		{"webhooks rotate", remoteWebhooksRotateCmd, []string{"w1"}},
		{"teams invitations create", remoteTeamsInvitationsCmd, []string{"create", "invitee@example.test"}},
		{"orgs invitations create", remoteOrgsInvitationsCmd, []string{"create", "invitee@example.test"}},
		{"forge repo-bots create", remoteForgeRepoBotsCmd, []string{"create"}},
		{"forge repo-bots update", remoteForgeRepoBotsCmd, []string{"update", "i1"}},
		{"orgs approvals approve", remoteOrgsApprovalsCmd, []string{"approve", "a1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.cmd.SetContext(ctx)
			err := tc.cmd.RunE(tc.cmd, tc.args)
			if err == nil {
				t.Fatalf("`%s` was not refused with a personal access token stored", tc.name)
			}
			for _, want := range []string{"personal access token", "studio"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal of `%s` reads %q — it must carry %q", tc.name, err, want)
				}
			}
			if strings.Contains(err.Error(), "remote login") {
				t.Errorf("the refusal of `%s` advises a login, which stores another token that mints nothing: %q", tc.name, err)
			}
		})
	}
}

// TestRemoteTeamTokenMints_TheReadsAndTheNonMintsStayOpen: the refusal is on
// the MINT, not on the command group. A stored token still lists and still
// drives what mints nothing — the rejection of a parked provisioning, the
// repo-bot preview — so the guard cannot be "refuse the whole command".
func TestRemoteTeamTokenMints_TheReadsAndTheNonMintsStayOpen(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	t.Setenv("HOME", t.TempDir())
	t.Setenv("ITERION_REMOTE_URL", srv.URL)
	t.Setenv("ITERION_REMOTE_TOKEN", "iap_stored_token")
	t.Setenv("ITERION_REMOTE_TEAM", "11111111-2222-3333-4444-555555555555")
	t.Setenv("ITERION_REMOTE_ORG", "o1")
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		cmd  *cobra.Command
		args []string
	}{
		{"webhooks list", remoteWebhooksListCmd, nil},
		{"teams invitations (list)", remoteTeamsInvitationsCmd, nil},
		{"orgs approvals reject", remoteOrgsApprovalsCmd, []string{"reject", "a1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(seen)
			tc.cmd.SetContext(ctx)
			if err := tc.cmd.RunE(tc.cmd, tc.args); err != nil {
				t.Fatalf("`%s` was refused with a stored token: %v — it mints nothing", tc.name, err)
			}
			if len(seen) == before {
				t.Fatalf("`%s` sent nothing: the witness proves the wrong thing", tc.name)
			}
		})
	}
}

// TestHtmlMsg_EscapesWhatItRenders: the loopback listener of the browser
// approval serves this page on http://127.0.0.1:<port>, an origin whose
// cookies are written for every port of the loopback host. Its sentences are
// literals today; the escaping is at the sink so the day one carries a value
// the instance chose, it reads as text and does not run.
func TestHtmlMsg_EscapesWhatItRenders(t *testing.T) {
	page := htmlMsg(`<img src=x onerror="alert(1)">`)
	if strings.Contains(page, "<img") {
		t.Fatalf("htmlMsg rendered markup it was handed: %s", page)
	}
	if !strings.Contains(page, "&lt;img") {
		t.Fatalf("htmlMsg dropped what it was handed instead of escaping it: %s", page)
	}
	if !strings.Contains(htmlMsg("iterion CLI authorized ✓"), "iterion CLI authorized ✓") {
		t.Fatal("htmlMsg mangled an ordinary sentence")
	}
}
