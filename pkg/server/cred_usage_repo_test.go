package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/credusage"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

func seedRepoSpend(t *testing.T, c credusage.Counter, when time.Time, fp, tenant, repo string, cost float64) {
	t.Helper()
	if err := c.AddSpend(context.Background(), when, credusage.Spend{
		Key: credusage.Key{
			Fingerprint: fp, Provider: "anthropic", Tier: credusage.TierTeam,
			TenantID: tenant, RepoID: repo,
		},
		Nature:  credusage.NatureMetered,
		Backend: "claw",
		CostUSD: cost, InputTokens: 100, OutputTokens: 10,
	}); err != nil {
		t.Fatalf("seed %s/%s: %v", fp, repo, err)
	}
}

// #950 — the repository is an accounting dimension, which is only useful if
// asking for one repository's spend answers about THAT repository. Two ways
// to get it wrong, both silent, both pinned here:
//
//   - the team route forwarding a cross-tenant listing, so a team reads
//     another team's spend on a repo they happen to share;
//   - the listings that predate the dimension reporting only the
//     unattributed rows, so every figure drops the day runs start naming a
//     repo.
func TestCredentialUsage_RepoDimension(t *testing.T) {
	const repo = "SocialGouv/iterion"
	newSrv := func(t *testing.T) (*Server, credusage.Counter, time.Time) {
		t.Helper()
		counter := credusage.NewMemoryCounter()
		return &Server{credUsage: counter, logger: iterlog.Nop()}, counter, time.Now().UTC()
	}
	teamReq := func(teamID, query string) *http.Request {
		r := httptest.NewRequest("GET", "/api/teams/"+teamID+"/credentials/usage"+query, nil)
		r.SetPathValue("id", teamID)
		return r.WithContext(store.WithTenant(auth.WithIdentity(r.Context(),
			auth.Identity{UserID: "u1", IsSuperAdmin: true, TeamID: teamID}), teamID))
	}
	decode := func(t *testing.T, w *httptest.ResponseRecorder) credentialUsageListView {
		t.Helper()
		if w.Code != http.StatusOK {
			t.Fatalf("usage: %d %s", w.Code, w.Body.String())
		}
		var body credentialUsageListView
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v (%s)", err, w.Body.String())
		}
		return body
	}

	t.Run("a team's repo view never carries another team's spend", func(t *testing.T) {
		srv, counter, now := newSrv(t)
		seedRepoSpend(t, counter, now, "fp-a", "team-a", repo, 2.0)
		// The SAME repository, served by another team's credential. The
		// counter answers across tenants on purpose; the team route must not.
		seedRepoSpend(t, counter, now, "fp-b", "team-b", repo, 99.0)

		w := httptest.NewRecorder()
		srv.handleTeamCredentialUsage(w, teamReq("team-a", "?repo="+repo))
		body := decode(t, w)
		if len(body.Credentials) != 1 {
			t.Fatalf("credentials = %+v, want only team-a's row", body.Credentials)
		}
		if body.Credentials[0].Fingerprint != "fp-a" {
			t.Fatalf("row fingerprint = %q, want fp-a", body.Credentials[0].Fingerprint)
		}
		if body.MeteredUSD != 2.0 {
			t.Fatalf("metered = $%.2f, want $2.00 — $101.00 would mean team-b's spend leaked", body.MeteredUSD)
		}
		if body.Credentials[0].RepoID != repo {
			t.Fatalf("repo_id = %q, want %q — a ?repo= answer names the repo it is about", body.Credentials[0].RepoID, repo)
		}
	})

	t.Run("the unfiltered view sums the repositories back in", func(t *testing.T) {
		srv, counter, now := newSrv(t)
		seedRepoSpend(t, counter, now, "fp-a", "team-a", repo, 2.0)
		seedRepoSpend(t, counter, now, "fp-a", "team-a", "SocialGouv/other", 1.0)
		seedRepoSpend(t, counter, now, "fp-a", "team-a", "", 0.5) // a run that named none

		w := httptest.NewRecorder()
		srv.handleTeamCredentialUsage(w, teamReq("team-a", ""))
		body := decode(t, w)
		if len(body.Credentials) != 1 {
			t.Fatalf("credentials = %+v, want ONE row — repositories are summed into their credential", body.Credentials)
		}
		if body.MeteredUSD != 3.5 {
			t.Fatalf("metered = $%.2f, want $3.50 — an unfiltered view that reported only the unattributed row would say $0.50", body.MeteredUSD)
		}
		if body.Credentials[0].RepoID != "" {
			t.Fatalf("a summed row named repo %q; it is the total of three", body.Credentials[0].RepoID)
		}
	})

	t.Run("an unknown repo is an empty bill, not everyone's", func(t *testing.T) {
		srv, counter, now := newSrv(t)
		seedRepoSpend(t, counter, now, "fp-a", "team-a", repo, 2.0)
		seedRepoSpend(t, counter, now, "fp-a", "team-a", "", 7.0)

		w := httptest.NewRecorder()
		srv.handleTeamCredentialUsage(w, teamReq("team-a", "?repo=SocialGouv/never-ran"))
		body := decode(t, w)
		if len(body.Credentials) != 0 || body.MeteredUSD != 0 {
			t.Fatalf("unknown repo = %+v ($%.2f), want an empty bill", body.Credentials, body.MeteredUSD)
		}
	})

	t.Run("admin refuses ?fingerprint= and ?repo= together", func(t *testing.T) {
		srv, counter, now := newSrv(t)
		seedRepoSpend(t, counter, now, "fp-a", "team-a", repo, 2.0)

		r := httptest.NewRequest("GET", "/api/admin/credentials/usage?fingerprint=fp-a&repo="+repo, nil)
		r = r.WithContext(auth.WithIdentity(r.Context(), auth.Identity{UserID: "root", IsSuperAdmin: true}))
		w := httptest.NewRecorder()
		srv.handleAdminCredentialUsage(w, r)
		// Answering one of the two would return a number that is not the one
		// asked for — the failure `?tier=org` had on this very endpoint.
		if w.Code != http.StatusBadRequest {
			t.Fatalf("fingerprint+repo = %d %s, want 400", w.Code, w.Body.String())
		}
	})

	t.Run("admin ?repo= spans the tenants that served it", func(t *testing.T) {
		srv, counter, now := newSrv(t)
		seedRepoSpend(t, counter, now, "fp-a", "team-a", repo, 2.0)
		seedRepoSpend(t, counter, now, "fp-b", "team-b", repo, 3.0)

		r := httptest.NewRequest("GET", "/api/admin/credentials/usage?repo="+repo, nil)
		r = r.WithContext(auth.WithIdentity(r.Context(), auth.Identity{UserID: "root", IsSuperAdmin: true}))
		w := httptest.NewRecorder()
		srv.handleAdminCredentialUsage(w, r)
		body := decode(t, w)
		if len(body.Credentials) != 2 || body.MeteredUSD != 5.0 {
			t.Fatalf("admin repo view = %+v ($%.2f), want both tenants' rows totalling $5.00", body.Credentials, body.MeteredUSD)
		}
	})
}
