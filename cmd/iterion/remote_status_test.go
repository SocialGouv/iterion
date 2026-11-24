package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
)

// runRemoteStatus runs `iterion remote status` (with --json when asJSON)
// against the instance the environment names, and returns what it printed.
func runRemoteStatus(t *testing.T, asJSON bool) string {
	t.Helper()
	prev := jsonOutput
	t.Cleanup(func() { jsonOutput = prev })
	jsonOutput = asJSON
	remoteStatusCmd.SetContext(context.Background())
	return captureStdout(t, func() error { return remoteStatusCmd.RunE(remoteStatusCmd, nil) })
}

// decodeRemoteStatus decodes `remote status --json`.
func decodeRemoteStatus(t *testing.T, out string) cli.RemoteStatus {
	t.Helper()
	var st cli.RemoteStatus
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("remote status --json is not the status JSON: %v\n%s", err, out)
	}
	return st
}

// remoteStatusStub serves /api/auth/me and, when info is set, /api/server/info,
// and points the env-only remote config at it.
func remoteStatusStub(t *testing.T, info string) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/auth/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"user":{"email":"ops@example.test","is_super_admin":true},"active_team_id":"team1"}`))
	})
	if info != "" {
		mux.HandleFunc("GET /api/server/info", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(info))
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ITERION_REMOTE_URL", srv.URL)
	t.Setenv("ITERION_REMOTE_TOKEN", "iap_test")
	t.Setenv("ITERION_TOKEN", "")
	t.Setenv("ITERION_REMOTE_TEAM", "")
	t.Setenv("ITERION_REMOTE_ORG", "")
	t.Setenv("ITERION_REMOTE_SERVER_INFO_TIMEOUT", "")
}

// TestRemoteStatus_printsTheServerAndClientVersions: `remote status` shows
// what the instance runs and this client's version, on the terminal and in
// --json.
func TestRemoteStatus_printsTheServerAndClientVersions(t *testing.T) {
	remoteStatusStub(t, `{"mode":"cloud","version":"v7.21.0","commit":"2ecfc44cf","auth_required":true}`)

	out := runRemoteStatus(t, false)
	for _, want := range []string{
		"Account:  ops@example.test (super-admin)\n",
		"Server:   v7.21.0 (commit 2ecfc44cf)\n",
		"Client:   " + cli.RawVersion() + "\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("remote status does not print %q:\n%s", want, out)
		}
	}

	st := decodeRemoteStatus(t, runRemoteStatus(t, true))
	if st.Server == nil || st.Server.Version != "v7.21.0" || st.Server.Commit != "2ecfc44cf" {
		t.Fatalf("--json server = %+v, want version v7.21.0 commit 2ecfc44cf", st.Server)
	}
	if st.ClientVersion != cli.RawVersion() || st.ServerInfoError != "" || st.ActiveTeamID != "team1" {
		t.Fatalf("--json status = %+v", st)
	}
}

// TestRemoteStatus_saysWhyTheServerVersionIsUnknown: an instance without the
// info route leaves the version unknown with the reason, and the status
// still succeeds.
func TestRemoteStatus_saysWhyTheServerVersionIsUnknown(t *testing.T) {
	remoteStatusStub(t, "")

	out := runRemoteStatus(t, false)
	for _, want := range []string{
		"Account:  ops@example.test (super-admin)\n",
		"Server:   version unknown (HTTP 404 GET /api/server/info: 404 page not found)\n",
		"Client:   " + cli.RawVersion() + "\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("remote status does not print %q:\n%s", want, out)
		}
	}

	st := decodeRemoteStatus(t, runRemoteStatus(t, true))
	if st.Server != nil || !strings.Contains(st.ServerInfoError, "HTTP 404 GET /api/server/info") {
		t.Fatalf("--json server = %+v, server_info_error = %q; want no server and the 404", st.Server, st.ServerInfoError)
	}
}

// TestRemoteStatus_againstARealServer logs in to the real pkg/server surface
// and reads its status: the account from its /api/auth/me and the version
// from its /api/server/info, both as the server emits them.
func TestRemoteStatus_againstARealServer(t *testing.T) {
	const email, password = "ops@example.test", "password-forever-9"
	base, _ := realIterionServer(t, email, password)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ITERION_TOKEN", "")
	t.Setenv("ITERION_PASSWORD", "")
	for _, env := range []string{"ITERION_REMOTE_URL", "ITERION_REMOTE_TOKEN", "ITERION_REMOTE_TEAM", "ITERION_REMOTE_ORG", "ITERION_REMOTE_SERVER_INFO_TIMEOUT"} {
		t.Setenv(env, "")
	}
	prevEmail, prevPassword, prevToken := remoteEmail, remotePassword, remoteToken
	t.Cleanup(func() { remoteEmail, remotePassword, remoteToken = prevEmail, prevPassword, prevToken })
	remoteEmail, remotePassword, remoteToken = email, password, ""
	remoteLoginCmd.SetContext(context.Background())
	captureStdout(t, func() error { return runRemoteLogin(remoteLoginCmd, []string{base}) })

	st := decodeRemoteStatus(t, runRemoteStatus(t, true))
	if st.Instance != base || st.Email != email {
		t.Fatalf("status = %+v, want the account %s on %s", st, email, base)
	}
	want := cli.RemoteServerBuild{Version: cli.RawVersion(), Commit: cli.RawCommit()}
	if st.Server == nil || *st.Server != want {
		t.Fatalf("server = %+v (server_info_error %q), want the real server's %+v", st.Server, st.ServerInfoError, want)
	}
	if st.ClientVersion != cli.RawVersion() {
		t.Fatalf("client_version = %q, want %q", st.ClientVersion, cli.RawVersion())
	}
}
