package server

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/connectors"
	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The durable connector tiers on the server side (#2757): every site that
// knows the tenant — the facade per grant, the connection CRUD, the connector
// webhook lane — resolves through that tenant's composition, team → platform →
// baked. A connector a team publishes for itself is listed, pinned and
// believed by the same composition, or the facade lists tools no connection
// can ever be created for.

// teamProbePackage is facadePkg as a PUBLISHER writes it: the format's own
// writer stamps the documents, so the stored rows are what a publication
// route would hand the store.
func teamProbePackage(baseURL string) *spec.Package {
	pkg := facadePkg(baseURL)
	pkg.Connector.SchemaVersion = spec.SchemaVersion
	pkg.Connector.DisplayName = "Probe"
	pkg.Connector.Description = "a team's own connector"
	for i := range pkg.Ops {
		pkg.Ops[i].SchemaVersion = spec.SchemaVersion
		pkg.Ops[i].Connector = pkg.Connector.ID
		pkg.Ops[i].Domain = "issue"
	}
	return pkg
}

// connectorPackageFiles serializes pkg through spec.Write into the file map a
// PackageStore row holds.
func connectorPackageFiles(t *testing.T, pkg *spec.Package) map[string]string {
	t.Helper()
	dir := t.TempDir()
	if err := spec.Write(dir, pkg); err != nil {
		t.Fatalf("spec.Write: %v", err)
	}
	return filesUnder(t, os.DirFS(dir), ".")
}

// filesUnder reads every regular file under root of fsys as a file map keyed
// by its root-relative slash path.
func filesUnder(t *testing.T, fsys fs.FS, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := fs.WalkDir(fsys, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		rel := p
		if root != "." {
			rel = strings.TrimPrefix(p, root+"/")
		}
		files[filepath.ToSlash(rel)] = string(body)
		return nil
	})
	if err != nil || len(files) == 0 {
		t.Fatalf("no files read under %s (err=%v) — the fixture would prove nothing", root, err)
	}
	return files
}

func publishTeamPackage(t *testing.T, st connection.PackageStore, tenant, id string, files map[string]string) {
	t.Helper()
	if err := st.Publish(context.Background(), connection.StoredPackage{
		TenantID: tenant, ConnectorID: id, Files: files, CreatedBy: "test",
	}); err != nil {
		t.Fatalf("publish %s/%s: %v", tenant, id, err)
	}
}

// The composition itself: nil with no store wired (the static catalog
// serves), and with one wired the team's own row is served FROM THE TEAM
// TIER while the floor still answers below it.
func TestCatalogForTenant_ComposesTheThreeTiers(t *testing.T) {
	s := &Server{}
	if got := s.catalogForTenant("t1"); got != nil {
		t.Fatalf("no package store wired: got %v, want nil (the static catalog serves)", got)
	}

	pkgs := connection.NewMemoryPackageStore()
	publishTeamPackage(t, pkgs, "t1", "probe", connectorPackageFiles(t, teamProbePackage("https://probe.example")))
	s.connectorPackages = pkgs
	cat := s.catalogForTenant("t1")
	if cat == nil {
		t.Fatal("a wired store must compose the tiers")
	}
	if _, tier, err := cat.Package("probe"); err != nil || tier.Kind != connection.TierTeam {
		t.Fatalf("the team's own package through the composition: tier=%v err=%v", tier.Kind, err)
	}
	if _, tier, err := cat.Package("forgejo"); err != nil || tier.Kind != connection.TierBaked {
		t.Fatalf("baked through the composition: tier=%v err=%v", tier.Kind, err)
	}
	// Another tenant's composition does not hold t1's row.
	if _, _, err := s.catalogForTenant("t2").Package("probe"); err == nil {
		t.Fatal("t2's composition served t1's package")
	}
}

// The M1 witness of review round 1: a CatalogForTenant answering nil — the
// documented "no store wired" shape — must fall back to the static catalog,
// not hand nil to the facade (the probe panicked in facadeFor).
func TestCatalogFor_ANilTenantCatalogFallsBackToTheStaticOne(t *testing.T) {
	static := connection.NewMemoryCatalog()
	h := &connectorMCPHandler{deps: ConnectorMCPDeps{
		Catalog: static,
		CatalogForTenant: func(string) connection.Catalog {
			return nil
		},
	}}
	grant := boardMCPGrant{TenantID: "t1"}
	cat, err := h.catalogFor(grant)
	if err != nil {
		t.Fatal(err)
	}
	if cat != static {
		t.Fatal("a nil tenant catalog must fall back to the static one")
	}
	// And the facade path holds it — the probe's panic was one call further.
	if _, _, err := h.facadeFor(grant); err != nil {
		t.Fatalf("facadeFor on the fallback: %v", err)
	}
}

// The CRUD pins a connection against the TEAM's composition: a connector
// the team published for itself gets its connection, and another team —
// which never published it — is refused.
func TestConnectionCreate_ResolvesTheTeamsOwnConnector(t *testing.T) {
	s := newConnectionTestServer(t)
	pkgs := connection.NewMemoryPackageStore()
	publishTeamPackage(t, pkgs, "t1", "probe", connectorPackageFiles(t, teamProbePackage("https://probe.example")))
	s.connectorPackages = pkgs

	body := `{"connector":"probe","alias":"main","scheme_id":"token","token":"probe-token"}`
	w := httptest.NewRecorder()
	s.handleCreateConnection(w, connReq(connUserCtx("admin1"), "POST", "/api/teams/t1/connections", body, "t1", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("create for the team's own connector: code=%d body=%s", w.Code, w.Body.String())
	}
	var got connectionDTO
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	// Pinned from the TEAM's package: its default instance is the origin.
	if got.Connector != "probe" || got.BaseURL != "https://probe.example" {
		t.Fatalf("created = %+v, want probe pinned at the team package's default instance", got)
	}

	w = httptest.NewRecorder()
	s.handleCreateConnection(w, connReq(connUserCtx("admin2"), "POST", "/api/teams/t2/connections", body, "t2", ""))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "probe") {
		t.Fatalf("t2 never published probe: code=%d body=%s, want a 400 naming it", w.Code, w.Body.String())
	}
}

// The connector webhook lane believes the event the TEAM's package declares:
// a team override of forgejo whose triggers.yaml adds `push` has its push
// deliveries launch, where the baked package (which declares only
// issue_comment) filters them.
func TestConnectorWebhook_TheTeamsOverrideDeclaresTheEvent(t *testing.T) {
	s, cfg, pt := connectorBoundServer(t, []connection.Capability{connection.CapTrigger})
	got := connectorLaunches(s)

	files := filesUnder(t, connectors.FS(), "forgejo")
	files["triggers.yaml"] += "  - id: forgejo.repo.pushed\n    event: push\n    description: commits were pushed\n"
	pkgs := connection.NewMemoryPackageStore()
	publishTeamPackage(t, pkgs, cfg.TenantID, "forgejo", files)
	s.connectorPackages = pkgs

	w := httptest.NewRecorder()
	s.handleForgejoWebhook(w, fjReq(gitlabCtx(cfg), fjComment, "X-Forgejo-Event", "push", pt, "X-Forgejo-Signature"))
	if w.Code != http.StatusAccepted {
		t.Fatalf("a push the team's package declares: code=%d body=%s", w.Code, w.Body.String())
	}
	if len(*got) != 1 || !strings.Contains((*got)[0], "|forgejo.repo.pushed|") {
		t.Fatalf("launches = %v, want one carrying the team package's trigger id", *got)
	}
}

// The facade as the server WIRES it (connectorMCPDeps, the deps
// boardMCPServiceOption registers): a t1 run lists the team's own tool and
// reaches the vendor through it, with the connection the CRUD pinned; a t2
// run is served no such tool.
func TestTheWiredFacadeServesTheTeamsOwnPackage(t *testing.T) {
	t.Setenv("ITERION_CONNECTOR_ALLOW_PRIVATE", "1")
	hits := make(chan string, 4)
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"number":1,"title":"from the team's vendor"}]`))
	}))
	t.Cleanup(vendor.Close)

	sealer, err := secrets.NewAESGCMSealer([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	conns := connection.NewMemoryStore()
	sealed, err := connection.SealToken(sealer, "c1", "the-token", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if err := conns.Create(context.Background(), connection.Connection{
		ID: "c1", TenantID: "t1", Connector: "probe", Alias: "main",
		SchemeID: "token", Status: connection.StatusActive,
		Capabilities: []connection.Capability{connection.CapAgent},
		BaseURL:      vendor.URL,
		AuthPlacement: connection.AuthPlacement{
			Kind: "api_key", In: "header", Name: "Authorization", ValuePrefix: "token ",
		},
		SealedPayload: sealed,
	}); err != nil {
		t.Fatal(err)
	}
	pkgs := connection.NewMemoryPackageStore()
	publishTeamPackage(t, pkgs, "t1", "probe", connectorPackageFiles(t, teamProbePackage(vendor.URL)))

	// The cloud shape: the static catalog is the platform tier over the
	// floor (cmd/iterion/server.go), the store composes the team tier.
	s := &Server{
		connectionStore:   conns,
		connectorCatalog:  connection.NewLayeredCatalog(connection.PlatformCatalog(pkgs), connection.BakedCatalog()),
		connectorPackages: pkgs,
		sealer:            sealer,
	}
	deps, ok := s.connectorMCPDeps()
	if !ok {
		t.Fatal("the connection plane is wired: the facade deps must be built")
	}
	reg := NewBoardMCPTokenRegistry()
	for _, tenant := range []string{"t1", "t2"} {
		if err := reg.Register("tok-"+tenant, []string{connection.CapConnectors}, "", tenant, store.RunTrustDefault, true, ""); err != nil {
			t.Fatal(err)
		}
	}
	mux := http.NewServeMux()
	RegisterConnectorMCPRoutes(mux, "/api/v1/mcp/connectors", deps, reg)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	rig := &facadeTestRig{server: srv}

	toolNames := func(token string) string {
		t.Helper()
		code, out := rig.rpc(t, token, "tools/list", nil)
		if code != http.StatusOK || out["error"] != nil {
			t.Fatalf("tools/list for %s: code=%d out=%v", token, code, out)
		}
		res, _ := out["result"].(map[string]any)
		tools, _ := res["tools"].([]any)
		var names []string
		for _, tool := range tools {
			names = append(names, tool.(map[string]any)["name"].(string))
		}
		return strings.Join(names, ",")
	}
	if names := toolNames("tok-t1"); !strings.Contains(names, "probe_issue_list") {
		t.Fatalf("t1's facade lacks its own package's tool: %s", names)
	}
	if names := toolNames("tok-t2"); strings.Contains(names, "probe_issue_list") {
		t.Fatalf("t2's facade lists t1's package: %s", names)
	}

	_, out := rig.rpc(t, "tok-t1", "tools/call", map[string]any{
		"name":      "probe_issue_list",
		"arguments": map[string]any{"connection": "main"},
	})
	text, isError := rpcText(t, out)
	if isError || !strings.Contains(text, "from the team's vendor") {
		t.Fatalf("the call through the team's package: isError=%v %s", isError, text)
	}
	if auth := <-hits; auth != "token the-token" {
		t.Fatalf("the vendor saw Authorization=%q, want the server-placed credential", auth)
	}
}

// collidingConnector is a one-operation package whose curated tool is
// `a_b_c_get` whichever connector builds it (`a.b_c.get`, `a_b.c.get`).
func collidingConnector(connector, opID string) *spec.Package {
	return &spec.Package{
		Connector: spec.Connector{ID: connector, Version: "1.0.0"},
		Ops: []spec.OpsFile{{Operations: []spec.Operation{{
			ID: opID, Summary: "the colliding read", Effect: spec.EffectRead, MCP: true,
		}}}},
	}
}

// A package whose tool collides with another's is left out of the facade
// with a loud log; every other package keeps serving. Which collider keeps
// the tool is the sorted-id order's, whatever order the catalog was filled
// in — both orders witnessed.
func TestTheFacadeLeavesOutACollidingPackageNotTheFacade(t *testing.T) {
	a := collidingConnector("a", "a.b_c.get")
	ab := collidingConnector("a_b", "a_b.c.get")
	for _, order := range [][]*spec.Package{
		{a, ab, facadePkg("https://probe.example")},
		{facadePkg("https://probe.example"), ab, a},
	} {
		logs := &lockedBuffer{}
		f, err := facadeOverCatalog(connection.NewMemoryCatalog(order...), iterlog.New(iterlog.LevelError, logs))
		if err != nil {
			t.Fatalf("a cross-package collision took the facade down: %v", err)
		}
		var names []string
		for _, tool := range f.Tools() {
			names = append(names, tool.Name)
		}
		joined := strings.Join(names, ",")
		if !strings.Contains(joined, "probe_issue_list") || !strings.Contains(joined, "a_b_c_get") {
			t.Fatalf("every package that joins must keep serving: %v", names)
		}
		if hits, _ := f.SearchOperations("a_b.c.get", 5); len(hits) != 0 {
			t.Fatalf("the left-out package a_b is still reachable by id: %+v", hits)
		}
		if hits, _ := f.SearchOperations("a.b_c.get", 5); len(hits) != 1 {
			t.Fatalf("a (the first id in sort order) must keep its operation: %+v", hits)
		}
		if out := logs.String(); !strings.Contains(out, `"a_b"`) || !strings.Contains(out, "cannot join") {
			t.Fatalf("the exclusion must be logged naming the package: %q", out)
		}
	}
}

// flakyListCatalog fails its first listing and serves afterwards. Called
// from the server's handler goroutines, so the count takes a lock.
type flakyListCatalog struct {
	connection.Catalog
	mu    sync.Mutex
	calls int
}

func (c *flakyListCatalog) Connectors() ([]string, error) {
	c.mu.Lock()
	c.calls++
	first := c.calls == 1
	c.mu.Unlock()
	if first {
		return nil, errors.New("list blip")
	}
	return c.Catalog.Connectors()
}

// A catalog that cannot list fails the facade by name rather than serving
// what the other tiers list; and the shared facade keeps only a SUCCESS, so
// the next call after a failed listing is served instead of answering
// "facade unavailable" until a restart.
func TestTheFacadeSaysWhenItsCatalogCannotList(t *testing.T) {
	flaky := &flakyListCatalog{Catalog: connection.NewMemoryCatalog(facadePkg("https://probe.example"))}
	h := &connectorMCPHandler{deps: ConnectorMCPDeps{Catalog: flaky}}
	if _, err := h.theFacade(); err == nil || !strings.Contains(err.Error(), "list blip") {
		t.Fatalf("first build: err = %v, want the listing's error", err)
	}
	f, err := h.theFacade()
	if err != nil {
		t.Fatalf("the next build after a failed listing: %v — a failure must not be kept", err)
	}
	var names []string
	for _, tool := range f.Tools() {
		names = append(names, tool.Name)
	}
	if !strings.Contains(strings.Join(names, ","), "probe_issue_list") {
		t.Fatalf("the rebuilt facade lacks the catalog's tools: %v", names)
	}
}
