package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// #2644, who may pin. A credential pin chooses which stored api key or
// secret the spine spends on every fire, under triggerSpineActor — a machine
// principal no rung reads. The member rung at the trigger mount admits
// automation work; a pin is a credential decision, the team admin's right on
// every other surface (a webhook's pins, the team's own keys and secrets),
// and the only right the trigger routes may take for it. Without it a member
// binds any org-scoped secret of the team — a project bot's managed forge
// token included — to a workflow secret slot of a bot of their choosing, or
// points an admin's pin at one.

// seedTriggerPinRecords wires real key and secret stores into a server and
// seeds org-scoped records of the tenant — the shapes the validators admit,
// so a refusal below is the RIGHT, never the record.
func seedTriggerPinRecords(t *testing.T, srv *Server, tenant string) (secrets.ApiKey, secrets.GenericSecret, secrets.GenericSecret) {
	t.Helper()
	keys := secrets.NewMemoryApiKeyStore()
	key := seedVendorKey(t, keys, tenant, "", "org-default", true)
	gen := secrets.NewMemoryGenericSecretStore()
	var secs []secrets.GenericSecret
	for _, id := range []string{"sec-org", "sec-org-2"} {
		sec := secrets.GenericSecret{ID: id, TenantID: tenant, ScopeTeamID: tenant, Name: "forge_token", CreatedAt: time.Now().UTC()}
		if err := gen.Create(store.WithTenant(t.Context(), tenant), sec); err != nil {
			t.Fatal(err)
		}
		secs = append(secs, sec)
	}
	srv.apiKeys = keys
	srv.genericSecrets = gen
	return key, secs[0], secs[1]
}

// getSub reads one subscription back from the store — the effect, never the
// answer's screen.
func getSub(t *testing.T, subs trigger.SubscriptionStore, id string) trigger.Subscription {
	t.Helper()
	got, err := subs.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("read subscription %s: %v", id, err)
	}
	return got
}

// Through the full stack — requireAuth, the member rung at the mount, the
// handler — so the right is witnessed where a real bearer meets it.
func TestTriggerCredentialPinsAreATeamAdminsDecision(t *testing.T) {
	srv, ts, subs := triggerRungFixture(t)
	key, sec, sec2 := seedTriggerPinRecords(t, srv, "tenant-A")
	member := roleGateCaller("u-member-a", "tenant-A", identity.RoleMember)
	admin := roleGateCaller("u-both", "tenant-A", identity.RoleAdmin)
	const base = `"bot_id":"review-pr","invocation":"board","mode":"board","match":{"sources":["board"]}`
	keyPin := `"key_overrides":{"anthropic":"` + key.ID + `"}`
	secretPin := `"secret_overrides":{"forge_token":"` + sec.ID + `"}`

	// A member's create that pins is refused — one pin map at a time, so
	// neither arm can cover for the other — and nothing is stored.
	for _, pin := range []string{keyPin, secretPin} {
		code, msg := boardReq(t, srv, ts, http.MethodPost, "/api/v1/triggers", "{"+base+","+pin+"}", member)
		if code != http.StatusForbidden || !strings.Contains(msg, "team admin") {
			t.Fatalf("a member's create pinning %s = %d %q, want 403 naming the team admin's right", pin, code, msg)
		}
	}
	if got := subsOf(t, subs, "tenant-A"); len(got) != 0 {
		t.Fatalf("the refused creates stored %d subscription(s): %+v", len(got), got)
	}

	// The admin's create lands both pins.
	code, msg := boardReq(t, srv, ts, http.MethodPost, "/api/v1/triggers", "{"+base+","+keyPin+","+secretPin+"}", admin)
	if code != http.StatusCreated {
		t.Fatalf("an admin's pinned create = %d %q, want 201", code, msg)
	}
	stored := subsOf(t, subs, "tenant-A")
	if len(stored) != 1 || stored[0].KeyOverrides["anthropic"] != key.ID || stored[0].SecretOverrides["forge_token"] != sec.ID {
		t.Fatalf("store = %+v, want one subscription carrying both pins", stored)
	}
	pinned := stored[0]
	path := "/api/v1/triggers/" + pinned.ID

	// A member cannot decide the pinned credential by ANY edit: pointing the
	// stored pins at another bot or other vars (the request omits the pins,
	// so they would stay), replacing a pin, clearing them. Each is its own
	// write, and the stored row is what is read after it.
	for _, tc := range []struct{ name, body string }{
		{"point the pins at another bot", `{"bot_id":"feature-dev","invocation":"board","mode":"board","match":{"sources":["board"]}}`},
		{"point the pins at other vars", "{" + base + `,"vars":{"task":"anything"}}`},
		{"replace a pin", "{" + base + `,"secret_overrides":{"forge_token":"` + sec2.ID + `"}}`},
		{"clear the pins", "{" + base + `,"key_overrides":{},"secret_overrides":{}}`},
	} {
		code, msg := boardReq(t, srv, ts, http.MethodPut, path, tc.body, member)
		if code != http.StatusForbidden || !strings.Contains(msg, "team admin") {
			t.Fatalf("a member's edit to %s = %d %q, want 403 naming the team admin's right", tc.name, code, msg)
		}
		got := getSub(t, subs, pinned.ID)
		if got.BotID != "review-pr" || len(got.Vars) != 0 || got.KeyOverrides["anthropic"] != key.ID || got.SecretOverrides["forge_token"] != sec.ID {
			t.Fatalf("a refused edit to %s moved the stored row: bot=%q vars=%v keys=%v secrets=%v", tc.name, got.BotID, got.Vars, got.KeyOverrides, got.SecretOverrides)
		}
	}

	// Pausing a pinned subscription is a member's work: the studio's toggle
	// sends the stored row back with enabled flipped, and it lands.
	if code, msg := boardReq(t, srv, ts, http.MethodPut, path, "{"+base+`,"enabled":false}`, member); code != http.StatusOK {
		t.Fatalf("a member pausing a pinned subscription = %d %q, want 200 — the right takes credential decisions, not the automation's switch", code, msg)
	}
	if got := getSub(t, subs, pinned.ID); got.Enabled || got.KeyOverrides["anthropic"] != key.ID || got.SecretOverrides["forge_token"] != sec.ID {
		t.Fatalf("after the member's pause: enabled=%v keys=%v secrets=%v, want paused with both pins kept", got.Enabled, got.KeyOverrides, got.SecretOverrides)
	}

	// The admin decides: the same re-pointing lands for them.
	if code, msg := boardReq(t, srv, ts, http.MethodPut, path, `{"bot_id":"feature-dev","invocation":"board","mode":"board","match":{"sources":["board"]}}`, admin); code != http.StatusOK {
		t.Fatalf("an admin pointing the pins at another bot = %d %q, want 200", code, msg)
	}
	if got := getSub(t, subs, pinned.ID); got.BotID != "feature-dev" || got.SecretOverrides["forge_token"] != sec.ID {
		t.Fatalf("after the admin's edit: bot=%q secrets=%v, want feature-dev with the pin kept", got.BotID, got.SecretOverrides)
	}

	// A subscription that pins nothing carries no credential decision: a
	// member still creates one and re-points it.
	if code, msg := boardReq(t, srv, ts, http.MethodPost, "/api/v1/triggers", `{"bot_id":"triage","invocation":"board","mode":"board","match":{"sources":["board"]}}`, member); code != http.StatusCreated {
		t.Fatalf("a member's unpinned create = %d %q, want 201", code, msg)
	}
	var unpinned trigger.Subscription
	for _, s := range subsOf(t, subs, "tenant-A") {
		if s.BotID == "triage" {
			unpinned = s
		}
	}
	if unpinned.ID == "" {
		t.Fatal("the member's unpinned create is not in the store")
	}
	if code, msg := boardReq(t, srv, ts, http.MethodPut, "/api/v1/triggers/"+unpinned.ID, `{"bot_id":"review-pr","invocation":"board","mode":"board","match":{"sources":["board"]}}`, member); code != http.StatusOK {
		t.Fatalf("a member re-pointing an unpinned subscription = %d %q, want 200", code, msg)
	}
	if got := getSub(t, subs, unpinned.ID); got.BotID != "review-pr" {
		t.Fatalf("the member's edit of an unpinned subscription left bot %q", got.BotID)
	}

	// And a member may still remove a pinned subscription outright: deleting
	// spends nothing.
	if code, msg := boardReq(t, srv, ts, http.MethodDelete, path, "", member); code != http.StatusNoContent {
		t.Fatalf("a member deleting a pinned subscription = %d %q, want 204", code, msg)
	}
}

// A right the identity store could not judge is an outage, not a refusal:
// a 403 there would tell an admin they are not one over a Mongo blip, and
// nothing would be logged. A healthy store that grants nothing is the 403.
// Every outage shape runs: one where only an org read fails beside a
// definitive "no grant" is the case a gate dropping the org read's failure
// answers 403.
func TestTriggerCredentialGateTellsAnOutageFromARefusal(t *testing.T) {
	srv, org, _ := triggerPinServer(t)
	body := `{"bot_id":"feature-dev","invocation":"board","mode":"board","match":{"sources":["board"]},"key_overrides":{"anthropic":"` + org.ID + `"}}`
	type gateCase struct {
		name  string
		store identity.Store
		want  int
	}
	cases := []gateCase{{"no grant", noGrantIdentityStore{}, http.StatusForbidden}}
	for _, outage := range identityStoreOutages() {
		cases = append(cases, gateCase{"identity store outage: " + outage.name, outage.store, http.StatusInternalServerError})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			authSvc := &auth.Service{}
			authSvc.SetStoreForTest(tc.store)
			srv.authSvc = authSvc
			rec := httptest.NewRecorder()
			srv.handleCreateTrigger(rec, teamTriggerReq(t, http.MethodPost, "/api/v1/triggers", "", body))
			if rec.Code != tc.want {
				t.Fatalf("a pinned create under %s = %d, want %d; body=%s", tc.name, rec.Code, tc.want, rec.Body.String())
			}
			if n, _ := srv.cfg.TriggerStore.ListByTenant(store.WithTenant(t.Context(), "t1"), "t1"); len(n) != 0 {
				t.Fatalf("the refused create stored %d subscription(s)", len(n))
			}
		})
	}
}

// The predicate reads the LAUNCH, not its spelling: the studio's toggle
// round-trips the stored row through JSON, where a nil map or slice and an
// empty one are the same omitted field — a predicate that told them apart
// would refuse a member's pause of every pinned subscription.
func TestDecidesTriggerCredentialsReadsTheLaunchNotItsSpelling(t *testing.T) {
	pinned := trigger.Subscription{
		ID: "s1", TenantID: "t1", BotID: "review-pr", Invocation: bundle.InvocationKindBoard,
		Match:           trigger.Matcher{Sources: []trigger.Source{trigger.SourceBoard}},
		KeyOverrides:    map[string]string{"anthropic": "key-1"},
		SecretOverrides: map[string]string{"forge_token": "sec-1"},
		Enabled:         true,
	}
	unpinned := pinned
	unpinned.KeyOverrides, unpinned.SecretOverrides = nil, nil
	edit := func(base trigger.Subscription, f func(*trigger.Subscription)) trigger.Subscription {
		f(&base)
		return base
	}
	for _, tc := range []struct {
		name   string
		before *trigger.Subscription
		after  trigger.Subscription
		want   bool
	}{
		{"create pinning nothing", nil, unpinned, false},
		{"create with empty pin maps", nil, edit(unpinned, func(s *trigger.Subscription) {
			s.KeyOverrides, s.SecretOverrides = map[string]string{}, map[string]string{}
		}), false},
		{"create pinning a key", nil, edit(unpinned, func(s *trigger.Subscription) { s.KeyOverrides = map[string]string{"anthropic": "key-1"} }), true},
		{"create pinning a secret", nil, edit(unpinned, func(s *trigger.Subscription) { s.SecretOverrides = map[string]string{"forge_token": "sec-1"} }), true},
		{"unpinned edit of the bot", &unpinned, edit(unpinned, func(s *trigger.Subscription) { s.BotID = "other" }), false},
		{"unpinned edit adding a pin", &unpinned, edit(unpinned, func(s *trigger.Subscription) { s.SecretOverrides = map[string]string{"forge_token": "sec-1"} }), true},
		{"pinned, nothing changed", &pinned, pinned, false},
		{"pinned, paused", &pinned, edit(pinned, func(s *trigger.Subscription) { s.Enabled = false }), false},
		{"pinned, update stamp moved", &pinned, edit(pinned, func(s *trigger.Subscription) { s.UpdatedAt = time.Now() }), false},
		{"pinned, nil vars spelled empty", &pinned, edit(pinned, func(s *trigger.Subscription) { s.Vars = map[string]string{} }), false},
		{"pinned, nil match lists spelled empty", &pinned, edit(pinned, func(s *trigger.Subscription) { s.Match.Kinds, s.Match.Labels = []string{}, []string{} }), false},
		{"pinned, bot changed", &pinned, edit(pinned, func(s *trigger.Subscription) { s.BotID = "other" }), true},
		{"pinned, repo changed", &pinned, edit(pinned, func(s *trigger.Subscription) { s.Repo = "group/other" }), true},
		{"pinned, vars changed", &pinned, edit(pinned, func(s *trigger.Subscription) { s.Vars = map[string]string{"task": "x"} }), true},
		{"pinned, match widened", &pinned, edit(pinned, func(s *trigger.Subscription) { s.Match.Sources = nil }), true},
		{"pinned, cadence changed", &pinned, edit(pinned, func(s *trigger.Subscription) { s.Cron = "* * * * *" }), true},
		{"pinned, a pin replaced", &pinned, edit(pinned, func(s *trigger.Subscription) { s.SecretOverrides = map[string]string{"forge_token": "sec-2"} }), true},
		{"pinned, pins cleared", &pinned, edit(pinned, func(s *trigger.Subscription) {
			s.KeyOverrides, s.SecretOverrides = map[string]string{}, map[string]string{}
		}), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decidesTriggerCredentials(tc.before, tc.after)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("decidesTriggerCredentials = %v, want %v", got, tc.want)
			}
		})
	}
}
