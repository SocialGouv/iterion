package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// sourcesWorld wires a catalog (a team package that declares a trigger, a
// package that declares none, the baked forgejo), one probe connection and
// one forgejo connection in ANOTHER tenant, and one t1 subscription.
func sourcesWorld(t *testing.T) (*Server, *trigger.MemorySubscriptionStore) {
	t.Helper()
	s := newConnectionTestServer(t)
	pkgs := connection.NewMemoryPackageStore()
	probe := teamProbePackage("https://probe.example")
	probe.Connector.Category = spec.CategorySourceControl
	probe.Connector.BrandColor = "#ff9e1b"
	files := connectorPackageFiles(t, probe)
	files["triggers.yaml"] = "schema_version: 1\nconnector: probe\n" +
		"ingress:\n  credential:\n    scheme: hmac-sha256\n    in: header\n    name: X-Probe-Signature\n    secret: minted\n" +
		"  event:\n    header: X-Probe-Event\n" +
		"triggers:\n" +
		"- id: probe.issue.commented\n  event: issue_comment\n  display_name: Issue comment\n" +
		"  description: A comment landed.\n" +
		"  outputs:\n  - name: prompt\n    from: payload.body\n    description: The comment body.\n" +
		"  filters:\n  - id: author\n    from: payload.sender\n    type: enum\n    options: [alice, bob]\n    default: [alice]\n"
	if err := pkgs.Publish(context.Background(), connection.StoredPackage{
		TenantID: "t1", ConnectorID: "probe", Files: files, CreatedBy: "test",
	}); err != nil {
		t.Fatalf("publish probe: %v", err)
	}
	// An outbound-only package (connector.yaml alone, no ops, no
	// triggers): a tool, not a source.
	toolYaml := "schema_version: 1\nid: tool\nversion: 0.1.0\n" +
		"base_url:\n  default: https://tool.example\n" +
		"auth:\n- id: token\n  kind: api_key\n  in: header\n  name: Authorization\n"
	if err := pkgs.Publish(context.Background(), connection.StoredPackage{
		TenantID: "t1", ConnectorID: "tool",
		Files: map[string]string{"connector.yaml": toolYaml}, CreatedBy: "test",
	}); err != nil {
		t.Fatalf("publish tool: %v", err)
	}
	s.connectorPackages = pkgs
	if err := s.connectionStore.Create(context.Background(), connection.Connection{
		ID: "conn-probe", TenantID: "t1", Connector: "probe", Alias: "main",
		Status:        connection.StatusActive,
		BaseURL:       "https://probe.example",
		SchemeID:      "token",
		AuthPlacement: connection.AuthPlacement{Kind: "api_key", In: "header", Name: "Authorization"},
		Capabilities:  []connection.Capability{connection.CapTrigger},
	}); err != nil {
		t.Fatalf("create probe connection: %v", err)
	}
	if err := s.connectionStore.Create(context.Background(), connection.Connection{
		ID: "conn-forgejo-t2", TenantID: "t2", Connector: "forgejo", Alias: "main",
		Status:        connection.StatusActive,
		BaseURL:       "https://forgejo.example",
		SchemeID:      "token",
		AuthPlacement: connection.AuthPlacement{Kind: "api_key", In: "header", Name: "Authorization"},
		Capabilities:  []connection.Capability{connection.CapTrigger},
	}); err != nil {
		t.Fatalf("create forgejo connection: %v", err)
	}
	subs := trigger.NewMemorySubscriptionStore()
	s.cfg.TriggerStore = subs
	if err := subs.Create(context.Background(), trigger.Subscription{
		ID: "s-probe", TenantID: "t1", BotID: "steward", Enabled: true,
		Invocation: "connector", ConnectionID: "conn-probe", TriggerID: "probe.issue.commented",
	}); err != nil {
		t.Fatalf("seed subscription: %v", err)
	}
	return s, subs
}

// sourcesView drives a sources listing for one team identity, indexed by
// the entry id.
func sourcesView(t *testing.T, s *Server, userID, teamID string) map[string]map[string]any {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/v1/sources", nil)
	req = req.WithContext(auth.WithIdentity(context.Background(), auth.Identity{
		UserID: userID, TeamID: teamID, Credential: auth.CredentialAccessToken,
	}))
	rec := httptest.NewRecorder()
	s.handleListSources(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("sources(%s/%s): code=%d body=%s", userID, teamID, rec.Code, rec.Body.String())
	}
	var out struct {
		Sources []map[string]any `json:"sources"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("sources body: %v", err)
	}
	byID := map[string]map[string]any{}
	for _, e := range out.Sources {
		byID[e["id"].(string)] = e
	}
	return byID
}

// TestSourcesListsTriggersPackagesAndBuiltins: the cards are the packages
// that DECLARE triggers (an outbound-only tool is not a source), each with
// the tier that served it and this tenant's counts, and the four built-in
// families the studio already has dialogs for.
func TestSourcesListsTriggersPackagesAndBuiltins(t *testing.T) {
	s, _ := sourcesWorld(t)
	byID := sourcesView(t, s, "viewer1", "t1")

	probe, ok := byID["probe"]
	if !ok {
		t.Fatalf("probe is not in the catalog: %v", byID)
	}
	if probe["kind"] != "connector" || probe["tier"] != "team" || probe["delivery"] != "push" {
		t.Fatalf("probe card: %v", probe)
	}
	if probe["category"] != string(spec.CategorySourceControl) {
		t.Fatalf("probe category: %v", probe["category"])
	}
	if probe["connections"].(float64) != 1 || probe["subscriptions"].(float64) != 1 {
		t.Fatalf("probe counts: %v", probe)
	}
	auths := probe["auth"].([]any)
	if len(auths) != 1 || auths[0].(map[string]any)["id"] != "token" {
		t.Fatalf("probe auth: %v", probe["auth"])
	}
	trigs := probe["triggers"].([]any)
	if len(trigs) != 1 || trigs[0].(map[string]any)["id"] != "probe.issue.commented" {
		t.Fatalf("probe triggers: %v", probe["triggers"])
	}
	trig := trigs[0].(map[string]any)
	outs := trig["outputs"].([]any)
	if len(outs) != 1 || outs[0].(map[string]any)["name"] != "prompt" {
		t.Fatalf("trigger outputs: %v", trig["outputs"])
	}
	filts := trig["filters"].([]any)
	if len(filts) != 1 || filts[0].(map[string]any)["id"] != "author" || filts[0].(map[string]any)["type"] != "enum" {
		t.Fatalf("trigger filters: %v", trig["filters"])
	}
	ing := probe["ingress"].(map[string]any)
	if ing["credential_scheme"] != "hmac-sha256" || ing["secret_origin"] != "minted" {
		t.Fatalf("probe ingress: %v", ing)
	}
	forgejo, ok := byID["forgejo"]
	if !ok {
		t.Fatal("the baked forgejo is not in the catalog")
	}
	if forgejo["ingress"] != nil {
		t.Fatalf("forgejo ships no ingress block, got %v", forgejo["ingress"])
	}
	if forgejo["tier"] != "baked" || forgejo["connections"].(float64) != 0 {
		t.Fatalf("forgejo card: %v", forgejo)
	}
	if _, ok := byID["tool"]; ok {
		t.Fatal("an outbound-only package is listed as a source")
	}
	for _, b := range []string{"schedule", "board", "run_outcome", "emit"} {
		e, ok := byID[b]
		if !ok || e["kind"] != "builtin" {
			t.Fatalf("builtin %s: got %v", b, e)
		}
	}
	if len(byID) != 6 {
		t.Fatalf("catalog holds %d entries, want 6: %v", len(byID), byID)
	}
}

// TestSourcesCountsAreTenantScoped: another tenant's connection and
// subscription never leak into a card's counts.
func TestSourcesCountsAreTenantScoped(t *testing.T) {
	s, _ := sourcesWorld(t)
	byID := sourcesView(t, s, "admin2", "t2")
	forgejo, ok := byID["forgejo"]
	if !ok {
		t.Fatal("t2 does not see the baked forgejo")
	}
	if forgejo["connections"].(float64) != 1 {
		t.Fatalf("t2's forgejo connections: %v", forgejo["connections"])
	}
	if _, ok := byID["probe"]; ok {
		t.Fatal("t2 sees t1's team-tier package")
	}
	if byID["forgejo"]["subscriptions"] != 0.0 {
		t.Fatalf("t2's forgejo subscriptions: %v", byID["forgejo"])
	}
	// And t1 does not see t2's forgejo connection.
	t1 := sourcesView(t, s, "viewer1", "t1")
	if t1["forgejo"]["connections"] != 0.0 {
		t.Fatalf("t1 sees t2's forgejo connection: %v", t1["forgejo"])
	}
}

// TestTeamSourcesReadsThePathTeam: the team family serves the PATH team's
// view — a member of the path team reading it from another active team
// still gets the path team's counts — and a non-member is refused.
func TestTeamSourcesReadsThePathTeam(t *testing.T) {
	s, _ := sourcesWorld(t)
	req := httptest.NewRequest("GET", "/api/teams/t2/sources", nil)
	req.SetPathValue("id", "t2")
	req = req.WithContext(auth.WithIdentity(context.Background(), auth.Identity{
		UserID: "admin2", TeamID: "t1", Credential: auth.CredentialAccessToken,
	}))
	rec := httptest.NewRecorder()
	s.handleTeamListSources(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("t2 member via t1 session: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Sources []map[string]any `json:"sources"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	var forgejo map[string]any
	for _, e := range out.Sources {
		if e["id"] == "forgejo" {
			forgejo = e
		}
	}
	if forgejo == nil || forgejo["connections"].(float64) != 1 {
		t.Fatalf("the path team's counts: %v", forgejo)
	}

	outsider := httptest.NewRequest("GET", "/api/teams/t2/sources", nil)
	outsider.SetPathValue("id", "t2")
	outsider = outsider.WithContext(auth.WithIdentity(context.Background(), auth.Identity{
		UserID: "admin1", TeamID: "t1", Credential: auth.CredentialAccessToken,
	}))
	rec403 := httptest.NewRecorder()
	s.handleTeamListSources(rec403, outsider)
	if rec403.Code != http.StatusForbidden {
		t.Fatalf("non-member: code=%d", rec403.Code)
	}
}

// failingConnStore and failingSubStore answer every listing with an error:
// an outage is the route's 503, never a silent zero on the cards.
type sourcesFailingConnStore struct{ connection.Store }

func (f sourcesFailingConnStore) List(ctx context.Context, tenantID, connector string) ([]connection.Connection, error) {
	return nil, context.DeadlineExceeded
}

type sourcesFailingSubStore struct{ trigger.SubscriptionStore }

func (f sourcesFailingSubStore) ListByTenant(ctx context.Context, tenantID string) ([]trigger.Subscription, error) {
	return nil, context.DeadlineExceeded
}

type sourcesFailingCatalog struct{ connection.Catalog }

func (f sourcesFailingCatalog) Connectors() ([]string, error) {
	return nil, context.DeadlineExceeded
}

// TestSourcesOutagesAre503: a connections, subscriptions or catalog
// listing that fails answers 503 with its cause — a card promising
// "already connected: 0" during an outage is a lie an operator acts on.
func TestSourcesOutagesAre503(t *testing.T) {
	for _, tc := range []struct {
		name string
		wire func(*Server)
	}{
		{"connections", func(s *Server) { s.connectionStore = sourcesFailingConnStore{s.connectionStore} }},
		{"subscriptions", func(s *Server) { s.cfg.TriggerStore = sourcesFailingSubStore{s.cfg.TriggerStore} }},
		{"catalog", func(s *Server) { s.connectorPackages = nil; s.connectorCatalog = sourcesFailingCatalog{s.connectorCatalog} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A fresh world per case: the stubs wrap the real stores.
			s, _ := sourcesWorld(t)
			tc.wire(s)
			req := httptest.NewRequest("GET", "/api/v1/sources", nil)
			req = req.WithContext(auth.WithIdentity(context.Background(), auth.Identity{
				UserID: "viewer1", TeamID: "t1", Credential: auth.CredentialAccessToken,
			}))
			rec := httptest.NewRecorder()
			s.handleListSources(rec, req)
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("%s outage: code=%d body=%s", tc.name, rec.Code, rec.Body.String())
			}
		})
	}
}
