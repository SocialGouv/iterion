package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

// The #2761 witness: one connector webhook end to end, with no live vendor.
// A Forgejo delivery declared by the SHIPPED package (the baked catalog's
// forgejo/triggers.yaml, not a fixture copy) reaches a bot run through the
// connection the webhook was bound with; an unsigned or wrongly-signed
// delivery is refused before any of it.

// fjComment is the vendor's issue_comment shape, cut to what the lane reads.
const fjComment = `{
  "action": "created",
  "issue": {"number": 7},
  "comment": {"id": 99},
  "repository": {"id": 42, "full_name": "acme/widgets", "clone_url": "https://codeberg.org/acme/widgets.git"},
  "sender": {"login": "alice"}
}`

// connectorBoundServer wires the connection plane the lane reads: a memory
// store holding the bound connection and the BAKED catalog, so the event the
// test delivers is matched against the package iterion actually ships.
func connectorBoundServer(t *testing.T, caps []connection.Capability) (*Server, webhooks.Config, string) {
	t.Helper()
	s := newWebhookTestServer(t)
	store := connection.NewMemoryStore()
	conn := connection.Connection{
		ID: "conn-fj", TenantID: "t1", Connector: "forgejo", Alias: "main",
		SchemeID: "token", Status: connection.StatusActive,
		Capabilities: caps,
		BaseURL:      "https://codeberg.org",
		AuthPlacement: connection.AuthPlacement{
			Kind: "api_key", In: "header", Name: "Authorization",
		},
	}
	if err := store.Create(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
	s.connectionStore = store
	s.connectorCatalog = connection.BakedCatalog()

	cfg, pt := fjConfig(t, s)
	cfg.DefaultBotID = "review-pr"
	cfg.ConnectorConnectionID = "conn-fj"
	return s, cfg, pt
}

func connectorLaunches(s *Server) *[]string {
	var got []string
	s.webhookLaunchBot = func(_ context.Context, botID string, vars map[string]string, repoURL, repoRef, projectPath string, _, _ map[string]string) (string, error) {
		got = append(got, botID+"|"+vars["connector_trigger"]+"|"+vars["forgejo_action"]+"|"+vars["repo"]+"|"+repoURL)
		return "run-conn-1", nil
	}
	return &got
}

func TestConnectorWebhook_ADeclaredEventReachesABot(t *testing.T) {
	s, cfg, pt := connectorBoundServer(t, []connection.Capability{connection.CapTrigger})
	got := connectorLaunches(s)

	w := httptest.NewRecorder()
	s.handleForgejoWebhook(w, fjReq(gitlabCtx(cfg), fjComment, "X-Forgejo-Event", prforge.EventHeaderIssueComment, pt, "X-Forgejo-Signature"))
	if w.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if len(*got) != 1 {
		t.Fatalf("launches = %v", *got)
	}
	want := "review-pr|forgejo.issue.commented|created|acme/widgets|https://codeberg.org/acme/widgets.git"
	if (*got)[0] != want {
		t.Fatalf("launch = %q, want %q — the bot, the DECLARED trigger id, and the payload's facts", (*got)[0], want)
	}
}

func TestConnectorWebhook_UnsignedOrWronglySignedIsRefused(t *testing.T) {
	s, cfg, pt := connectorBoundServer(t, []connection.Capability{connection.CapTrigger})
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		t.Fatal("an unverified delivery must never launch")
		return "", nil
	}

	// Unsigned: no signature header at all.
	w := httptest.NewRecorder()
	s.handleForgejoWebhook(w, fjReq(gitlabCtx(cfg), fjComment, "X-Forgejo-Event", prforge.EventHeaderIssueComment, "", "X-Forgejo-Signature"))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned: code=%d, want 401", w.Code)
	}
	// Wrongly-signed.
	w = httptest.NewRecorder()
	s.handleForgejoWebhook(w, fjReq(gitlabCtx(cfg), fjComment, "X-Forgejo-Event", prforge.EventHeaderIssueComment, pt+"-wrong", "X-Forgejo-Signature"))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("wrongly-signed: code=%d, want 401", w.Code)
	}
}

func TestConnectorWebhook_AnUndeclaredEventIsFilteredByThePackage(t *testing.T) {
	s, cfg, pt := connectorBoundServer(t, []connection.Capability{connection.CapTrigger})
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		t.Fatal("an event the package does not declare must never launch")
		return "", nil
	}

	// `push` is a real Forgejo event the shipped package does NOT declare:
	// the refusal must come from the package's word, not an engine list.
	w := httptest.NewRecorder()
	s.handleForgejoWebhook(w, fjReq(gitlabCtx(cfg), fjComment, "X-Forgejo-Event", "push", pt, "X-Forgejo-Signature"))
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{"filtered", "issue_comment"} {
		if !strings.Contains(body, want) {
			t.Errorf("the filtered answer must contain %q — the operator reads what the package DOES declare, got: %s", want, body)
		}
	}
}

func TestConnectorWebhook_TheEventAllowlistStillNarrows(t *testing.T) {
	s, cfg, pt := connectorBoundServer(t, []connection.Capability{connection.CapTrigger})
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		t.Fatal("an event the webhook's own allowlist excludes must never launch")
		return "", nil
	}
	cfg.EventAllowlist = []string{"pull_request"}

	w := httptest.NewRecorder()
	s.handleForgejoWebhook(w, fjReq(gitlabCtx(cfg), fjComment, "X-Forgejo-Event", prforge.EventHeaderIssueComment, pt, "X-Forgejo-Signature"))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "allowlist") {
		t.Fatalf("code=%d body=%s — the operator's narrowing applies on top of the package's declaration", w.Code, w.Body.String())
	}
}

func TestConnectorWebhook_AConnectionWithoutTheGrantIsRefusedTyped(t *testing.T) {
	// The connection holds `action` only: automation may spend it, no
	// delivery may be believed on its word.
	s, cfg, pt := connectorBoundServer(t, []connection.Capability{connection.CapAction})
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		t.Fatal("a connection without the trigger grant must never dispatch")
		return "", nil
	}

	w := httptest.NewRecorder()
	s.handleForgejoWebhook(w, fjReq(gitlabCtx(cfg), fjComment, "X-Forgejo-Event", prforge.EventHeaderIssueComment, pt, "X-Forgejo-Signature"))
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, "filtered") || !strings.Contains(body, "may not serve") {
		t.Fatalf("the refusal must be the typed capability verdict, got: %s", body)
	}
}

func TestConnectorWebhook_TheBindingIsTenantScoped(t *testing.T) {
	s, cfg, pt := connectorBoundServer(t, []connection.Capability{connection.CapTrigger})
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		t.Fatal("another tenant's connection must never vouch for a delivery")
		return "", nil
	}
	// The config belongs to t2 and names t1's connection id: the store's
	// tenant-scoped Get reads not-found, and the delivery is filtered.
	cfg.TenantID = "t2"

	w := httptest.NewRecorder()
	s.handleForgejoWebhook(w, fjReq(gitlabCtx(cfg), fjComment, "X-Forgejo-Event", prforge.EventHeaderIssueComment, pt, "X-Forgejo-Signature"))
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, "filtered") || !strings.Contains(body, "no longer exists") {
		t.Fatalf("a cross-tenant binding must read as a missing connection, got: %s", body)
	}
}

// A REVOKED connection vouches for no delivery (the HIGH of review r1): the
// trigger grant rides on a LIVE record, as at the action/agent chokepoint.
func TestConnectorWebhook_ARevokedConnectionVouchesForNothing(t *testing.T) {
	s, cfg, pt := connectorBoundServer(t, []connection.Capability{connection.CapTrigger})
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		t.Fatal("a revoked connection must never dispatch")
		return "", nil
	}
	conn, err := s.connectionStore.Get(context.Background(), "t1", "conn-fj")
	if err != nil {
		t.Fatal(err)
	}
	conn.Status = connection.StatusRevoked
	if err := s.connectionStore.Update(context.Background(), conn); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	s.handleForgejoWebhook(w, fjReq(gitlabCtx(cfg), fjComment, "X-Forgejo-Event", prforge.EventHeaderIssueComment, pt, "X-Forgejo-Signature"))
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, "filtered") || !strings.Contains(body, "revoked") {
		t.Fatalf("the refusal must name the revoked connection, got: %s", body)
	}
}

// The project allowlist binds an org-level hook to the repos the operator
// listed (r1 MEDIUM).
func TestConnectorWebhook_TheProjectAllowlistStillNarrows(t *testing.T) {
	s, cfg, pt := connectorBoundServer(t, []connection.Capability{connection.CapTrigger})
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		t.Fatal("a repo outside the project allowlist must never launch")
		return "", nil
	}
	cfg.ProjectAllowlist = []string{"acme/other"}

	w := httptest.NewRecorder()
	s.handleForgejoWebhook(w, fjReq(gitlabCtx(cfg), fjComment, "X-Forgejo-Event", prforge.EventHeaderIssueComment, pt, "X-Forgejo-Signature"))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "project allowlist") {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
}

// The author allowlist reads the delivery's sender, like the legacy lanes
// (r1 MEDIUM).
func TestConnectorWebhook_TheAuthorAllowlistStillNarrows(t *testing.T) {
	s, cfg, pt := connectorBoundServer(t, []connection.Capability{connection.CapTrigger})
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		t.Fatal("an author outside the allowlist must never launch")
		return "", nil
	}
	cfg.AuthorAllowlist = []string{"bob"}

	w := httptest.NewRecorder()
	s.handleForgejoWebhook(w, fjReq(gitlabCtx(cfg), fjComment, "X-Forgejo-Event", prforge.EventHeaderIssueComment, pt, "X-Forgejo-Signature"))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "author allowlist") {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
}

// The hold-label veto pauses this auto-launch lane like every other (r1 LOW).
func TestConnectorWebhook_AHoldLabelSuppressesTheLaunch(t *testing.T) {
	s, cfg, pt := connectorBoundServer(t, []connection.Capability{connection.CapTrigger})
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		t.Fatal("a held subject must never launch")
		return "", nil
	}
	cfg.HoldLabels = []string{"pause"}
	body := strings.Replace(fjComment, `"issue": {"number": 7}`, `"issue": {"number": 7, "labels": [{"name": "pause"}]}`, 1)

	w := httptest.NewRecorder()
	s.handleForgejoWebhook(w, fjReq(gitlabCtx(cfg), body, "X-Forgejo-Event", prforge.EventHeaderIssueComment, pt, "X-Forgejo-Signature"))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "filtered") {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
}

// The CRUD half: the binding is validated when it is WRITTEN, not discovered
// per delivery.
func TestWebhookCRUD_ConnectorBindingIsValidatedAtWrite(t *testing.T) {
	s, _, _ := connectorBoundServer(t, []connection.Capability{connection.CapTrigger})
	ctx := superAdminCtx()

	// A valid binding is stored.
	w := httptest.NewRecorder()
	s.handleCreateWebhook(w, whReq(ctx, "POST", "/api/teams/t1/webhooks",
		`{"name":"fj-conn","provider":"forgejo","bot_ids":["review-pr"],"connector_connection_id":"conn-fj"}`, "t1", ""))
	if w.Code != http.StatusCreated {
		t.Fatalf("create with a valid binding: code=%d body=%s", w.Code, w.Body.String())
	}
	var created webhookWithToken
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Config.ConnectorConnectionID != "conn-fj" {
		t.Fatalf("the binding was not stored: %+v", created.Config)
	}

	// An unknown connection is refused.
	w = httptest.NewRecorder()
	s.handleCreateWebhook(w, whReq(ctx, "POST", "/api/teams/t1/webhooks",
		`{"name":"fj-bad","provider":"forgejo","bot_ids":["review-pr"],"connector_connection_id":"nope"}`, "t1", ""))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown connection: code=%d body=%s", w.Code, w.Body.String())
	}

	// A gitlab webhook cannot bind a forgejo connection.
	w = httptest.NewRecorder()
	s.handleCreateWebhook(w, whReq(ctx, "POST", "/api/teams/t1/webhooks",
		`{"name":"gl-bad","provider":"gitlab","bot_ids":["review-pr"],"connector_connection_id":"conn-fj"}`, "t1", ""))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("cross-connector binding: code=%d body=%s", w.Code, w.Body.String())
	}

	// Clearing the binding on update returns the webhook to the legacy lane.
	w = httptest.NewRecorder()
	s.handleUpdateWebhook(w, whReq(ctx, "PATCH", "/api/teams/t1/webhooks/"+created.Config.ID,
		`{"connector_connection_id":""}`, "t1", created.Config.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("clear binding: code=%d body=%s", w.Code, w.Body.String())
	}
	stored, err := s.webhookConfigs.Get(context.Background(), created.Config.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ConnectorConnectionID != "" {
		t.Fatalf("the clear did not land: %q", stored.ConnectorConnectionID)
	}
}

// M4: a connection WITHOUT the trigger grant cannot be bound — the refusal
// happens at write, with the fix named.
func TestWebhookCRUD_BindingWithoutTheTriggerGrantIsRefused(t *testing.T) {
	s, _, _ := connectorBoundServer(t, []connection.Capability{connection.CapAction})
	ctx := superAdminCtx()

	w := httptest.NewRecorder()
	s.handleCreateWebhook(w, whReq(ctx, "POST", "/api/teams/t1/webhooks",
		`{"name":"fj-nogrant","provider":"forgejo","bot_ids":["review-pr"],"connector_connection_id":"conn-fj"}`, "t1", ""))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "trigger") {
		t.Fatalf("the refusal must name the missing grant, got: %s", w.Body.String())
	}
}

// The HIGH's write half: a REVOKED connection cannot be bound either.
func TestWebhookCRUD_BindingARevokedConnectionIsRefused(t *testing.T) {
	s, _, _ := connectorBoundServer(t, []connection.Capability{connection.CapTrigger})
	conn, err := s.connectionStore.Get(context.Background(), "t1", "conn-fj")
	if err != nil {
		t.Fatal(err)
	}
	conn.Status = connection.StatusRevoked
	if err := s.connectionStore.Update(context.Background(), conn); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	s.handleCreateWebhook(w, whReq(superAdminCtx(), "POST", "/api/teams/t1/webhooks",
		`{"name":"fj-revoked","provider":"forgejo","bot_ids":["review-pr"],"connector_connection_id":"conn-fj"}`, "t1", ""))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "revoked") {
		t.Fatalf("the refusal must name the status, got: %s", w.Body.String())
	}
}

// M5: an INVALID binding on PATCH is refused and the stored binding stands.
func TestWebhookCRUD_APatchWithAnInvalidBindingIsRefused(t *testing.T) {
	s, _, _ := connectorBoundServer(t, []connection.Capability{connection.CapTrigger})
	ctx := superAdminCtx()

	w := httptest.NewRecorder()
	s.handleCreateWebhook(w, whReq(ctx, "POST", "/api/teams/t1/webhooks",
		`{"name":"fj-conn","provider":"forgejo","bot_ids":["review-pr"],"connector_connection_id":"conn-fj"}`, "t1", ""))
	if w.Code != http.StatusCreated {
		t.Fatalf("create: code=%d body=%s", w.Code, w.Body.String())
	}
	var created webhookWithToken
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	w = httptest.NewRecorder()
	s.handleUpdateWebhook(w, whReq(ctx, "PATCH", "/api/teams/t1/webhooks/"+created.Config.ID,
		`{"connector_connection_id":"nope"}`, "t1", created.Config.ID))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("patch with an unknown connection: code=%d body=%s", w.Code, w.Body.String())
	}
	stored, err := s.webhookConfigs.Get(context.Background(), created.Config.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ConnectorConnectionID != "conn-fj" {
		t.Fatalf("the refused patch must not move the stored binding, got %q", stored.ConnectorConnectionID)
	}
}

// M6: a connection flipped to ANOTHER connector after the binding stops
// vouching for this webhook's deliveries — the write-time validation is
// re-checked at read, because the record may have changed since.
func TestConnectorWebhook_AFlippedConnectorStopsVouching(t *testing.T) {
	s, cfg, pt := connectorBoundServer(t, []connection.Capability{connection.CapTrigger})
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		t.Fatal("a connection flipped to another connector must never dispatch")
		return "", nil
	}
	// A catalog serving BOTH connectors, each declaring the delivered event:
	// without the connector match, the flipped connection's own package would
	// happily vouch for the delivery — which is the confusion the match
	// refuses. (The baked catalog holding only forgejo would catch the flip
	// one arm later, and the match would go unpinned.)
	s.connectorCatalog = connection.NewMemoryCatalog(
		&spec.Package{Connector: spec.Connector{ID: "forgejo"},
			Triggers: []spec.Trigger{{ID: "forgejo.issue.commented", Event: "issue_comment"}}},
		&spec.Package{Connector: spec.Connector{ID: "gitlab"},
			Triggers: []spec.Trigger{{ID: "gitlab.note.commented", Event: "issue_comment"}}},
	)
	conn, err := s.connectionStore.Get(context.Background(), "t1", "conn-fj")
	if err != nil {
		t.Fatal(err)
	}
	conn.Connector = "gitlab"
	if err := s.connectionStore.Update(context.Background(), conn); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	s.handleForgejoWebhook(w, fjReq(gitlabCtx(cfg), fjComment, "X-Forgejo-Event", prforge.EventHeaderIssueComment, pt, "X-Forgejo-Signature"))
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, "filtered") || !strings.Contains(body, "gitlab") {
		t.Fatalf("the refusal must name the connector mismatch, got: %s", body)
	}
}
