package server

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/audit"
	"github.com/SocialGouv/iterion/pkg/auth"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// tenantSpyKeyStore records the tenant the handler put on the context of each
// write — the value the real (Mongo) store stamps onto the row and later
// filters reads by.
type tenantSpyKeyStore struct {
	secrets.ApiKeyStore
	createTenant  string
	clearDefaults int
}

func (s *tenantSpyKeyStore) ClearDefault(ctx context.Context, teamID, userID string, provider secrets.Provider, exceptID string) error {
	s.clearDefaults++
	return s.ApiKeyStore.ClearDefault(ctx, teamID, userID, provider, exceptID)
}

func (s *tenantSpyKeyStore) Create(ctx context.Context, k secrets.ApiKey) error {
	s.createTenant, _ = store.TenantFromContext(ctx)
	return s.ApiKeyStore.Create(ctx, k)
}

// The composition the unit tests below cannot show: a team admin creating a key
// FOR another team must have it stamped with THAT team, or the runs it funds
// resolve nothing and fall back to the platform credential without a word.
func TestCreateTeamApiKey_StampsThePathTeam(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealer(bytes.Repeat([]byte{3}, 32))
	if err != nil {
		t.Fatal(err)
	}
	spy := &tenantSpyKeyStore{ApiKeyStore: secrets.NewMemoryApiKeyStore()}
	srv := &Server{apiKeys: spy, sealer: sealer}

	body := `{"provider":"anthropic","name":"for-team-b","secret":"sk-ant-api-x"}`
	r := httptest.NewRequest("POST", "/api/teams/team-b/api-keys", strings.NewReader(body))
	r.SetPathValue("id", "team-b")
	// What requireAuth stamps: the caller's ACTIVE team, which is NOT the one
	// the key is for. A super-admin identity clears canManageTeam without a
	// membership store.
	ctx := auth.WithIdentity(r.Context(), auth.Identity{UserID: "u1", IsSuperAdmin: true, TeamID: "team-a"})
	r = r.WithContext(store.WithTenant(ctx, "team-a"))

	w := httptest.NewRecorder()
	srv.handleCreateTeamApiKey(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("create: status %d body %s", w.Code, w.Body.String())
	}
	if spy.createTenant != "team-b" {
		t.Fatalf("the row must be stamped with the team it is scoped to; got tenant %q", spy.createTenant)
	}
}

// The api-keys store derives tenant_id from the context: on write it stamps
// the row, on read it filters. requireAuth stamps the CALLER'S ACTIVE team, so
// a route that names a different team in its path has to re-scope — otherwise
// a key created for that team is stamped with the caller's, and the runs it
// was meant to fund resolve nothing at all.
func TestApiKeyTenantCtx_UsesThePathTeamNotTheActiveOne(t *testing.T) {
	r, err := http.NewRequest("POST", "/api/teams/team-b/api-keys", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.SetPathValue("id", "team-b")
	r = r.WithContext(store.WithTenant(r.Context(), "team-a")) // what requireAuth stamped

	got, ok := store.TenantFromContext(teamPathTenantCtx(r))
	if !ok || got != "team-b" {
		t.Fatalf("want the path team team-b, got %q (ok=%v)", got, ok)
	}
}

// The /api/me family names no team: there the caller's active tenant IS the
// scope, and re-scoping would have nothing to scope to.
func TestApiKeyTenantCtx_KeepsTheActiveTeamWhenNoPathTeam(t *testing.T) {
	r, err := http.NewRequest("POST", "/api/me/api-keys", nil)
	if err != nil {
		t.Fatal(err)
	}
	r = r.WithContext(store.WithTenant(r.Context(), "team-a"))

	got, ok := store.TenantFromContext(teamPathTenantCtx(r))
	if !ok || got != "team-a" {
		t.Fatalf("want the active team team-a, got %q (ok=%v)", got, ok)
	}
}

// A BYOK secret with whitespace or a control character cannot possibly
// authenticate: a bearer token has none, and the shape has been paid for
// live (a transcript pasted as accessToken, hours of dead-on-arrival runs
// before the cause was found). #627's ingestion gate refuses it before it
// ever lands in the store — same contract as sealOAuthRecord.
//
// The oracle is the STORE, not the response status: a 400 with a silent
// write would still leave the garbage on disk for the runs to pick up.
func TestCreateApiKey_RejectsMalformedSecret(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealer(bytes.Repeat([]byte{5}, 32))
	if err != nil {
		t.Fatal(err)
	}
	spy := &tenantSpyKeyStore{ApiKeyStore: secrets.NewMemoryApiKeyStore()}
	srv := &Server{apiKeys: spy, sealer: sealer}

	cases := []struct {
		name, body, wantInErr string
	}{
		{"newline in secret", `{"provider":"anthropic","name":"bad","secret":"sk-ant-good\nWelcome"}`, "newline"},
		{"tab in secret", `{"provider":"anthropic","name":"bad","secret":"\tsk-ant-good"}`, "tab"},
		{"space in secret", `{"provider":"anthropic","name":"bad","secret":"sk-ant good"}`, "space"},
		// The JSON string "[32msecret" carries a real ESC byte, which
		// is a control character even though it looks like ANSI on screen.
		{"ANSI escape (terminal transcript)", `{"provider":"anthropic","name":"bad","secret":"\u001b[32msk-ant-good\u001b[0m"}`, "control character"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/api/teams/team-a/api-keys", strings.NewReader(tc.body))
			r.SetPathValue("id", "team-a")
			ctx := auth.WithIdentity(r.Context(), auth.Identity{UserID: "u1", IsSuperAdmin: true, TeamID: "team-a"})
			r = r.WithContext(store.WithTenant(ctx, "team-a"))

			w := httptest.NewRecorder()
			srv.handleCreateTeamApiKey(w, r)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body=%s, want 400 (garbage secret rejected at ingestion)", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.wantInErr) {
				t.Fatalf("error body = %q, want it to mention %q", w.Body.String(), tc.wantInErr)
			}
		})
	}
}

// A well-formed secret still passes and lands sealed — the shape gate is
// format-agnostic on the token contents (no vendor prefix pin) so a legal
// value crosses it unchanged.
func TestCreateApiKey_AcceptsHealthySecret(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealer(bytes.Repeat([]byte{5}, 32))
	if err != nil {
		t.Fatal(err)
	}
	spy := &tenantSpyKeyStore{ApiKeyStore: secrets.NewMemoryApiKeyStore()}
	srv := &Server{apiKeys: spy, sealer: sealer}

	body := `{"provider":"anthropic","name":"healthy","secret":"sk-ant-api03-realkey"}`
	r := httptest.NewRequest("POST", "/api/teams/team-a/api-keys", strings.NewReader(body))
	r.SetPathValue("id", "team-a")
	ctx := auth.WithIdentity(r.Context(), auth.Identity{UserID: "u1", IsSuperAdmin: true, TeamID: "team-a"})
	r = r.WithContext(store.WithTenant(ctx, "team-a"))

	w := httptest.NewRecorder()
	srv.handleCreateTeamApiKey(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("healthy create = %d body=%s", w.Code, w.Body.String())
	}
}

// byokServer builds the minimal handler-level server the BYOK tests use,
// with a Warn-level log buffer and a memory audit store so a refusal's
// trace is observable.
func byokServer(t *testing.T) (*Server, *tenantSpyKeyStore, *bytes.Buffer, *audit.MemoryStore) {
	t.Helper()
	sealer, err := secrets.NewAESGCMSealer(bytes.Repeat([]byte{5}, 32))
	if err != nil {
		t.Fatal(err)
	}
	spy := &tenantSpyKeyStore{ApiKeyStore: secrets.NewMemoryApiKeyStore()}
	var logs bytes.Buffer
	auditStore := audit.NewMemoryStore()
	srv := &Server{apiKeys: spy, sealer: sealer, logger: iterlog.New(iterlog.LevelWarn, &logs), auditStore: auditStore}
	return srv, spy, &logs, auditStore
}

func createTeamKey(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "/api/teams/team-a/api-keys", strings.NewReader(body))
	r.SetPathValue("id", "team-a")
	ctx := auth.WithIdentity(r.Context(), auth.Identity{UserID: "u1", IsSuperAdmin: true, TeamID: "team-a"})
	r = r.WithContext(store.WithTenant(ctx, "team-a"))
	w := httptest.NewRecorder()
	srv.handleCreateTeamApiKey(w, r)
	return w
}

// Bedrock and Vertex BYOK values are JSON credential documents — the
// ApiKey doc, the studio picker and docs/byok.md all say so — and the
// token rule refused them as "a terminal transcript" for their newlines
// (#627 round 1). The shape rule is per provider: a JSON object passes,
// a bearer-looking value on those providers is refused with its own
// message, and the bearer providers keep the token rule.
func TestCreateApiKey_ShapeRuleIsPerProvider(t *testing.T) {
	srv, spy, _, _ := byokServer(t)
	awsBlob := `{\n  \"aws_access_key_id\": \"AKIA...\",\n  \"aws_secret_access_key\": \"x\"\n}`

	if w := createTeamKey(t, srv, `{"provider":"bedrock","name":"aws","secret":"`+awsBlob+`"}`); w.Code != http.StatusOK {
		t.Fatalf("bedrock JSON blob = %d body=%s, want 200", w.Code, w.Body.String())
	}
	if w := createTeamKey(t, srv, `{"provider":"vertex","name":"gcp","secret":"{\"type\":\"service_account\",\"project_id\":\"p\"}"}`); w.Code != http.StatusOK {
		t.Fatalf("vertex JSON blob = %d body=%s, want 200", w.Code, w.Body.String())
	}
	keys, err := spy.ListByTeam(store.WithTenant(context.Background(), "team-a"), "team-a", "")
	if err != nil || len(keys) != 2 {
		t.Fatalf("stored keys = %d (%v), want the two JSON credentials sealed", len(keys), err)
	}
	if w := createTeamKey(t, srv, `{"provider":"bedrock","name":"aws","secret":"AKIAIOSFODNN7EXAMPLE"}`); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "JSON credential object") {
		t.Fatalf("bedrock bearer-looking = %d body=%s, want 400 with the JSON-object message", w.Code, w.Body.String())
	}
	if w := createTeamKey(t, srv, `{"provider":"anthropic","name":"k","secret":"{\"not\":\"a token\"}"}`); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "space") {
		t.Fatalf("anthropic JSON-looking = %d body=%s, want 400 under the token rule", w.Code, w.Body.String())
	}
}

// The ASCII-only gate let a NO-BREAK SPACE through; refused now, naming
// the rune, and the refusal leaves a Warn + an audit event naming the
// field and reason — never the value.
func TestCreateApiKey_RefusesUnicodeWhitespaceAndLeavesATrace(t *testing.T) {
	srv, spy, logs, auditStore := byokServer(t)
	w := createTeamKey(t, srv, `{"provider":"anthropic","name":"k","secret":"sk-ant-api03-real\u00a0key"}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "U+00A0") {
		t.Fatalf("status = %d body=%s, want 400 naming U+00A0", w.Code, w.Body.String())
	}
	if keys, _ := spy.ListByTeam(store.WithTenant(context.Background(), "team-a"), "team-a", ""); len(keys) != 0 {
		t.Fatalf("a refused key was stored: %+v", keys)
	}
	if l := logs.String(); !strings.Contains(l, "REFUSED at ingestion") || !strings.Contains(l, "provider=anthropic") || strings.Contains(l, "sk-ant-api03-real") {
		t.Fatalf("want a Warn naming the provider and never the value; got:\n%s", l)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		events, err := auditStore.ListByTenant(context.Background(), "team-a", audit.Page{Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range events {
			if e.Action == "byok.refused" {
				if e.Meta["provider"] != "anthropic" || e.Meta["field"] != "api-key secret" || strings.Contains(fmt.Sprint(e.Meta), "sk-ant-api03-real") {
					t.Fatalf("audit event = %+v, want provider + field + reason and never the value", e)
				}
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no byok.refused audit event; got %+v", events)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A route whose {id} is NOT the store scope — the org credential tier,
// where {id} is an ORG id while the rows live under the reserved org-tier
// scope — passes the scope explicitly. Deriving it from the path there
// would filter on an id no row carries, and the org's own key would read as
// absent while the run silently fell through to the platform credential.
func TestApiKeyTenantCtx_ExplicitScopeBeatsThePath(t *testing.T) {
	r, err := http.NewRequest("POST", "/api/orgs/org-1/api-keys", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.SetPathValue("id", "org-1")
	r = r.WithContext(store.WithTenant(r.Context(), "team-a"))

	scope := secrets.OrgTierTenantID("org-1")
	got, ok := store.TenantFromContext(apiKeyScopeCtx(r, scope))
	if !ok || got != scope {
		t.Fatalf("want the explicit org-tier scope %q, got %q (ok=%v)", scope, got, ok)
	}
	if got == "org-1" {
		t.Fatal("the raw org id reached the store scope — no api-key row is stamped with it")
	}
}

// A row whose provider this build does not know was written by a newer
// server — beside this pod in a rolling update, or the one it was rolled
// back from. The shape rule for its secret is unknown here and its
// provider-specific columns are invisible to this struct, so every mutation
// is refused (409, audited, logged) and the row is left byte for byte as
// it was; deleting it stays possible from any build.
func TestUpdateApiKey_RefusesARowFromANewerBuild(t *testing.T) {
	srv, spy, logs, auditStore := byokServer(t)
	ctx := store.WithTenant(context.Background(), "team-a")
	row := secrets.ApiKey{ID: "key-future", ScopeTeamID: "team-a", Provider: secrets.Provider("x-future"), Name: "from-tomorrow", SealedSecret: []byte("sealed-elsewhere"), Fingerprint: "fp-future"}
	if err := spy.Create(ctx, row); err != nil {
		t.Fatal(err)
	}
	call := func(method, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/api/teams/team-a/api-keys/key-future", strings.NewReader(body))
		r.SetPathValue("id", "team-a")
		r.SetPathValue("key_id", "key-future")
		rctx := auth.WithIdentity(r.Context(), auth.Identity{UserID: "u1", IsSuperAdmin: true, TeamID: "team-a"})
		r = r.WithContext(store.WithTenant(rctx, "team-a"))
		w := httptest.NewRecorder()
		if method == "DELETE" {
			srv.handleDeleteApiKey(w, r)
		} else {
			srv.handleUpdateApiKey(w, r)
		}
		return w
	}
	before, err := spy.Get(ctx, "key-future")
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"name":"renamed"}`, `{"secret":"sk-rotated-value"}`, `{"is_default":true}`, `{"bots":["review-pr"]}`, `{"max_concurrent_runs":0}`, `{}`} {
		if w := call("PATCH", body); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "x-future") {
			t.Fatalf("PATCH %s = %d body=%s, want 409 naming the provider", body, w.Code, w.Body.String())
		}
	}
	back, err := spy.Get(ctx, "key-future")
	if err != nil || !reflect.DeepEqual(back, before) {
		t.Fatalf("a refused update touched the row:\n got %+v\nwant %+v (%v)", back, before, err)
	}
	if l := logs.String(); !strings.Contains(l, "update REFUSED") || !strings.Contains(l, "provider=x-future") {
		t.Fatalf("want a Warn naming the provider; got:\n%s", l)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		events, err := auditStore.ListByTenant(context.Background(), "team-a", audit.Page{Limit: 20})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, e := range events {
			if e.Action == "byok.refused" && e.Meta["provider"] == "x-future" && e.Meta["field"] == "provider" {
				found = true
			}
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no byok.refused audit event naming the provider; got %+v", events)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if w := call("DELETE", ""); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d body=%s, want 204 — a row from a newer build must stay deletable", w.Code, w.Body.String())
	}
	if _, err := spy.Get(ctx, "key-future"); err == nil {
		t.Fatal("the row survived its deletion")
	}
}

// Only a request that PROMOTES a key clears the other defaults. A rename of
// the current default used to call ClearDefault too — on the stale row it
// had read — and could erase a promotion that landed in between.
func TestUpdateApiKey_OnlyAPromotionClearsTheOtherDefaults(t *testing.T) {
	srv, spy, _, _ := byokServer(t)
	if w := createTeamKey(t, srv, `{"provider":"anthropic","name":"current","secret":"sk-ant-current","is_default":true}`); w.Code != http.StatusOK {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	ctx := store.WithTenant(context.Background(), "team-a")
	keys, _ := spy.ListByTeam(ctx, "team-a", "")
	patch := func(id, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("PATCH", "/api/teams/team-a/api-keys/"+id, strings.NewReader(body))
		r.SetPathValue("id", "team-a")
		r.SetPathValue("key_id", id)
		rctx := auth.WithIdentity(r.Context(), auth.Identity{UserID: "u1", IsSuperAdmin: true, TeamID: "team-a"})
		r = r.WithContext(store.WithTenant(rctx, "team-a"))
		w := httptest.NewRecorder()
		srv.handleUpdateApiKey(w, r)
		return w
	}
	spy.clearDefaults = 0
	if w := patch(keys[0].ID, `{"name":"still the default"}`); w.Code != http.StatusOK {
		t.Fatalf("rename: %d %s", w.Code, w.Body.String())
	}
	if spy.clearDefaults != 0 {
		t.Fatalf("a rename of the default called ClearDefault %d time(s); it must not touch the other keys", spy.clearDefaults)
	}
	if w := patch(keys[0].ID, `{"is_default":true}`); w.Code != http.StatusOK {
		t.Fatalf("promote: %d %s", w.Code, w.Body.String())
	}
	if spy.clearDefaults != 1 {
		t.Fatalf("a promotion must clear the other defaults exactly once; got %d", spy.clearDefaults)
	}
}

// A promotion lowers the other default of ITS tuple — (team, user,
// provider) — and nothing else: a second team default that slipped in is
// lowered, the personal default of the same provider is untouched, and a
// personal promotion leaves the team default alone. Read back from the
// store, not from the response (the view is built before the clear).
func TestUpdateApiKey_APromotionLowersOnlyItsTupleOtherDefault(t *testing.T) {
	srv, spy, _, _ := byokServer(t)
	ctx := store.WithTenant(context.Background(), "team-a")
	seed := func(id, user string, def bool) {
		t.Helper()
		if err := spy.Create(ctx, secrets.ApiKey{ID: id, ScopeTeamID: "team-a", ScopeUserID: user, Provider: secrets.ProviderOpenAI, Name: id, SealedSecret: []byte("sealed-" + id), IsDefault: def}); err != nil {
			t.Fatal(err)
		}
	}
	seed("A", "", true)
	seed("B", "", true) // a second team default that slipped in
	seed("P1", "u1", true)
	seed("P2", "u1", false)
	promote := func(id string, who auth.Identity) {
		t.Helper()
		r := httptest.NewRequest("PATCH", "/api/teams/team-a/api-keys/"+id, strings.NewReader(`{"is_default":true}`))
		r.SetPathValue("id", "team-a")
		r.SetPathValue("key_id", id)
		r = r.WithContext(store.WithTenant(auth.WithIdentity(r.Context(), who), "team-a"))
		w := httptest.NewRecorder()
		srv.handleUpdateApiKey(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("promote %s: %d %s", id, w.Code, w.Body.String())
		}
	}
	isDefault := func(id string) bool {
		t.Helper()
		k, err := spy.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return k.IsDefault
	}
	promote("A", auth.Identity{UserID: "u1", IsSuperAdmin: true, TeamID: "team-a"})
	if !isDefault("A") || isDefault("B") || !isDefault("P1") {
		t.Fatalf("team promotion: A=%v B=%v P1=%v, want A and P1 only", isDefault("A"), isDefault("B"), isDefault("P1"))
	}
	promote("P2", auth.Identity{UserID: "u1", TeamID: "team-a"})
	if !isDefault("P2") || isDefault("P1") || !isDefault("A") {
		t.Fatalf("personal promotion: P2=%v P1=%v A=%v, want P2 and A", isDefault("P2"), isDefault("P1"), isDefault("A"))
	}
}
