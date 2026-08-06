package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/identity"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/valkey"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// Valkey exists for exactly one promise: a cloud deployment runs SEVERAL
// server replicas behind a load balancer, and the ephemeral state one
// replica writes must be honoured by whichever replica the next request
// lands on. Three state kinds ride it — the forge OAuth/CSRF pending
// state, the per-run board-MCP tokens, and the auth rate-limit buckets —
// and each has a failure mode that only shows up ACROSS replicas: an
// OAuth callback that dies with "state expired or invalid", a sandboxed
// bot whose every board write 401s, a login brute-force budget silently
// multiplied by the replica count.
//
// So the test boots TWO independent servers over ONE Valkey (miniredis)
// through the real New() wiring — nothing hand-injects a store — starts
// each flow on replica A and finishes it on replica B.

// twoReplicas is a pair of servers sharing one Valkey plus the durable
// stores a real deployment shares (Mongo/identity/board), each with its
// own client + workdir the way two pods would.
type twoReplicas struct {
	a, b       *Server
	hsA, hsB   *httptest.Server
	board      *native.Store
	conns      forge.ConnectionStore
	oauthApps  forge.OAuthAppStore
	sealer     secrets.Sealer
	adminToken string
}

func newTwoReplicas(t *testing.T) *twoReplicas {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	signer, err := auth.NewJWTSigner(base64.RawStdEncoding.EncodeToString(key), 15*time.Minute)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	// One identity store + one session store: replicas share Mongo in cloud.
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
	sealKey := make([]byte, 32)
	if _, err := rand.Read(sealKey); err != nil {
		t.Fatalf("rand: %v", err)
	}
	sealer, err := secrets.NewAESGCMSealer(sealKey)
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	board, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("board store: %v", err)
	}
	shared := struct {
		conns     forge.ConnectionStore
		integs    forge.RepoIntegrationStore
		oauthApps forge.OAuthAppStore
		hooks     webhooks.ConfigStore
		gsecrets  secrets.GenericSecretStore
	}{
		conns:     forge.NewMemoryConnectionStore(),
		integs:    forge.NewMemoryRepoIntegrationStore(),
		oauthApps: forge.NewMemoryOAuthAppStore(),
		hooks:     webhooks.NewMemoryConfigStore(),
		gsecrets:  secrets.NewMemoryGenericSecretStore(),
	}

	newReplica := func() *Server {
		// A per-replica client, like a per-pod connection pool.
		rc, err := valkey.New(valkey.Options{URL: "redis://" + mr.Addr()})
		if err != nil {
			t.Fatalf("valkey client: %v", err)
		}
		t.Cleanup(func() { _ = rc.Close() })
		return New(Config{
			WorkDir:                 t.TempDir(),
			Bind:                    "127.0.0.1",
			SkipProjectRegistration: true,
			AuthService:             svc,
			AuthSigner:              signer,
			NativeTrackerStore:      board,
			ForgeConnections:        shared.conns,
			ForgeIntegrations:       shared.integs,
			ForgeOAuthApps:          shared.oauthApps,
			WebhookConfigs:          shared.hooks,
			GenericSecrets:          shared.gsecrets,
			Sealer:                  sealer,
			Redis:                   rc,
		}, iterlog.New(iterlog.LevelError, nil))
	}
	a, b := newReplica(), newReplica()
	// Guard the premise: without the Valkey-backed stores selected by the
	// real wiring, every assertion below would be about two private
	// in-memory maps that happen to be seeded identically.
	for i, s := range []*Server{a, b} {
		if _, ok := s.boardMCPTokens.(*valkeyBoardMCPTokenStore); !ok {
			t.Fatalf("replica %d did not select the Valkey board-token store: %T", i, s.boardMCPTokens)
		}
		if _, ok := s.forgeStates.(*valkeyForgeStateStore); !ok {
			t.Fatalf("replica %d did not select the Valkey forge-state store: %T", i, s.forgeStates)
		}
		if _, ok := s.authLimiter.(*valkeyAuthRateLimiter); !ok {
			t.Fatalf("replica %d did not select the Valkey rate limiter: %T", i, s.authLimiter)
		}
	}
	hsA := httptest.NewServer(a.handler)
	hsB := httptest.NewServer(b.handler)
	t.Cleanup(hsA.Close)
	t.Cleanup(hsB.Close)

	adminTok, _, err := signer.IssueAccess(auth.Identity{UserID: "root", IsSuperAdmin: true})
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	return &twoReplicas{
		a: a, b: b, hsA: hsA, hsB: hsB,
		board: board, conns: shared.conns, oauthApps: shared.oauthApps,
		sealer: sealer, adminToken: adminTok,
	}
}

func replicaPost(t *testing.T, hs *httptest.Server, path, token string, body any) (int, []byte, *http.Response) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, hs.URL+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out, resp
}

// A run's board-MCP token is minted by the replica that launched it; the
// bot's tool calls are load-balanced anywhere. The grant — and its
// capability set — must hold on the other replica.
func TestValkeyCrossReplica_BoardTokenMintedOnAAuthorizesWriteOnB(t *testing.T) {
	r := newTwoReplicas(t)

	if err := r.a.BoardMCPTokens().Register("run-tok", []string{"board.create", "board.read"}, ""); err != nil {
		t.Fatalf("register on replica A: %v", err)
	}

	// The board MCP transport authenticates with the run token header.
	boardCall := func(token, payload string) (int, []byte) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, r.hsB.URL+"/api/v1/mcp/board", strings.NewReader(payload))
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		req.Header.Set("X-Iterion-Run", token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("board call: %v", err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, out
	}

	status, body := boardCall("run-tok", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"create_issue","arguments":{"title":"written through replica B"}}}`)
	if status != http.StatusOK {
		t.Fatalf("token minted on A rejected by B: status=%d body=%s", status, body)
	}
	var rpc struct {
		Result struct {
			Content []map[string]any `json:"content"`
			IsError bool             `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &rpc); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if rpc.Result.IsError {
		t.Fatalf("create_issue on replica B failed: %+v", rpc.Result.Content)
	}
	var iss native.Issue
	if err := json.Unmarshal([]byte(rpc.Result.Content[0]["text"].(string)), &iss); err != nil {
		t.Fatalf("decode issue: %v", err)
	}
	got, _ := r.board.Get(iss.ID)
	if got == nil || got.Title != "written through replica B" {
		t.Fatalf("the card never landed on the board: %+v", got)
	}

	// The grant's capability set travels too: board.move was not granted,
	// so replica B must refuse transition_issue even though the token
	// authenticates — a grant that widened in transit would be worse than
	// one that vanished.
	_, moved := boardCall("run-tok", fmt.Sprintf(
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"transition_issue","arguments":{"id":%q,"to":"done"}}}`, iss.ID))
	var refusal struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(moved, &refusal); err != nil {
		t.Fatalf("decode transition response: %v (%s)", err, moved)
	}
	if refusal.Error == nil || !strings.Contains(refusal.Error.Message, "board.move") {
		t.Fatalf("ungranted board.move was not refused on replica B: %s", moved)
	}
	after, _ := r.board.Get(iss.ID)
	if after == nil || after.State == "done" {
		t.Fatalf("ungranted move actually moved the card: %+v", after)
	}

	// Revoking on A is immediately effective on B.
	r.a.BoardMCPTokens().Revoke("run-tok")
	if status, body := boardCall("run-tok", `{"jsonrpc":"2.0","id":3,"method":"initialize"}`); status != http.StatusUnauthorized {
		t.Fatalf("token revoked on A still accepted by B: status=%d body=%s", status, body)
	}
}

// Login brute-force protection is a budget over the whole deployment, not
// per pod: five failed attempts on replica A must exhaust the bucket the
// sixth attempt hits on replica B.
func TestValkeyCrossReplica_LoginRateBudgetIsSharedNotPerPod(t *testing.T) {
	r := newTwoReplicas(t)
	creds := map[string]any{"email": "victim@example.com", "password": "wrong"}

	for i := 0; i < 5; i++ {
		code, body, _ := replicaPost(t, r.hsA, "/api/auth/login", "", creds)
		if code != http.StatusUnauthorized {
			t.Fatalf("attempt %d on replica A: status=%d body=%s (want 401 — the burst is 5)", i+1, code, body)
		}
	}
	code, body, resp := replicaPost(t, r.hsB, "/api/auth/login", "", creds)
	if code != http.StatusTooManyRequests {
		t.Fatalf("6th attempt on replica B: status=%d body=%s — the rate budget was NOT shared across replicas", code, body)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Errorf("throttled response carries no Retry-After header")
	}
	// A different account still gets its own budget (the key is per-subject,
	// not a deployment-wide kill switch)… but the shared per-IP tier is
	// already spent, so replica A throttles that too — which is the point:
	// one budget, wherever the request lands.
	if code, _, _ := replicaPost(t, r.hsA, "/api/auth/login", "", map[string]any{"email": "other@example.com", "password": "x"}); code != http.StatusTooManyRequests {
		t.Errorf("per-IP tier was not shared either: status=%d", code)
	}
}

// The forge OAuth handshake starts on one replica (which stashes the CSRF
// state + PKCE verifier) and the provider redirects the browser back to
// whichever replica the LB picks. The callback must find the state, and
// consuming it must be one-shot across the whole deployment.
func TestValkeyCrossReplica_ForgeOAuthStartedOnACompletesOnB(t *testing.T) {
	r := newTwoReplicas(t)

	// A stub Forgejo: the token endpoint + the identity endpoint the
	// callback calls with the freshly-exchanged token.
	var exchanged url.Values
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/login/oauth/access_token":
			_ = req.ParseForm()
			exchanged = req.PostForm
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "at-1", "refresh_token": "rt-1", "expires_in": 3600,
			})
		case "/api/v1/user":
			_ = json.NewEncoder(w).Encode(map[string]any{"login": "botuser", "id": 7, "email": "bot@example.com"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer provider.Close()

	sealed, err := forge.SealOAuthAppSecret(r.sealer, "app-1", "s3cr3t")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if err := r.oauthApps.Create(context.Background(), forge.ForgeOAuthApp{
		ID: "app-1", TenantID: "team-1", Provider: forge.ProviderForgejo,
		ForgeBaseURL: provider.URL, ClientID: "cid", SealedSecret: sealed,
	}); err != nil {
		t.Fatalf("create oauth app: %v", err)
	}

	// --- replica A: start the flow ---
	code, body, resp := replicaPost(t, r.hsA, "/api/teams/team-1/forge/connections", r.adminToken, map[string]any{
		"provider": "forgejo", "mode": "oauth", "forge_base_url": provider.URL,
	})
	if code != http.StatusOK {
		t.Fatalf("connect start on replica A: status=%d body=%s", code, body)
	}
	var started struct {
		AuthorizeURL string `json:"authorize_url"`
	}
	if err := json.Unmarshal(body, &started); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	au, err := url.Parse(started.AuthorizeURL)
	if err != nil {
		t.Fatalf("authorize url %q: %v", started.AuthorizeURL, err)
	}
	state := au.Query().Get("state")
	if state == "" {
		t.Fatalf("no state in the authorize URL: %s", started.AuthorizeURL)
	}
	var binding *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == forgeAgentBindingCookie {
			binding = c
		}
	}
	if binding == nil {
		t.Fatalf("replica A set no agent-binding cookie")
	}

	// --- replica B: the provider redirects the browser here ---
	callback := func(hs *httptest.Server) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, hs.URL+"/api/forge/oauth/callback?state="+url.QueryEscape(state)+"&code=the-code", nil)
		req.AddCookie(&http.Cookie{Name: binding.Name, Value: binding.Value})
		cl := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		cbResp, err := cl.Do(req)
		if err != nil {
			t.Fatalf("callback: %v", err)
		}
		defer cbResp.Body.Close()
		out, _ := io.ReadAll(cbResp.Body)
		return cbResp.StatusCode, string(out)
	}
	status, out := callback(r.hsB)
	if status != http.StatusFound {
		t.Fatalf("callback on replica B: status=%d body=%s — the state minted on A did not cross", status, out)
	}
	// The PKCE verifier stashed on A is what the exchange proves crossed:
	// replica B could not have produced it on its own.
	if exchanged.Get("code") != "the-code" || exchanged.Get("code_verifier") == "" {
		t.Fatalf("token exchange did not carry the code + the verifier minted on replica A: %v", exchanged)
	}
	conns, err := r.conns.ListByTenant(context.Background(), "team-1")
	if err != nil {
		t.Fatalf("list connections: %v", err)
	}
	if len(conns) != 1 || conns[0].AccountLogin != "botuser" || conns[0].Kind != forge.KindOAuthApp {
		t.Fatalf("the connection the flow exists to create never landed: %+v", conns)
	}

	// One-shot across the deployment: replaying the same state on the
	// replica that MINTED it must fail too (GETDEL, not a local delete).
	if status, out := callback(r.hsA); status != http.StatusBadRequest || !strings.Contains(out, "state expired or invalid") {
		t.Fatalf("a consumed state was replayable on replica A: status=%d body=%s", status, out)
	}
	if conns, _ := r.conns.ListByTenant(context.Background(), "team-1"); len(conns) != 1 {
		t.Fatalf("the replay created a second connection: %d", len(conns))
	}
}
