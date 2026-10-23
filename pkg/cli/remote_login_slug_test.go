package cli_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
)

// TestLoginWithPassword_ResolvesTheTeamSlugUnderTheLoginsJWT: `--team` takes
// what an operator types, and what the CLI's own advice prints — a slug. The
// mint pins `team_id` VERBATIM, so an unresolved slug mints nothing and the
// instance answers 403 "not a member of team <slug>" for a team the account
// belongs to. The resolution reads the org tree under the LOGIN's JWT: it
// runs before any credential is stored, and the stored one may not even
// exist yet.
func TestLoginWithPassword_ResolvesTheTeamSlugUnderTheLoginsJWT(t *testing.T) {
	const uuid = "87a2ee99-6b91-43ca-bb51-659a8ce7f705"
	var mintedWith, meBearer atomic.Value
	var mints atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "access-jwt-1"})
	})
	mux.HandleFunc("GET /api/auth/me", func(w http.ResponseWriter, r *http.Request) {
		meBearer.Store(r.Header.Get("Authorization"))
		fmt.Fprintf(w, `{"user":{"id":"u1"},"orgs":[{"org_id":"o1","teams":[{"team_id":%q,"team_name":"PIC GRAAL","team_slug":"pic-graal"}]}]}`, uuid)
	})
	mux.HandleFunc("POST /api/me/tokens", func(w http.ResponseWriter, r *http.Request) {
		mints.Add(1)
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		team, _ := req["team_id"].(string)
		mintedWith.Store(team)
		if team != uuid {
			// The instance's own answer to a team_id that resolves to no
			// membership, which is what a raw slug is.
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprintf(w, `{"error":"not a member of team %q"}`, team)
			return
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"pat":{"id":"p-1"},"token":"iap_minted"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := cli.NewRemoteClientFor(cli.RemoteConfig{BaseURL: srv.URL})
	tok, pinned, err := c.LoginWithPassword(context.Background(), "ops@example.test", "password-forever-9", "cli", "pic-graal")
	if err != nil {
		t.Fatalf("a login pinned to a team by slug: %v", err)
	}
	if tok != "iap_minted" {
		t.Fatalf("the login returned %q, want the minted token", tok)
	}
	if got, _ := mintedWith.Load().(string); got != uuid {
		t.Fatalf("the mint received team_id %q, want the UUID the slug resolves to", got)
	}
	if pinned != uuid {
		t.Fatalf("the login reports the token pinned to %q, want the UUID: the caller persists this as the CLI's scope", pinned)
	}
	if got, _ := meBearer.Load().(string); got != "Bearer access-jwt-1" {
		t.Fatalf("the org tree was read with %q, want the login's own JWT: nothing is stored yet", got)
	}

	// A UUID is passed through untouched — no read, the server is the
	// authority on a team the account does not belong to.
	meBearer.Store("")
	if _, _, err := c.LoginWithPassword(context.Background(), "ops@example.test", "password-forever-9", "cli", uuid); err != nil {
		t.Fatalf("a login pinned to a team by UUID: %v", err)
	}
	if got, _ := meBearer.Load().(string); got != "" {
		t.Fatalf("a UUID was resolved against the org tree (%q): the mint pins it verbatim", got)
	}

	// A slug the account has no team for: named, and nothing minted.
	before := mints.Load()
	_, _, err = c.LoginWithPassword(context.Background(), "ops@example.test", "password-forever-9", "cli", "not-my-team")
	if err == nil {
		t.Fatal("a login pinned to a team the account has none of succeeded")
	}
	if !strings.Contains(err.Error(), "not-my-team") {
		t.Fatalf("the refusal does not name the team asked for: %v", err)
	}
	if n := mints.Load(); n != before {
		t.Fatalf("the refusal minted %d token(s): a slug that resolves to nothing must send no mint", n-before)
	}
}
