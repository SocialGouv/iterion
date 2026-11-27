package cli_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
)

// TestRemoteTokensList_AllAsksForTheDeadTokensAndNamesEveryState: the
// default list asks for the live and disabled tokens; --all adds
// include_revoked=1, and the STATE column names disabled apart from active.
func TestRemoteTokensList_AllAsksForTheDeadTokensAndNamesEveryState(t *testing.T) {
	var queries []string
	c := remoteTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		fmt.Fprint(w, `{"tokens":[
			{"id":"p-live","name":"live","token_last4":"aaaa"},
			{"id":"p-off","name":"off","token_last4":"bbbb","disabled_at":"2026-11-01T00:00:00Z"},
			{"id":"p-dead","name":"dead","token_last4":"cccc","disabled_at":"2026-11-01T00:00:00Z","revoked_at":"2026-11-02T00:00:00Z"}]}`)
	}))
	p, buf := remotePrinter(cli.OutputHuman)
	if err := cli.RemoteTokensList(context.Background(), c, p, false); err != nil {
		t.Fatalf("list: %v", err)
	}
	if err := cli.RemoteTokensList(context.Background(), c, p, true); err != nil {
		t.Fatalf("list --all: %v", err)
	}
	if len(queries) != 2 || queries[0] != "" || queries[1] != "include_revoked=1" {
		t.Fatalf("the list sent the queries %q, want none then include_revoked=1", queries)
	}
	for _, want := range []string{"p-live", "active", "p-off", "disabled", "p-dead", "revoked"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("the list output does not name %q:\n%s", want, buf.String())
		}
	}
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, "p-dead") && strings.Contains(line, "disabled") {
			t.Errorf("a revoked token reads as disabled: %q", line)
		}
	}
}

// TestRemoteTokensRegenerate_PrintsTheNewTokenAndWhatTheOldOneEnded: the new
// plaintext is printed once, with the invitations the replaced token's revoke
// ended.
func TestRemoteTokensRegenerate_PrintsTheNewTokenAndWhatTheOldOneEnded(t *testing.T) {
	c := remoteTestSessionClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/me/tokens/p-old/regenerate" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"pat":{"id":"p-new","name":"ci"},"token":"iap_fresh","replaced":"p-old",
			"invitations_revoked":[{"id":"i1","email":"lucas@example.org","team_id":"team-a","role":"admin"}]}`)
	}))
	p, buf := remotePrinter(cli.OutputHuman)
	if err := cli.RemoteTokensRegenerate(context.Background(), c, p, "p-old"); err != nil {
		t.Fatalf("regenerate: %v", err)
	}
	for _, want := range []string{"regenerated as p-new", "iap_fresh", "Token p-old revoked", "lucas@example.org (team team-a, role admin)"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("the regenerate output does not name %q:\n%s", want, buf.String())
		}
	}
}

// TestRemoteTokensRegenerate_ARevokeThatFailedFailsTheCommandAfterTheToken:
// the replacement is minted first; when its revoke failed the plaintext is
// printed all the same, and the command fails naming the token to revoke.
func TestRemoteTokensRegenerate_ARevokeThatFailedFailsTheCommandAfterTheToken(t *testing.T) {
	c := remoteTestSessionClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"pat":{"id":"p-new","name":"ci"},"token":"iap_fresh","replaced":"p-old","invitations_revoked":[],
			"revoke_error":"revoking token p-old, which this one replaces, failed (store down): it still authenticates"}`)
	}))
	for _, format := range []cli.OutputFormat{cli.OutputHuman, cli.OutputJSON} {
		p, buf := remotePrinter(format)
		err := cli.RemoteTokensRegenerate(context.Background(), c, p, "p-old")
		if err == nil || !strings.Contains(err.Error(), "iterion remote tokens revoke p-old") {
			t.Fatalf("format %v: a regenerate whose revoke failed answered %v, want an error naming the revoke to run", format, err)
		}
		if !strings.Contains(buf.String(), "iap_fresh") {
			t.Fatalf("format %v: a regenerate whose revoke failed did not print the new token:\n%s", format, buf.String())
		}
	}
}

// TestRemoteTokensRearm_RefusesAPersonalAccessTokenBeforeSendingAnything:
// regenerate and enable put a credential in the caller's hands, which a
// personal access token may not do: the CLI says so and sends nothing.
func TestRemoteTokensRearm_RefusesAPersonalAccessTokenBeforeSendingAnything(t *testing.T) {
	c := remoteTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("sent %s %s from a personal access token", r.Method, r.URL.Path)
	}))
	p, _ := remotePrinter(cli.OutputHuman)
	for name, run := range map[string]func() error{
		"regenerate": func() error { return cli.RemoteTokensRegenerate(context.Background(), c, p, "p1") },
		"enable":     func() error { return cli.RemoteTokensEnable(context.Background(), c, p, "p1") },
	} {
		err := run()
		if err == nil || !strings.Contains(err.Error(), "cannot mint another credential") {
			t.Fatalf("%s from a personal access token: %v, want the refusal", name, err)
		}
		// A login stores a personal access token too: advising one would
		// send the operator round in a circle. The studio is the place.
		if !strings.Contains(err.Error(), "Account → Access tokens") || strings.Contains(err.Error(), "iterion remote login") {
			t.Fatalf("%s from a personal access token advises %q, want the studio and no CLI login", name, err)
		}
	}
}

// TestRemoteTokensDisableAndPurge: disable sends its route from any
// credential; purge names the invitations its sweep ended, and --output json
// carries the purged id.
func TestRemoteTokensDisableAndPurge(t *testing.T) {
	c := remoteTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/me/tokens/p1/disable":
			fmt.Fprint(w, `{"id":"p1","name":"ci","disabled_at":"2026-11-01T00:00:00Z"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/me/tokens/p1/purge":
			fmt.Fprint(w, `{"invitations_revoked":[{"id":"i1","email":"lucas@example.org","team_id":"team-a","role":"member"}]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/me/tokens/p2/purge":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	p, buf := remotePrinter(cli.OutputHuman)
	if err := cli.RemoteTokensDisable(context.Background(), c, p, "p1"); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if err := cli.RemoteTokensPurge(context.Background(), c, p, "p1"); err != nil {
		t.Fatalf("purge: %v", err)
	}
	for _, want := range []string{"Token p1 disabled", "Token p1 purged", "lucas@example.org (team team-a, role member)"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("the output does not name %q:\n%s", want, buf.String())
		}
	}
	p, buf = remotePrinter(cli.OutputJSON)
	if err := cli.RemoteTokensPurge(context.Background(), c, p, "p2"); err != nil {
		t.Fatalf("purge p2: %v", err)
	}
	if got := buf.String(); !strings.Contains(got, `"purged": "p2"`) || !strings.Contains(got, `"invitations_revoked": []`) {
		t.Errorf("the JSON of a purge = %s, want the purged id and an empty list", got)
	}
}

// TestRemoteTokensPurge_AnUnfinishedSweepNamesWhatItEnded: the instance keeps
// the row and answers 409 pat_sweep_unfinished naming what its sweep did end:
// the command prints those, then fails asking for the purge again.
func TestRemoteTokensPurge_AnUnfinishedSweepNamesWhatItEnded(t *testing.T) {
	c := remoteTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		fmt.Fprint(w, `{"error":"token p1 is kept: revoking invitation i2 failed","error_code":"pat_sweep_unfinished",
			"invitations_revoked":[{"id":"i1","email":"lucas@example.org","team_id":"team-a","role":"member"}],
			"invitations_error":"revoking invitation i2 failed"}`)
	}))
	p, buf := remotePrinter(cli.OutputHuman)
	err := cli.RemoteTokensPurge(context.Background(), c, p, "p1")
	if err == nil || !strings.Contains(err.Error(), "tokens purge p1") {
		t.Fatalf("purge with an unfinished sweep: %v, want an error asking for the purge again", err)
	}
	for _, want := range []string{"lucas@example.org (team team-a, role member)", "revoking invitation i2 failed"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("the purge output does not name %q:\n%s", want, buf.String())
		}
	}
}
