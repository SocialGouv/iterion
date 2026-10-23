package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
)

// TestRemoteLogin_KeepsATokenTheServerDidNotReject drives `iterion remote
// login --email` against a server that mints the token, then cannot describe
// the account (GET /api/auth/me answers 500). The token is valid — the server
// just minted it — so it is saved; only a token the server REJECTS (401/403)
// is refused and not saved.
func TestRemoteLogin_KeepsATokenTheServerDidNotReject(t *testing.T) {
	for _, tc := range []struct {
		name    string
		meCode  int
		wantErr bool
	}{
		{"the server cannot describe the account", http.StatusInternalServerError, false},
		{"the server rejects the token", http.StatusUnauthorized, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/auth/login":
					_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "access-1"})
				case "/api/me/tokens":
					_ = json.NewEncoder(w).Encode(map[string]string{"token": "iap_minted"})
				case "/api/auth/me":
					w.WriteHeader(tc.meCode)
					_ = json.NewEncoder(w).Encode(map[string]string{"error": "identity unavailable"})
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			t.Setenv("HOME", t.TempDir())
			t.Setenv("ITERION_TOKEN", "")
			t.Setenv("ITERION_PASSWORD", "")
			for _, env := range []string{"ITERION_REMOTE_URL", "ITERION_REMOTE_TOKEN", "ITERION_REMOTE_TEAM", "ITERION_REMOTE_ORG"} {
				t.Setenv(env, "")
			}
			prevEmail, prevPassword, prevToken := remoteEmail, remotePassword, remoteToken
			t.Cleanup(func() { remoteEmail, remotePassword, remoteToken = prevEmail, prevPassword, prevToken })
			remoteEmail, remotePassword, remoteToken = "ops@example.test", "password-forever-9", ""
			remoteLoginCmd.SetContext(context.Background())

			var err error
			out := captureStdoutAllowingError(t, func() error {
				err = runRemoteLogin(remoteLoginCmd, []string{srv.URL})
				return err
			})
			saved, loadErr := cli.LoadRemoteConfig()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("a token the server rejected logged in: %s", out)
				}
				if loadErr == nil && saved.Token != "" {
					t.Fatalf("a rejected token was saved: %+v", saved)
				}
				return
			}
			if err != nil {
				t.Fatalf("the login whose /api/auth/me answered %d: %v", tc.meCode, err)
			}
			if loadErr != nil || saved.Token != "iap_minted" {
				t.Fatalf("the minted token was not saved: %+v (err %v)", saved, loadErr)
			}
		})
	}
}

// TestRemoteLogin_ATokenTheCommandDidNotMintNeedsA2xxIdentity drives
// `iterion remote login --token <PAT> <url>` against a URL that is not an
// iterion API: a typo'd host's 404 page, a reverse proxy with no backend.
// The probe of GET /api/auth/me is the only check that URL ever gets, and
// this command minted nothing there, so anything but a 2xx refuses — a saved
// credential would be sent, as a Bearer, to that host by every later
// `iterion remote` command.
func TestRemoteLogin_ATokenTheCommandDidNotMintNeedsA2xxIdentity(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		body string
	}{
		{"an HTML 404 — not an iterion server", http.StatusNotFound, "<html><body>Not Found</body></html>"},
		{"a 502 from a proxy with no backend", http.StatusBadGateway, "<html>502 Bad Gateway</html>"},
		{"a 500 the instance answers for an identity it cannot read", http.StatusInternalServerError, `{"error":"identity unavailable"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/auth/me" {
					t.Errorf("a --token login called %s %s: it mints nothing", r.Method, r.URL.Path)
					http.NotFound(w, r)
					return
				}
				w.WriteHeader(tc.code)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			out, saved, err := runRemoteLoginForTest(t, srv.URL, "", "iap_the_users_own_pat")
			if err == nil {
				t.Fatalf("a --token login against a URL answering %d succeeded: %s", tc.code, out)
			}
			if !strings.Contains(err.Error(), strconv.Itoa(tc.code)) {
				t.Errorf("the refusal does not name the status %d: %v", tc.code, err)
			}
			if saved.Token != "" {
				t.Errorf("the PAT was saved for a URL answering %d: every later command sends it there", tc.code)
			}
		})
	}
}

// runRemoteLoginForTest runs `remote login <url>` with the flags set, under a
// private HOME, and returns its stdout, whatever it saved and its error.
func runRemoteLoginForTest(t *testing.T, url, email, token string) (string, cli.RemoteConfig, error) {
	t.Helper()
	return runRemoteLoginWithTeam(t, context.Background(), url, email, token, "")
}

// runRemoteLoginWithTeam is runRemoteLoginForTest with --team set and the
// command's context chosen by the caller (the browser flow waits on it).
func runRemoteLoginWithTeam(t *testing.T, ctx context.Context, url, email, token, team string) (string, cli.RemoteConfig, error) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ITERION_TOKEN", "")
	t.Setenv("ITERION_PASSWORD", "")
	for _, env := range []string{"ITERION_REMOTE_URL", "ITERION_REMOTE_TOKEN", "ITERION_REMOTE_TEAM", "ITERION_REMOTE_ORG"} {
		t.Setenv(env, "")
	}
	prevEmail, prevPassword, prevToken, prevTeam := remoteEmail, remotePassword, remoteToken, remoteLoginTeam
	t.Cleanup(func() {
		remoteEmail, remotePassword, remoteToken, remoteLoginTeam = prevEmail, prevPassword, prevToken, prevTeam
	})
	remoteEmail, remotePassword, remoteToken, remoteLoginTeam = email, "password-forever-9", token, team
	remoteLoginCmd.SetContext(ctx)
	var err error
	out := captureStdoutAllowingError(t, func() error {
		err = runRemoteLogin(remoteLoginCmd, []string{url})
		return err
	})
	saved, _ := cli.LoadRemoteConfig()
	return out, saved, err
}

// captureStdoutAllowingError is captureStdout for a run whose error the
// caller judges itself.
func captureStdoutAllowingError(t *testing.T, run func() error) string {
	t.Helper()
	return captureStdout(t, func() error {
		_ = run()
		return nil
	})
}
