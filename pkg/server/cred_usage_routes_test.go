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

func seedCredUsage(t *testing.T, c credusage.Counter, when time.Time, fp, provider string, tier credusage.Tier, tenant string, nature credusage.Nature, backend string, cost float64) {
	t.Helper()
	if err := c.AddSpend(context.Background(), when, credusage.Spend{
		Key:     credusage.Key{Fingerprint: fp, Provider: provider, Tier: tier, TenantID: tenant},
		Nature:  nature,
		Backend: backend,
		CostUSD: cost, InputTokens: 100, OutputTokens: 10,
	}); err != nil {
		t.Fatalf("seed %s: %v", fp, err)
	}
}

// #641 — the API states the NATURE of every amount. A subscription bills
// nothing per call, so its figure is what the calls WOULD have cost
// metered; a key's is a charge on an invoice. A client reading only
// cost_usd would sum the two, which is exactly the misreading the counter
// exists to remove — so the two totals come back apart, too.
func TestTeamCredentialUsage_TypesEveryAmount(t *testing.T) {
	counter := credusage.NewMemoryCounter()
	now := time.Now().UTC()
	seedCredUsage(t, counter, now, "fp-key", "anthropic", credusage.TierTeam, "team-a", credusage.NatureMetered, "claw", 3.0)
	seedCredUsage(t, counter, now, "fp-forfait", "claude_code", credusage.TierTeam, "team-a", credusage.NatureEstimate, "claude_code", 11.0)
	seedCredUsage(t, counter, now, "fp-other", "anthropic", credusage.TierTeam, "team-b", credusage.NatureMetered, "claw", 99.0)

	srv := &Server{credUsage: counter, logger: iterlog.Nop()}
	r := httptest.NewRequest("GET", "/api/teams/team-a/credentials/usage", nil)
	r.SetPathValue("id", "team-a")
	r = r.WithContext(store.WithTenant(auth.WithIdentity(r.Context(),
		auth.Identity{UserID: "u1", IsSuperAdmin: true, TeamID: "team-a"}), "team-a"))
	w := httptest.NewRecorder()
	srv.handleTeamCredentialUsage(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("usage: %d %s", w.Code, w.Body.String())
	}
	var body credentialUsageListView
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if len(body.Credentials) != 2 {
		t.Fatalf("credentials = %+v, want the team's two (another team's must not leak)", body.Credentials)
	}
	byFP := map[string]credentialUsageView{}
	for _, c := range body.Credentials {
		byFP[c.Fingerprint] = c
	}
	if byFP["fp-key"].Nature != string(credusage.NatureMetered) {
		t.Fatalf("api key nature = %q, want metered", byFP["fp-key"].Nature)
	}
	if byFP["fp-forfait"].Nature != string(credusage.NatureEstimate) {
		t.Fatalf("forfait nature = %q, want estimate", byFP["fp-forfait"].Nature)
	}
	if body.MeteredUSD != 3.0 || body.EstimatedUSD != 11.0 {
		t.Fatalf("totals = metered $%.2f / estimated $%.2f, want $3.00 / $11.00 kept apart",
			body.MeteredUSD, body.EstimatedUSD)
	}
}

// The platform tier's rows live under the TENANTS it served, so the admin
// view asks by tier — no tenant holds its month.
func TestAdminCredentialUsage_PlatformTierAcrossTenants(t *testing.T) {
	counter := credusage.NewMemoryCounter()
	now := time.Now().UTC()
	seedCredUsage(t, counter, now, "fp-plat", "openai", credusage.TierPlatform, "team-a", credusage.NatureMetered, "codex", 2.0)
	seedCredUsage(t, counter, now, "fp-plat", "openai", credusage.TierPlatform, "team-b", credusage.NatureMetered, "codex", 4.0)
	seedCredUsage(t, counter, now, "fp-team", "anthropic", credusage.TierTeam, "team-a", credusage.NatureMetered, "claw", 7.0)

	srv := &Server{credUsage: counter, logger: iterlog.Nop()}
	get := func(query string) credentialUsageListView {
		t.Helper()
		r := httptest.NewRequest("GET", "/api/admin/credentials/usage"+query, nil)
		r = r.WithContext(auth.WithIdentity(r.Context(), auth.Identity{UserID: "root", IsSuperAdmin: true}))
		w := httptest.NewRecorder()
		srv.handleAdminCredentialUsage(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("admin usage%s: %d %s", query, w.Code, w.Body.String())
		}
		var body credentialUsageListView
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return body
	}
	plat := get("")
	if len(plat.Credentials) != 2 || plat.MeteredUSD != 6.0 {
		t.Fatalf("platform tier = %+v ($%.2f), want both tenants' slices summing to $6.00", plat.Credentials, plat.MeteredUSD)
	}
	// One credential across tenants — "what did this key cost", full stop.
	fp := get("?fingerprint=fp-plat")
	if len(fp.Credentials) != 2 || fp.MeteredUSD != 6.0 {
		t.Fatalf("by fingerprint = %+v ($%.2f), want $6.00", fp.Credentials, fp.MeteredUSD)
	}
	if team := get("?tier=team"); len(team.Credentials) != 1 || team.Credentials[0].Fingerprint != "fp-team" {
		t.Fatalf("?tier=team = %+v, want the one team row", team.Credentials)
	}
}

// A deployment with no counter says so rather than reporting zero: an empty
// ledger and an absent feature are different answers.
func TestCredentialUsage_NotEnabled(t *testing.T) {
	srv := &Server{logger: iterlog.Nop()}
	r := httptest.NewRequest("GET", "/api/teams/team-a/credentials/usage", nil)
	r.SetPathValue("id", "team-a")
	r = r.WithContext(auth.WithIdentity(r.Context(), auth.Identity{UserID: "u1", IsSuperAdmin: true, TeamID: "team-a"}))
	w := httptest.NewRecorder()
	srv.handleTeamCredentialUsage(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (not a zeroed view)", w.Code)
	}
}

// #1087 — the reading that opened a defect against a healthy counter. The
// admin listing filters on the PLATFORM tier when the caller names none, so
// a team forfait's spend is absent from it by design; nothing in the answer
// said so, and two identical reads around a run that spent $2.40 on that
// team forfait read as a meter that had stopped recording.
//
// The fix is not a different default — it is an answer that states its own
// question.
func TestAdminCredentialUsage_SaysWhichTierItAnswered(t *testing.T) {
	counter := credusage.NewMemoryCounter()
	now := time.Now().UTC()
	seedCredUsage(t, counter, now, "fp-plat", "openai", credusage.TierPlatform, "team-a", credusage.NatureMetered, "codex", 2.0)
	seedCredUsage(t, counter, now, "fp-team", "claude_code", credusage.TierTeam, "team-a", credusage.NatureEstimate, "claude_code", 2.40)

	srv := &Server{credUsage: counter, logger: iterlog.Nop()}
	body := adminCredUsage(t, srv, "")
	if body.Scope.Tier != string(credusage.TierPlatform) {
		t.Fatalf("scope.tier = %q on an unfiltered listing, want the platform default named — a listing that hides its filter reads as 'everything', and a credential missing from it reads as one that spent nothing",
			body.Scope.Tier)
	}
	// The row the probe was looking for, and the reason it was not there.
	for _, c := range body.Credentials {
		if c.Fingerprint == "fp-team" {
			t.Fatalf("the team row leaked into the platform listing: %+v", c)
		}
	}
	if team := adminCredUsage(t, srv, "?tier=team"); team.Scope.Tier != string(credusage.TierTeam) {
		t.Fatalf("scope.tier = %q for ?tier=team", team.Scope.Tier)
	}
	if fp := adminCredUsage(t, srv, "?fingerprint=fp-team"); fp.Scope.Fingerprint != "fp-team" || fp.Scope.Tier != "" {
		t.Fatalf("scope = %+v for ?fingerprint=, want the fingerprint named and no tier claimed", fp.Scope)
	}
}

// A month the caller did not ask for, wearing the label of the month they
// did, is the shape of #1087's probe: `?month=2026-08` was served September
// byte for byte, and the numbers being identical is what read as "frozen".
func TestCredentialUsage_AnswersTheMonthAsked(t *testing.T) {
	counter := credusage.NewMemoryCounter()
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	past := time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC)
	seedCredUsage(t, counter, now, "fp-sep", "anthropic", credusage.TierTeam, "team-a", credusage.NatureMetered, "claw", 5.0)
	seedCredUsage(t, counter, past, "fp-aug", "anthropic", credusage.TierTeam, "team-a", credusage.NatureMetered, "claw", 3.0)

	srv := &Server{credUsage: counter, logger: iterlog.Nop()}
	aug := teamCredUsage(t, srv, "team-a", "?month=2026-08")
	if aug.Month != "2026-08" {
		t.Fatalf("month = %q, want the month asked for", aug.Month)
	}
	if len(aug.Credentials) != 1 || aug.Credentials[0].Fingerprint != "fp-aug" {
		t.Fatalf("?month=2026-08 = %+v, want August's single row — serving another month under this label is a wrong answer, not a partial one",
			aug.Credentials)
	}
	// And the same question on the admin route, which had the identical bug.
	if a := adminCredUsage(t, srv, "?tier=team&month=2026-08"); len(a.Credentials) != 1 || a.Credentials[0].Fingerprint != "fp-aug" {
		t.Fatalf("admin ?month=2026-08 = %+v, want August's row", a.Credentials)
	}
}

// A month it cannot parse is refused, for the reason an unknown ?tier= is:
// falling back to now would answer a question nobody asked.
func TestCredentialUsage_MalformedMonthIsRefused(t *testing.T) {
	counter := credusage.NewMemoryCounter()
	srv := &Server{credUsage: counter, logger: iterlog.Nop()}
	for _, raw := range []string{"august", "2026", "2026-13", "2026-08-03"} {
		for _, call := range []struct {
			name string
			code int
		}{
			{"team", teamCredUsageStatus(t, srv, "team-a", "?month="+raw)},
			{"admin", adminCredUsageStatus(t, srv, "?month="+raw)},
		} {
			if call.code != http.StatusBadRequest {
				t.Errorf("%s route, month=%q: status %d, want 400", call.name, raw, call.code)
			}
		}
	}
	// The current month stays the default when none is named.
	if body := teamCredUsage(t, srv, "team-a", ""); body.Month != time.Now().UTC().Format("2006-01") {
		t.Errorf("default month = %q, want the current one", body.Month)
	}
}

func adminCredUsage(t *testing.T, srv *Server, query string) credentialUsageListView {
	t.Helper()
	w := adminCredUsageRec(srv, query)
	if w.Code != http.StatusOK {
		t.Fatalf("admin usage%s: %d %s", query, w.Code, w.Body.String())
	}
	var body credentialUsageListView
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body
}

func adminCredUsageStatus(t *testing.T, srv *Server, query string) int {
	t.Helper()
	return adminCredUsageRec(srv, query).Code
}

func adminCredUsageRec(srv *Server, query string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "/api/admin/credentials/usage"+query, nil)
	r = r.WithContext(auth.WithIdentity(r.Context(), auth.Identity{UserID: "root", IsSuperAdmin: true}))
	w := httptest.NewRecorder()
	srv.handleAdminCredentialUsage(w, r)
	return w
}

func teamCredUsage(t *testing.T, srv *Server, teamID, query string) credentialUsageListView {
	t.Helper()
	w := teamCredUsageRec(srv, teamID, query)
	if w.Code != http.StatusOK {
		t.Fatalf("team usage%s: %d %s", query, w.Code, w.Body.String())
	}
	var body credentialUsageListView
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Scope.TeamID != teamID {
		t.Fatalf("scope.team_id = %q, want %q — these rows are one tenant's slice of each credential, not the whole of it", body.Scope.TeamID, teamID)
	}
	return body
}

func teamCredUsageStatus(t *testing.T, srv *Server, teamID, query string) int {
	t.Helper()
	return teamCredUsageRec(srv, teamID, query).Code
}

func teamCredUsageRec(srv *Server, teamID, query string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "/api/teams/"+teamID+"/credentials/usage"+query, nil)
	r.SetPathValue("id", teamID)
	r = r.WithContext(store.WithTenant(auth.WithIdentity(r.Context(),
		auth.Identity{UserID: "u1", IsSuperAdmin: true, TeamID: teamID}), teamID))
	w := httptest.NewRecorder()
	srv.handleTeamCredentialUsage(w, r)
	return w
}

// The admin per-credential view filters by tier. The switch behind it is
// CLOSED, and its old fallthrough was silent: an unknown value answered
// with the platform tier's numbers, so `?tier=org` did not report "I do not
// know that tier" — it reported another credential's spend. That is how the
// org tier was missed here when it was added to credusage.
func TestTierOrPlatform(t *testing.T) {
	if got, err := tierOrPlatform(""); err != nil || got != credusage.TierPlatform {
		t.Errorf("empty filter = (%q, %v), want the platform default", got, err)
	}
	for _, tier := range []credusage.Tier{
		credusage.TierTeam, credusage.TierOrg, credusage.TierPool, credusage.TierPlatform,
	} {
		got, err := tierOrPlatform(string(tier))
		if err != nil || got != tier {
			t.Errorf("tierOrPlatform(%q) = (%q, %v), want it accepted verbatim", tier, got, err)
		}
	}
	if _, err := tierOrPlatform("nonesuch"); err == nil {
		t.Error("an unknown tier was accepted — it would answer with the platform tier's numbers, which is a wrong answer rather than a missing one")
	}
}
