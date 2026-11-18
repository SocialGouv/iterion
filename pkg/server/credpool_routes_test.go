package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/identity"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// Regressions for consent defects an adversarial review found. Both let a
// contributor's subscription be used in a way they had not agreed to.

// A parked credential must not return to service just because its donor
// touched their terms. Only a genuine reconnection clears it — otherwise
// other people's runs rediscover the same dead token, one at a time.
func TestReconnected(t *testing.T) {
	parked := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		health    credpool.Health
		connected time.Time // rec.CreatedAt — stamped only by a real connect
		refreshed time.Time // rec.UpdatedAt — bumped hourly by the worker
		want      bool
	}{
		{"a healthy pledge is always clear", credpool.HealthOK, parked.Add(-time.Hour), parked.Add(time.Hour), true},
		{"an unset health is treated as healthy", "", parked.Add(-time.Hour), parked.Add(time.Hour), true},
		{
			"parked, credential untouched — stays parked",
			credpool.HealthAuthFailed, parked.Add(-time.Hour), parked.Add(-time.Hour), false,
		},
		{
			// THE defect: a provider can keep rotating tokens for a
			// subscription it has revoked. Trusting the refresh timestamp
			// let a dead credential un-park itself every hour and burn
			// another round of borrowers' runs on the same 401.
			"parked, only the token refresh moved — stays parked",
			credpool.HealthAuthFailed, parked.Add(-time.Hour), parked.Add(6 * time.Hour), false,
		},
		{
			"parked, genuinely reconnected since — cleared",
			credpool.HealthAuthFailed, parked.Add(time.Minute), parked.Add(time.Minute), true,
		},
		{
			"an expired token that was reconnected — cleared",
			credpool.HealthTokenExpired, parked.Add(time.Minute), parked.Add(time.Minute), true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := credpool.Pledge{Health: tc.health, UpdatedAt: parked}
			rec := secrets.OAuthRecord{CreatedAt: tc.connected, UpdatedAt: tc.refreshed}
			if got := reconnected(p, rec); got != tc.want {
				t.Errorf("reconnected = %v, want %v", got, tc.want)
			}
		})
	}
}

// A contribution joins the pool of the donor's OWN org, never a stranger's.
// A "the instance has only one pool, use that" fallback would attach a
// personal subscription to an org the donor never chose.
func TestPoolIDForUser_neverAttachesToAnotherOrgsPool(t *testing.T) {
	ctx := context.Background()
	pools := credpool.NewMemoryPoolStore()
	if err := pools.Upsert(ctx, credpool.Pool{ID: "pool-other", OrgID: "org-other", Enabled: true}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s := &Server{credPoolPools: pools, logger: iterlog.New(iterlog.LevelError, nil)}

	// A user of an org with no pool of its own gets nothing, even though
	// exactly one pool exists on the instance.
	r := httptest.NewRequest("GET", "/api/me/pool", nil)
	r = r.WithContext(auth.WithIdentity(ctx, auth.Identity{UserID: "alice", OrgID: "org-mine"}))
	if got := s.poolIDForUser(r); got != "" {
		t.Errorf("pool = %q, want none — alice was enrolled into another org's pool", got)
	}

	// Their own org's pool resolves normally.
	if err := pools.Upsert(ctx, credpool.Pool{ID: "pool-mine", OrgID: "org-mine", Enabled: true}); err != nil {
		t.Fatalf("seed own: %v", err)
	}
	if got := s.poolIDForUser(r); got != "pool-mine" {
		t.Errorf("pool = %q, want pool-mine", got)
	}

	// No active org at all → nothing.
	r2 := httptest.NewRequest("GET", "/api/me/pool", nil)
	r2 = r2.WithContext(auth.WithIdentity(ctx, auth.Identity{UserID: "bob"}))
	if got := s.poolIDForUser(r2); got != "" {
		t.Errorf("pool = %q, want none for a user with no active org", got)
	}
}

// twoKeyPledges seeds one lender with two api_key pledges of the same
// provider, each naming its own key — the shape two Anthropic plans make.
func twoKeyPledges(t *testing.T, pledges *credpool.MemoryPledgeStore, userID, poolID, ref string, keyIDs ...string) []credpool.Pledge {
	t.Helper()
	out := make([]credpool.Pledge, 0, len(keyIDs))
	for _, keyID := range keyIDs {
		p := credpool.Pledge{
			PoolID: poolID, UserID: userID,
			Credential: credpool.Credential{Source: credpool.SourceAPIKey, Ref: ref, KeyID: keyID},
			Enabled:    true, Health: credpool.HealthOK,
		}
		p.ID = credpool.APIKeyPledgeID(userID, ref, keyID)
		if err := pledges.Upsert(context.Background(), p); err != nil {
			t.Fatalf("seed %s: %v", keyID, err)
		}
		out = append(out, p)
	}
	return out
}

func deleteMyPledgeByID(s *Server, userID, pledgeID string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("DELETE", "/api/me/pool/entries/x", nil)
	r.SetPathValue("pledge", pledgeID)
	r = r.WithContext(auth.WithIdentity(context.Background(), auth.Identity{UserID: userID}))
	w := httptest.NewRecorder()
	s.handleDeleteMyPledgeByID(w, r)
	return w
}

// Withdrawing by pledge id must remove EXACTLY the key named. A view that
// hands the donor a (source, ref) address instead would withdraw whichever
// of their two keys the store happened to resolve — the second half of a
// donation gone because they withdrew the first.
// Mutants: (1) the handler deletes the pledge it looked up without the
// ownership check → the foreign-pledge case reds and eve's row disappears;
// (2) the id is not compared to the caller → the 404 case reds with 204.
func TestDeleteMyPledgeByID_deletesOnlyThatKey(t *testing.T) {
	ctx := context.Background()
	pledges := credpool.NewMemoryPledgeStore()
	seeded := twoKeyPledges(t, pledges, "jo", "pool-1", "anthropic", "key-forfait-1", "key-forfait-2")
	s := &Server{credPoolPledges: pledges, logger: iterlog.New(iterlog.LevelError, nil)}

	w := deleteMyPledgeByID(s, "jo", seeded[0].ID)
	if w.Code != 204 {
		t.Fatalf("delete = %d (%s), want 204", w.Code, w.Body.String())
	}
	if _, err := pledges.Get(ctx, seeded[0].ID); !errors.Is(err, credpool.ErrNotFound) {
		t.Errorf("the named key is still pledged (%v) — the withdrawal did nothing", err)
	}
	if _, err := pledges.Get(ctx, seeded[1].ID); err != nil {
		t.Fatalf("the OTHER key of the same lender went with it (%v) — one withdrawal took both", err)
	}

	// A pledge of another donor is indistinguishable from one that does not
	// exist: 404 both ways, and nothing is deleted.
	foreign := twoKeyPledges(t, pledges, "eve", "pool-1", "anthropic", "key-eve")
	w = deleteMyPledgeByID(s, "jo", foreign[0].ID)
	if w.Code != 404 {
		t.Fatalf("deleting another donor's pledge = %d (%s), want 404", w.Code, w.Body.String())
	}
	if _, err := pledges.Get(ctx, foreign[0].ID); err != nil {
		t.Fatalf("another donor's pledge was deleted (%v) — the ownership check is the whole guard", err)
	}
	if w := deleteMyPledgeByID(s, "jo", "no-such-pledge"); w.Code != 404 {
		t.Errorf("unknown id = %d, want 404 — a different status would tell a prober the id exists", w.Code)
	}
}

// The (source, ref) shim stays exact while it lasts: an api_key pair that
// names several pledges refuses instead of guessing, and ?key_id= picks one.
// Mutant: the several-match case deletes matches[0] → the 400 assertion
// reds and jo silently loses a key they never named.
func TestDeleteMyPledge_shimNamesTheKey(t *testing.T) {
	ctx := context.Background()
	pledges := credpool.NewMemoryPledgeStore()
	seeded := twoKeyPledges(t, pledges, "jo", "pool-1", "anthropic", "key-forfait-1", "key-forfait-2")
	s := &Server{credPoolPledges: pledges, logger: iterlog.New(iterlog.LevelError, nil)}

	shim := func(query string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("DELETE", "/api/me/pool/api_key/anthropic"+query, nil)
		r.SetPathValue("source", "api_key")
		r.SetPathValue("ref", "anthropic")
		r = r.WithContext(auth.WithIdentity(ctx, auth.Identity{UserID: "jo"}))
		w := httptest.NewRecorder()
		s.handleDeleteMyPledge(w, r)
		return w
	}

	if w := shim(""); w.Code != http.StatusBadRequest {
		t.Fatalf("unnamed key with two pledges = %d (%s), want 400", w.Code, w.Body.String())
	}
	if _, err := pledges.Get(ctx, seeded[0].ID); err != nil {
		t.Fatalf("the refused shim deleted something anyway: %v", err)
	}
	if w := shim("?key_id=key-forfait-2"); w.Code != 204 {
		t.Fatalf("named key = %d (%s), want 204", w.Code, w.Body.String())
	}
	if _, err := pledges.Get(ctx, seeded[1].ID); !errors.Is(err, credpool.ErrNotFound) {
		t.Errorf("the named key is still pledged (%v)", err)
	}
	if _, err := pledges.Get(ctx, seeded[0].ID); err != nil {
		t.Errorf("the other key went with it (%v)", err)
	}
}

// Two keys of one lender are two rows of the operator roster. The roster's
// row key used to be (user, source, ref), which names ONE row for both — so
// the second key's row was written over the first and the operator saw one
// donation where there are two.
// Mutants: (1) ID dropped from the donorView literal → every row's id is
// empty and the distinctness assertion reds; (2) KeyID dropped → the two
// rows read identically and the pairing assertion reds.
func TestToPoolView_twoKeysOfSameLenderAreDistinguishable(t *testing.T) {
	ctx := context.Background()
	pledges := credpool.NewMemoryPledgeStore()
	seeded := twoKeyPledges(t, pledges, "jo", "pool-1", "anthropic", "key-forfait-1", "key-forfait-2")
	s := &Server{
		credPoolPledges: pledges,
		credPoolLedger:  credpool.NewMemoryLedger(),
		logger:          iterlog.New(iterlog.LevelError, nil),
	}
	r := httptest.NewRequest("GET", "/api/teams/team-1/pool", nil)
	r = r.WithContext(auth.WithIdentity(ctx, auth.Identity{UserID: "jo"}))
	v := s.toPoolView(r, credpool.Pool{ID: "pool-1", OrgID: "org-1", Enabled: true}, true)

	if len(v.Donors) != 2 {
		t.Fatalf("roster rows = %d, want 2 — one row per key", len(v.Donors))
	}
	rowOf := map[string]string{}
	for _, d := range v.Donors {
		if d.ID == "" {
			t.Fatalf("donor row %+v has no id — (user, source, ref) cannot key this roster", d)
		}
		if d.KeyID == "" {
			t.Fatalf("donor row %+v names no key — the operator cannot tell which key it is", d)
		}
		rowOf[d.ID] = d.KeyID
	}
	if len(rowOf) != 2 {
		t.Fatalf("roster ids collided: %v — two rows share one id", rowOf)
	}
	for _, p := range seeded {
		if got, ok := rowOf[p.ID]; !ok || got != p.KeyID {
			t.Errorf("entry %s (key %s) reads as %q in the roster, present=%v", p.ID, p.KeyID, got, ok)
		}
	}
}

// The donor roster is the contributors' business, not every org member's.
func TestToPoolView_hidesTheRosterFromNonManagers(t *testing.T) {
	ctx := context.Background()
	pledges := credpool.NewMemoryPledgeStore()
	if err := pledges.Upsert(ctx, credpool.Pledge{
		ID: credpool.PledgeID("alice", credpool.SourceOAuth, "claude_code", ""), PoolID: "pool-1",
		UserID: "alice", Credential: credpool.Credential{Source: credpool.SourceOAuth, Ref: "claude_code"}, Enabled: true, Health: credpool.HealthOK,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s := &Server{
		credPoolPledges: pledges,
		credPoolLedger:  credpool.NewMemoryLedger(),
		logger:          iterlog.New(iterlog.LevelError, nil),
	}
	r := httptest.NewRequest("GET", "/api/teams/team-1/pool", nil)
	r = r.WithContext(auth.WithIdentity(ctx, auth.Identity{UserID: "carol"}))
	pool := credpool.Pool{ID: "pool-1", OrgID: "org-1", Enabled: true}

	if v := s.toPoolView(r, pool, false); len(v.Donors) != 0 {
		t.Errorf("a non-manager saw %d donor(s) — who lends is not org-wide public information", len(v.Donors))
	}
	if v := s.toPoolView(r, pool, true); len(v.Donors) != 1 {
		t.Errorf("a manager saw %d donor(s), want 1", len(v.Donors))
	}
}

// The `iterion remote pool` surface is driven by an `iap_` bearer, and a
// PAT identity is built from a TEAM — the browser's active-org JWT claim
// is not there. Resolving the pool from the org alone therefore made every
// CLI contribution fail with "no pool accepts contributions", which is how
// this shipped: three review passes read the handler, none ran it as the
// CLI does.
//
// Built through identityFromPAT rather than a hand-made Identity: an
// Identity written by the test would assert only what the test believes
// production produces, which is exactly the belief that was wrong.
func TestPoolIDForUser_resolvesForACLIToken(t *testing.T) {
	s, ctx := newPATTestServer(t)
	if _, err := s.authStore().CreateOrg(ctx, identity.Org{ID: "org-1", Name: "acme", Slug: "acme"}); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if err := s.authStore().UpdateTeam(ctx, identity.Team{ID: "t1", Name: "t1", Slug: "acme", OrgID: "org-1"}); err != nil {
		t.Fatalf("attach team to org: %v", err)
	}
	pools := credpool.NewMemoryPoolStore()
	if err := pools.Upsert(ctx, credpool.Pool{ID: "pool-1", OrgID: "org-1", Enabled: true}); err != nil {
		t.Fatalf("seed pool: %v", err)
	}
	s.credPoolPools = pools

	_, plaintext := createPAT(t, s, ctx, `{"name":"cli"}`)
	id, err := s.identityFromPAT(ctx, plaintext)
	if err != nil {
		t.Fatalf("identityFromPAT: %v", err)
	}

	r := httptest.NewRequest("GET", "/api/me/pool", nil)
	r = r.WithContext(auth.WithIdentity(ctx, id))
	if got := s.poolIDForUser(r); got != "pool-1" {
		t.Errorf("pool = %q, want pool-1 — a CLI contributor cannot reach their own org's pool", got)
	}
}
