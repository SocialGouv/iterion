package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/connector/binding"
	"github.com/SocialGouv/iterion/pkg/connector/intake"
	"github.com/SocialGouv/iterion/pkg/connector/poller"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
)

// The server's own poller wiring — the poller's tenant's catalog, its
// resolver over the tenant's connections, the tier's guarded client — reads
// what a subscription saved through the route asks: one pass accepts the
// item the vendor answered, primed, on the poller the save derived.
func TestTheServersPollerReadsWhatASavedSubscriptionAsks(t *testing.T) {
	var (
		mu    sync.Mutex
		asked []string
	)
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.RequestURI()+" auth="+r.Header.Get("Authorization"))
		mu.Unlock()
		_, _ = w.Write([]byte(`{"messages":[{"id":"m1","room":"ops","text":"polled"}]}`))
	}))
	defer vendor.Close()
	// The local tier's operator hatch: the vendor double is loopback.
	t.Setenv(connection.AllowPrivateHostsEnv, "1")

	w := newConnectorTriggerWorld(t)
	pkg, _, err := w.s.connectorCatalog.Package("chat")
	if err != nil {
		t.Fatal(err)
	}
	polled := *pkg
	polled.Connector.Maturity = spec.MaturityExperimental
	polled.Connector.Auth = []spec.AuthScheme{{ID: "token", Kind: spec.AuthAPIKey, In: "header", Name: "Authorization"}}
	polled.Connector.DefaultSecurity = []spec.SecurityRequirement{{Terms: []spec.SecurityTerm{{SchemeID: "token"}}}}
	polled.Ops = []spec.OpsFile{{Connector: "chat", Domain: "message", Operations: []spec.Operation{{
		ID: "chat.message.search", Resource: "message", Verb: "search",
		HTTP:    spec.HTTPBinding{Method: "GET", Path: "/search"},
		Params:  []spec.Param{{Key: "room", Name: "room", In: spec.InQuery, Type: "string", Required: true}},
		Results: []spec.ResultCase{{Status: 200}}, Effect: spec.EffectRead, Deterministic: true,
	}}}}
	trig := pkg.Triggers[0]
	trig.Event, trig.Delivery, trig.SampleBody = "", spec.DeliveryPoll, nil
	trig.Poll = &spec.TriggerPoll{
		Operation: "chat.message.search", Params: map[string]string{"room": "query.room"},
		Query: []spec.PollQueryField{{ID: "room", Required: true}},
		Items: "response.messages", ItemID: "item.id",
		Interval: spec.PollInterval{Default: "5m", Min: "1m"},
	}
	polled.Triggers = []spec.Trigger{trig}
	w.s.connectorCatalog = connection.NewMemoryCatalog(&polled)
	w.s.connectorPollers = poller.NewMemoryStore()

	ctx := context.Background()
	conn, err := w.conns.Get(ctx, "t1", "conn-chat")
	if err != nil {
		t.Fatal(err)
	}
	conn.Capabilities = []connection.Capability{connection.CapTrigger, connection.CapAction}
	conn.BaseURL = vendor.URL
	if conn.SealedPayload, err = connection.SealToken(w.s.sealer, conn.ID, "vendor-token", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := w.conns.Update(ctx, conn); err != nil {
		t.Fatal(err)
	}

	rec := w.create(`{` + validConnectorSub + `,"poll_query":{"room":"ops"}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("save = %d %s", rec.Code, rec.Body.String())
	}
	wk, err := w.s.connectorPollerWorker(func(string) string { return "" })
	if err != nil {
		t.Fatalf("the server's poller worker: %v", err)
	}
	if _, err := wk.Pass(ctx); err != nil {
		t.Fatalf("pass: %v", err)
	}
	tenant0 := binding.ConnectionTenant(w.s.triggerTenant(teamT1(httptest.NewRequest(http.MethodGet, "/", nil))))
	if ps, _ := w.s.connectorPollers.List(ctx, tenant0); len(ps) != 1 || ps[0].LastError != "" {
		t.Fatalf("pollers of %q = %+v, want one that read without error", tenant0, ps)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(asked) != 1 || !strings.Contains(asked[0], "/search?room=ops") || !strings.HasSuffix(asked[0], "auth=vendor-token") {
		t.Fatalf("the vendor was asked %q, want one search for the saved query with the connection's credential", asked)
	}
	tenant := binding.ConnectionTenant(w.s.triggerTenant(teamT1(httptest.NewRequest(http.MethodGet, "/", nil))))
	pid := poller.ID(tenant, "conn-chat", "chat.message.posted", map[string]string{"room": "ops"})
	rows, err := w.s.connectorIntakes.ListPollerIntakes(ctx, tenant, pid, 1<<62, 0)
	if err != nil || len(rows) != 1 || rows[0].State != intake.IntakePrimed || !strings.Contains(string(rows[0].Event), `"prompt":"polled"`) {
		t.Fatalf("intakes of %s = (%+v, %v), want the item primed with its output", pid, rows, err)
	}
}
