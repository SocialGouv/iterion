package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// A route that ALWAYS mints a team token refuses a personal access token
// (403, auth.MintTeamToken), and a personal access token is the only
// credential the CLI ever stores. So each first-hand client of those routes
// states the refusal itself, with the studio named — the advice a login would
// give is a dead end, since the login stores another token that mints no more.
//
// The witness drives the COMMANDS, against a server that fails the test on
// any request: "the refusal happens before anything is sent" is the claim
// docs/cloud-cli.md makes, and the only way to hold it is to send nothing —
// not even the /api/auth/me the team and org scope resolution would read.
//
// The repo-bot provisioning is NOT in this list and must not be: whether it
// mints depends on state only the instance holds, so it sends and the
// instance answers (the test below).
func TestRemoteTeamTokenMints_EveryAlwaysMintingCommandRefusesAStoredTokenBeforeSendingAnything(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("a command whose route always mints a team token sent %s %s with a personal access token stored: the refusal must come first", r.Method, r.URL.Path)
		http.Error(w, `{"error":"a personal access token cannot mint another credential: sign in to mint one"}`, http.StatusForbidden)
	}))
	defer srv.Close()

	t.Setenv("HOME", t.TempDir())
	t.Setenv("ITERION_REMOTE_URL", srv.URL)
	t.Setenv("ITERION_REMOTE_TOKEN", "iap_stored_token")
	t.Setenv("ITERION_REMOTE_TEAM", "11111111-2222-3333-4444-555555555555")
	t.Setenv("ITERION_REMOTE_ORG", "o1")
	ctx := context.Background()

	// The data flag the create command reads, so the refusal is reached
	// rather than an argument error.
	remoteWebhooksData = `{"name":"hook","wildcard_bots":true}`
	t.Cleanup(func() { remoteWebhooksData = "" })

	for _, tc := range []struct {
		name string
		cmd  *cobra.Command
		args []string
	}{
		{"webhooks create", remoteWebhooksCreateCmd, nil},
		{"webhooks rotate", remoteWebhooksRotateCmd, []string{"w1"}},
		{"teams invitations create", remoteTeamsInvitationsCmd, []string{"create", "invitee@example.test"}},
		{"orgs invitations create", remoteOrgsInvitationsCmd, []string{"create", "invitee@example.test"}},
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

// TestRemoteTeamTokenMints_AServerRefusalCarriesTheAdviceWhereverItLands: the
// commands whose route mints only SOMETIMES send, and a refusal the instance
// does state must still tell the operator where the deed can be done. The
// advice is rendered at the ONE place a non-2xx becomes an error
// (RemoteClient.refuseAPI), so it reaches the typed commands, the --data
// senders AND the generic `remote api` passthrough that docs/auto-maintenance
// prescribes — which is the path a guard recopied per command always misses.
//
// It also covers the credential the local pre-flight cannot recognise: the
// token here has no `iap_` prefix, exactly the case asPATMintRefusal exists
// for (an access token pasted into ITERION_REMOTE_TOKEN).
func TestRemoteTeamTokenMints_AServerRefusalCarriesTheAdviceWhereverItLands(t *testing.T) {
	var sent []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent = append(sent, r.Method+" "+r.URL.Path)
		if strings.Contains(r.URL.Path, "/auth/me") {
			_, _ = w.Write([]byte(`{"user":{"id":"u1","email":"a@example.test"},"active_team_id":"11111111-2222-3333-4444-555555555555","active_org_id":"o1"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"forge: the hook token this provisioning mints was refused by the derivation chokepoint: a personal access token cannot mint another credential: sign in to mint one"}`))
	}))
	defer srv.Close()

	t.Setenv("HOME", t.TempDir())
	t.Setenv("ITERION_REMOTE_URL", srv.URL)
	t.Setenv("ITERION_REMOTE_TOKEN", "an-access-token-with-no-iap-prefix")
	t.Setenv("ITERION_REMOTE_TEAM", "11111111-2222-3333-4444-555555555555")
	t.Setenv("ITERION_REMOTE_ORG", "o1")
	ctx := context.Background()

	// Through the FLAG, not the variable: the command reads
	// Flags().Changed("data") before the body, so a value set behind the
	// flag's back would answer "--data is required" and send nothing.
	if err := remoteForgeRepoBotsCmd.Flags().Set("data", `{"connection_id":"c1","repo":"group/api","bot_ids":["review-pr"]}`); err != nil {
		t.Fatal(err)
	}
	remoteAPIData = `{"bot_ids":["review-pr"]}`
	t.Cleanup(func() {
		remoteAPIData = ""
		_ = remoteForgeRepoBotsCmd.Flags().Set("data", "")
		remoteForgeRepoBotsCmd.Flags().Lookup("data").Changed = false
	})

	for _, tc := range []struct {
		name string
		cmd  *cobra.Command
		args []string
	}{
		{"forge repo-bots create", remoteForgeRepoBotsCmd, []string{"create"}},
		{"forge repo-bots update", remoteForgeRepoBotsCmd, []string{"update", "i1"}},
		{"orgs approvals approve", remoteOrgsApprovalsCmd, []string{"approve", "a1"}},
		{"api PATCH repo-bots", remoteAPICmd, []string{"PATCH", "/api/teams/t1/forge/repo-bots/i1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(sent)
			tc.cmd.SetContext(ctx)
			err := tc.cmd.RunE(tc.cmd, tc.args)
			if err == nil {
				t.Fatalf("`%s` reported success on a 403", tc.name)
			}
			if len(sent) == before {
				t.Fatalf("`%s` sent nothing: only the instance knows whether the request mints", tc.name)
			}
			for _, want := range []string{"cannot mint another credential", "studio"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal of `%s` reads %q — it must carry %q", tc.name, err, want)
				}
			}
		})
	}
}

// TestRemoteTeamTokenMints_TheReadsAndTheNonMintsStayOpen: the refusal is on
// the MINT, not on the command group. A stored token still lists and still
// drives what mints nothing — the rejection of a parked provisioning, the
// repo-bot preview, an invitation's deletion — so the guard cannot be
// "refuse the whole command".
//
// The provisioning verbs are here too, with an `iap_` token stored: whether
// they mint is the instance's answer, and a local pre-flight on them refuses
// the reconcile and the settings patch that mint nothing.
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

	// preview reads its target from flags, not from a body.
	remoteForgeRepoBotsConnectionID, remoteForgeRepoBotsBots = "c1", "review-pr"
	t.Cleanup(func() { remoteForgeRepoBotsConnectionID, remoteForgeRepoBotsBots = "", "" })

	for _, tc := range []struct {
		name string
		cmd  *cobra.Command
		args []string
		// data marks the verbs that read the --data flag: it is set for
		// those alone, since the others refuse a body they have no use for.
		data bool
	}{
		{name: "webhooks list", cmd: remoteWebhooksListCmd},
		{name: "teams invitations (list)", cmd: remoteTeamsInvitationsCmd},
		{name: "teams invitations delete", cmd: remoteTeamsInvitationsCmd, args: []string{"delete", "i1"}},
		{name: "orgs approvals reject", cmd: remoteOrgsApprovalsCmd, args: []string{"reject", "a1"}},
		{name: "forge repo-bots (list)", cmd: remoteForgeRepoBotsCmd},
		{name: "forge repo-bots preview", cmd: remoteForgeRepoBotsCmd, args: []string{"preview"}},
		{name: "forge repo-bots delete", cmd: remoteForgeRepoBotsCmd, args: []string{"delete", "i1"}},
		{name: "forge repo-bots create (only the instance knows if it mints)", cmd: remoteForgeRepoBotsCmd, args: []string{"create"}, data: true},
		{name: "forge repo-bots update (the reconcile mints nothing)", cmd: remoteForgeRepoBotsCmd, args: []string{"update", "i1"}, data: true},
		{name: "orgs approvals approve (an update approval mints nothing)", cmd: remoteOrgsApprovalsCmd, args: []string{"approve", "a1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.data {
				if err := remoteForgeRepoBotsCmd.Flags().Set("data", `{"connection_id":"c1","repo":"group/api","bot_ids":["review-pr"]}`); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					_ = remoteForgeRepoBotsCmd.Flags().Set("data", "")
					remoteForgeRepoBotsCmd.Flags().Lookup("data").Changed = false
				})
			}
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

// TestRemoteForgeRepoBots_AVerbWithoutItsIdAnswersTheUsage: `repo-bots
// update` with no integration id is a usage mistake, and the command says
// so. It used to be read as a --data verb — the shape (verb, id) decides,
// not args[0] alone — and answered about a flag the operator never passed.
func TestRemoteForgeRepoBots_AVerbWithoutItsIdAnswersTheUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("a usage mistake sent %s %s", r.Method, r.URL.Path)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ITERION_REMOTE_URL", srv.URL)
	t.Setenv("ITERION_REMOTE_TOKEN", "iap_stored_token")
	t.Setenv("ITERION_REMOTE_TEAM", "11111111-2222-3333-4444-555555555555")
	remoteForgeRepoBotsCmd.SetContext(context.Background())
	err := remoteForgeRepoBotsCmd.RunE(remoteForgeRepoBotsCmd, []string{"update"})
	if err == nil {
		t.Fatal("`repo-bots update` with no id reported success")
	}
	if !strings.Contains(err.Error(), "usage:") {
		t.Errorf("the refusal reads %q — it must state the usage", err)
	}
}
