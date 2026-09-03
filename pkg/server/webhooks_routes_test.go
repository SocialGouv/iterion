package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/gitlab"
)

func newWebhookTestServer(t *testing.T) *Server {
	t.Helper()
	s := newOrgTestServer(t)
	s.webhookConfigs = webhooks.NewMemoryConfigStore()
	s.webhookDeliveries = webhooks.NewMemoryDeliveryStore()
	s.webhookCounter = webhooks.NewMemoryCounter()
	s.authLimiter = newAuthRateLimiter()
	// Wire a sealer so hmac-mode webhook tests can seal/verify
	// signatures. Same construction as the BYOK + bindings tests.
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	sealer, err := secrets.NewAESGCMSealer(key)
	if err != nil {
		t.Fatal(err)
	}
	s.sealer = sealer
	// Allow-all conversational gate: the real gate needs a forge token
	// + live GitLab API (loop-guard, reply-in-thread classification, role
	// authz) — exercised by the pkg/webhooks/gitlab unit tests; handler
	// tests pin the flow around it. The default stub authorizes a /revi
	// command (replyInThread=false); tests targeting the reply-in-thread
	// route override this seam to return replyInThread=true.
	s.webhookNoteGate = func(context.Context, webhooks.Config, gitlab.ParsedNote, string) (bool, bool, string, string, error) {
		return true, false, "", "test-gate", nil
	}
	return s
}

func whReq(ctx context.Context, method, path, body, teamID, webhookID string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	r = r.WithContext(ctx)
	if teamID != "" {
		r.SetPathValue("id", teamID)
	}
	if webhookID != "" {
		r.SetPathValue("webhook_id", webhookID)
	}
	return r
}

func TestHandleCreateWebhook_TokenOnceAndScope(t *testing.T) {
	s := newWebhookTestServer(t)
	ctx := superAdminCtx()

	// missing bot scope → 400
	w := httptest.NewRecorder()
	s.handleCreateWebhook(w, whReq(ctx, "POST", "/api/teams/t1/webhooks", `{"name":"gl"}`, "t1", ""))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing bot scope: code=%d body=%s", w.Code, w.Body.String())
	}

	// bare "*" without wildcard_bots → 400
	w = httptest.NewRecorder()
	s.handleCreateWebhook(w, whReq(ctx, "POST", "/api/teams/t1/webhooks", `{"name":"gl","bot_ids":["*"]}`, "t1", ""))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bare wildcard: code=%d", w.Code)
	}

	// valid create → 201, token present once + last4
	w = httptest.NewRecorder()
	s.handleCreateWebhook(w, whReq(ctx, "POST", "/api/teams/t1/webhooks", `{"name":"gl","bot_ids":["review-pr"]}`, "t1", ""))
	if w.Code != http.StatusCreated {
		t.Fatalf("create: code=%d body=%s", w.Code, w.Body.String())
	}
	var created webhookWithToken
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Token == "" || !strings.HasPrefix(created.Token, webhooks.TokenPrefix) {
		t.Fatalf("token not returned once: %q", created.Token)
	}
	if created.Config.TokenLast4 == "" || created.Config.TenantID != "t1" {
		t.Fatalf("bad config: %+v", created.Config)
	}

	// GET never leaks the token (token_hash is json:"-").
	w = httptest.NewRecorder()
	s.handleGetWebhook(w, whReq(ctx, "GET", "/api/teams/t1/webhooks/"+created.Config.ID, "", "t1", created.Config.ID))
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), created.Token) || strings.Contains(w.Body.String(), "token_hash") {
		t.Fatalf("GET leaked token material: code=%d body=%s", w.Code, w.Body.String())
	}
}

// TestHandleCreateWebhook_ProviderEnum pins the closed provider enum:
// gitlab|github|forgejo|generic accepted, anything else → 400.
func TestHandleCreateWebhook_ProviderEnum(t *testing.T) {
	s := newWebhookTestServer(t)
	ctx := superAdminCtx()
	for _, p := range []string{"gitlab", "github", "forgejo", "generic"} {
		w := httptest.NewRecorder()
		body := `{"name":"x","bot_ids":["review-pr"],"provider":"` + p + `"}`
		s.handleCreateWebhook(w, whReq(ctx, "POST", "/api/teams/t1/webhooks", body, "t1", ""))
		if w.Code != http.StatusCreated {
			t.Fatalf("provider=%s: code=%d body=%s", p, w.Code, w.Body.String())
		}
	}
	// unknown provider → 400
	w := httptest.NewRecorder()
	s.handleCreateWebhook(w, whReq(ctx, "POST", "/api/teams/t1/webhooks", `{"name":"x","bot_ids":["b"],"provider":"slack"}`, "t1", ""))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown provider should 400: code=%d", w.Code)
	}
}

// TestHandleCreateWebhook_SignModeAutoAndSeal pins the sign-mode
// dispatch + the HMAC seal-on-create contract.
//
//   - GitHub auto-picks SignModeHMAC; HMACSecretSealed is populated and
//     decrypts to the same plaintext returned to the operator.
//   - GitLab auto-picks SignModeToken; no sealed secret.
//   - Generic accepts an explicit hmac override.
//   - GitHub rejects an explicit "token" override (would break body auth).
func TestHandleCreateWebhook_SignModeAutoAndSeal(t *testing.T) {
	s := newWebhookTestServer(t)
	ctx := superAdminCtx()

	// GitHub → hmac, sealed plaintext matches returned token. We read
	// the sealed blob from the store directly: HMACSecretSealed is
	// json:"-" so the create-response body never leaks it.
	w := httptest.NewRecorder()
	s.handleCreateWebhook(w, whReq(ctx, "POST", "/api/teams/t1/webhooks", `{"name":"gh","provider":"github","bot_ids":["review-pr"]}`, "t1", ""))
	if w.Code != http.StatusCreated {
		t.Fatalf("github create: code=%d body=%s", w.Code, w.Body.String())
	}
	var created webhookWithToken
	json.Unmarshal(w.Body.Bytes(), &created)
	if created.Config.SignMode != webhooks.SignModeHMAC {
		t.Fatalf("github should auto-pick hmac, got %q", created.Config.SignMode)
	}
	stored, err := s.webhookConfigs.Get(context.Background(), created.Config.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.HMACSecretSealed) == 0 {
		t.Fatal("github should seal the hmac plaintext")
	}
	opened, err := s.sealer.Open(stored.HMACSecretSealed, []byte("webhook_hmac_secret:"+stored.ID))
	if err != nil || string(opened) != created.Token {
		t.Fatalf("sealed hmac plaintext mismatch: err=%v opened=%q token=%q", err, string(opened), created.Token)
	}

	// GitLab → token, no sealed secret. Use a fresh struct so leftover
	// fields from the previous unmarshal (SignMode default omitempty
	// "" is fine, but the safer pattern is a brand-new value).
	w = httptest.NewRecorder()
	s.handleCreateWebhook(w, whReq(ctx, "POST", "/api/teams/t1/webhooks", `{"name":"gl","provider":"gitlab","bot_ids":["review-pr"]}`, "t1", ""))
	if w.Code != http.StatusCreated {
		t.Fatalf("gitlab create: %d", w.Code)
	}
	var createdGL webhookWithToken
	json.Unmarshal(w.Body.Bytes(), &createdGL)
	if createdGL.Config.SignMode != webhooks.SignModeToken {
		t.Fatalf("gitlab should be token mode: %+v", createdGL.Config)
	}
	storedGL, _ := s.webhookConfigs.Get(context.Background(), createdGL.Config.ID)
	if len(storedGL.HMACSecretSealed) != 0 {
		t.Fatalf("gitlab should NOT seal a hmac plaintext: %v", storedGL.HMACSecretSealed)
	}

	// Generic + explicit hmac → seals
	w = httptest.NewRecorder()
	s.handleCreateWebhook(w, whReq(ctx, "POST", "/api/teams/t1/webhooks", `{"name":"gn","provider":"generic","sign_mode":"hmac","bot_ids":["review-pr"]}`, "t1", ""))
	if w.Code != http.StatusCreated {
		t.Fatalf("generic hmac: %d body=%s", w.Code, w.Body.String())
	}
	var createdGen webhookWithToken
	json.Unmarshal(w.Body.Bytes(), &createdGen)
	if createdGen.Config.SignMode != webhooks.SignModeHMAC {
		t.Fatalf("generic hmac should be hmac: %+v", createdGen.Config)
	}
	storedGen, _ := s.webhookConfigs.Get(context.Background(), createdGen.Config.ID)
	if len(storedGen.HMACSecretSealed) == 0 {
		t.Fatal("generic hmac should seal the plaintext")
	}

	// GitHub + explicit token override → 400 (sign mode is fixed per forge)
	w = httptest.NewRecorder()
	s.handleCreateWebhook(w, whReq(ctx, "POST", "/api/teams/t1/webhooks", `{"name":"gh2","provider":"github","sign_mode":"token","bot_ids":["b"]}`, "t1", ""))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("github with explicit token should 400: %d", w.Code)
	}
}

// TestHandleRotateWebhook_ResealsHMAC pins that rotating an hmac webhook
// produces a fresh sealed plaintext aligned with the freshly minted
// token.
func TestHandleRotateWebhook_ResealsHMAC(t *testing.T) {
	s := newWebhookTestServer(t)
	ctx := superAdminCtx()
	// Create an hmac webhook to get a baseline sealed blob (read from
	// the store — HMACSecretSealed is json:"-").
	w := httptest.NewRecorder()
	s.handleCreateWebhook(w, whReq(ctx, "POST", "/api/teams/t1/webhooks", `{"name":"gh","provider":"github","bot_ids":["b"]}`, "t1", ""))
	var created webhookWithToken
	json.Unmarshal(w.Body.Bytes(), &created)
	storedBefore, _ := s.webhookConfigs.Get(context.Background(), created.Config.ID)
	originalSealed := append([]byte{}, storedBefore.HMACSecretSealed...)
	originalToken := created.Token

	w = httptest.NewRecorder()
	s.handleRotateWebhook(w, whReq(ctx, "POST", "/api/teams/t1/webhooks/"+created.Config.ID+"/rotate", "", "t1", created.Config.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("rotate: %d", w.Code)
	}
	var rotated webhookWithToken
	json.Unmarshal(w.Body.Bytes(), &rotated)
	if rotated.Token == originalToken {
		t.Fatal("rotate should mint a new token")
	}
	storedAfter, _ := s.webhookConfigs.Get(context.Background(), rotated.Config.ID)
	if len(storedAfter.HMACSecretSealed) == 0 || string(storedAfter.HMACSecretSealed) == string(originalSealed) {
		t.Fatal("rotate should reseal the hmac plaintext")
	}
	opened, err := s.sealer.Open(storedAfter.HMACSecretSealed, []byte("webhook_hmac_secret:"+storedAfter.ID))
	if err != nil || string(opened) != rotated.Token {
		t.Fatalf("rotated sealed != new token: err=%v opened=%q token=%q", err, string(opened), rotated.Token)
	}
}

func TestHandleCreateWebhook_WildcardRequiresFlag(t *testing.T) {
	s := newWebhookTestServer(t)
	ctx := superAdminCtx()
	w := httptest.NewRecorder()
	s.handleCreateWebhook(w, whReq(ctx, "POST", "/api/teams/t1/webhooks", `{"name":"any","wildcard_bots":true}`, "t1", ""))
	if w.Code != http.StatusCreated {
		t.Fatalf("wildcard create: code=%d body=%s", w.Code, w.Body.String())
	}
	var created webhookWithToken
	json.Unmarshal(w.Body.Bytes(), &created)
	if !created.Config.WildcardBots || len(created.Config.BotIDs) != 1 || created.Config.BotIDs[0] != "*" {
		t.Fatalf("wildcard not normalised: %+v", created.Config)
	}
	if !created.Config.AllowsBot("anything") {
		t.Fatal("wildcard should allow any bot")
	}
}

func TestHandleRotateWebhook(t *testing.T) {
	s := newWebhookTestServer(t)
	ctx := superAdminCtx()
	// seed a config with a known token
	pt, hash, last4, fp, _ := webhooks.MintToken()
	cfg := webhooks.Config{ID: "w1", TenantID: "t1", Provider: webhooks.ProviderGitLab, Enabled: true,
		TokenHash: hash, TokenLast4: last4, Fingerprint: fp, BotIDs: []string{"review-pr"}, CreatedAt: time.Now()}
	if err := s.webhookConfigs.Create(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.handleRotateWebhook(w, whReq(ctx, "POST", "/api/teams/t1/webhooks/w1/rotate", "", "t1", "w1"))
	if w.Code != http.StatusOK {
		t.Fatalf("rotate: code=%d", w.Code)
	}
	var rotated webhookWithToken
	json.Unmarshal(w.Body.Bytes(), &rotated)
	if rotated.Token == "" || rotated.Token == pt {
		t.Fatalf("rotate should mint a fresh token, got %q", rotated.Token)
	}
	stored, _ := s.webhookConfigs.Get(ctx, "w1")
	if webhooks.VerifyToken(pt, stored.TokenHash) {
		t.Fatal("old token must no longer verify after rotate")
	}
	if !webhooks.VerifyToken(rotated.Token, stored.TokenHash) {
		t.Fatal("new token must verify after rotate")
	}
}

// webhookAuth admission: drive the middleware with a recording next.
func runWebhookAuth(s *Server, cfgID, token string) (status int, ran bool, tenant, gotCfg string) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ran = true
		if c, ok := webhookConfigFromContext(r.Context()); ok {
			gotCfg = c.ID
		}
		if id, ok := auth.FromContext(r.Context()); ok {
			tenant = id.TeamID
		}
		w.WriteHeader(http.StatusOK)
	})
	h := s.webhookAuth(webhooks.ProviderGitLab, next)
	r := httptest.NewRequest("POST", "/api/webhooks/gitlab/"+cfgID, strings.NewReader("{}"))
	r.SetPathValue("id", cfgID)
	if token != "" {
		r.Header.Set("X-Gitlab-Token", token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code, ran, tenant, gotCfg
}

func TestWebhookAuth_Admission(t *testing.T) {
	s := newWebhookTestServer(t)
	pt, hash, last4, fp, _ := webhooks.MintToken()
	cfg := webhooks.Config{ID: "w1", TenantID: "t1", Provider: webhooks.ProviderGitLab, Enabled: true,
		TokenHash: hash, TokenLast4: last4, Fingerprint: fp, BotIDs: []string{"review-pr"},
		RateLimit: webhooks.Rate{Rate: 100, Burst: 100}, CreatedAt: time.Now()}
	if err := s.webhookConfigs.Create(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}

	// valid token → next runs, tenant + config stamped
	if code, ran, tenant, gotCfg := runWebhookAuth(s, "w1", pt); code != http.StatusOK || !ran || tenant != "t1" || gotCfg != "w1" {
		t.Fatalf("valid: code=%d ran=%v tenant=%q cfg=%q", code, ran, tenant, gotCfg)
	}
	// bad token → 401, next not run
	if code, ran, _, _ := runWebhookAuth(s, "w1", pt+"x"); code != http.StatusUnauthorized || ran {
		t.Fatalf("bad token: code=%d ran=%v", code, ran)
	}
	// missing token → 401
	if code, _, _, _ := runWebhookAuth(s, "w1", ""); code != http.StatusUnauthorized {
		t.Fatalf("missing token: code=%d", code)
	}
	// unknown id → 401 (not probeable)
	if code, _, _, _ := runWebhookAuth(s, "ghost", pt); code != http.StatusUnauthorized {
		t.Fatalf("unknown id: code=%d", code)
	}
}

// TestWebhookAuth_HMACModeSkipsTokenCheck pins the SignModeHMAC branch:
// a webhook configured for HMAC signing MUST NOT require a header
// token. Body verification happens in the provider handler — the
// middleware just admits the call. Other admission checks (enabled,
// rate, quota, suspend) still apply, but no token header is read.
func TestWebhookAuth_HMACModeSkipsTokenCheck(t *testing.T) {
	s := newWebhookTestServer(t)
	cfg := webhooks.Config{
		ID: "ghub", TenantID: "t1", Provider: webhooks.ProviderGitHub,
		SignMode: webhooks.SignModeHMAC, Enabled: true, BotIDs: []string{"review-pr"},
		// TokenHash deliberately empty: an hmac-mode webhook may have
		// a TokenHash mirror (for management UX) but the middleware
		// must NOT consult it.
		RateLimit: webhooks.Rate{Rate: 100, Burst: 100}, CreatedAt: time.Now(),
	}
	if err := s.webhookConfigs.Create(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	var ran bool
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		ran = true
		w.WriteHeader(http.StatusOK)
	})
	h := s.webhookAuth(webhooks.ProviderGitHub, next)
	r := httptest.NewRequest("POST", "/api/webhooks/github/ghub", strings.NewReader(`{}`))
	r.SetPathValue("id", "ghub")
	// No X-Gitlab-Token / X-Iterion-Webhook-Token / etc.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !ran {
		t.Fatalf("hmac-mode admission failed: code=%d ran=%v body=%s", w.Code, ran, w.Body.String())
	}
}

func TestWebhookAuth_DisabledAndRateLimitAndQuota(t *testing.T) {
	s := newWebhookTestServer(t)
	ctx := context.Background()

	// disabled → 410
	pt, hash, l4, fp, _ := webhooks.MintToken()
	s.webhookConfigs.Create(ctx, webhooks.Config{ID: "off", TenantID: "t1", Provider: webhooks.ProviderGitLab,
		Enabled: false, TokenHash: hash, TokenLast4: l4, Fingerprint: fp, BotIDs: []string{"x"}, RateLimit: webhooks.Rate{Rate: 100, Burst: 100}})
	if code, ran, _, _ := runWebhookAuth(s, "off", pt); code != http.StatusGone || ran {
		t.Fatalf("disabled: code=%d ran=%v", code, ran)
	}

	// rate limit: burst 1 → first ok, second 429
	pt2, h2, l42, fp2, _ := webhooks.MintToken()
	s.webhookConfigs.Create(ctx, webhooks.Config{ID: "rl", TenantID: "t1", Provider: webhooks.ProviderGitLab,
		Enabled: true, TokenHash: h2, TokenLast4: l42, Fingerprint: fp2, BotIDs: []string{"x"}, RateLimit: webhooks.Rate{Rate: 0.0001, Burst: 1}})
	if code, _, _, _ := runWebhookAuth(s, "rl", pt2); code != http.StatusOK {
		t.Fatalf("rl first: code=%d", code)
	}
	if code, _, _, _ := runWebhookAuth(s, "rl", pt2); code != http.StatusTooManyRequests {
		t.Fatalf("rl second should be 429: code=%d", code)
	}

	// monthly quota: per-webhook limit 1, high rate so RL doesn't trip
	pt3, h3, l43, fp3, _ := webhooks.MintToken()
	s.webhookConfigs.Create(ctx, webhooks.Config{ID: "mq", TenantID: "t2", Provider: webhooks.ProviderGitLab,
		Enabled: true, TokenHash: h3, TokenLast4: l43, Fingerprint: fp3, BotIDs: []string{"x"},
		RateLimit: webhooks.Rate{Rate: 100, Burst: 100}, MonthlyCallLimit: 1})
	if code, _, _, _ := runWebhookAuth(s, "mq", pt3); code != http.StatusOK {
		t.Fatalf("mq first: code=%d", code)
	}
	if code, _, _, _ := runWebhookAuth(s, "mq", pt3); code != http.StatusTooManyRequests {
		t.Fatalf("mq second should be 429 (monthly quota): code=%d", code)
	}
}

// TestWebhookHoldLabelsRoundTrip pins the config INGRESS for hold_labels.
//
// The suppression matcher, the four wired lanes and the docs were all correct,
// but the field was declared on webhooks.Config and read by
// suppressedByHoldLabel while nothing ever WROTE it: not the create builder,
// not the PATCH applier, not the forge provisioning path. HeldByLabel
// short-circuits on an empty set, so the operator's documented escape hatch to
// pause automation on one PR was permanently a no-op, reachable only by a
// direct Mongo write.
func TestWebhookHoldLabelsRoundTrip(t *testing.T) {
	s := newWebhookTestServer(t)
	ctx := superAdminCtx()

	w := httptest.NewRecorder()
	body := `{"name":"gl","bot_ids":["review-pr"],"hold_labels":["iterion:hold","do-not-automate"]}`
	s.handleCreateWebhook(w, whReq(ctx, "POST", "/api/teams/t1/webhooks", body, "t1", ""))
	if w.Code != http.StatusCreated {
		t.Fatalf("create: code=%d body=%s", w.Code, w.Body.String())
	}
	var created struct {
		Config webhooks.Config `json:"config"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if !slices.Equal(created.Config.HoldLabels, []string{"iterion:hold", "do-not-automate"}) {
		t.Fatalf("hold_labels dropped on create: %v", created.Config.HoldLabels)
	}

	// PATCH replaces the set...
	w = httptest.NewRecorder()
	s.handleUpdateWebhook(w, whReq(ctx, "PATCH", "/api/teams/t1/webhooks/"+created.Config.ID,
		`{"hold_labels":["paused"]}`, "t1", created.Config.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("patch: code=%d body=%s", w.Code, w.Body.String())
	}
	stored, err := s.webhookConfigs.Get(context.Background(), created.Config.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !slices.Equal(stored.HoldLabels, []string{"paused"}) {
		t.Fatalf("hold_labels not applied on patch: %v", stored.HoldLabels)
	}

	// ...an omitted field leaves it untouched (a PATCH of something else must
	// not silently unpause every held PR)...
	w = httptest.NewRecorder()
	s.handleUpdateWebhook(w, whReq(ctx, "PATCH", "/api/teams/t1/webhooks/"+created.Config.ID,
		`{"name":"renamed"}`, "t1", created.Config.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("patch name: code=%d body=%s", w.Code, w.Body.String())
	}
	if stored, err = s.webhookConfigs.Get(context.Background(), created.Config.ID); err != nil {
		t.Fatalf("get: %v", err)
	}
	if !slices.Equal(stored.HoldLabels, []string{"paused"}) {
		t.Fatalf("an unrelated patch cleared hold_labels: %v", stored.HoldLabels)
	}

	// ...and an explicit empty list is how the hold is turned off.
	w = httptest.NewRecorder()
	s.handleUpdateWebhook(w, whReq(ctx, "PATCH", "/api/teams/t1/webhooks/"+created.Config.ID,
		`{"hold_labels":[]}`, "t1", created.Config.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("patch clear: code=%d body=%s", w.Code, w.Body.String())
	}
	if stored, err = s.webhookConfigs.Get(context.Background(), created.Config.ID); err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(stored.HoldLabels) != 0 {
		t.Fatalf("hold_labels not clearable: %v", stored.HoldLabels)
	}
}

// TestWebhookReviewRequestLoginsPatch pins the PATCH ingress for
// review_request_logins. A managed webhook is created by the orchestrator
// (Provision), never by POST — so PATCH is the ONLY writer that can arm the
// re-request lane on the configs the feature exists for. Create-only would
// make the field dead on every provisioned repo.
func TestWebhookReviewRequestLoginsPatch(t *testing.T) {
	s := newWebhookTestServer(t)
	ctx := superAdminCtx()

	w := httptest.NewRecorder()
	body := `{"name":"gh","bot_ids":["review-pr"],"review_request_logins":["iterion-bot"]}`
	s.handleCreateWebhook(w, whReq(ctx, "POST", "/api/teams/t1/webhooks", body, "t1", ""))
	if w.Code != http.StatusCreated {
		t.Fatalf("create: code=%d body=%s", w.Code, w.Body.String())
	}
	var created struct {
		Config webhooks.Config `json:"config"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if !slices.Equal(created.Config.ReviewRequestLogins, []string{"iterion-bot"}) {
		t.Fatalf("review_request_logins dropped on create: %v", created.Config.ReviewRequestLogins)
	}

	// PATCH replaces the set — arming a provisioned webhook after the fact.
	w = httptest.NewRecorder()
	s.handleUpdateWebhook(w, whReq(ctx, "PATCH", "/api/teams/t1/webhooks/"+created.Config.ID,
		`{"review_request_logins":["revu-bot"]}`, "t1", created.Config.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("patch: code=%d body=%s", w.Code, w.Body.String())
	}
	stored, err := s.webhookConfigs.Get(context.Background(), created.Config.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !slices.Equal(stored.ReviewRequestLogins, []string{"revu-bot"}) {
		t.Fatalf("review_request_logins not applied on patch: %v", stored.ReviewRequestLogins)
	}

	// An omitted field leaves it untouched (a PATCH of something else must
	// not silently disarm the button)...
	w = httptest.NewRecorder()
	s.handleUpdateWebhook(w, whReq(ctx, "PATCH", "/api/teams/t1/webhooks/"+created.Config.ID,
		`{"name":"renamed"}`, "t1", created.Config.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("patch name: code=%d body=%s", w.Code, w.Body.String())
	}
	if stored, err = s.webhookConfigs.Get(context.Background(), created.Config.ID); err != nil {
		t.Fatalf("get: %v", err)
	}
	if !slices.Equal(stored.ReviewRequestLogins, []string{"revu-bot"}) {
		t.Fatalf("an unrelated patch cleared review_request_logins: %v", stored.ReviewRequestLogins)
	}

	// ...and an explicit empty list disarms it.
	w = httptest.NewRecorder()
	s.handleUpdateWebhook(w, whReq(ctx, "PATCH", "/api/teams/t1/webhooks/"+created.Config.ID,
		`{"review_request_logins":[]}`, "t1", created.Config.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("patch clear: code=%d body=%s", w.Code, w.Body.String())
	}
	if stored, err = s.webhookConfigs.Get(context.Background(), created.Config.ID); err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(stored.ReviewRequestLogins) != 0 {
		t.Fatalf("review_request_logins not clearable: %v", stored.ReviewRequestLogins)
	}
}
