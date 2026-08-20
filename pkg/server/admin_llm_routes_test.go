package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/audit"
	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Platform LLM credentials routes: the super-admin surface that makes the
// deployment's own provider credential a DB row instead of a k8s secret.

// llmSpyKeyStore records the tenant each write carried — the value the
// real Mongo store stamps onto the row and filters reads by.
type llmSpyKeyStore struct {
	secrets.ApiKeyStore
	createTenant string
}

func (s *llmSpyKeyStore) Create(ctx context.Context, k secrets.ApiKey) error {
	s.createTenant, _ = store.TenantFromContext(ctx)
	return s.ApiKeyStore.Create(ctx, k)
}

// newAdminLLMServer boots a cloud-shaped server through the real
// New()/routes() path so the admin endpoints are reached through the
// production auth middleware. Returns the store spy, the raw stores, the
// audit store, the live server, and a super-admin + plain-member bearer.
func newAdminLLMServer(t *testing.T) (*llmSpyKeyStore, *secrets.MemoryOAuthStore, secrets.Sealer, audit.Store, *httptest.Server, string, string) {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	signer, err := auth.NewJWTSigner(base64.RawStdEncoding.EncodeToString(key), 15*time.Minute)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	svc, err := auth.NewService(auth.Config{
		Store:      identity.NewMemoryStore(),
		Sessions:   auth.NewMemorySessionStore(),
		Signer:     signer,
		SignupMode: auth.SignupOpen,
		RefreshTTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	spy := &llmSpyKeyStore{ApiKeyStore: secrets.NewMemoryApiKeyStore()}
	oauth := secrets.NewMemoryOAuthStore()
	auditStore := audit.NewMemoryStore()
	s := New(Config{
		WorkDir:                 t.TempDir(),
		SkipProjectRegistration: true,
		AuthService:             svc,
		AuthSigner:              signer,
		Audit:                   auditStore,
		ApiKeys:                 spy,
		OAuthForfait:            oauth,
		Sealer:                  sealer,
	}, iterlog.New(iterlog.LevelError, nil))

	adminTok, _, err := signer.IssueAccess(auth.Identity{UserID: "root", IsSuperAdmin: true, TeamID: "team-root"})
	if err != nil {
		t.Fatalf("issue admin token: %v", err)
	}
	userTok, _, err := signer.IssueAccess(auth.Identity{UserID: "u1", TeamID: "team-1", Role: identity.RoleAdmin})
	if err != nil {
		t.Fatalf("issue user token: %v", err)
	}
	hs := httptest.NewServer(s.handler)
	t.Cleanup(hs.Close)
	return spy, oauth, sealer, auditStore, hs, adminTok, userTok
}

func llmDo(t *testing.T, hs *httptest.Server, method, path, token, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, hs.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, out
}

// The full life of a platform key through the production route stack:
// created under the sentinel tenant, listed without its plaintext, rotated
// to a fresh sealed value, deleted.
func TestAdminLLM_PlatformKeyLifecycle(t *testing.T) {
	spy, _, sealer, _, hs, adminTok, _ := newAdminLLMServer(t)

	code, body := llmDo(t, hs, "POST", "/api/admin/llm/api-keys", adminTok,
		`{"provider":"anthropic","name":"prod","secret":"sk-ant-platform-v1"}`)
	if code != http.StatusOK {
		t.Fatalf("create: status=%d body=%s", code, body)
	}
	if spy.createTenant != secrets.PlatformTenantID {
		t.Fatalf("row stamped with tenant %q, want the sentinel %q — a run's platform resolve would find nothing",
			spy.createTenant, secrets.PlatformTenantID)
	}
	var created struct {
		ID    string `json:"id"`
		Last4 string `json:"last4"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode create: %v (%s)", err, body)
	}
	if created.Last4 == "" {
		t.Fatalf("create returned no last4: %s", body)
	}

	code, body = llmDo(t, hs, "GET", "/api/admin/llm/api-keys", adminTok, "")
	if code != http.StatusOK {
		t.Fatalf("list: status=%d body=%s", code, body)
	}
	if strings.Contains(string(body), "sk-ant-platform-v1") {
		t.Fatal("the list leaked the plaintext secret")
	}
	var list struct {
		Keys []struct {
			ID       string `json:"id"`
			Provider string `json:"provider"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Keys) != 1 || list.Keys[0].Provider != "anthropic" {
		t.Fatalf("list = %s, want the one platform key", body)
	}

	// Rotation: the stored sealed blob must open to the NEW value.
	code, body = llmDo(t, hs, "PATCH", "/api/admin/llm/api-keys/"+created.ID, adminTok,
		`{"secret":"sk-ant-platform-v2"}`)
	if code != http.StatusOK {
		t.Fatalf("rotate: status=%d body=%s", code, body)
	}
	rotated, err := spy.Get(store.WithTenant(context.Background(), secrets.PlatformTenantID), created.ID)
	if err != nil {
		t.Fatalf("re-read rotated key: %v", err)
	}
	pt, err := secrets.OpenApiKey(sealer, rotated)
	if err != nil {
		t.Fatalf("open rotated key: %v", err)
	}
	if string(pt) != "sk-ant-platform-v2" {
		t.Fatalf("rotated plaintext = %q, want the new value", pt)
	}

	if code, body = llmDo(t, hs, "DELETE", "/api/admin/llm/api-keys/"+created.ID, adminTok, ""); code != http.StatusNoContent {
		t.Fatalf("delete: status=%d body=%s", code, body)
	}
}

// The admin surface must never reach a TENANT's row by id: the sentinel
// scope is the boundary in both directions.
func TestAdminLLM_TenantKeyIsInvisibleToTheAdminSurface(t *testing.T) {
	spy, _, sealer, _, hs, adminTok, _ := newAdminLLMServer(t)
	id := secrets.NewApiKeyID()
	sealed, err := secrets.SealAPIKey(sealer, id, []byte("sk-tenant-own"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if err := spy.ApiKeyStore.Create(context.Background(), secrets.ApiKey{
		ID: id, ScopeTeamID: "team-1", Provider: secrets.ProviderAnthropic,
		Name: "tenant", SealedSecret: sealed, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed tenant key: %v", err)
	}

	if code, body := llmDo(t, hs, "DELETE", "/api/admin/llm/api-keys/"+id, adminTok, ""); code != http.StatusNotFound {
		t.Fatalf("delete of a tenant key: status=%d body=%s, want 404", code, body)
	}
	if _, err := spy.Get(store.WithTenant(context.Background(), "team-1"), id); err != nil {
		t.Fatal("the tenant's key was touched by the admin surface")
	}
	if code, body := llmDo(t, hs, "GET", "/api/admin/llm/api-keys", adminTok, ""); code != http.StatusOK || strings.Contains(string(body), id) {
		t.Fatalf("platform list exposes a tenant key: status=%d body=%s", code, body)
	}
}

// Platform credentials fund every tenant — managing them is a super-admin
// decision, and nobody below that (nor anonymous) may even list them.
func TestAdminLLM_NonSuperAdminIsRefused(t *testing.T) {
	_, _, _, _, hs, _, userTok := newAdminLLMServer(t)
	cases := []struct {
		method, path, token string
		want                int
	}{
		{"GET", "/api/admin/llm/api-keys", userTok, http.StatusForbidden},
		{"POST", "/api/admin/llm/api-keys", userTok, http.StatusForbidden},
		{"GET", "/api/admin/llm/oauth/connections", userTok, http.StatusForbidden},
		{"POST", "/api/admin/llm/oauth/claude_code/credentials", userTok, http.StatusForbidden},
		{"GET", "/api/admin/llm/api-keys", "", http.StatusUnauthorized},
		{"POST", "/api/admin/llm/oauth/claude_code/credentials", "", http.StatusUnauthorized},
	}
	for _, c := range cases {
		if code, body := llmDo(t, hs, c.method, c.path, c.token, "{}"); code != c.want {
			t.Errorf("%s %s (token=%q): status=%d want %d body=%s", c.method, c.path, c.token, code, c.want, body)
		}
	}
}

// The platform forfait paste path: the blob lands sealed under the
// reserved owner key — the exact record the publisher's platform tier and
// the background refresh worker read.
func TestAdminLLM_OAuthPasteStoresUnderThePlatformOwner(t *testing.T) {
	_, oauth, sealer, _, hs, adminTok, _ := newAdminLLMServer(t)

	code, body := llmDo(t, hs, "POST", "/api/admin/llm/oauth/claude_code/credentials", adminTok,
		`{"claudeAiOauth":{"accessToken":"sk-ant-platform-forfait"}}`)
	if code != http.StatusOK {
		t.Fatalf("paste: status=%d body=%s", code, body)
	}

	rec, err := oauth.Get(context.Background(), secrets.PlatformOwnerKey, secrets.OAuthKindClaudeCode)
	if err != nil {
		t.Fatalf("no record under the platform owner key: %v", err)
	}
	blob, err := secrets.OpenOAuthPayload(sealer, secrets.PlatformOwnerKey, secrets.OAuthKindClaudeCode, rec.SealedPayload)
	if err != nil {
		t.Fatalf("unseal: %v", err)
	}
	if !strings.Contains(string(blob), "sk-ant-platform-forfait") {
		t.Fatalf("stored blob = %s, want the pasted credentials", blob)
	}

	code, body = llmDo(t, hs, "GET", "/api/admin/llm/oauth/connections", adminTok, "")
	if code != http.StatusOK || !strings.Contains(string(body), "claude_code") {
		t.Fatalf("connections list: status=%d body=%s", code, body)
	}
	if strings.Contains(string(body), "sk-ant-platform-forfait") {
		t.Fatal("the connections list leaked the token")
	}

	if code, body = llmDo(t, hs, "DELETE", "/api/admin/llm/oauth/claude_code", adminTok, ""); code >= 300 {
		t.Fatalf("disconnect: status=%d body=%s", code, body)
	}
	if _, err := oauth.Get(context.Background(), secrets.PlatformOwnerKey, secrets.OAuthKindClaudeCode); err == nil {
		t.Fatal("record still present after disconnect")
	}
}

// The platform audit log is what one uses to check on super-admins, so it
// must record only what actually happened: a REJECTED connect or a delete
// of a credential that was never there must forge no "connected"/"deleted"
// event. Regression for the audit-on-failure class (the audit used to fire
// at the caller, unconditionally after the delegate helper returned).
func TestAdminLLM_OAuthAuditsOnlyRealMutations(t *testing.T) {
	_, oauth, _, auditStore, hs, adminTok, _ := newAdminLLMServer(t)
	ctx := context.Background()

	platformOAuthEvents := func() int {
		evs, err := auditStore.ListPlatform(ctx, audit.Page{})
		if err != nil {
			t.Fatalf("list platform audit: %v", err)
		}
		n := 0
		for _, e := range evs {
			if strings.HasPrefix(e.Action, "platform.llm_oauth.") {
				n++
			}
		}
		return n
	}

	// A rejected paste (empty body → 400) must not audit "connected".
	if code, _ := llmDo(t, hs, "POST", "/api/admin/llm/oauth/claude_code/credentials", adminTok, ""); code != http.StatusBadRequest {
		t.Fatalf("empty paste: status=%d, want 400", code)
	}
	// A delete of an absent connection (404→204 no-op) must not audit "deleted".
	if code, _ := llmDo(t, hs, "DELETE", "/api/admin/llm/oauth/codex", adminTok, ""); code >= 300 {
		t.Fatalf("delete absent: status=%d, want 2xx no-op", code)
	}
	if n := platformOAuthEvents(); n != 0 {
		t.Fatalf("a rejected connect / no-op delete forged %d platform oauth event(s)", n)
	}

	// A real connect, then a real delete, DO audit — exactly one each.
	if code, _ := llmDo(t, hs, "POST", "/api/admin/llm/oauth/claude_code/credentials", adminTok,
		`{"claudeAiOauth":{"accessToken":"sk-ant-real"}}`); code != http.StatusOK {
		t.Fatal("real connect failed")
	}
	// Guard the fixture: the connect really landed under the platform owner.
	if _, err := oauth.Get(ctx, secrets.PlatformOwnerKey, secrets.OAuthKindClaudeCode); err != nil {
		t.Fatalf("connect did not store: %v", err)
	}
	if code, _ := llmDo(t, hs, "DELETE", "/api/admin/llm/oauth/claude_code", adminTok, ""); code >= 300 {
		t.Fatal("real delete failed")
	}
	if n := platformOAuthEvents(); n != 2 {
		t.Fatalf("real connect+delete produced %d platform oauth events, want 2", n)
	}
}
