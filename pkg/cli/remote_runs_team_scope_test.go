package cli_test

// Witnesses for r7c F8: the run reads' --team flag. The server reads
// ?team_id= as an id verbatim (the r7c probe: a slug answers a member and an
// org admin 403 "not authorised to scope this route to team_id=<slug>", and
// a super-admin an empty scope), so the CLI resolves the flag through the
// resolver the login pins its mint with: a UUID passes through untouched, a
// slug resolves against the account's org tree, and a slug that resolves to
// nothing is refused, before anything is sent.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
)

// newRunScopeFake mounts /api/auth/me (one team: UUID pic-graal) and a run
// read recording the team_id its query carries, refusing a raw slug the way
// the instance refuses an id that resolves to no membership.
func newRunScopeFake(t *testing.T) (*httptest.Server, *atomic.Value, *atomic.Int64) {
	t.Helper()
	const uuid = "87a2ee99-6b91-43ca-bb51-659a8ce7f705"
	var listedTeam atomic.Value
	var lists atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/auth/me", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"user":{"id":"u1"},"orgs":[{"org_id":"o1","teams":[{"team_id":%q,"team_name":"PIC GRAAL","team_slug":"pic-graal"}]}]}`, uuid)
	})
	mux.HandleFunc("GET /api/runs", func(w http.ResponseWriter, r *http.Request) {
		lists.Add(1)
		team := r.URL.Query().Get("team_id")
		listedTeam.Store(team)
		if team != "" && team != uuid {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprintf(w, `{"error":"not authorised to scope this route to team_id=%s"}`, team)
			return
		}
		fmt.Fprint(w, `{"runs":[]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &listedTeam, &lists
}

// TestRemoteRunsListResolvesTheTeamSlugBeforeItIsSent drives `runs list
// --team` and `resume --usage-blocked --team` end to end against the fake:
// the flag arrives at the server as the UUID it names.
func TestRemoteRunsListResolvesTheTeamSlugBeforeItIsSent(t *testing.T) {
	const uuid = "87a2ee99-6b91-43ca-bb51-659a8ce7f705"
	srv, listedTeam, lists := newRunScopeFake(t)
	c := cli.NewRemoteClientFor(cli.RemoteConfig{BaseURL: srv.URL})
	ctx := context.Background()
	p := &cli.Printer{W: io.Discard}

	t.Run("runs list", func(t *testing.T) {
		err := cli.RemoteRunsList(ctx, c, p, cli.RemoteRunsListOptions{Team: "pic-graal"})
		if err != nil {
			t.Fatalf("runs list --team <slug>: %v", err)
		}
		got, _ := listedTeam.Load().(string)
		if got != uuid {
			t.Fatalf("the list sent ?team_id=%q, want the UUID the slug resolves to: the server answers a raw slug 403", got)
		}
	})

	t.Run("resume --usage-blocked", func(t *testing.T) {
		err := cli.RemoteRunsResumeUsageBlocked(ctx, c, p, cli.RemoteRunsListOptions{Team: "pic-graal"})
		if err != nil {
			t.Fatalf("resume --usage-blocked --team <slug>: %v", err)
		}
		got, _ := listedTeam.Load().(string)
		if got != uuid {
			t.Fatalf("the sweep sent ?team_id=%q, want the UUID the slug resolves to", got)
		}
	})

	t.Run("a UUID is sent untouched", func(t *testing.T) {
		err := cli.RemoteRunsList(ctx, c, p, cli.RemoteRunsListOptions{Team: uuid})
		if err != nil {
			t.Fatalf("runs list --team <uuid>: %v", err)
		}
		got, _ := listedTeam.Load().(string)
		if got != uuid {
			t.Fatalf("the list sent ?team_id=%q, want the UUID verbatim", got)
		}
	})

	t.Run("a slug that resolves to nothing is refused before it is sent", func(t *testing.T) {
		before := lists.Load()
		err := cli.RemoteRunsList(ctx, c, p, cli.RemoteRunsListOptions{Team: "not-my-team"})
		if err == nil {
			t.Fatal("a list scoped to a team the account has none of succeeded")
		}
		if !strings.Contains(err.Error(), "not-my-team") {
			t.Fatalf("the refusal does not name the team asked for: %v", err)
		}
		n := lists.Load()
		if n != before {
			t.Fatalf("the refusal sent %d list request(s): a slug that resolves to nothing must send nothing", n-before)
		}
	})
}

// TestRemoteRunScopeTeamResolverCoversTheStatsAndReposFlags drives the
// resolver the stats and repos aggregations build their ?team_id= with: the
// flag resolves like the mint's — a UUID through with no read, a slug
// against the org tree, an unknown slug refused with its name.
func TestRemoteRunScopeTeamResolverCoversTheStatsAndReposFlags(t *testing.T) {
	const uuid = "87a2ee99-6b91-43ca-bb51-659a8ce7f705"
	var meReads atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/auth/me", func(w http.ResponseWriter, r *http.Request) {
		meReads.Add(1)
		fmt.Fprintf(w, `{"user":{"id":"u1"},"orgs":[{"org_id":"o1","teams":[{"team_id":%q,"team_name":"PIC GRAAL","team_slug":"pic-graal"}]}]}`, uuid)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := cli.NewRemoteClientFor(cli.RemoteConfig{BaseURL: srv.URL})
	ctx := context.Background()

	resolved, err := cli.ResolveRemoteRunScopeTeam(ctx, c, "pic-graal")
	if err != nil {
		t.Fatalf("resolving a --team slug: %v", err)
	}
	if resolved != uuid {
		t.Fatalf("the slug resolved to %q, want the UUID", resolved)
	}
	resolved, err = cli.ResolveRemoteRunScopeTeam(ctx, c, uuid)
	if err != nil || resolved != uuid {
		t.Fatalf("a UUID resolved to %q, %v; want it untouched", resolved, err)
	}
	if n := meReads.Load(); n != 1 {
		t.Fatalf("the org tree was read %d time(s), want once: a UUID is passed through without a read", n)
	}
	if _, err := cli.ResolveRemoteRunScopeTeam(ctx, c, "not-my-team"); err == nil || !strings.Contains(err.Error(), "not-my-team") {
		t.Fatalf("an unknown slug: %v, want a refusal naming it", err)
	}
	resolved, err = cli.ResolveRemoteRunScopeTeam(ctx, c, "   ")
	if err != nil || resolved != "" {
		t.Fatalf("an empty flag resolved to %q, %v; want no scope", resolved, err)
	}
}
