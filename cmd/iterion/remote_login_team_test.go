package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestRemoteLogin_TeamIsPinnedAtTheMintOrRefused: a token's team is fixed when
// it is minted, and the instance lets no token mint another, so a LOGIN is the
// CLI's only way onto another team. `--email --team <id|slug>` pins the token
// it mints (a slug resolves against the account's org tree first); the two
// paths that mint nothing of their own say so instead of storing a credential
// whose scope is not the one asked for.
func TestRemoteLogin_TeamIsPinnedAtTheMintOrRefused(t *testing.T) {
	t.Run("--email --team pins the minted token", func(t *testing.T) {
		var mintBody atomic.Value
		var mintAuth atomic.Value
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/auth/login":
				_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "access-jwt-1"})
			case "/api/me/tokens":
				raw, _ := io.ReadAll(r.Body)
				mintBody.Store(string(raw))
				mintAuth.Store(r.Header.Get("Authorization"))
				_ = json.NewEncoder(w).Encode(map[string]string{"token": "iap_minted"})
			case "/api/auth/me":
				// The org tree the --team resolution reads: this account's one
				// team answers to "t-7" by slug and by id.
				_ = json.NewEncoder(w).Encode(map[string]any{
					"user": map[string]string{"id": "u1", "email": "ops@example.test"},
					"orgs": []map[string]any{{
						"org_id": "o1",
						"teams":  []map[string]any{{"team_id": "t-7", "team_name": "Seven", "team_slug": "t-7"}},
					}},
				})
			default:
				http.NotFound(w, r)
			}
		}))
		defer srv.Close()

		out, saved, err := runRemoteLoginWithTeam(t, context.Background(), srv.URL, "ops@example.test", "", "t-7")
		if err != nil {
			t.Fatalf("the login pinned to a team: %v (%s)", err, out)
		}
		var body map[string]any
		if raw, _ := mintBody.Load().(string); raw == "" {
			t.Fatal("no token was minted")
		} else if uerr := json.Unmarshal([]byte(raw), &body); uerr != nil {
			t.Fatalf("the mint body is not JSON (%q): %v", raw, uerr)
		}
		if body["team_id"] != "t-7" {
			t.Fatalf("the mint body carries team_id %v, want t-7 (%v)", body["team_id"], body)
		}
		if got, _ := mintAuth.Load().(string); got != "Bearer access-jwt-1" {
			t.Fatalf("the mint presented %q, want the login's JWT: only a session credential may mint", got)
		}
		if saved.Token != "iap_minted" || saved.TeamID != "t-7" {
			t.Fatalf("the stored credential is %+v, want the minted token pinned to t-7", saved)
		}
	})

	t.Run("--token --team is refused, and nothing is sent or saved", func(t *testing.T) {
		var seen atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen.Add(1)
			http.NotFound(w, r)
		}))
		defer srv.Close()

		out, saved, err := runRemoteLoginWithTeam(t, context.Background(), srv.URL, "", "iap_the_users_own_pat", "t-7")
		if err == nil {
			t.Fatalf("--token --team reported success: %s", out)
		}
		msg := err.Error()
		for _, want := range []string{
			"a token's team is fixed when it is minted",
			"iterion remote login " + srv.URL + " --email <you> --team t-7",
		} {
			if !strings.Contains(msg, want) {
				t.Errorf("the refusal does not carry %q: %v", want, msg)
			}
		}
		if n := seen.Load(); n != 0 {
			t.Errorf("the refusal sent %d request(s)", n)
		}
		if saved.Token != "" {
			t.Errorf("the refusal saved %+v", saved)
		}
	})

	t.Run("the browser flow refuses --team instead of ignoring it", func(t *testing.T) {
		var seen atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen.Add(1)
			http.NotFound(w, r)
		}))
		defer srv.Close()

		// A bounded context: without the refusal this path opens a browser and
		// waits for an approval that never comes, and the test says so by
		// failing on the message rather than hanging.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		out, saved, err := runRemoteLoginWithTeam(t, ctx, srv.URL, "", "", "t-7")
		if err == nil {
			t.Fatalf("the browser login accepted --team: %s", out)
		}
		msg := err.Error()
		for _, want := range []string{
			"--team is not supported by the browser login",
			"Switch to team t-7 in the studio",
			"iterion remote login " + srv.URL + " --email <you> --team t-7",
		} {
			if !strings.Contains(msg, want) {
				t.Errorf("the refusal does not carry %q: %v", want, msg)
			}
		}
		if n := seen.Load(); n != 0 {
			t.Errorf("the refusal sent %d request(s)", n)
		}
		if saved.Token != "" {
			t.Errorf("the refusal saved %+v", saved)
		}
	})
}
