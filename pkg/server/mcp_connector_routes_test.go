package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The connector MCP facade's end-to-end witness (#2758's Done-when): an agent
// reaches an operation through the facade with a credential it NEVER holds —
// the server resolves and spends it, and the tool's answer carries the
// vendor's data and nothing of the grant.

func facadePkg(baseURL string) *spec.Package {
	return &spec.Package{
		Connector: spec.Connector{
			ID: "probe", Version: "1.0.0",
			// The package floor clamps every operation's level, so the
			// fixture declares the same floor the shipped packages ship.
			Maturity: spec.MaturityExperimental,
			BaseURL:  spec.BaseURL{Default: baseURL},
			Auth: []spec.AuthScheme{{
				ID: "token", Kind: spec.AuthAPIKey, In: "header", Name: "Authorization", ValuePrefix: "token ",
			}},
		},
		Ops: []spec.OpsFile{{Operations: []spec.Operation{
			{
				ID: "probe.issue.list", Resource: "issue", Verb: "list",
				Summary: "List issues", Effect: spec.EffectRead, Maturity: spec.MaturityExperimental, MCP: true, Deterministic: true,
				HTTP:    spec.HTTPBinding{Method: "GET", Path: "/issues"},
				Results: []spec.ResultCase{{Status: 200}},
				Security: []spec.SecurityRequirement{
					{Terms: []spec.SecurityTerm{{SchemeID: "token"}}},
				},
			},
			{
				ID: "probe.issue.create", Resource: "issue", Verb: "create",
				Summary: "Create an issue", Effect: spec.EffectCreate, Maturity: spec.MaturityExperimental, Deterministic: true,
				HTTP:    spec.HTTPBinding{Method: "POST", Path: "/issues", RequestBody: spec.BodyJSON},
				Params:  []spec.Param{{Key: "title", Name: "title", In: "body", Type: "string", Required: true}},
				Results: []spec.ResultCase{{Status: 201}},
				Security: []spec.SecurityRequirement{
					{Terms: []spec.SecurityTerm{{SchemeID: "token"}}},
				},
			},
			{
				// The CURATED MUTATING operation: what the trust gate is
				// witnessed against, now that the curation bounds
				// call_operation — a mutating op a model may call is one an
				// operator curated.
				ID: "probe.issue.close", Resource: "issue", Verb: "close",
				Summary: "Close an issue", Effect: spec.EffectUpdate, Maturity: spec.MaturityExperimental, MCP: true, Deterministic: true,
				HTTP:    spec.HTTPBinding{Method: "POST", Path: "/issues", RequestBody: spec.BodyJSON},
				Results: []spec.ResultCase{{Status: 201}},
				Security: []spec.SecurityRequirement{
					{Terms: []spec.SecurityTerm{{SchemeID: "token"}}},
				},
			},
		}}},
	}
}

// facadeTestRig is the wired endpoint: an httptest facade in front of an
// httptest VENDOR, so the whole call — token, grant, resolution, guarded
// client, vendor answer — is real.
type facadeTestRig struct {
	server *httptest.Server
	vendor *httptest.Server
	reg    *BoardMCPTokenRegistry
	// gotAuth records what the vendor saw, so the test proves the SERVER
	// placed the credential.
	gotAuth chan string
}

func newFacadeTestRig(t *testing.T, caps []string, trust store.RunTrust, trustStated bool, connCaps []connection.Capability) *facadeTestRig {
	t.Helper()
	// The guarded dialer refuses loopback; the local tier's hatch is the
	// greppable way out, and a test's vendor IS loopback.
	t.Setenv("ITERION_CONNECTOR_ALLOW_PRIVATE", "1")

	rig := &facadeTestRig{gotAuth: make(chan string, 8)}
	rig.vendor = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rig.gotAuth <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" {
			_, _ = w.Write([]byte(`[{"number":1,"title":"hello"}]`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"number":2}`))
	}))
	t.Cleanup(rig.vendor.Close)

	sealer, err := secrets.NewAESGCMSealer([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	st := connection.NewMemoryStore()
	sealed, err := connection.SealToken(sealer, "c1", "the-token", time.Time{})
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if err := st.Create(context.Background(), connection.Connection{
		ID: "c1", TenantID: "t1", Connector: "probe", Alias: "main",
		SchemeID: "token", Status: connection.StatusActive,
		Capabilities: connCaps,
		BaseURL:      rig.vendor.URL,
		AuthPlacement: connection.AuthPlacement{
			Kind: "api_key", In: "header", Name: "Authorization", ValuePrefix: "token ",
		},
		SealedPayload: sealed,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	rig.reg = NewBoardMCPTokenRegistry()
	if err := rig.reg.Register("run-token", caps, "", "t1", trust, trustStated, ""); err != nil {
		t.Fatalf("register: %v", err)
	}

	mux := http.NewServeMux()
	RegisterConnectorMCPRoutes(mux, "/api/v1/mcp/connectors", ConnectorMCPDeps{
		Store:   st,
		Catalog: connection.NewMemoryCatalog(facadePkg(rig.vendor.URL)),
		Sealer:  sealer,
		Client:  connection.LocalHTTPClient(),
	}, rig.reg)
	rig.server = httptest.NewServer(mux)
	t.Cleanup(rig.server.Close)
	return rig
}

func (rig *facadeTestRig) rpc(t *testing.T, token, method string, params map[string]any) (int, map[string]any) {
	t.Helper()
	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		body["params"] = params
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequest("POST", rig.server.URL+"/api/v1/mcp/connectors", strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if token != "" {
		req.Header.Set("X-Iterion-Run", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil && resp.StatusCode == http.StatusOK {
		t.Fatalf("decode: %v", err)
	}
	return resp.StatusCode, out
}

// rpcText extracts the tool's text answer.
func rpcText(t *testing.T, out map[string]any) (text string, isError bool) {
	t.Helper()
	res, _ := out["result"].(map[string]any)
	content, _ := res["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("no content in %v", out)
	}
	first, _ := content[0].(map[string]any)
	text, _ = first["text"].(string)
	isError, _ = res["isError"].(bool)
	return text, isError
}

func TestConnectorFacadeServesAnAgentWithACredentialItNeverHolds(t *testing.T) {
	rig := newFacadeTestRig(t,
		[]string{connection.CapConnectors},
		store.RunTrustDefault, true,
		[]connection.Capability{connection.CapAgent},
	)

	// tools/list: the curated read is a named tool, the mutating op is not,
	// and the discovery pair is there.
	code, out := rig.rpc(t, "run-token", "tools/list", nil)
	if code != http.StatusOK {
		t.Fatalf("tools/list: code=%d", code)
	}
	res, _ := out["result"].(map[string]any)
	tools, _ := res["tools"].([]any)
	var names []string
	for _, tool := range tools {
		names = append(names, tool.(map[string]any)["name"].(string))
	}
	joined := strings.Join(names, ",")
	for _, want := range []string{"probe_issue_list", "search_operations", "call_operation"} {
		if !strings.Contains(joined, want) {
			t.Errorf("tools/list lacks %q: %v", want, names)
		}
	}
	if strings.Contains(joined, "probe_issue_create") {
		t.Errorf("the mutating op is NOT curated and must stay in the long tail: %v", names)
	}

	// The curated call: the vendor answers, the SERVER spent the credential.
	code, out = rig.rpc(t, "run-token", "tools/call", map[string]any{
		"name":      "probe_issue_list",
		"arguments": map[string]any{"connection": "main"},
	})
	if code != http.StatusOK {
		t.Fatalf("tools/call: code=%d", code)
	}
	text, isError := rpcText(t, out)
	if isError {
		t.Fatalf("the curated call failed: %s", text)
	}
	if !strings.Contains(text, "hello") {
		t.Errorf("the vendor's data must reach the agent: %s", text)
	}
	if strings.Contains(text, "the-token") {
		t.Errorf("the credential leaked into the tool's answer: %s", text)
	}
	select {
	case auth := <-rig.gotAuth:
		if auth != "token the-token" {
			t.Errorf("the vendor saw Authorization=%q — the server must place the credential, pinned as granted", auth)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the vendor saw no call at all")
	}

	// The long tail by operation id — a CURATED mutating operation on a
	// trusted run: the discovery pair reaches what the operator curated.
	_, out = rig.rpc(t, "run-token", "tools/call", map[string]any{
		"name":      "call_operation",
		"arguments": map[string]any{"operation": "probe.issue.close", "connection": "main"},
	})
	text, isError = rpcText(t, out)
	if isError || !strings.Contains(text, `"number":2`) {
		t.Errorf("call_operation on a curated operation failed: isError=%v %s", isError, text)
	}

	// An UNCURATED operation is refused even on a trusted run: the curated
	// set is what the operator read, and trust widens neither it nor any
	// other list. The vendor must not see the call at all.
	for len(rig.gotAuth) > 0 {
		<-rig.gotAuth
	}
	_, out = rig.rpc(t, "run-token", "tools/call", map[string]any{
		"name":      "call_operation",
		"arguments": map[string]any{"operation": "probe.issue.create", "connection": "main", "arguments": map[string]any{"title": "hi"}},
	})
	text, isError = rpcText(t, out)
	if !isError || !strings.Contains(text, "curated set") {
		t.Errorf("call_operation on the uncurated create: isError=%v %s — the curation must be the bound", isError, text)
	}
	select {
	case auth := <-rig.gotAuth:
		t.Errorf("the refused call reached the vendor anyway: %q", auth)
	case <-time.After(200 * time.Millisecond):
	}

	// Discovery.
	_, out = rig.rpc(t, "run-token", "tools/call", map[string]any{
		"name":      "search_operations",
		"arguments": map[string]any{"query": "create"},
	})
	text, isError = rpcText(t, out)
	if isError || !strings.Contains(text, "probe.issue.create") {
		t.Errorf("search must find the long tail: isError=%v %s", isError, text)
	}
}

func TestConnectorFacadeRefusesTheWrongGrants(t *testing.T) {
	// No token, an unknown token, and a token minted for board caps only.
	rig := newFacadeTestRig(t,
		[]string{connection.CapConnectors},
		store.RunTrustDefault, true,
		[]connection.Capability{connection.CapAgent},
	)
	if code, _ := rig.rpc(t, "", "initialize", nil); code != http.StatusUnauthorized {
		t.Errorf("no token: code=%d, want 401", code)
	}
	if code, _ := rig.rpc(t, "nope", "initialize", nil); code != http.StatusUnauthorized {
		t.Errorf("unknown token: code=%d, want 401", code)
	}
	if err := rig.reg.Register("board-only", []string{"board.read"}, "", "t1", store.RunTrustDefault, true, ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	if code, _ := rig.rpc(t, "board-only", "initialize", nil); code != http.StatusForbidden {
		t.Errorf("a board-only token: code=%d, want 403 — any run token must not spend `agent` connections", code)
	}
	// GET is the Streamable-HTTP probe: 405, not 404, so the client falls
	// back to POST-only instead of abandoning the connection.
	resp, err := http.Get(rig.server.URL + "/api/v1/mcp/connectors")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET: code=%d, want 405", resp.StatusCode)
	}
}

func TestConnectorFacadeRefusesAnActionOnlyConnection(t *testing.T) {
	rig := newFacadeTestRig(t,
		[]string{connection.CapConnectors},
		store.RunTrustDefault, true,
		[]connection.Capability{connection.CapAction}, // granted to automation, NOT to an agent
	)
	_, out := rig.rpc(t, "run-token", "tools/call", map[string]any{
		"name":      "probe_issue_list",
		"arguments": map[string]any{"connection": "main"},
	})
	text, isError := rpcText(t, out)
	if !isError {
		t.Fatalf("an action-only connection must not serve an agent, got: %s", text)
	}
	if !strings.Contains(text, "agent") {
		t.Errorf("the refusal must name the missing capability: %s", text)
	}
	if strings.Contains(text, "the-token") {
		t.Errorf("the refusal leaked the credential: %s", text)
	}
}

func TestConnectorFacadeHonoursTheRunsTrust(t *testing.T) {
	// A run whose workspace the tenant does not control (the fork review
	// lane) resolves READ operations only — the facade must not become the
	// way round #1802's refusal.
	rig := newFacadeTestRig(t,
		[]string{connection.CapConnectors},
		store.RunTrustFork, true,
		[]connection.Capability{connection.CapAgent},
	)
	_, out := rig.rpc(t, "run-token", "tools/call", map[string]any{
		"name":      "call_operation",
		"arguments": map[string]any{"operation": "probe.issue.close", "connection": "main"},
	})
	text, isError := rpcText(t, out)
	if !isError || !strings.Contains(text, "read operations only") {
		t.Errorf("a fork run mutating through the facade: isError=%v %s", isError, text)
	}
	// Reads still work.
	_, out = rig.rpc(t, "run-token", "tools/call", map[string]any{
		"name":      "probe_issue_list",
		"arguments": map[string]any{"connection": "main"},
	})
	if _, isError = rpcText(t, out); isError {
		t.Errorf("a fork run must still READ: %v", out)
	}

	// And an UNSTATED trust (the minter could not read the run) fails the
	// mutation closed rather than reading the zero value as trusted.
	rig2 := newFacadeTestRig(t,
		[]string{connection.CapConnectors},
		"", false,
		[]connection.Capability{connection.CapAgent},
	)
	_, out = rig2.rpc(t, "run-token", "tools/call", map[string]any{
		"name":      "call_operation",
		"arguments": map[string]any{"operation": "probe.issue.close", "connection": "main"},
	})
	text, isError = rpcText(t, out)
	if !isError || !strings.Contains(text, "nobody stated") {
		t.Errorf("an unstated trust mutating through the facade: isError=%v %s", isError, text)
	}
}

// TestTheFacadeServesEachRunItsOwnCatalog: the workspace tier a facade call
// resolves through is THE RUN'S — its own working directory, its own trust —
// never the process-wide catalog a startup pinned. A catalog shared across
// runs serves one run's workspace packages to every other run, and the
// workspace tier is exactly the tier a run's own code can plant packages in.
//
// The shared Catalog here is EMPTY, so anything any run is served came from
// the per-run builder — the test cannot pass because the fallback leaked.
//
// Mutations that redden this: have the handler call the builder with a fixed
// workDir (or ignore its error and fall back to the shared catalog); or drop
// CatalogForRun from the deps.
func TestTheFacadeServesEachRunItsOwnCatalog(t *testing.T) {
	t.Setenv("ITERION_CONNECTOR_ALLOW_PRIVATE", "1")

	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"number":1,"title":"hello"}]`))
	}))
	defer vendor.Close()

	sealer, err := secrets.NewAESGCMSealer([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	st := connection.NewMemoryStore()
	sealed, err := connection.SealToken(sealer, "c1", "the-token", time.Time{})
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if err := st.Create(context.Background(), connection.Connection{
		ID: "c1", TenantID: "t1", Connector: "probe", Alias: "main",
		SchemeID: "token", Status: connection.StatusActive,
		Capabilities: []connection.Capability{connection.CapAgent},
		BaseURL:      vendor.URL,
		AuthPlacement: connection.AuthPlacement{
			Kind: "api_key", In: "header", Name: "Authorization", ValuePrefix: "token ",
		},
		SealedPayload: sealed,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	reg := NewBoardMCPTokenRegistry()
	if err := reg.Register("run-a", []string{connection.CapConnectors}, "", "t1", store.RunTrustDefault, true, "/srv/proj-a"); err != nil {
		t.Fatalf("register run-a: %v", err)
	}
	if err := reg.Register("run-b", []string{connection.CapConnectors}, "", "t1", store.RunTrustFork, true, "/srv/other"); err != nil {
		t.Fatalf("register run-b: %v", err)
	}
	if err := reg.Register("run-c", []string{connection.CapConnectors}, "", "t1", store.RunTrustDefault, true, "/srv/boom"); err != nil {
		t.Fatalf("register run-c: %v", err)
	}

	var gotWorkDirs []string
	var gotTrusts []connection.ActionTrust
	mux := http.NewServeMux()
	RegisterConnectorMCPRoutes(mux, "/api/v1/mcp/connectors", ConnectorMCPDeps{
		Store:   st,
		Catalog: connection.NewMemoryCatalog(), // empty: nothing may leak from it
		Sealer:  sealer,
		Client:  connection.LocalHTTPClient(),
		CatalogForRun: func(workDir string, trust connection.ActionTrust) (connection.Catalog, error) {
			gotWorkDirs = append(gotWorkDirs, workDir)
			gotTrusts = append(gotTrusts, trust)
			switch workDir {
			case "/srv/proj-a":
				return connection.NewMemoryCatalog(facadePkg(vendor.URL)), nil
			case "/srv/boom":
				return nil, errors.New("no such workspace")
			default:
				return connection.NewMemoryCatalog(), nil
			}
		},
	}, reg)
	server := httptest.NewServer(mux)
	defer server.Close()

	rpc := func(t *testing.T, token, method string, params map[string]any) (int, map[string]any) {
		t.Helper()
		body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
		if params != nil {
			body["params"] = params
		}
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		req, err := http.NewRequest("POST", server.URL+"/api/v1/mcp/connectors", strings.NewReader(string(raw)))
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		req.Header.Set("X-Iterion-Run", token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		defer resp.Body.Close()
		var out map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return resp.StatusCode, out
	}
	textOf := func(t *testing.T, out map[string]any) string {
		t.Helper()
		res, _ := out["result"].(map[string]any)
		contents, _ := res["content"].([]any)
		if len(contents) == 0 {
			return ""
		}
		first, _ := contents[0].(map[string]any)
		text, _ := first["text"].(string)
		return text
	}

	// run-a's own workspace holds the probe package: its tools/list serves it.
	_, out := rpc(t, "run-a", "tools/list", nil)
	res, _ := out["result"].(map[string]any)
	tools, _ := res["tools"].([]any)
	var names []string
	for _, tool := range tools {
		names = append(names, tool.(map[string]any)["name"].(string))
	}
	if !strings.Contains(strings.Join(names, ","), "probe_issue_list") {
		t.Errorf("run-a's catalog served %v — its own workspace's package is missing", names)
	}

	// run-b's workspace holds nothing: the same endpoint serves only the
	// discovery pair, and call_operation cannot resolve what its catalog
	// does not hold — the facade and the resolver read ONE catalog.
	_, out = rpc(t, "run-b", "tools/list", nil)
	res, _ = out["result"].(map[string]any)
	tools, _ = res["tools"].([]any)
	names = nil
	for _, tool := range tools {
		names = append(names, tool.(map[string]any)["name"].(string))
	}
	if strings.Contains(strings.Join(names, ","), "probe_issue_list") {
		t.Errorf("run-b was served run-a's workspace package: %v", names)
	}
	_, out = rpc(t, "run-b", "tools/call", map[string]any{
		"name":      "call_operation",
		"arguments": map[string]any{"operation": "probe.issue.list", "connection": "main"},
	})
	if text := textOf(t, out); !strings.Contains(text, "no operation") {
		t.Errorf("run-b resolved through a catalog that is not its own: %s", text)
	}

	// The builder saw each run's OWN trust, read at mint time — never a
	// hardcoded default.
	if len(gotTrusts) < 2 {
		t.Fatalf("the builder was consulted %d time(s)", len(gotTrusts))
	}
	forkSeen := false
	for i, tr := range gotTrusts {
		if gotWorkDirs[i] == "/srv/other" && tr == connection.TrustOfRun(store.RunTrustFork) {
			forkSeen = true
		}
	}
	if !forkSeen {
		t.Errorf("the builder never saw run-b's fork trust: dirs=%v trusts=%v", gotWorkDirs, gotTrusts)
	}

	// A builder refusal is the CALL's error, never a silent fall back to the
	// shared (here: empty) catalog.
	_, out = rpc(t, "run-c", "tools/list", nil)
	res, _ = out["result"].(map[string]any)
	if _, hasTools := res["tools"]; hasTools {
		t.Errorf("a refused catalog build served a tools/list anyway: %v", out)
	}
	if e := out["error"].(map[string]any); !strings.Contains(e["message"].(string), "cannot be built") {
		t.Errorf("the refusal must name the catalog: %v", e)
	}
}

// TestAFacadeCallBuildsTheCatalogOnce.
//
// `call` resolved its catalog and its facade through TWO catalogFor reads:
// the RESOLVER and the FACADE could then read two different instantiations of
// the run's workspace — a package planted between the two reads is missing
// from exactly one side, and every call paid two catalog walks. One read, one
// catalog, both consumers: the comment on resolverFor already promised it.
//
// Mutation that reddens this: have call call catalogFor itself again instead
// of reading the catalog facadeFor answered.
func TestAFacadeCallBuildsTheCatalogOnce(t *testing.T) {
	t.Setenv("ITERION_CONNECTOR_ALLOW_PRIVATE", "1")

	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"number":1,"title":"hello"}]`))
	}))
	defer vendor.Close()

	sealer, err := secrets.NewAESGCMSealer([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	st := connection.NewMemoryStore()
	sealed, err := connection.SealToken(sealer, "c1", "the-token", time.Time{})
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if err := st.Create(context.Background(), connection.Connection{
		ID: "c1", TenantID: "t1", Connector: "probe", Alias: "main",
		SchemeID: "token", Status: connection.StatusActive,
		Capabilities: []connection.Capability{connection.CapAgent},
		BaseURL:      vendor.URL,
		AuthPlacement: connection.AuthPlacement{
			Kind: "api_key", In: "header", Name: "Authorization", ValuePrefix: "token ",
		},
		SealedPayload: sealed,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	reg := NewBoardMCPTokenRegistry()
	if err := reg.Register("run-a", []string{connection.CapConnectors}, "", "t1", store.RunTrustDefault, true, "/srv/proj-a"); err != nil {
		t.Fatalf("register run-a: %v", err)
	}

	var builds int
	mux := http.NewServeMux()
	RegisterConnectorMCPRoutes(mux, "/api/v1/mcp/connectors", ConnectorMCPDeps{
		Store:   st,
		Catalog: connection.NewMemoryCatalog(),
		Sealer:  sealer,
		Client:  connection.LocalHTTPClient(),
		CatalogForRun: func(string, connection.ActionTrust) (connection.Catalog, error) {
			builds++
			return connection.NewMemoryCatalog(facadePkg(vendor.URL)), nil
		},
	}, reg)
	server := httptest.NewServer(mux)
	defer server.Close()

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"call_operation","arguments":{"operation":"probe.issue.list","connection":"main"}}}`
	req, err := http.NewRequest("POST", server.URL+"/api/v1/mcp/connectors", strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("X-Iterion-Run", "run-a")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res, hasResult := out["result"]; !hasResult || res == nil {
		t.Fatalf("the call never reached the vendor: %v", out)
	}
	if builds != 1 {
		t.Errorf("the per-run builder ran %d times for one call, want 1 — the facade and the resolver read ONE catalog", builds)
	}
}
