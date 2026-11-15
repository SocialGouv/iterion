package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/connector/intake"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
	"github.com/SocialGouv/iterion/pkg/identity"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// The ADR-168 witnesses of the connector ingress lane, driven through the
// registered route (the per-address limit included) against ingresses
// created through the webhook CRUD — the write half and the read half of the
// same contract.
//
// Slack's request is the vendor's documented example (docs.slack.dev,
// "Verifying requests from Slack"), its signature the documented one. The
// other packages are minted or token schemes whose signatures the tests
// compute with crypto/hmac, an oracle independent of the code under test;
// the reader's own vendor vectors live in pkg/connector/ingress.

const (
	ingSlackBody      = "token=xyzz0WbapA4vBCDEFasx0q6G&team_id=T1DC2JH3J&team_domain=testteamnow&channel_id=G8PSS9T3V&channel_name=foobar&user_id=U2CERLKJA&user_name=roadrunner&command=%2Fwebhook-collect&text=&response_url=https%3A%2F%2Fhooks.slack.com%2Fcommands%2FT1DC2JH3J%2F397700885554%2F96rGlfmibIGlgcZRskXaIFfN&trigger_id=398738663015.47445629121.803a0bc887a14d10d2c447fce8b6703c"
	ingSlackSecret    = "8f742231b10e8888abcd99yyyzzz85a5"
	ingSlackTimestamp = "1531420618"
	ingSlackSignature = "v0=a2114d57b48eac39b9ad189dd8316235a7b4a8d21a10bd27519666489c69b503"
	ingSlackTeam      = "T1DC2JH3J"
	// The form's legacy verification token: never stored.
	ingSlackFormToken = "xyzz0WbapA4vBCDEFasx0q6G"
)

func ingSlackPackage() *spec.Package {
	return &spec.Package{
		Connector: spec.Connector{ID: "slack"},
		Ingress: &spec.Ingress{
			Body: "form",
			Credential: spec.IngressCredential{Scheme: spec.SchemeHMACSHA256, In: "header", Name: "X-Slack-Signature",
				Encoding: "hex", Prefix: "v0=", Signed: "v0:{timestamp}:{body}", Secret: "vendor"},
			Timestamp:    &spec.IngressTimestamp{In: "header", Name: "X-Slack-Request-Timestamp", Unit: "s", Tolerance: "5m"},
			Installation: "payload.team_id",
			Event:        spec.IngressLocator{From: "payload.command"},
			Headers:      []string{"X-Slack-Request-Timestamp"},
			Redact:       []string{"token"},
		},
		Triggers: []spec.Trigger{{
			ID: "slack.command.invoked", Event: "/webhook-collect",
			Outputs: []spec.TriggerOutput{{Name: "actor", From: "payload.user_name"}},
		}},
	}
}

func ingSlackHeaders() http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/x-www-form-urlencoded")
	h.Set("X-Slack-Signature", ingSlackSignature)
	h.Set("X-Slack-Request-Timestamp", ingSlackTimestamp)
	return h
}

// ingForgePackage is a minted HMAC package, with a handshake and a declared
// acknowledgement that is not the default.
func ingForgePackage() *spec.Package {
	return &spec.Package{
		Connector: spec.Connector{ID: "forge"},
		Ingress: &spec.Ingress{
			Credential: spec.IngressCredential{Scheme: spec.SchemeHMACSHA256, In: "header", Name: "X-Forge-Signature",
				Encoding: "hex", Signed: "{body}", Secret: "minted"},
			Event:      spec.IngressLocator{Header: "X-Forge-Event"},
			DeliveryID: &spec.IngressLocator{Header: "X-Forge-Delivery"},
			Headers:    []string{"X-Forge-Event", "X-Forge-Delivery"},
			Handshake:  &spec.IngressHandshake{When: `payload.type == "url_verification"`, Respond: "payload.challenge"},
			Ack:        &spec.IngressAck{Status: http.StatusAccepted, Body: `{"ok":true}`},
		},
		Triggers: []spec.Trigger{{
			ID: "forge.issue.commented", Event: "issue_comment",
			Outputs: []spec.TriggerOutput{{Name: "actor", From: "payload.sender.login"}},
		}},
	}
}

// ingChatPackage carries its vendor's token in the body (a Mattermost
// outgoing webhook's shape).
func ingChatPackage() *spec.Package {
	return &spec.Package{
		Connector: spec.Connector{ID: "chat"},
		Ingress: &spec.Ingress{
			Credential: spec.IngressCredential{Scheme: spec.SchemeToken, In: "body", Name: "token", Secret: "vendor"},
			Event:      spec.IngressLocator{From: "payload.trigger_word"},
			Headers:    []string{"X-Chat-Listed"},
		},
		Triggers: []spec.Trigger{{ID: "chat.message.posted", Event: "deploy"}},
	}
}

const ingChatSecret = "chat-vendor-token-0f3a9c"

func ingHMAC(key, msg string) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(msg))
	return hex.EncodeToString(m.Sum(nil))
}

type ingressFixture struct {
	s       *Server
	intakes *intake.MemoryStore
	conns   *connection.MemoryStore
}

func newIngressFixture(t *testing.T, pkgs ...*spec.Package) *ingressFixture {
	t.Helper()
	s := newWebhookTestServer(t)
	conns := connection.NewMemoryStore()
	for _, p := range pkgs {
		if err := conns.Create(context.Background(), ingConnection(p.Connector.ID)); err != nil {
			t.Fatal(err)
		}
	}
	s.connectionStore = conns
	s.connectorCatalog = connection.NewMemoryCatalog(pkgs...)
	in := intake.NewMemoryStore()
	s.connectorIntakes = in
	s.registerConnectorIngressRoute()
	return &ingressFixture{s: s, intakes: in, conns: conns}
}

func ingConnection(connector string) connection.Connection {
	return connection.Connection{
		ID: "conn-" + connector, TenantID: "t1", Connector: connector, Alias: connector,
		SchemeID: "token", Status: connection.StatusActive,
		Capabilities:  []connection.Capability{connection.CapTrigger},
		BaseURL:       "https://" + connector + ".example",
		AuthPlacement: connection.AuthPlacement{Kind: "api_key", In: "header", Name: "Authorization"},
	}
}

// create creates an ingress through the CRUD and returns the row and the
// secret it showed.
func (f *ingressFixture) create(t *testing.T, body string) (webhooks.Config, string) {
	t.Helper()
	w := httptest.NewRecorder()
	f.s.handleCreateWebhook(w, whReq(superAdminCtx(), "POST", "/api/teams/t1/webhooks", body, "t1", ""))
	if w.Code != http.StatusCreated {
		t.Fatalf("create ingress %s: code=%d body=%s", body, w.Code, w.Body.String())
	}
	var got webhookWithToken
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got.Config, got.Token
}

func (f *ingressFixture) createSlack(t *testing.T, installation string) webhooks.Config {
	t.Helper()
	cfg, shown := f.create(t, `{"name":"slack","provider":"connector","connector_connection_id":"conn-slack","installation":"`+installation+`","vendor_secret":"`+ingSlackSecret+`"}`)
	if shown != "" {
		t.Fatalf("a vendor secret is never answered back, got %q", shown)
	}
	// Slack's documented request was signed at its own instant.
	ts := time.Unix(1531420618, 0).Add(30 * time.Second)
	f.s.ingressClock = func() time.Time { return ts }
	return cfg
}

func (f *ingressFixture) deliver(id, body string, h http.Header) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/api/webhooks/c/"+id, strings.NewReader(body))
	r.RemoteAddr = "203.0.113.7:4242"
	for k, v := range h {
		r.Header[k] = v
	}
	w := httptest.NewRecorder()
	f.s.mux.ServeHTTP(w, r)
	return w
}

func (f *ingressFixture) forgeDelivery(secret, body, event, deliveryID string) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("X-Forge-Event", event)
	h.Set("X-Forge-Delivery", deliveryID)
	h.Set("X-Forge-Signature", ingHMAC(secret, body))
	return h
}

func (f *ingressFixture) intakeRows(t *testing.T) []intake.Intake {
	t.Helper()
	rows, err := f.intakes.ListIntakes(context.Background(), "t1", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func (f *ingressFixture) monthly(t *testing.T) int {
	t.Helper()
	n, err := f.s.webhookCounter.OrgCount(context.Background(), "t1", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *ingressFixture) bucketTouched(t *testing.T, cfgID string) bool {
	t.Helper()
	lim, ok := f.s.authLimiter.(*authRateLimiter)
	if !ok {
		t.Fatalf("the fixture's limiter is %T, not the in-memory one whose buckets it reads", f.s.authLimiter)
	}
	lim.mu.Lock()
	defer lim.mu.Unlock()
	_, touched := lim.buckets["wh:"+cfgID]
	return touched
}

// assertNothingWritten is the "changes no durable state" half of a refusal:
// no intake row, no monthly count, no per-webhook bucket.
func (f *ingressFixture) assertNothingWritten(t *testing.T, cfgID string) {
	t.Helper()
	if rows := f.intakeRows(t); len(rows) != 0 {
		t.Fatalf("a refused delivery wrote %d intake rows", len(rows))
	}
	if n := f.monthly(t); n != 0 {
		t.Fatalf("a refused delivery was charged to the monthly quota (count %d)", n)
	}
	if f.bucketTouched(t, cfgID) {
		t.Fatal("a refused delivery spent the webhook's rate bucket")
	}
}

// Slack's documented request, through the route: answered with the package's
// ack, stored as one intake for its trigger, without the form's token.
func TestIngress_SlackDocumentedRequestIsStoredAndAcknowledged(t *testing.T) {
	f := newIngressFixture(t, ingSlackPackage())
	cfg := f.createSlack(t, ingSlackTeam)

	w := f.deliver(cfg.ID, ingSlackBody, ingSlackHeaders())
	if w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Fatalf("documented request: code=%d body=%q, want the default ack (200, empty)", w.Code, w.Body.String())
	}
	rows := f.intakeRows(t)
	if len(rows) != 1 {
		t.Fatalf("intakes = %d, want 1", len(rows))
	}
	got := rows[0]
	if got.TriggerID != "slack.command.invoked" || got.ConnectionID != "conn-slack" || got.IngressID != cfg.ID || got.State != intake.IntakePending {
		t.Fatalf("intake = %+v", got)
	}
	if got.ExpiresAt != nil {
		t.Fatalf("the fixture's retention keeps rows, got expiry %v", got.ExpiresAt)
	}
	ev := string(got.Event)
	if strings.Contains(ev, ingSlackFormToken) || strings.Contains(ev, ingSlackSignature) {
		t.Fatalf("the stored event carries a credential: %s", ev)
	}
	if !strings.Contains(ev, `"actor":"roadrunner"`) {
		t.Fatalf("the stored event lacks its output: %s", ev)
	}
	if n := f.monthly(t); n != 1 {
		t.Fatalf("monthly count = %d, want 1", n)
	}
}

// Every way to fail the proof answers 401 and writes nothing — not the
// intake, not the monthly counter, not the per-webhook bucket — for a header
// HMAC with a header timestamp, a minted header HMAC, a token in the body,
// and an installation bound to another value.
func TestIngress_AnUnauthenticatedDeliveryWritesNothing(t *testing.T) {
	forgeBody := `{"sender":{"login":"alice"}}`
	cases := []struct {
		name string
		run  func(t *testing.T, f *ingressFixture) (string, *httptest.ResponseRecorder)
	}{
		{"slack wrong signature", func(t *testing.T, f *ingressFixture) (string, *httptest.ResponseRecorder) {
			cfg := f.createSlack(t, ingSlackTeam)
			h := ingSlackHeaders()
			h.Set("X-Slack-Signature", "v0="+ingHMAC("another-secret", "v0:"+ingSlackTimestamp+":"+ingSlackBody))
			return cfg.ID, f.deliver(cfg.ID, ingSlackBody, h)
		}},
		{"slack missing signature", func(t *testing.T, f *ingressFixture) (string, *httptest.ResponseRecorder) {
			cfg := f.createSlack(t, ingSlackTeam)
			h := ingSlackHeaders()
			h.Del("X-Slack-Signature")
			return cfg.ID, f.deliver(cfg.ID, ingSlackBody, h)
		}},
		{"slack repeated signature", func(t *testing.T, f *ingressFixture) (string, *httptest.ResponseRecorder) {
			cfg := f.createSlack(t, ingSlackTeam)
			h := ingSlackHeaders()
			h.Add("X-Slack-Signature", ingSlackSignature)
			return cfg.ID, f.deliver(cfg.ID, ingSlackBody, h)
		}},
		{"slack timestamp out of window", func(t *testing.T, f *ingressFixture) (string, *httptest.ResponseRecorder) {
			cfg := f.createSlack(t, ingSlackTeam)
			f.s.ingressClock = func() time.Time { return time.Unix(1531420618, 0).Add(6 * time.Minute) }
			return cfg.ID, f.deliver(cfg.ID, ingSlackBody, ingSlackHeaders())
		}},
		{"slack another installation", func(t *testing.T, f *ingressFixture) (string, *httptest.ResponseRecorder) {
			cfg := f.createSlack(t, "T0THERTEAM")
			return cfg.ID, f.deliver(cfg.ID, ingSlackBody, ingSlackHeaders())
		}},
		{"minted hmac wrong secret", func(t *testing.T, f *ingressFixture) (string, *httptest.ResponseRecorder) {
			cfg, _ := f.create(t, `{"name":"forge","provider":"connector","connector_connection_id":"conn-forge"}`)
			return cfg.ID, f.deliver(cfg.ID, forgeBody, f.forgeDelivery("iwh_not-the-minted-one", forgeBody, "issue_comment", "d-1"))
		}},
		{"token in the body missing", func(t *testing.T, f *ingressFixture) (string, *httptest.ResponseRecorder) {
			cfg, _ := f.create(t, `{"name":"chat","provider":"connector","connector_connection_id":"conn-chat","vendor_secret":"`+ingChatSecret+`"}`)
			return cfg.ID, f.deliver(cfg.ID, `{"trigger_word":"deploy"}`, http.Header{"Content-Type": {"application/json"}})
		}},
		{"token in the body wrong", func(t *testing.T, f *ingressFixture) (string, *httptest.ResponseRecorder) {
			cfg, _ := f.create(t, `{"name":"chat","provider":"connector","connector_connection_id":"conn-chat","vendor_secret":"`+ingChatSecret+`"}`)
			return cfg.ID, f.deliver(cfg.ID, `{"token":"guessed","trigger_word":"deploy"}`, http.Header{"Content-Type": {"application/json"}})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newIngressFixture(t, ingSlackPackage(), ingForgePackage(), ingChatPackage())
			id, w := tc.run(t, f)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("code=%d body=%s, want 401", w.Code, w.Body.String())
			}
			f.assertNothingWritten(t, id)
		})
	}
}

// The control of the table above: the same fixtures, proven, are accepted —
// a refusal is the proof failing, not the fixture.
func TestIngress_TheProvenTwinsOfTheRefusalsAreAccepted(t *testing.T) {
	f := newIngressFixture(t, ingForgePackage(), ingChatPackage())
	forge, minted := f.create(t, `{"name":"forge","provider":"connector","connector_connection_id":"conn-forge"}`)
	if !strings.HasPrefix(minted, webhooks.TokenPrefix) {
		t.Fatalf("a minted ingress shows its secret once, got %q", minted)
	}
	body := `{"sender":{"login":"alice"}}`
	if w := f.deliver(forge.ID, body, f.forgeDelivery(minted, body, "issue_comment", "d-1")); w.Code != http.StatusAccepted {
		t.Fatalf("minted hmac: code=%d body=%s", w.Code, w.Body.String())
	}
	chat, _ := f.create(t, `{"name":"chat","provider":"connector","connector_connection_id":"conn-chat","vendor_secret":"`+ingChatSecret+`"}`)
	if w := f.deliver(chat.ID, `{"token":"`+ingChatSecret+`","trigger_word":"deploy"}`, http.Header{"Content-Type": {"application/json"}}); w.Code != http.StatusOK {
		t.Fatalf("token in the body: code=%d body=%s", w.Code, w.Body.String())
	}
	if rows := f.intakeRows(t); len(rows) != 2 {
		t.Fatalf("intakes = %d, want 2", len(rows))
	}
}

// The pin: a package whose contract changed after the bind — here a team
// override that drops the replay window — is refused, naming both digests,
// with nothing written, until the operator re-binds.
func TestIngress_AChangedContractIsRefusedUntilReBound(t *testing.T) {
	f := newIngressFixture(t, ingSlackPackage())
	cfg := f.createSlack(t, ingSlackTeam)

	weakened := ingSlackPackage()
	weakened.Ingress.Timestamp = nil
	weakened.Ingress.Credential.Signed = "{body}"
	f.s.connectorCatalog = connection.NewMemoryCatalog(weakened)
	h := ingSlackHeaders()
	h.Set("X-Slack-Signature", "v0="+ingHMAC(ingSlackSecret, ingSlackBody))

	w := f.deliver(cfg.ID, ingSlackBody, h)
	newDigest, err := spec.IngressDigest(weakened.Ingress)
	if err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), cfg.ContractDigest) || !strings.Contains(w.Body.String(), newDigest) {
		t.Fatalf("changed contract: code=%d body=%s, want 409 naming %s and %s", w.Code, w.Body.String(), cfg.ContractDigest, newDigest)
	}
	f.assertNothingWritten(t, cfg.ID)

	// Restating the binding consents to nothing — a client that echoes the
	// row back (get, edit the name, update) leaves the pin where it was.
	if pw := f.patch(t, cfg.ID, `{"name":"slack renamed","connector_connection_id":"conn-slack","installation":"`+ingSlackTeam+`"}`); pw.Code != http.StatusOK {
		t.Fatalf("echo update: code=%d body=%s", pw.Code, pw.Body.String())
	}
	if w := f.deliver(cfg.ID, ingSlackBody, h); w.Code != http.StatusConflict {
		t.Fatalf("after an echo update: code=%d body=%s, want the contract still refused", w.Code, w.Body.String())
	}
	// A digest that is not the current contract's consents to nothing.
	if pw := f.patch(t, cfg.ID, `{"contract_digest":"`+cfg.ContractDigest+`"}`); pw.Code != http.StatusConflict {
		t.Fatalf("a stale digest: code=%d body=%s, want 409", pw.Code, pw.Body.String())
	}
	// Restating the contract the 409 named is the consent: the same delivery
	// is then accepted.
	if pw := f.patch(t, cfg.ID, `{"contract_digest":"`+newDigest+`"}`); pw.Code != http.StatusOK {
		t.Fatalf("consent: code=%d body=%s", pw.Code, pw.Body.String())
	}
	if w := f.deliver(cfg.ID, ingSlackBody, h); w.Code != http.StatusOK {
		t.Fatalf("after the consent: code=%d body=%s", w.Code, w.Body.String())
	}
}

// An accepted delivery is answered with the package's exact ack while its
// matching is still pending — no worker has seen it.
func TestIngress_TheDeclaredAckAnswersWhileMatchingIsPending(t *testing.T) {
	f := newIngressFixture(t, ingForgePackage())
	f.s.cfg.ConnectorIntakeRetention = 72 * time.Hour
	at := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	f.s.ingressClock = func() time.Time { return at }
	cfg, minted := f.create(t, `{"name":"forge","provider":"connector","connector_connection_id":"conn-forge"}`)
	body := `{"sender":{"login":"alice"}}`
	w := f.deliver(cfg.ID, body, f.forgeDelivery(minted, body, "issue_comment", "d-1"))
	if w.Code != http.StatusAccepted || w.Body.String() != `{"ok":true}` || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("ack: code=%d body=%q type=%q, want the declared 202 {\"ok\":true}", w.Code, w.Body.String(), w.Header().Get("Content-Type"))
	}
	rows := f.intakeRows(t)
	if len(rows) != 1 || rows[0].State != intake.IntakePending {
		t.Fatalf("intakes = %+v, want one pending", rows)
	}
	// The row keeps the retention the deployment states: its dedup window.
	if !rows[0].ReceivedAt.Equal(at) || rows[0].ExpiresAt == nil || !rows[0].ExpiresAt.Equal(at.Add(72*time.Hour)) {
		t.Fatalf("received %v, expires %v; want %v and 72h later", rows[0].ReceivedAt, rows[0].ExpiresAt, at)
	}
}

// Over the backlog cap the answer is 503 with a Retry-After and nothing is
// written; a redelivery of a stored delivery is still acknowledged.
func TestIngress_OverTheBacklogCapAnswers503AndWritesNothing(t *testing.T) {
	f := newIngressFixture(t, ingForgePackage())
	f.s.cfg.ConnectorIntakeBacklogMax = 1
	cfg, minted := f.create(t, `{"name":"forge","provider":"connector","connector_connection_id":"conn-forge"}`)
	first := `{"sender":{"login":"alice"}}`
	if w := f.deliver(cfg.ID, first, f.forgeDelivery(minted, first, "issue_comment", "d-1")); w.Code != http.StatusAccepted {
		t.Fatalf("first: code=%d body=%s", w.Code, w.Body.String())
	}
	second := `{"sender":{"login":"bob"}}`
	w := f.deliver(cfg.ID, second, f.forgeDelivery(minted, second, "issue_comment", "d-2"))
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
		t.Fatalf("over the cap: code=%d retry-after=%q body=%s, want 503 with Retry-After", w.Code, w.Header().Get("Retry-After"), w.Body.String())
	}
	if rows := f.intakeRows(t); len(rows) != 1 {
		t.Fatalf("intakes = %d after the refusal, want 1", len(rows))
	}
	if n := f.monthly(t); n != 1 {
		t.Fatalf("the refusal was charged: monthly count = %d, want 1", n)
	}
	if w := f.deliver(cfg.ID, first, f.forgeDelivery(minted, first, "issue_comment", "d-1")); w.Code != http.StatusAccepted {
		t.Fatalf("a redelivery while full: code=%d body=%s, want its ack", w.Code, w.Body.String())
	}
}

// One delivery redelivered — and replayed with a changed delivery-id header,
// which is unsigned — opens one intake, is acknowledged each time, and is
// charged once.
func TestIngress_ARedeliveryOpensNoSecondIntake(t *testing.T) {
	f := newIngressFixture(t, ingForgePackage())
	cfg, minted := f.create(t, `{"name":"forge","provider":"connector","connector_connection_id":"conn-forge"}`)
	body := `{"sender":{"login":"alice"}}`
	for _, id := range []string{"d-1", "d-1", "d-forged"} {
		if w := f.deliver(cfg.ID, body, f.forgeDelivery(minted, body, "issue_comment", id)); w.Code != http.StatusAccepted {
			t.Fatalf("delivery %s: code=%d body=%s", id, w.Code, w.Body.String())
		}
	}
	if rows := f.intakeRows(t); len(rows) != 1 {
		t.Fatalf("intakes = %d, want 1", len(rows))
	}
	if n := f.monthly(t); n != 1 {
		t.Fatalf("monthly count = %d, want 1: a redelivery spends nothing", n)
	}
}

// Slack re-signs a retry at its own instant (its retries run up to five
// minutes after the first attempt, past a window that kept the first
// instant): the retry meets the first delivery's row.
func TestIngress_AVendorRetryReSignedLaterOpensNoSecondIntake(t *testing.T) {
	f := newIngressFixture(t, ingSlackPackage())
	cfg := f.createSlack(t, ingSlackTeam)
	if w := f.deliver(cfg.ID, ingSlackBody, ingSlackHeaders()); w.Code != http.StatusOK {
		t.Fatalf("first attempt: code=%d body=%s", w.Code, w.Body.String())
	}
	const retryTS = "1531420918" // five minutes on
	f.s.ingressClock = func() time.Time { return time.Unix(1531420918, 0).Add(2 * time.Second) }
	h := ingSlackHeaders()
	h.Set("X-Slack-Request-Timestamp", retryTS)
	h.Set("X-Slack-Signature", "v0="+ingHMAC(ingSlackSecret, "v0:"+retryTS+":"+ingSlackBody))
	h.Set("X-Slack-Retry-Num", "3")
	if w := f.deliver(cfg.ID, ingSlackBody, h); w.Code != http.StatusOK {
		t.Fatalf("re-signed retry: code=%d body=%s", w.Code, w.Body.String())
	}
	if rows := f.intakeRows(t); len(rows) != 1 {
		t.Fatalf("intakes = %d, want 1: a re-signed retry is the same delivery", len(rows))
	}
	if n := f.monthly(t); n != 1 {
		t.Fatalf("monthly count = %d, want 1", n)
	}
}

// The canary: a unique value in the credential field and in an unlisted
// header reaches no stored intake; a listed header does.
func TestIngress_TheCanaryReachesNoIntake(t *testing.T) {
	f := newIngressFixture(t, ingChatPackage())
	cfg, _ := f.create(t, `{"name":"chat","provider":"connector","connector_connection_id":"conn-chat","vendor_secret":"`+ingChatSecret+`"}`)
	const canary = "CANARY-7d1e"
	h := http.Header{"Content-Type": {"application/json"}}
	h.Set("X-Chat-Unlisted", canary)
	h.Set("X-Chat-Listed", "listed-value")
	if w := f.deliver(cfg.ID, `{"token":"`+ingChatSecret+`","trigger_word":"deploy"}`, h); w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	rows := f.intakeRows(t)
	if len(rows) != 1 {
		t.Fatalf("intakes = %d", len(rows))
	}
	ev := string(rows[0].Event)
	if strings.Contains(ev, canary) || strings.Contains(ev, ingChatSecret) {
		t.Fatalf("the stored event carries the canary or the credential: %s", ev)
	}
	if !strings.Contains(ev, "listed-value") {
		t.Fatalf("the stored event lacks the listed header: %s", ev)
	}
}

// A handshake is answered with its challenge and a filtered delivery with
// the ack; neither is stored nor charged.
func TestIngress_HandshakeAndFilteredWriteNothing(t *testing.T) {
	f := newIngressFixture(t, ingForgePackage())
	cfg, minted := f.create(t, `{"name":"forge","provider":"connector","connector_connection_id":"conn-forge"}`)
	hs := `{"type":"url_verification","challenge":"chal-42"}`
	w := f.deliver(cfg.ID, hs, f.forgeDelivery(minted, hs, "ping", "d-1"))
	if w.Code != http.StatusOK || w.Body.String() != "chal-42" {
		t.Fatalf("handshake: code=%d body=%q", w.Code, w.Body.String())
	}
	undeclared := `{"sender":{"login":"alice"}}`
	w = f.deliver(cfg.ID, undeclared, f.forgeDelivery(minted, undeclared, "push", "d-2"))
	if w.Code != http.StatusAccepted || w.Body.String() != `{"ok":true}` {
		t.Fatalf("filtered: code=%d body=%q, want the ack", w.Code, w.Body.String())
	}
	if rows := f.intakeRows(t); len(rows) != 0 {
		t.Fatalf("intakes = %d, want 0", len(rows))
	}
	if n := f.monthly(t); n != 0 {
		t.Fatalf("monthly count = %d, want 0", n)
	}
}

// A binding that cannot vouch — a revoked connection, a connection without
// the trigger grant — refuses before verification and writes nothing.
func TestIngress_ABindingThatCannotVouchWritesNothing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*connection.Connection)
	}{
		{"revoked", func(c *connection.Connection) { c.Status = connection.StatusRevoked }},
		{"no trigger grant", func(c *connection.Connection) { c.Capabilities = []connection.Capability{connection.CapAction} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newIngressFixture(t, ingForgePackage())
			cfg, minted := f.create(t, `{"name":"forge","provider":"connector","connector_connection_id":"conn-forge"}`)
			conn, err := f.conns.Get(context.Background(), "t1", "conn-forge")
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(&conn)
			if err := f.conns.Update(context.Background(), conn); err != nil {
				t.Fatal(err)
			}
			body := `{"sender":{"login":"alice"}}`
			if w := f.deliver(cfg.ID, body, f.forgeDelivery(minted, body, "issue_comment", "d-1")); w.Code != http.StatusConflict {
				t.Fatalf("code=%d body=%s, want 409", w.Code, w.Body.String())
			}
			f.assertNothingWritten(t, cfg.ID)
		})
	}
}

// An unknown id and another provider's id answer alike; a body over the cap
// is refused before anything reads it.
func TestIngress_UnknownIDsAnswerAlikeAndTheBodyIsCapped(t *testing.T) {
	f := newIngressFixture(t, ingForgePackage())
	other, _ := fjConfig(t, f.s)
	if err := f.s.webhookConfigs.Create(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"no-such-ingress", other.ID} {
		if w := f.deliver(id, `{}`, nil); w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "invalid webhook") {
			t.Fatalf("id %s: code=%d body=%s", id, w.Code, w.Body.String())
		}
	}
	cfg, _ := f.create(t, `{"name":"forge","provider":"connector","connector_connection_id":"conn-forge"}`)
	big := `{"pad":"` + strings.Repeat("x", maxWebhookBodyBytes) + `"}`
	if w := f.deliver(cfg.ID, big, nil); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over the cap: code=%d", w.Code)
	}
}

// A disabled ingress is refused after the proof, and nothing is written.
func TestIngress_ADisabledIngressWritesNothing(t *testing.T) {
	f := newIngressFixture(t, ingForgePackage())
	cfg, minted := f.create(t, `{"name":"forge","provider":"connector","connector_connection_id":"conn-forge","enabled":false}`)
	body := `{"sender":{"login":"alice"}}`
	if w := f.deliver(cfg.ID, body, f.forgeDelivery(minted, body, "issue_comment", "d-1")); w.Code != http.StatusGone {
		t.Fatalf("code=%d body=%s, want 410", w.Code, w.Body.String())
	}
	f.assertNothingWritten(t, cfg.ID)
}

// A team that may not launch has its deliveries refused after the proof,
// and nothing is written.
func TestIngress_ASuspendedTeamWritesNothing(t *testing.T) {
	f := newIngressFixture(t, ingForgePackage())
	cfg, minted := f.create(t, `{"name":"forge","provider":"connector","connector_connection_id":"conn-forge"}`)
	team, err := f.s.authStore().GetTeam(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	team.Status = identity.TeamStatusSuspended
	if err := f.s.authStore().UpdateTeam(context.Background(), team); err != nil {
		t.Fatal(err)
	}
	body := `{"sender":{"login":"alice"}}`
	if w := f.deliver(cfg.ID, body, f.forgeDelivery(minted, body, "issue_comment", "d-1")); w.Code != http.StatusForbidden {
		t.Fatalf("code=%d body=%s, want 403", w.Code, w.Body.String())
	}
	if rows := f.intakeRows(t); len(rows) != 0 {
		t.Fatalf("a suspended team's delivery wrote %d intakes", len(rows))
	}
	if n := f.monthly(t); n != 0 {
		t.Fatalf("a suspended team's delivery was charged (count %d)", n)
	}
}

// Before the proof only the per-(address, ingress) limit applies: a client
// hammering an ingress with unsigned deliveries meets 429 without any of the
// ingress's state being touched.
func TestIngress_ThePerAddressLimitAppliesBeforeTheProof(t *testing.T) {
	f := newIngressFixture(t, ingForgePackage())
	cfg, _ := f.create(t, `{"name":"forge","provider":"connector","connector_connection_id":"conn-forge"}`)
	var limited bool
	for i := 0; i <= int(connectorIngressIPBucket.burst); i++ {
		if w := f.deliver(cfg.ID, `{}`, nil); w.Code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatalf("no 429 after %v unsigned deliveries from one address", connectorIngressIPBucket.burst+1)
	}
	f.assertNothingWritten(t, cfg.ID)
}

// An id the sender invents creates no limiter key: the limiter's keys are
// shared with login, and junk ids would evict its buckets.
func TestIngress_AnInventedIDCreatesNoLimiterKey(t *testing.T) {
	f := newIngressFixture(t, ingForgePackage())
	lim, ok := f.s.authLimiter.(*authRateLimiter)
	if !ok {
		t.Fatalf("the fixture's limiter is %T", f.s.authLimiter)
	}
	count := func() int {
		lim.mu.Lock()
		defer lim.mu.Unlock()
		return len(lim.buckets)
	}
	before := count()
	for i := 0; i < 300; i++ {
		if w := f.deliver(fmt.Sprintf("junk-%d", i), `{}`, nil); w.Code != http.StatusUnauthorized {
			t.Fatalf("junk id: code=%d", w.Code)
		}
	}
	if after := count(); after != before {
		t.Fatalf("300 invented ids created %d limiter keys", after-before)
	}
}

// A package the catalog holds and cannot load waits for a repair: 409 — a
// retry would not heal it — naming no file of the deployment.
func TestIngress_APackageThatDoesNotLoadIsABindingVerdict(t *testing.T) {
	f := newIngressFixture(t, ingForgePackage())
	cfg, minted := f.create(t, `{"name":"forge","provider":"connector","connector_connection_id":"conn-forge"}`)
	f.s.connectorCatalog = brokenCatalog{}
	body := `{"sender":{"login":"alice"}}`
	w := f.deliver(cfg.ID, body, f.forgeDelivery(minted, body, "issue_comment", "d-1"))
	if w.Code != http.StatusConflict || strings.Contains(w.Body.String(), "/var/lib") {
		t.Fatalf("code=%d body=%s, want 409 naming no deployment path", w.Code, w.Body.String())
	}
	f.assertNothingWritten(t, cfg.ID)
}

func (f *ingressFixture) createCode(t *testing.T, body string) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	f.s.handleCreateWebhook(w, whReq(superAdminCtx(), "POST", "/api/teams/t1/webhooks", body, "t1", ""))
	return w.Code, w.Body.String()
}

func (f *ingressFixture) patch(t *testing.T, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	f.s.handleUpdateWebhook(w, whReq(superAdminCtx(), "PATCH", "/api/teams/t1/webhooks/"+id, body, "t1", id))
	return w
}

func (f *ingressFixture) rotate(t *testing.T, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	f.s.handleRotateWebhook(w, whReq(superAdminCtx(), "POST", "/api/teams/t1/webhooks/"+id+"/rotate", body, "t1", id))
	return w
}

// The write half: an ingress binds only what its package can be read
// through, and records the contract it consented to.
func TestIngressCRUD_TheBindingIsValidatedAndPinned(t *testing.T) {
	plain := &spec.Package{Connector: spec.Connector{ID: "plain"}}
	f := newIngressFixture(t, ingSlackPackage(), ingForgePackage(), plain)
	refused := map[string]string{
		"no connection":                    `{"name":"x","provider":"connector"}`,
		"a package without an ingress":     `{"name":"x","provider":"connector","connector_connection_id":"conn-plain"}`,
		"an installation left unset":       `{"name":"x","provider":"connector","connector_connection_id":"conn-slack","vendor_secret":"s"}`,
		"an installation nothing reads":    `{"name":"x","provider":"connector","connector_connection_id":"conn-forge","installation":"T1"}`,
		"a vendor secret left unset":       `{"name":"x","provider":"connector","connector_connection_id":"conn-slack","installation":"T1"}`,
		"a vendor secret for a minted one": `{"name":"x","provider":"connector","connector_connection_id":"conn-forge","vendor_secret":"s"}`,
		"a sign mode":                      `{"name":"x","provider":"connector","connector_connection_id":"conn-forge","sign_mode":"hmac"}`,
		"a bot scope":                      `{"name":"x","provider":"connector","connector_connection_id":"conn-forge","bot_ids":["review-pr"]}`,
		"an installation on a forge hook":  `{"name":"x","provider":"gitlab","bot_ids":["review-pr"],"installation":"T1"}`,
		"a vendor secret on a forge hook":  `{"name":"x","provider":"gitlab","bot_ids":["review-pr"],"vendor_secret":"s"}`,
		"an unknown connection":            `{"name":"x","provider":"connector","connector_connection_id":"conn-nope"}`,
	}
	for name, body := range refused {
		if code, out := f.createCode(t, body); code != http.StatusBadRequest {
			t.Errorf("%s: code=%d body=%s, want 400", name, code, out)
		}
	}
	if rows, err := f.s.webhookConfigs.ListByTenant(context.Background(), "t1"); err != nil || len(rows) != 0 {
		t.Fatalf("a refused create stored %d rows (err %v)", len(rows), err)
	}

	cfg := f.createSlack(t, ingSlackTeam)
	want, err := spec.IngressDigest(ingSlackPackage().Ingress)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := f.s.webhookConfigs.Get(context.Background(), cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ContractDigest != want || stored.Installation != ingSlackTeam || stored.ConnectorConnectionID != "conn-slack" || len(stored.HMACSecretSealed) == 0 {
		t.Fatalf("stored ingress = digest %q installation %q connection %q sealed %d bytes; want digest %q",
			stored.ContractDigest, stored.Installation, stored.ConnectorConnectionID, len(stored.HMACSecretSealed), want)
	}
}

// A connector ingress is its binding: it cannot be unbound, moved to another
// vendor's connection, or given a vendor secret by an update.
func TestIngressCRUD_AnIngressKeepsItsBinding(t *testing.T) {
	f := newIngressFixture(t, ingSlackPackage(), ingForgePackage())
	cfg := f.createSlack(t, ingSlackTeam)
	// Each refusal is read by its own words: two of these guards are backed
	// by a later check that would answer 400 too.
	for _, tc := range []struct{ name, body, says string }{
		{"unbind", `{"connector_connection_id":""}`, "cannot be unbound"},
		{"another vendor", `{"connector_connection_id":"conn-forge","installation":""}`, "create a new ingress for forge"},
		{"a vendor secret", `{"vendor_secret":"new"}`, "rotation"},
		{"a bot scope", `{"bot_ids":["review-pr"]}`, "do not apply to a connector ingress"},
		{"an installation removed", `{"installation":""}`, "set installation"},
	} {
		if w := f.patch(t, cfg.ID, tc.body); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), tc.says) {
			t.Errorf("%s: code=%d body=%s, want 400 saying %q", tc.name, w.Code, w.Body.String(), tc.says)
		}
	}
	gl, _ := fjConfig(t, f.s)
	gl.ID, gl.Provider, gl.SignMode = "gl-1", webhooks.ProviderGitLab, webhooks.SignModeToken
	gl.TenantID = "t1"
	if err := f.s.webhookConfigs.Create(context.Background(), gl); err != nil {
		t.Fatal(err)
	}
	if w := f.patch(t, gl.ID, `{"installation":"T1"}`); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "connector ingress only") {
		t.Errorf("an installation on a forge hook: code=%d body=%s", w.Code, w.Body.String())
	}
	stored, err := f.s.webhookConfigs.Get(context.Background(), cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ConnectorConnectionID != "conn-slack" || stored.Installation != ingSlackTeam {
		t.Fatalf("a refused update changed the binding: %+v", stored)
	}
}

// Rotation replaces the secret from where the package declares it — pasted
// again for a vendor's, minted again otherwise — and the old one stops
// verifying. A revoked connection does not block it: rotating a leaked
// secret is incident response.
func TestIngressCRUD_RotationReplacesTheSecret(t *testing.T) {
	f := newIngressFixture(t, ingForgePackage(), ingChatPackage())

	chat, _ := f.create(t, `{"name":"chat","provider":"connector","connector_connection_id":"conn-chat","vendor_secret":"`+ingChatSecret+`"}`)
	if w := f.rotate(t, chat.ID, ""); w.Code != http.StatusBadRequest {
		t.Fatalf("vendor rotation without a secret: code=%d body=%s", w.Code, w.Body.String())
	}
	w := f.rotate(t, chat.ID, `{"vendor_secret":"chat-rotated-secret"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("vendor rotation: code=%d body=%s", w.Code, w.Body.String())
	}
	var rotated webhookWithToken
	if err := json.Unmarshal(w.Body.Bytes(), &rotated); err != nil || rotated.Token != "" {
		t.Fatalf("a vendor rotation answers no secret, got %q (%v)", rotated.Token, err)
	}
	h := http.Header{"Content-Type": {"application/json"}}
	if w := f.deliver(chat.ID, `{"token":"`+ingChatSecret+`","trigger_word":"deploy"}`, h); w.Code != http.StatusUnauthorized {
		t.Fatalf("the old vendor secret still verifies: code=%d", w.Code)
	}
	if w := f.deliver(chat.ID, `{"token":"chat-rotated-secret","trigger_word":"deploy"}`, h); w.Code != http.StatusOK {
		t.Fatalf("the new vendor secret: code=%d body=%s", w.Code, w.Body.String())
	}

	forge, minted := f.create(t, `{"name":"forge","provider":"connector","connector_connection_id":"conn-forge"}`)
	conn, err := f.conns.Get(context.Background(), "t1", "conn-forge")
	if err != nil {
		t.Fatal(err)
	}
	conn.Status = connection.StatusRevoked
	if err := f.conns.Update(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
	if w := f.rotate(t, forge.ID, `{"vendor_secret":"x"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("a minted rotation refuses a pasted secret: code=%d body=%s", w.Code, w.Body.String())
	}
	w = f.rotate(t, forge.ID, "")
	if w.Code != http.StatusOK {
		t.Fatalf("minted rotation with a revoked connection: code=%d body=%s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rotated); err != nil || !strings.HasPrefix(rotated.Token, webhooks.TokenPrefix) || rotated.Token == minted {
		t.Fatalf("a minted rotation shows a new secret once, got %q (%v)", rotated.Token, err)
	}
	stored, err := f.s.webhookConfigs.Get(context.Background(), forge.ID)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := webhooks.OpenHMACSecret(f.s.sealer, forge.ID, stored.HMACSecretSealed)
	if err != nil || string(opened) != rotated.Token {
		t.Fatalf("the sealed secret is not the one shown (%v)", err)
	}
}

// --- the first adversarial round's witnesses ---

// failingConnections answers every read with an outage, not a verdict.
type failingConnections struct{ *connection.MemoryStore }

func (failingConnections) Get(context.Context, string, string) (connection.Connection, error) {
	return connection.Connection{}, errors.New("server selection error: mongo-0.mongo.svc.cluster.local:27017 10.42.3.17")
}

// failingCatalog cannot read its tier.
type failingCatalog struct{}

func (failingCatalog) Package(id string) (*spec.Package, connection.Tier, error) {
	return nil, connection.Tier{}, &connection.CatalogUnavailableError{Tier: connection.TierTeam, Connector: id,
		Err: errors.New("get package: server selection error: mongo-0.mongo.svc.cluster.local 10.42.3.17")}
}

// brokenCatalog holds the package and cannot load it.
type brokenCatalog struct{}

func (brokenCatalog) Package(id string) (*spec.Package, connection.Tier, error) {
	return nil, connection.Tier{}, fmt.Errorf("connector %q in the team tier (tenant t1) does not load: /var/lib/iterion/connectors/%s/connector.yaml: yaml: line 1", id, id)
}
func (brokenCatalog) Connectors() ([]string, error)  { return nil, nil }
func (failingCatalog) Connectors() ([]string, error) { return nil, nil }

// A store that cannot answer is an outage — 503, retried — never a binding
// verdict, and its cause stays out of the answer.
func TestIngress_AStoreOutageIsNotABindingVerdict(t *testing.T) {
	for name, break_ := range map[string]func(*ingressFixture){
		"connections": func(f *ingressFixture) { f.s.connectionStore = failingConnections{f.conns} },
		"catalog":     func(f *ingressFixture) { f.s.connectorCatalog = failingCatalog{} },
	} {
		t.Run(name, func(t *testing.T) {
			f := newIngressFixture(t, ingForgePackage())
			cfg, minted := f.create(t, `{"name":"forge","provider":"connector","connector_connection_id":"conn-forge"}`)
			break_(f)
			body := `{"sender":{"login":"alice"}}`
			w := f.deliver(cfg.ID, body, f.forgeDelivery(minted, body, "issue_comment", "d-1"))
			if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "10.42.3.17") {
				t.Fatalf("code=%d body=%s, want 503 naming no internal address", w.Code, w.Body.String())
			}
			f.assertNothingWritten(t, cfg.ID)
		})
	}
}

// acceptFailing fails every write; twinWritten hides a row a concurrent twin
// wrote between the lookup and the write.
type acceptFailing struct{ *intake.MemoryStore }

func (acceptFailing) Accept(context.Context, intake.Intake) (intake.Intake, bool, error) {
	return intake.Intake{}, false, errors.New("document too large")
}

type twinWritten struct{ *intake.MemoryStore }

func (twinWritten) GetIntake(context.Context, string, string) (intake.Intake, error) {
	return intake.Intake{}, intake.ErrNotFound
}

// A delivery that is charged and then not stored — its write failed, or a
// concurrent twin stored it first — gives its quota unit back.
func TestIngress_AnUnstoredDeliveryGivesItsQuotaUnitBack(t *testing.T) {
	f := newIngressFixture(t, ingForgePackage())
	cfg, minted := f.create(t, `{"name":"forge","provider":"connector","connector_connection_id":"conn-forge"}`)
	body := `{"sender":{"login":"alice"}}`
	f.s.connectorIntakes = acceptFailing{f.intakes}
	for i := 0; i < 3; i++ {
		if w := f.deliver(cfg.ID, body, f.forgeDelivery(minted, body, "issue_comment", "d-1")); w.Code != http.StatusServiceUnavailable {
			t.Fatalf("a failed write: code=%d", w.Code)
		}
	}
	if n := f.monthly(t); n != 0 {
		t.Fatalf("three failed writes left %d units charged, want 0", n)
	}
	f.s.connectorIntakes = f.intakes
	if w := f.deliver(cfg.ID, body, f.forgeDelivery(minted, body, "issue_comment", "d-1")); w.Code != http.StatusAccepted {
		t.Fatalf("first stored delivery: code=%d", w.Code)
	}
	f.s.connectorIntakes = twinWritten{f.intakes}
	if w := f.deliver(cfg.ID, body, f.forgeDelivery(minted, body, "issue_comment", "d-1")); w.Code != http.StatusAccepted {
		t.Fatalf("the twin: code=%d", w.Code)
	}
	if n := f.monthly(t); n != 1 {
		t.Fatalf("one stored delivery and its twin: %d units charged, want 1", n)
	}
}

// An event past the store's document bound is refused 413 before anything
// is charged.
func TestIngress_AnEventPastItsBoundIsRefused(t *testing.T) {
	pkg := ingForgePackage()
	for i := 0; i < 3; i++ {
		pkg.Triggers[0].Outputs = append(pkg.Triggers[0].Outputs, spec.TriggerOutput{Name: fmt.Sprintf("copy%d", i), From: "payload.pad"})
	}
	f := newIngressFixture(t, pkg)
	cfg, minted := f.create(t, `{"name":"forge","provider":"connector","connector_connection_id":"conn-forge"}`)
	body := `{"sender":{"login":"alice"},"pad":"` + strings.Repeat("<", 4<<20) + `"}`
	if w := f.deliver(cfg.ID, body, f.forgeDelivery(minted, body, "issue_comment", "d-1")); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code=%d body=%.200s, want 413", w.Code, w.Body.String())
	}
	f.assertNothingWritten(t, cfg.ID)
}

// Only an answered delivery marks the ingress used.
func TestIngress_ARefusedDeliveryDoesNotMarkTheIngressUsed(t *testing.T) {
	lastUsed := func(t *testing.T, f *ingressFixture, id string) *time.Time {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		f.s.joinBackgroundWorkers(ctx)
		got, err := f.s.webhookConfigs.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		return got.LastUsedAt
	}
	f := newIngressFixture(t, ingForgePackage())
	f.s.cfg.ConnectorIntakeBacklogMax = 1
	cfg, minted := f.create(t, `{"name":"forge","provider":"connector","connector_connection_id":"conn-forge"}`)
	if _, _, err := f.intakes.Accept(context.Background(), intake.Intake{TenantID: cfg.TenantID, ConnectionID: "conn-forge", IngressID: cfg.ID,
		TriggerID: "forge.issue.commented", DedupKey: "seed", ReceivedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	body := `{"sender":{"login":"bob"}}`
	if w := f.deliver(cfg.ID, body, f.forgeDelivery(minted, body, "issue_comment", "d-2")); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("over the cap: code=%d", w.Code)
	}
	if got := lastUsed(t, f, cfg.ID); got != nil {
		t.Fatalf("a refused delivery marked the ingress used at %v", got)
	}

	g := newIngressFixture(t, ingForgePackage())
	cfg2, minted2 := g.create(t, `{"name":"forge","provider":"connector","connector_connection_id":"conn-forge"}`)
	if w := g.deliver(cfg2.ID, body, g.forgeDelivery(minted2, body, "issue_comment", "d-1")); w.Code != http.StatusAccepted {
		t.Fatalf("accepted: code=%d", w.Code)
	}
	if got := lastUsed(t, g, cfg2.ID); got == nil {
		t.Fatal("an accepted delivery did not mark the ingress used")
	}
}

// An ingress keeps its vendor even when the connection it was bound to is
// gone: the sealed secret is that vendor's. And a delivery through a
// connection that now names another connector is refused.
func TestIngress_AnIngressKeepsItsVendorWhenItsConnectionIsGone(t *testing.T) {
	f := newIngressFixture(t, ingForgePackage(), ingChatPackage())
	cfg, minted := f.create(t, `{"name":"forge","provider":"connector","connector_connection_id":"conn-forge"}`)
	if cfg.Connector != "forge" {
		t.Fatalf("the ingress records connector %q, want forge", cfg.Connector)
	}
	if err := f.conns.Delete(context.Background(), "t1", "conn-forge"); err != nil {
		t.Fatal(err)
	}
	if w := f.patch(t, cfg.ID, `{"connector_connection_id":"conn-chat"}`); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "create a new ingress for chat") {
		t.Fatalf("moving to chat once the forge connection is gone: code=%d body=%s", w.Code, w.Body.String())
	}
	if w := f.deliver(cfg.ID, `{"token":"`+minted+`","trigger_word":"deploy"}`, http.Header{"Content-Type": {"application/json"}}); w.Code == http.StatusOK {
		t.Fatalf("a chat delivery carrying the forge secret was accepted")
	}

	g := newIngressFixture(t, ingForgePackage())
	other, minted2 := g.create(t, `{"name":"forge","provider":"connector","connector_connection_id":"conn-forge"}`)
	stored, err := g.s.webhookConfigs.Get(context.Background(), other.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored.Connector = "chat"
	if err := g.s.webhookConfigs.Update(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	body := `{"sender":{"login":"alice"}}`
	if w := g.deliver(other.ID, body, g.forgeDelivery(minted2, body, "issue_comment", "d-1")); w.Code != http.StatusConflict {
		t.Fatalf("a connection of another connector than the ingress's: code=%d body=%s, want 409", w.Code, w.Body.String())
	}
	g.assertNothingWritten(t, other.ID)
}

// A contract that moves the secret's origin is consented to only after a
// rotation re-sealed the secret from it.
func TestIngress_AContractThatMovesTheSecretOriginNeedsARotation(t *testing.T) {
	f := newIngressFixture(t, ingForgePackage())
	cfg, minted := f.create(t, `{"name":"forge","provider":"connector","connector_connection_id":"conn-forge"}`)
	if cfg.SecretOrigin != "minted" {
		t.Fatalf("secret origin = %q, want minted", cfg.SecretOrigin)
	}
	vendor := ingForgePackage()
	vendor.Ingress.Credential.Secret = "vendor"
	f.s.connectorCatalog = connection.NewMemoryCatalog(vendor)
	digest, err := spec.IngressDigest(vendor.Ingress)
	if err != nil {
		t.Fatal(err)
	}
	if w := f.patch(t, cfg.ID, `{"contract_digest":"`+digest+`"}`); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "rotate") {
		t.Fatalf("consent before the rotation: code=%d body=%s, want 409 asking for a rotation", w.Code, w.Body.String())
	}
	const vendorSecret = "forge-vendor-secret-7c1d"
	if w := f.rotate(t, cfg.ID, `{"vendor_secret":"`+vendorSecret+`"}`); w.Code != http.StatusOK {
		t.Fatalf("rotation: code=%d body=%s", w.Code, w.Body.String())
	}
	if w := f.patch(t, cfg.ID, `{"contract_digest":"`+digest+`"}`); w.Code != http.StatusOK {
		t.Fatalf("consent after the rotation: code=%d body=%s", w.Code, w.Body.String())
	}
	body := `{"sender":{"login":"alice"}}`
	if w := f.deliver(cfg.ID, body, f.forgeDelivery(minted, body, "issue_comment", "d-1")); w.Code != http.StatusUnauthorized {
		t.Fatalf("the old minted secret: code=%d, want 401", w.Code)
	}
	if w := f.deliver(cfg.ID, body, f.forgeDelivery(vendorSecret, body, "issue_comment", "d-1")); w.Code != http.StatusAccepted {
		t.Fatalf("the vendor's secret: code=%d body=%s", w.Code, w.Body.String())
	}
}

// A vendor secret is a credential's shape and length, and no piece of it is
// shown to the team's viewers.
func TestIngressCRUD_AVendorSecretIsCheckedAndNeverShown(t *testing.T) {
	f := newIngressFixture(t, ingChatPackage())
	for name, secret := range map[string]string{
		"too short":   "x",
		"a newline":   `chat-vendor-token\nsecond-line`,
		"inner space": "chat vendor token 0f3a9c",
	} {
		if code, out := f.createCode(t, `{"name":"chat","provider":"connector","connector_connection_id":"conn-chat","vendor_secret":"`+secret+`"}`); code != http.StatusBadRequest {
			t.Errorf("%s: code=%d body=%s, want 400", name, code, out)
		}
	}
	cfg, _ := f.create(t, `{"name":"chat","provider":"connector","connector_connection_id":"conn-chat","vendor_secret":"`+ingChatSecret+`"}`)
	stored, err := f.s.webhookConfigs.Get(context.Background(), cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.TokenLast4 != "" || stored.Fingerprint != "" || stored.TokenHash != "" {
		t.Fatalf("the row shows a piece of the vendor secret: last4=%q fingerprint=%q hash=%q", stored.TokenLast4, stored.Fingerprint, stored.TokenHash)
	}
}

// A stated contract_digest at creation must be the contract the package
// resolves to; it applies to a connector ingress only.
func TestIngressCRUD_AStatedDigestIsTheCurrentContract(t *testing.T) {
	f := newIngressFixture(t, ingForgePackage())
	digest, err := spec.IngressDigest(ingForgePackage().Ingress)
	if err != nil {
		t.Fatal(err)
	}
	if code, out := f.createCode(t, `{"name":"forge","provider":"connector","connector_connection_id":"conn-forge","contract_digest":"sha256:00"}`); code != http.StatusConflict {
		t.Fatalf("a stale digest: code=%d body=%s, want 409", code, out)
	}
	if cfg, _ := f.create(t, `{"name":"forge","provider":"connector","connector_connection_id":"conn-forge","contract_digest":"`+digest+`"}`); cfg.ContractDigest != digest {
		t.Fatalf("pinned %q, want %q", cfg.ContractDigest, digest)
	}
	if code, out := f.createCode(t, `{"name":"gl","provider":"gitlab","bot_ids":["review-pr"],"contract_digest":"`+digest+`"}`); code != http.StatusBadRequest {
		t.Fatalf("a digest on a forge hook: code=%d body=%s, want 400", code, out)
	}
}

// A refusal's cause reaches the log bounded, whatever the delivery carried:
// an unauthenticated body whose repeated key is 64 KiB long.
func TestIngress_ARefusalsCauseReachesTheLogBounded(t *testing.T) {
	f := newIngressFixture(t, ingChatPackage())
	var logged bytes.Buffer
	f.s.logger = iterlog.New(iterlog.LevelWarn, &logged)
	cfg, _ := f.create(t, `{"name":"chat","provider":"connector","connector_connection_id":"conn-chat","vendor_secret":"`+ingChatSecret+`"}`)
	long := strings.Repeat("k", 1<<16)
	if w := f.deliver(cfg.ID, `{"`+long+`":1,"`+long+`":1}`, http.Header{"Content-Type": {"application/json"}}); w.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d", w.Code)
	}
	if logged.Len() == 0 || logged.Len() > 4096 {
		t.Fatalf("the refusal logged %d bytes, want a bounded line", logged.Len())
	}
}

// A real change of the binding under a changed contract needs the consent
// too: the installation alone does not move the pin.
func TestIngress_ABindingChangeUnderAChangedContractNeedsConsent(t *testing.T) {
	f := newIngressFixture(t, ingSlackPackage())
	cfg := f.createSlack(t, ingSlackTeam)
	weakened := ingSlackPackage()
	weakened.Ingress.Timestamp = nil
	weakened.Ingress.Credential.Signed = "{body}"
	f.s.connectorCatalog = connection.NewMemoryCatalog(weakened)
	if w := f.patch(t, cfg.ID, `{"installation":"T0THERTEAM"}`); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "contract_digest") {
		t.Fatalf("an installation change under a changed contract: code=%d body=%s, want 409 asking for the consent", w.Code, w.Body.String())
	}
	stored, err := f.s.webhookConfigs.Get(context.Background(), cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ContractDigest != cfg.ContractDigest || stored.Installation != ingSlackTeam {
		t.Fatalf("a refused update moved the binding: %+v", stored)
	}
}

// A rotation reads the secret's origin from the ingress's own vendor: a
// connection that now names another connector is refused.
func TestIngressCRUD_ARotationReadsItsOwnVendor(t *testing.T) {
	f := newIngressFixture(t, ingForgePackage())
	cfg, _ := f.create(t, `{"name":"forge","provider":"connector","connector_connection_id":"conn-forge"}`)
	stored, err := f.s.webhookConfigs.Get(context.Background(), cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored.Connector = "chat"
	if err := f.s.webhookConfigs.Update(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	if w := f.rotate(t, cfg.ID, ""); w.Code != http.StatusConflict {
		t.Fatalf("rotation through a connection of another connector: code=%d body=%s, want 409", w.Code, w.Body.String())
	}
}

// warnIngress bounds the cause it logs whatever produced it: a store error,
// an expression's message — not only the reader's own, already bounded.
func TestIngress_WarnIngressBoundsItsCause(t *testing.T) {
	f := newIngressFixture(t)
	var logged bytes.Buffer
	f.s.logger = iterlog.New(iterlog.LevelWarn, &logged)
	f.s.warnIngress(webhooks.Config{ID: "ing", TenantID: "t1"}, "203.0.113.7", "intake write failed", errors.New(strings.Repeat("é", 1<<15)))
	if logged.Len() == 0 || logged.Len() > 2048 || !utf8.Valid(logged.Bytes()) {
		t.Fatalf("logged %d bytes (valid UTF-8: %v), want one bounded line cut on a rune boundary", logged.Len(), utf8.Valid(logged.Bytes()))
	}
}

// acceptLost stores the row and loses the answer — a read-back that failed,
// a vendor that hung up between the write and its answer.
type acceptLost struct{ *intake.MemoryStore }

func (a acceptLost) Accept(ctx context.Context, in intake.Intake) (intake.Intake, bool, error) {
	if _, _, err := a.MemoryStore.Accept(ctx, in); err != nil {
		return intake.Intake{}, false, err
	}
	return intake.Intake{}, false, errors.New("read back the stored intake: connection reset")
}

// A write whose answer was lost but whose row landed is a stored delivery:
// acknowledged, its unit kept.
func TestIngress_AWriteWhoseAnswerWasLostKeepsItsUnit(t *testing.T) {
	f := newIngressFixture(t, ingForgePackage())
	cfg, minted := f.create(t, `{"name":"forge","provider":"connector","connector_connection_id":"conn-forge"}`)
	f.s.connectorIntakes = acceptLost{f.intakes}
	body := `{"sender":{"login":"alice"}}`
	if w := f.deliver(cfg.ID, body, f.forgeDelivery(minted, body, "issue_comment", "d-1")); w.Code != http.StatusAccepted {
		t.Fatalf("a stored row whose answer was lost: code=%d, want the ack", w.Code)
	}
	if rows, n := f.intakeRows(t), f.monthly(t); len(rows) != 1 || n != 1 {
		t.Fatalf("rows=%d monthly=%d, want one row charged once", len(rows), n)
	}
}

// A contract that moves the secret's origin stays recoverable once the
// ingress's connection is gone: the rotation reads the recorded connector.
func TestIngress_AnIngressWhoseConnectionIsGoneStaysRecoverable(t *testing.T) {
	f := newIngressFixture(t, ingForgePackage())
	cfg, _ := f.create(t, `{"name":"forge","provider":"connector","connector_connection_id":"conn-forge"}`)
	if err := f.conns.Delete(context.Background(), "t1", "conn-forge"); err != nil {
		t.Fatal(err)
	}
	second := ingConnection("forge")
	second.ID, second.Alias = "conn-forge-2", "forge-2"
	if err := f.conns.Create(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	vendor := ingForgePackage()
	vendor.Ingress.Credential.Secret = "vendor"
	f.s.connectorCatalog = connection.NewMemoryCatalog(vendor)
	digest, err := spec.IngressDigest(vendor.Ingress)
	if err != nil {
		t.Fatal(err)
	}
	const vendorSecret = "forge-vendor-secret-7c1d"
	if w := f.rotate(t, cfg.ID, `{"vendor_secret":"`+vendorSecret+`"}`); w.Code != http.StatusOK {
		t.Fatalf("rotation with the connection gone: code=%d body=%s", w.Code, w.Body.String())
	}
	if w := f.patch(t, cfg.ID, `{"connector_connection_id":"conn-forge-2","contract_digest":"`+digest+`"}`); w.Code != http.StatusOK {
		t.Fatalf("re-bind with consent after the rotation: code=%d body=%s", w.Code, w.Body.String())
	}
	body := `{"sender":{"login":"alice"}}`
	if w := f.deliver(cfg.ID, body, f.forgeDelivery(vendorSecret, body, "issue_comment", "d-1")); w.Code != http.StatusAccepted {
		t.Fatalf("the recovered ingress: code=%d body=%s", w.Code, w.Body.String())
	}
}

// The write half answers an outage as one too: 503, its cause in the log,
// never a 400 or 409 carrying an internal address.
func TestIngressCRUD_AStoreOutageIsNotAVerdict(t *testing.T) {
	for name, break_ := range map[string]func(*ingressFixture){
		"connections": func(f *ingressFixture) { f.s.connectionStore = failingConnections{f.conns} },
		"catalog":     func(f *ingressFixture) { f.s.connectorCatalog = failingCatalog{} },
	} {
		t.Run(name, func(t *testing.T) {
			f := newIngressFixture(t, ingForgePackage())
			cfg, _ := f.create(t, `{"name":"forge","provider":"connector","connector_connection_id":"conn-forge"}`)
			break_(f)
			code, out := f.createCode(t, `{"name":"forge2","provider":"connector","connector_connection_id":"conn-forge"}`)
			if code != http.StatusServiceUnavailable || strings.Contains(out, "10.42.3.17") {
				t.Fatalf("create: code=%d body=%s, want 503 naming no internal address", code, out)
			}
			if w := f.patch(t, cfg.ID, `{"connector_connection_id":"conn-forge","installation":"","contract_digest":"`+cfg.ContractDigest+`"}`); w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "10.42.3.17") {
				t.Fatalf("patch: code=%d body=%s, want 503 naming no internal address", w.Code, w.Body.String())
			}
			if name == "catalog" {
				if w := f.rotate(t, cfg.ID, ""); w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "10.42.3.17") {
					t.Fatalf("rotate: code=%d body=%s, want 503 naming no internal address", w.Code, w.Body.String())
				}
			}
		})
	}
}

// One address exhausting one ingress's pre-proof bucket leaves another
// ingress it delivers to untouched: a vendor delivers every tenant's hooks
// from one shared pool of addresses.
func TestIngress_OneExhaustedIngressLeavesAnotherFromTheSameAddress(t *testing.T) {
	f := newIngressFixture(t, ingForgePackage())
	a, _ := f.create(t, `{"name":"a","provider":"connector","connector_connection_id":"conn-forge"}`)
	b, mintedB := f.create(t, `{"name":"b","provider":"connector","connector_connection_id":"conn-forge"}`)
	for i := 0; i <= int(connectorIngressIPBucket.burst); i++ {
		f.deliver(a.ID, `{}`, nil)
	}
	if w := f.deliver(a.ID, `{}`, nil); w.Code != http.StatusTooManyRequests {
		t.Fatalf("ingress a after its burst: code=%d, want 429", w.Code)
	}
	body := `{"sender":{"login":"alice"}}`
	if w := f.deliver(b.ID, body, f.forgeDelivery(mintedB, body, "issue_comment", "d-1")); w.Code != http.StatusAccepted {
		t.Fatalf("ingress b from the same address: code=%d", w.Code)
	}
}

// ctxHonouring refuses a write under a context that is done, as a real
// store's driver does.
type ctxHonouring struct{ *intake.MemoryStore }

func (c ctxHonouring) Accept(ctx context.Context, in intake.Intake) (intake.Intake, bool, error) {
	if err := ctx.Err(); err != nil {
		return intake.Intake{}, false, err
	}
	return c.MemoryStore.Accept(ctx, in)
}

// The intake write runs detached from the request: a vendor that hangs up
// does not cancel it halfway.
func TestIngress_TheIntakeWriteOutlivesTheRequest(t *testing.T) {
	f := newIngressFixture(t, ingForgePackage())
	cfg, minted := f.create(t, `{"name":"forge","provider":"connector","connector_connection_id":"conn-forge"}`)
	f.s.connectorIntakes = ctxHonouring{f.intakes}
	body := `{"sender":{"login":"alice"}}`
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest(http.MethodPost, "/api/webhooks/c/"+cfg.ID, strings.NewReader(body)).WithContext(ctx)
	r.RemoteAddr = "203.0.113.7:4242"
	for k, v := range f.forgeDelivery(minted, body, "issue_comment", "d-1") {
		r.Header[k] = v
	}
	w := httptest.NewRecorder()
	f.s.mux.ServeHTTP(w, r)
	if rows := f.intakeRows(t); w.Code != http.StatusAccepted || len(rows) != 1 {
		t.Fatalf("a request whose client hung up: code=%d rows=%d, want the write done and acked", w.Code, len(rows))
	}
}

// The legacy connector lane answers an outage of the connections store as
// one too.
func TestWebhookCRUD_AConnectionsOutageIsNotABindingVerdict(t *testing.T) {
	s, _, _ := connectorBoundServer(t, []connection.Capability{connection.CapTrigger})
	s.connectionStore = failingConnections{connection.NewMemoryStore()}
	w := httptest.NewRecorder()
	s.handleCreateWebhook(w, whReq(superAdminCtx(), "POST", "/api/teams/t1/webhooks",
		`{"name":"fj","provider":"forgejo","bot_ids":["review-pr"],"connector_connection_id":"conn-fj"}`, "t1", ""))
	if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "10.42.3.17") {
		t.Fatalf("create: code=%d body=%s, want 503 naming no internal address", w.Code, w.Body.String())
	}
	// The PATCH half of the same lane: a binding written while the store
	// answers, then re-bound during its outage.
	s2, _, _ := connectorBoundServer(t, []connection.Capability{connection.CapTrigger})
	w = httptest.NewRecorder()
	s2.handleCreateWebhook(w, whReq(superAdminCtx(), "POST", "/api/teams/t1/webhooks",
		`{"name":"fj","provider":"forgejo","bot_ids":["review-pr"],"connector_connection_id":"conn-fj"}`, "t1", ""))
	var created webhookWithToken
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || w.Code != http.StatusCreated {
		t.Fatalf("create: code=%d err=%v", w.Code, err)
	}
	s2.connectionStore = failingConnections{connection.NewMemoryStore()}
	w = httptest.NewRecorder()
	s2.handleUpdateWebhook(w, whReq(superAdminCtx(), "PATCH", "/api/teams/t1/webhooks/"+created.Config.ID, `{"connector_connection_id":"conn-fj"}`, "t1", created.Config.ID))
	if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "10.42.3.17") {
		t.Fatalf("patch: code=%d body=%s, want 503 naming no internal address", w.Code, w.Body.String())
	}
}
