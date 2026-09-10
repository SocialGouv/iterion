package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/modelcatalog"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

func getModels(t *testing.T, srv *Server, query string, ctxs ...context.Context) (*httptest.ResponseRecorder, modelcatalog.Catalog) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/models"+query, nil)
	if len(ctxs) > 0 && ctxs[0] != nil {
		req = req.WithContext(ctxs[0])
	}
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	var cat modelcatalog.Catalog
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &cat); err != nil {
			t.Fatalf("response is not a Catalog: %v\n%s", err, rec.Body.String())
		}
	}
	return rec, cat
}

func TestGetModels_ReturnsTheKnownSet(t *testing.T) {
	srv, _ := newTestServer(t)

	rec, cat := getModels(t, srv, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(cat.Models) == 0 {
		t.Fatal("catalog is empty")
	}
	if _, ok := cat.Find("anthropic/claude-opus-5"); !ok {
		t.Errorf("expected anthropic/claude-opus-5 in the default set, got %v", cat.SortedSpecs())
	}
	// Every row must carry the fields a picker needs to render a decision.
	for _, m := range cat.Models {
		if m.Spec == "" || m.Source == "" {
			t.Errorf("row is missing identity/source: %+v", m)
		}
		if !m.Usable && m.UnusableReason == "" {
			t.Errorf("%s is unusable without saying why", m.Spec)
		}
	}
}

// LaunchView asks about the specs its nodes actually pin, which may sit outside
// the curated set. Those must be ADDED to the known set, never replace it —
// narrowing the picker to the models already in use is the one list from which
// no new choice can be made.
func TestGetModels_ExtraSpecsAddToTheKnownSet(t *testing.T) {
	srv, _ := newTestServer(t)

	_, base := getModels(t, srv, "")
	_, cat := getModels(t, srv, "?spec=somevendor/some-model-9")

	if len(cat.Models) != len(base.Models)+1 {
		t.Fatalf("got %d models, want the %d known ones plus the requested one: %v",
			len(cat.Models), len(base.Models), cat.SortedSpecs())
	}
	if _, ok := cat.Find("somevendor/some-model-9"); !ok {
		t.Error("the requested spec is missing")
	}
	if _, ok := cat.Find("anthropic/claude-opus-5"); !ok {
		t.Error("asking about one model must not hide the rest")
	}
}

// A bot with twenty nodes on one model must not produce twenty rows, a spec
// already in the known set must not be duplicated, and a caller writing one
// comma-separated param must not get a 400.
func TestGetModels_DedupesAndSplitsSpecs(t *testing.T) {
	srv, _ := newTestServer(t)

	_, base := getModels(t, srv, "")
	_, cat := getModels(t, srv,
		"?spec=somevendor/one,anthropic/claude-opus-5&spec=somevendor/one")
	// +1: "somevendor/one" once, and claude-opus-5 is already known.
	if len(cat.Models) != len(base.Models)+1 {
		t.Fatalf("got %d models, want %d: %v", len(cat.Models), len(base.Models)+1, cat.SortedSpecs())
	}
}

// One malformed hint must not blank out the picker. LaunchView asks about
// every LLM node's DSL default in a single call, and a bot in this very repo
// pins a bare `model: "claude-opus-5"` — under a fail-whole contract that one
// bot made the registry unusable for every OTHER model the host can reach.
// The bad spec is skipped and REPORTED; the catalog still answers.
func TestGetModels_MalformedSpecDegradesInsteadOfBlankingTheCatalog(t *testing.T) {
	srv, _ := newTestServer(t)

	_, base := getModels(t, srv, "")
	rec, cat := getModels(t, srv, "?spec=no-provider-prefix&spec=somevendor/one")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	// The valid hint still lands; the malformed one costs only itself.
	if len(cat.Models) != len(base.Models)+1 {
		t.Fatalf("got %d models, want %d: %v", len(cat.Models), len(base.Models)+1, cat.SortedSpecs())
	}
	if len(cat.InvalidSpecs) != 1 || cat.InvalidSpecs[0].Spec != "no-provider-prefix" {
		t.Fatalf("invalid_specs = %+v, want exactly the malformed hint", cat.InvalidSpecs)
	}
	if cat.InvalidSpecs[0].Reason == "" {
		t.Fatal("a skipped spec must say why, or the caller cannot fix it")
	}
}

// The endpoint reports credential SOURCE names so an operator can fix a gap —
// it must never echo a credential value. Detection only ever produces variable
// names, and this test pins that contract at the HTTP boundary.
func TestGetModels_NeverLeaksCredentialValues(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-supersecret-test-value")
	srv, _ := newTestServer(t)

	rec, _ := getModels(t, srv, "?refresh=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); contains(body, "sk-ant-supersecret-test-value") {
		t.Fatalf("response leaked a credential value:\n%s", body)
	}
}

func TestGetModels_ConcurrentForcedRefreshesAreCoalesced(t *testing.T) {
	srv, _ := newTestServer(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var refreshCalls atomic.Int32
	var hookCalls atomic.Int32
	srv.OnForceRefresh = func() { hookCalls.Add(1) }
	srv.modelSpecsRefresh = func(context.Context) error {
		if refreshCalls.Add(1) == 1 {
			close(started)
		}
		<-release
		return nil
	}

	const n = 12
	var wg sync.WaitGroup
	wg.Add(n)
	statuses := make(chan int, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet,
				fmt.Sprintf("/api/models?refresh=1&spec=vendor/model-%d", i), nil)
			rec := httptest.NewRecorder()
			srv.mux.ServeHTTP(rec, req)
			statuses <- rec.Code
		}(i)
	}
	<-started
	deadline := time.Now().Add(2 * time.Second)
	for {
		srv.modelsRefreshMu.Lock()
		waiters := srv.modelsRefresh.waiters
		srv.modelsRefreshMu.Unlock()
		if waiters == n {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d/%d requests joined the refresh flight", waiters, n)
		}
		runtime.Gosched()
	}
	close(release)
	wg.Wait()
	close(statuses)
	for status := range statuses {
		if status != http.StatusOK {
			t.Errorf("refresh status = %d, want 200", status)
		}
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Errorf("model-spec refresh calls = %d, want 1", got)
	}
	if got := hookCalls.Load(); got != 1 {
		t.Errorf("credential refresh hooks = %d, want 1", got)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func TestDedupeSpecs(t *testing.T) {
	cases := []struct {
		in   []string
		want []string
	}{
		{nil, nil},
		{[]string{" a/b ", "a/b"}, []string{"a/b"}},
		{[]string{"a/b,c/d", "c/d"}, []string{"a/b", "c/d"}},
		{[]string{"", " ", ","}, []string{}},
	}
	for _, tc := range cases {
		got := dedupeSpecs(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("dedupeSpecs(%v) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("dedupeSpecs(%v) = %v, want %v", tc.in, got, tc.want)
				break
			}
		}
	}
}

func newCloudModelsServer(t *testing.T, queue ...QueueBackend) (*Server, *secrets.MemoryApiKeyStore, *secrets.MemoryOAuthStore) {
	t.Helper()
	keys := secrets.NewMemoryApiKeyStore()
	oauth := secrets.NewMemoryOAuthStore()
	var queueBackend QueueBackend
	if len(queue) > 0 {
		queueBackend = queue[0]
	}
	srv := New(Config{
		WorkDir:                 t.TempDir(),
		SkipProjectRegistration: true,
		DisableAuth:             true,
		Mode:                    "cloud",
		ApiKeys:                 keys,
		OAuthForfait:            oauth,
		Queue:                   queueBackend,
	}, iterlog.New(iterlog.LevelError, os.Stderr))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return srv, keys, oauth
}

func cloudIdentity(team, user string) context.Context {
	return auth.WithIdentity(context.Background(), auth.Identity{
		UserID: user,
		TeamID: team,
	})
}

func seedModelRouteTeamKey(t *testing.T, store *secrets.MemoryApiKeyStore, team, user string, p secrets.Provider, secret string) {
	t.Helper()
	id := secrets.NewApiKeyID()
	if err := store.Create(context.Background(), secrets.ApiKey{
		ID:           id,
		TenantID:     team,
		ScopeTeamID:  team,
		ScopeUserID:  user,
		Provider:     p,
		Name:         "test",
		SealedSecret: []byte("sealed:" + secret),
		CreatedAt:    time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create key: %v", err)
	}
}

func seedOAuthKind(t *testing.T, st *secrets.MemoryOAuthStore, owner string, kind secrets.OAuthKind, token string) {
	t.Helper()
	if err := st.Upsert(context.Background(), secrets.OAuthRecord{
		UserID:        owner,
		Kind:          kind,
		SealedPayload: []byte("sealed:" + token),
	}); err != nil {
		t.Fatalf("upsert oauth: %v", err)
	}
}

// A tenant BYOK that the control-plane process does not hold must still
// show as reachable. The picker used to read the server env and emit a
// blocking "unreachable" that a cloud run would not hit.
func TestGetModels_CloudTenantBYOKIsNotAFalseUnreachable(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-control-plane-only")
	t.Setenv("OPENAI_API_KEY", "")

	// Production cloud servers always wire a queue. Keep that shape in the
	// regression: it must not erase the tenant-scoped presence report.
	srv, keys, _ := newCloudModelsServer(t, newFakeDLQQueue())
	const tenantSecret = "sk-tenant-openai-never-on-the-server"
	seedModelRouteTeamKey(t, keys, "team-a", "", secrets.ProviderOpenAI, tenantSecret)

	rec, cat := getModels(t, srv, "?spec=openai/gpt-5.5&spec=anthropic/claude-opus-5", cloudIdentity("team-a", "alice"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if cat.Reachability != modelcatalog.ReachabilityCloud {
		t.Errorf("catalog reachability = %q, want cloud", cat.Reachability)
	}
	gpt, ok := cat.Find("openai/gpt-5.5")
	if !ok {
		t.Fatal("missing openai/gpt-5.5")
	}
	if !gpt.Usable || gpt.Reachability != modelcatalog.ReachabilityCloud {
		t.Errorf("tenant OpenAI BYOK must be cloud-proven usable: %+v", gpt)
	}
	if gpt.CredentialSource != "OPENAI_API_KEY" {
		t.Errorf("credential_source = %q, want the env-var name", gpt.CredentialSource)
	}
	claude, _ := cat.Find("anthropic/claude-opus-5")
	if claude.Usable {
		t.Errorf("control-plane ANTHROPIC_API_KEY must not make claude usable for this tenant: %+v", claude)
	}
	if claude.Reachability != modelcatalog.ReachabilityUnknown {
		t.Errorf("claude reachability = %q, want unknown (not host-unreachable)", claude.Reachability)
	}
	body := rec.Body.String()
	if contains(body, tenantSecret) || contains(body, "sk-ant-control-plane-only") {
		t.Fatalf("response leaked a credential value:\n%s", body)
	}
}

// A user/org OAuth forfait absent from the server env must not produce a
// false unreachable on Claude models — the publisher injects that blob.
func TestGetModels_CloudTenantOAuthIsNotAFalseUnreachable(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	srv, _, oauth := newCloudModelsServer(t)
	const oat = "sk-ant-oat-user-forfait"
	seedOAuthKind(t, oauth, "alice", secrets.OAuthKindClaudeCode, oat)

	rec, cat := getModels(t, srv, "?spec=anthropic/claude-opus-5", cloudIdentity("team-a", "alice"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	claude, _ := cat.Find("anthropic/claude-opus-5")
	if !claude.Usable || claude.Reachability != modelcatalog.ReachabilityCloud {
		t.Errorf("user claude_code forfait must be cloud-proven: %+v", claude)
	}
	if contains(rec.Body.String(), oat) {
		t.Fatalf("response leaked the forfait token:\n%s", rec.Body.String())
	}
}

// A server-process credential that the publisher will not inject must not
// make the model look reachable to that tenant.
func TestGetModels_CloudIgnoresControlPlaneEnv(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-control-plane-only")
	t.Setenv("OPENAI_API_KEY", "sk-openai-control-plane-only")
	srv, _, _ := newCloudModelsServer(t)

	rec, cat := getModels(t, srv, "", cloudIdentity("team-empty", "bob"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	for _, m := range cat.Models {
		if m.Usable {
			t.Errorf("%s is usable from control-plane env for a tenant with no keys: %+v", m.Spec, m)
		}
		if m.Reachability != modelcatalog.ReachabilityUnknown {
			t.Errorf("%s reachability = %q, want unknown", m.Spec, m.Reachability)
		}
	}
	body := rec.Body.String()
	if contains(body, "sk-ant-control-plane-only") || contains(body, "sk-openai-control-plane-only") {
		t.Fatalf("response leaked a control-plane credential:\n%s", body)
	}
}

func TestGetModels_CloudWithoutIdentityStaysUnknown(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-control-plane-only")
	srv, keys, _ := newCloudModelsServer(t)
	seedModelRouteTeamKey(t, keys, "team-a", "", secrets.ProviderAnthropic, "sk-tenant")

	_, cat := getModels(t, srv, "?spec=anthropic/claude-opus-5")
	claude, _ := cat.Find("anthropic/claude-opus-5")
	if claude.Usable {
		t.Errorf("unauthenticated cloud catalog must not use tenant or host creds: %+v", claude)
	}
	if claude.Reachability != modelcatalog.ReachabilityUnknown {
		t.Errorf("reachability = %q, want unknown", claude.Reachability)
	}
}

func TestGetModels_CloudPlatformKeyIsProven(t *testing.T) {
	srv, keys, _ := newCloudModelsServer(t)
	if err := keys.Create(context.Background(), secrets.ApiKey{
		ID:           secrets.NewApiKeyID(),
		TenantID:     secrets.PlatformTenantID,
		ScopeTeamID:  secrets.PlatformTenantID,
		Provider:     secrets.ProviderXAI,
		Name:         "platform",
		SealedSecret: []byte("sealed:platform-xai"),
		CreatedAt:    time.Now().UTC(),
	}); err != nil {
		t.Fatalf("platform key: %v", err)
	}

	_, cat := getModels(t, srv, "?spec=xai/grok-3", cloudIdentity("team-a", "alice"))
	grok, _ := cat.Find("xai/grok-3")
	if !grok.Usable || grok.Reachability != modelcatalog.ReachabilityCloud {
		t.Errorf("platform xAI key is injected into the run, so grok must be cloud-proven: %+v", grok)
	}
}

func getModelCaps(t *testing.T, base, spec, provider string) (modelCapabilitiesResponse, int) {
	t.Helper()
	q := url.Values{"spec": {spec}}
	if provider != "" {
		q.Set("provider", provider)
	}
	resp, err := http.Get(base + "/api/model-capabilities?" + q.Encode())
	if err != nil {
		t.Fatalf("GET model-capabilities: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return modelCapabilitiesResponse{}, resp.StatusCode
	}
	var out modelCapabilitiesResponse
	decodeJSONResp(t, resp, &out)
	return out, http.StatusOK
}

// The qualified path is the full answer: limits, published prices, the
// capability flags, and the source that tells a client how long the answer is
// worth caching.
func TestModelCapabilities_QualifiedSpec(t *testing.T) {
	t.Cleanup(modelspecs.SetDefault(modelspecs.NewSeeded(map[string]modelspecs.Spec{
		"anthropic/claude-sonnet-4-6": {
			ContextWindow: 1_000_000, MaxOutputTokens: 64_000,
			InputCostPerM: 3, OutputCostPerM: 15,
		},
	})))
	_, hs := newTestServer(t)

	got, status := getModelCaps(t, hs.URL, "anthropic/claude-sonnet-4-6", "")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if got.Source != string(model.SourceAggregator) {
		t.Errorf("Source = %q, want aggregator", got.Source)
	}
	if got.ContextWindow != 1_000_000 || got.MaxOutputTokens != 64_000 {
		t.Errorf("limits = %d/%d, want 1000000/64000", got.ContextWindow, got.MaxOutputTokens)
	}
	if got.InputCostPerM != 3 || got.OutputCostPerM != 15 {
		t.Errorf("price = %v/%v, want 3/15", got.InputCostPerM, got.OutputCostPerM)
	}
	if got.ToolCall == nil || !*got.ToolCall {
		t.Error("qualified spec must carry the capability flags")
	}
}

// A model the aggregator does not carry still answers — from the curated
// table — and says so, because "curated" is what tells a client the answer can
// still improve when the background refresh lands.
func TestModelCapabilities_CuratedSourceCanStillImprove(t *testing.T) {
	t.Cleanup(modelspecs.SetDefault(modelspecs.NewSeeded(nil)))
	_, hs := newTestServer(t)

	got, status := getModelCaps(t, hs.URL, "anthropic/glm-5.2", "")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if got.Source != string(model.SourceCurated) {
		t.Errorf("Source = %q, want curated", got.Source)
	}
	if got.ContextWindow != 1_000_000 {
		t.Errorf("ContextWindow = %d, want the curated 1000000", got.ContextWindow)
	}
	if got.InputCostPerM != 0 || got.OutputCostPerM != 0 {
		t.Errorf("price = %v/%v, want 0/0 — unknown, and a client must not print it as free",
			got.InputCostPerM, got.OutputCostPerM)
	}
}

// A BARE id must not 400. `.bot` files pin bare ids and a studio picker holds
// one while the operator types, so rejecting it would blank the caption in the
// common case.
func TestModelCapabilities_BareIDIsServedNotRejected(t *testing.T) {
	t.Cleanup(modelspecs.SetDefault(modelspecs.NewSeeded(map[string]modelspecs.Spec{
		"anthropic/claude-opus-5": {
			ContextWindow: 1_000_000, MaxOutputTokens: 64_000,
			InputCostPerM: 5, OutputCostPerM: 25,
		},
	})))
	_, hs := newTestServer(t)

	got, status := getModelCaps(t, hs.URL, "claude-opus-5", "")
	if status != http.StatusOK {
		t.Fatalf("bare id status = %d, want 200", status)
	}
	if got.Source != string(model.SourceAggregator) {
		t.Errorf("Source = %q, want aggregator", got.Source)
	}
	if got.ContextWindow != 1_000_000 || got.InputCostPerM != 5 {
		t.Errorf("bare answer = %d ctx / %v in, want 1000000 / 5", got.ContextWindow, got.InputCostPerM)
	}
	// No provider was named, so no curated branch could be taken — the flags
	// must be OMITTED rather than defaulted to a false nothing supports.
	if got.Reasoning != nil || got.ToolCall != nil || got.Temperature != nil {
		t.Errorf("bare answer carries capability flags (%v/%v/%v); a bare id names no provider to resolve them against",
			got.Reasoning, got.ToolCall, got.Temperature)
	}
	if got.Provider != "" {
		t.Errorf("Provider = %q, want empty — none was supplied and none may be invented", got.Provider)
	}
}

// An explicit provider is honoured: the caller genuinely had one, so the answer
// upgrades to the qualified path.
func TestModelCapabilities_ExplicitProviderQualifiesTheBareID(t *testing.T) {
	t.Cleanup(modelspecs.SetDefault(modelspecs.NewSeeded(map[string]modelspecs.Spec{
		"anthropic/claude-opus-5": {ContextWindow: 1_000_000, InputCostPerM: 5, OutputCostPerM: 25},
	})))
	_, hs := newTestServer(t)

	got, status := getModelCaps(t, hs.URL, "claude-opus-5", "anthropic")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if got.Provider != "anthropic" || got.Spec != "anthropic/claude-opus-5" {
		t.Errorf("resolved = %q / %q, want anthropic / anthropic/claude-opus-5", got.Provider, got.Spec)
	}
	if got.ToolCall == nil {
		t.Error("a provider was supplied, so the capability flags must resolve")
	}
}

// A bare id nothing knows still answers 200 with zeros — "unknown", which the
// caption renders as no information rather than as a free, context-less model.
func TestModelCapabilities_UnknownBareIDIsEmptyNotAnError(t *testing.T) {
	t.Cleanup(modelspecs.SetDefault(modelspecs.NewSeeded(nil)))
	_, hs := newTestServer(t)

	got, status := getModelCaps(t, hs.URL, "some-vendor-model-nobody-ships", "")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if got.Source != string(model.SourceCurated) {
		t.Errorf("Source = %q, want curated", got.Source)
	}
	if got.ContextWindow != 0 || got.MaxOutputTokens != 0 || got.InputCostPerM != 0 {
		t.Errorf("unknown model answered %+v, want zeros", got)
	}
}

func TestModelCapabilities_MissingSpecIs400(t *testing.T) {
	_, hs := newTestServer(t)
	resp, err := http.Get(hs.URL + "/api/model-capabilities")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}
