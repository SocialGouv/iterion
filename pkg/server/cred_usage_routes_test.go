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
