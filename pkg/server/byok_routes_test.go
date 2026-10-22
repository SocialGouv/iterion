package server

import (
	"bytes"
	"context"
	"encoding/json"
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

// A gateway key is created with its base URL (required, normalised,
// https only), sealed FOR that URL and identified by it; the URL is
// refused on every other provider; an unknown body field is refused
// rather than dropped; the repository audience round-trips and clears.
func TestCreateApiKey_GatewayKeyCarriesItsBaseURLAndRepos(t *testing.T) {
	srv, spy, _, _ := byokServer(t)
	ctx := store.WithTenant(context.Background(), "team-a")
	if w := createTeamKey(t, srv, `{"provider":"openai_compatible","name":"pliage","secret":"sk-litellm-virtual-key-1234567890"}`); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "base_url") {
		t.Fatalf("gateway key without base_url = %d %s, want 400 naming base_url", w.Code, w.Body.String())
	}
	if w := createTeamKey(t, srv, `{"provider":"openai_compatible","name":"pliage","secret":"sk-litellm-virtual-key-1234567890","base_url":"http://gw.example.test"}`); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "https") {
		t.Fatalf("plain http = %d %s, want 400 naming https", w.Code, w.Body.String())
	}
	if w := createTeamKey(t, srv, `{"provider":"anthropic","name":"k","secret":"sk-ant-api-x","base_url":"https://gw.example.test"}`); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "only accepted") {
		t.Fatalf("base_url on anthropic = %d %s, want 400", w.Code, w.Body.String())
	}
	if w := createTeamKey(t, srv, `{"provider":"anthropic","name":"k","secret":"sk-ant-api-x","future_field":1}`); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown field = %d %s, want 400 (strict decode)", w.Code, w.Body.String())
	}
	w := createTeamKey(t, srv, `{"provider":"openai_compatible","name":"pliage","secret":"sk-litellm-virtual-key-1234567890","base_url":"https://gw.example.test/v1/","repos":["DNUM-SocialGouv/Aides-Jeunes","dnum-socialgouv/aides-jeunes"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("gateway create = %d %s", w.Code, w.Body.String())
	}
	var view apiKeyView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.BaseURL != "https://gw.example.test" || len(view.Repos) != 1 || view.Repos[0] != "dnum-socialgouv/aides-jeunes" {
		t.Fatalf("view = %+v, want the normalised URL and one lower-cased repo", view)
	}
	k, err := spy.Get(ctx, view.ID)
	if err != nil {
		t.Fatal(err)
	}
	if k.Fingerprint != secrets.APIKeyFingerprint(secrets.ProviderOpenAICompatible, "sk-litellm-virtual-key-1234567890", "https://gw.example.test") {
		t.Fatalf("fingerprint %q is not the endpoint-aware identity", k.Fingerprint)
	}
	if pt, err := secrets.OpenApiKey(srv.sealer, k); err != nil || string(pt) != "sk-litellm-virtual-key-1234567890" {
		t.Fatalf("open under the registered URL: %q %v", pt, err)
	}
	repointed := k
	repointed.BaseURL = "https://attacker.example.test"
	if _, err := secrets.OpenApiKey(srv.sealer, repointed); err == nil {
		t.Fatal("the stored blob opens on another host — the AAD did not bind base_url")
	}

	patch := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("PATCH", "/api/teams/team-a/api-keys/"+view.ID, strings.NewReader(body))
		r.SetPathValue("id", "team-a")
		r.SetPathValue("key_id", view.ID)
		rctx := auth.WithIdentity(r.Context(), auth.Identity{UserID: "u1", IsSuperAdmin: true, TeamID: "team-a"})
		r = r.WithContext(store.WithTenant(rctx, "team-a"))
		rw := httptest.NewRecorder()
		srv.handleUpdateApiKey(rw, r)
		return rw
	}
	// Moving the URL is a rotation: without the secret the request is
	// refused and the row does not move (the audit of that refusal is
	// TestUpdateApiKey_MovingBaseURLRequiresTheSecret's).
	if rw := patch(`{"base_url":"https://gw2.example.test"}`); rw.Code != http.StatusBadRequest || !strings.Contains(rw.Body.String(), "secret") {
		t.Fatalf("base_url move without the secret = %d %s, want 400 asking for the secret", rw.Code, rw.Body.String())
	}
	if same, _ := spy.Get(ctx, view.ID); same.BaseURL != "https://gw.example.test" || !bytes.Equal(same.SealedSecret, k.SealedSecret) {
		t.Fatalf("the refused move changed the row: %+v", same)
	}
	// With the secret, the move seals it under the new URL and re-identifies it.
	if rw := patch(`{"base_url":"https://gw2.example.test","secret":"sk-litellm-virtual-key-1234567890"}`); rw.Code != http.StatusOK {
		t.Fatalf("base_url move with the secret = %d %s", rw.Code, rw.Body.String())
	}
	moved, _ := spy.Get(ctx, view.ID)
	if moved.BaseURL != "https://gw2.example.test" || moved.Fingerprint == k.Fingerprint || moved.Fingerprint != secrets.APIKeyFingerprint(secrets.ProviderOpenAICompatible, "sk-litellm-virtual-key-1234567890", "https://gw2.example.test") {
		t.Fatalf("moved key = %+v", moved)
	}
	if pt, err := secrets.OpenApiKey(srv.sealer, moved); err != nil || string(pt) != "sk-litellm-virtual-key-1234567890" {
		t.Fatalf("the moved key must open under its new URL: %q %v", pt, err)
	}
	// Re-sending the stored URL, in any of its spellings, is a no-op: no
	// secret is asked for, the blob is not re-sealed, the identity holds.
	if rw := patch(`{"base_url":"https://GW2.example.test/v1/","name":"renamed"}`); rw.Code != http.StatusOK {
		t.Fatalf("no-op base_url = %d %s", rw.Code, rw.Body.String())
	}
	if same, _ := spy.Get(ctx, view.ID); same.Name != "renamed" || !bytes.Equal(same.SealedSecret, moved.SealedSecret) || same.Fingerprint != moved.Fingerprint {
		t.Fatalf("a no-op URL re-sealed the key: %+v", same)
	}
	// Clearing and setting the repository audience.
	if rw := patch(`{"repos":[]}`); rw.Code != http.StatusOK {
		t.Fatalf("clear repos = %d %s", rw.Code, rw.Body.String())
	}
	if cleared, _ := spy.Get(ctx, view.ID); len(cleared.Repos) != 0 {
		t.Fatalf("repos not cleared: %+v", cleared.Repos)
	}
	if rw := patch(`{"repos":["not-a-path"]}`); rw.Code != http.StatusBadRequest {
		t.Fatalf("a bare word is not a repository path: %d %s", rw.Code, rw.Body.String())
	}
	tooMany := make([]string, maxBotAudienceEntries+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("owner/repo-%d", i)
	}
	tooManyBody, _ := json.Marshal(map[string]any{"repos": tooMany})
	if rw := patch(string(tooManyBody)); rw.Code != http.StatusBadRequest || !strings.Contains(rw.Body.String(), "at most") {
		t.Fatalf("%d repos = %d %s, want 400 naming the ceiling", len(tooMany), rw.Code, rw.Body.String())
	}
	// The update decodes as strictly as the create: an unknown field and a
	// second JSON value are both refused, and neither touches the row.
	before, _ := spy.Get(ctx, view.ID)
	if rw := patch(`{"name":"other","future_field":1}`); rw.Code != http.StatusBadRequest {
		t.Fatalf("unknown field on update = %d %s, want 400", rw.Code, rw.Body.String())
	}
	if rw := patch("{\"name\":\"other\"}\n{\"repos\":[\"owner/only-this\"]}"); rw.Code != http.StatusBadRequest {
		t.Fatalf("a second JSON value on update = %d %s, want 400", rw.Code, rw.Body.String())
	}
	if after, _ := spy.Get(ctx, view.ID); !reflect.DeepEqual(after, before) {
		t.Fatalf("a refused body changed the row:\n%+v\n%+v", before, after)
	}
	// A URL on a vendor key is refused on update too — any value, blank
	// included: a 200 that wrote nothing would read as accepted.
	wv := createTeamKey(t, srv, `{"provider":"anthropic","name":"v","secret":"sk-ant-api-v"}`)
	if !strings.Contains(wv.Body.String(), `"repos":[]`) {
		t.Fatalf("the view must carry repos as an empty list, never null: %s", wv.Body.String())
	}
	var vendor apiKeyView
	_ = json.Unmarshal(wv.Body.Bytes(), &vendor)
	for _, body := range []string{`{"base_url":"https://gw.example.test"}`, `{"base_url":""}`, `{"base_url":"   "}`} {
		rv := httptest.NewRequest("PATCH", "/api/teams/team-a/api-keys/"+vendor.ID, strings.NewReader(body))
		rv.SetPathValue("id", "team-a")
		rv.SetPathValue("key_id", vendor.ID)
		rv = rv.WithContext(store.WithTenant(auth.WithIdentity(rv.Context(), auth.Identity{UserID: "u1", IsSuperAdmin: true, TeamID: "team-a"}), "team-a"))
		rwv := httptest.NewRecorder()
		srv.handleUpdateApiKey(rwv, rv)
		if rwv.Code != http.StatusBadRequest {
			t.Fatalf("%s on a vendor key update = %d %s, want 400", body, rwv.Code, rwv.Body.String())
		}
	}
}

// The repository audience accepts what a launch can name — the forge's
// path of one repository — and refuses what matches none: a URL, a glob, a
// host, an empty or dot segment, whitespace or a control character. A key
// restricted to such an entry would silently fund nothing.
func TestNormalizeRepoAudience(t *testing.T) {
	ok := map[string][]string{
		"Owner/Repo/":                  {"owner/repo"},
		"group/Sub/project":            {"group/sub/project"},
		"o/r.git":                      {"o/r"},
		"o/r.git.git":                  {"o/r"},
		" o/r ":                        {"o/r"},
		"DNUM-SocialGouv/aides-jeunes": {"dnum-socialgouv/aides-jeunes"},
	}
	for in, want := range ok {
		got, err := normalizeRepoAudience([]string{in, in})
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%q → %v %v, want %v (deduplicated)", in, got, err, want)
		}
	}
	// Every refused value carries the marker ZZ: a refusal must not echo it.
	for _, bad := range []string{"ownerZZ//repo", "https://github.com/oZZ/r", "oZZ/*", "*ZZ/*", "../../xZZ", "oZZ/./r", "oZZ/r with space", "oZZ/r\rx", "oZZ/r\x00", "oZZ/r?x", "oZZ/r[1]", "ownerZZ", strings.Repeat("a", maxRepoPathBytes) + "/rZZ"} {
		if _, err := normalizeRepoAudience([]string{bad}); err == nil {
			t.Errorf("%q accepted", bad)
		} else if strings.Contains(err.Error(), "ZZ") {
			t.Errorf("the refusal echoes the value: %v", err)
		}
	}
	if _, err := normalizeRepoAudience([]string{"/", " "}); err == nil {
		t.Error("a list of blanks accepted")
	}
	if got, err := normalizeRepoAudience(nil); got != nil || err != nil {
		t.Errorf("nil → %v %v", got, err)
	}
}

// A mutator who never saw the secret cannot aim it at a host of theirs: a
// base_url move without the secret is refused, audited as a refusal of
// that field, and the row keeps its URL and its blob.
func TestUpdateApiKey_MovingBaseURLRequiresTheSecret(t *testing.T) {
	srv, spy, _, auditStore := byokServer(t)
	ctx := store.WithTenant(context.Background(), "team-a")
	w := createTeamKey(t, srv, `{"provider":"openai_compatible","name":"pliage","secret":"sk-litellm-virtual-key-1234567890","base_url":"https://gw.example.test"}`)
	var view apiKeyView
	_ = json.Unmarshal(w.Body.Bytes(), &view)
	before, _ := spy.Get(ctx, view.ID)
	r := httptest.NewRequest("PATCH", "/api/teams/team-a/api-keys/"+view.ID, strings.NewReader(`{"base_url":"https://collector.attacker.example"}`))
	r.SetPathValue("id", "team-a")
	r.SetPathValue("key_id", view.ID)
	r = r.WithContext(store.WithTenant(auth.WithIdentity(r.Context(), auth.Identity{UserID: "other-admin", IsSuperAdmin: true, TeamID: "team-a"}), "team-a"))
	rw := httptest.NewRecorder()
	srv.handleUpdateApiKey(rw, r)
	if rw.Code != http.StatusBadRequest || !strings.Contains(rw.Body.String(), "secret") {
		t.Fatalf("move without the secret = %d %s, want 400 asking for the secret", rw.Code, rw.Body.String())
	}
	after, _ := spy.Get(ctx, view.ID)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("the refused move changed the row:\n%+v\n%+v", before, after)
	}
	if pt, err := secrets.OpenApiKey(srv.sealer, after); err != nil || string(pt) != "sk-litellm-virtual-key-1234567890" {
		t.Fatalf("the row must still open under its own URL: %q %v", pt, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		events, err := auditStore.ListByTenant(context.Background(), "team-a", audit.Page{Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range events {
			if e.Action == "byok.updated" {
				t.Fatalf("a refused move was audited as an update: %+v", e)
			}
			if e.Action == "byok.refused" {
				if e.Meta["field"] != "base_url" || e.Meta["provider"] != "openai_compatible" {
					t.Fatalf("audit event = %+v, want field base_url on provider openai_compatible", e)
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

// A move is conditional on the blob the handler read: a rotation that
// landed between the read and the write answers 409 and is kept.
func TestUpdateApiKey_BaseURLChangeLosesToAConcurrentRotation(t *testing.T) {
	srv, spy, _, _ := byokServer(t)
	ctx := store.WithTenant(context.Background(), "team-a")
	w := createTeamKey(t, srv, `{"provider":"openai_compatible","name":"pliage","secret":"sk-litellm-virtual-key-1234567890","base_url":"https://gw.example.test"}`)
	var view apiKeyView
	_ = json.Unmarshal(w.Body.Bytes(), &view)
	// Simulate the interleaving: the handler's Get happens first, then the
	// rotation lands in the store, then the handler's Patch arrives. A spy
	// that rotates on the first Get reproduces exactly that order.
	racing := &rotateOnGetKeyStore{ApiKeyStore: spy, sealer: srv.sealer, id: view.ID, tenant: "team-a"}
	srv.apiKeys = racing
	r := httptest.NewRequest("PATCH", "/api/teams/team-a/api-keys/"+view.ID, strings.NewReader(`{"base_url":"https://gw2.example.test","secret":"sk-litellm-virtual-key-1234567890"}`))
	r.SetPathValue("id", "team-a")
	r.SetPathValue("key_id", view.ID)
	r = r.WithContext(store.WithTenant(auth.WithIdentity(r.Context(), auth.Identity{UserID: "u1", IsSuperAdmin: true, TeamID: "team-a"}), "team-a"))
	rw := httptest.NewRecorder()
	srv.handleUpdateApiKey(rw, r)
	if rw.Code != http.StatusConflict {
		t.Fatalf("stale move = %d %s, want 409", rw.Code, rw.Body.String())
	}
	back, _ := spy.Get(ctx, view.ID)
	if pt, err := secrets.OpenApiKey(srv.sealer, back); err != nil || string(pt) != "sk-rotated-by-someone-else-00" || back.BaseURL != "https://gw.example.test" {
		t.Fatalf("the concurrent rotation must survive and the URL must not move: %q %v %+v", pt, err, back)
	}
}

// The mirror: a plain rotation that landed after a concurrent MOVE answers
// 409 too. Written without the guard, the stale rotation would land last
// and silently revert the move — the row back on the old gateway, now with
// a fresh secret, and nobody told.
func TestUpdateApiKey_RotationLosesToAConcurrentMove(t *testing.T) {
	srv, spy, _, _ := byokServer(t)
	ctx := store.WithTenant(context.Background(), "team-a")
	w := createTeamKey(t, srv, `{"provider":"openai_compatible","name":"pliage","secret":"sk-litellm-virtual-key-1234567890","base_url":"https://gw.example.test"}`)
	var view apiKeyView
	_ = json.Unmarshal(w.Body.Bytes(), &view)
	racing := &rotateOnGetKeyStore{ApiKeyStore: spy, sealer: srv.sealer, id: view.ID, tenant: "team-a", moveTo: "https://gw2.example.test"}
	srv.apiKeys = racing
	r := httptest.NewRequest("PATCH", "/api/teams/team-a/api-keys/"+view.ID, strings.NewReader(`{"secret":"sk-litellm-rotated-key-0987654321"}`))
	r.SetPathValue("id", "team-a")
	r.SetPathValue("key_id", view.ID)
	r = r.WithContext(store.WithTenant(auth.WithIdentity(r.Context(), auth.Identity{UserID: "u1", IsSuperAdmin: true, TeamID: "team-a"}), "team-a"))
	rw := httptest.NewRecorder()
	srv.handleUpdateApiKey(rw, r)
	if rw.Code != http.StatusConflict {
		t.Fatalf("stale rotation = %d %s, want 409", rw.Code, rw.Body.String())
	}
	back, _ := spy.Get(ctx, view.ID)
	if pt, err := secrets.OpenApiKey(srv.sealer, back); err != nil || string(pt) != "sk-rotated-by-someone-else-00" || back.BaseURL != "https://gw2.example.test" {
		t.Fatalf("the concurrent move must survive, and the row must still open: %q %v %+v", pt, err, back)
	}
}

// A rename racing a concurrent move lands (nothing it wrote conflicts) and is
// audited as a rename: the row now holds the other writer's URL, but THIS
// request did not move it, so its audit event carries no gateway_host.
func TestUpdateApiKey_ARenameRacingAMoveIsNotAuditedAsTheMove(t *testing.T) {
	srv, spy, _, auditStore := byokServer(t)
	ctx := store.WithTenant(context.Background(), "team-a")
	w := createTeamKey(t, srv, `{"provider":"openai_compatible","name":"pliage","secret":"sk-litellm-virtual-key-1234567890","base_url":"https://gw.example.test"}`)
	var view apiKeyView
	_ = json.Unmarshal(w.Body.Bytes(), &view)
	srv.apiKeys = &rotateOnGetKeyStore{ApiKeyStore: spy, sealer: srv.sealer, id: view.ID, tenant: "team-a", moveTo: "https://gw2.example.test"}
	r := httptest.NewRequest("PATCH", "/api/teams/team-a/api-keys/"+view.ID, strings.NewReader(`{"name":"renamed"}`))
	r.SetPathValue("id", "team-a")
	r.SetPathValue("key_id", view.ID)
	r = r.WithContext(store.WithTenant(auth.WithIdentity(r.Context(), auth.Identity{UserID: "u1", IsSuperAdmin: true, TeamID: "team-a"}), "team-a"))
	rw := httptest.NewRecorder()
	srv.handleUpdateApiKey(rw, r)
	if rw.Code != http.StatusOK {
		t.Fatalf("rename = %d %s, want 200 (nothing it wrote conflicts)", rw.Code, rw.Body.String())
	}
	if back, _ := spy.Get(ctx, view.ID); back.Name != "renamed" || back.BaseURL != "https://gw2.example.test" {
		t.Fatalf("row = %+v, want the rename AND the concurrent move", back)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		events, err := auditStore.ListByTenant(context.Background(), "team-a", audit.Page{Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range events {
			if e.Action == "byok.updated" {
				if _, moved := e.Meta["gateway_host"]; moved {
					t.Fatalf("the rename was audited as the move: %+v", e)
				}
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no byok.updated audit event; got %+v", events)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// rotateOnGetKeyStore rotates the key once, right after the handler reads
// it — the store-level twin of "another writer got there first". With
// moveTo set, that writer also moves the key to another gateway.
type rotateOnGetKeyStore struct {
	secrets.ApiKeyStore
	sealer secrets.Sealer
	id     string
	tenant string
	moveTo string
	done   bool
}

func (s *rotateOnGetKeyStore) Get(ctx context.Context, id string) (secrets.ApiKey, error) {
	k, err := s.ApiKeyStore.Get(ctx, id)
	if err == nil && id == s.id && !s.done {
		s.done = true
		target := k
		patch := secrets.ApiKeyPatch{}
		if s.moveTo != "" {
			target.BaseURL = s.moveTo
			patch.BaseURL = &target.BaseURL
		}
		sealed, serr := secrets.SealAPIKeyFor(s.sealer, target, []byte("sk-rotated-by-someone-else-00"))
		if serr != nil {
			return k, serr
		}
		patch.Secret = &secrets.ApiKeySecretPatch{Sealed: sealed, Last4: "e-00", Fingerprint: "fp-rotated"}
		if _, perr := s.Patch(store.WithTenant(ctx, s.tenant), id, patch); perr != nil {
			return k, perr
		}
	}
	return k, err
}
