package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
	cloudmetrics "github.com/SocialGouv/iterion/pkg/cloud/metrics"
	"github.com/SocialGouv/iterion/pkg/eventbus"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// A trigger subscription is a STANDING launch order: the spine evaluates it
// against every matching event and launches through serviceLauncher, whose
// gate call carries machineLaunchCtx (trigger_launcher.go) — a machine
// principal the launch gate exempts. So the rung is owed where the
// subscription is written, exactly as it is on the board's Ready column, and
// a viewer may neither arm a team's automation nor silence it.

// triggerRungFixture is newRunScopeServer (a real auth store over seeded
// memberships: u-viewer-a is a viewer of tenant-A, u-member-a a member) with a
// real in-memory subscription store and the trigger routes mounted through
// their own production registrar, so a create that clears the rung really
// stores and a refused one leaves the store empty.
//
// The registrar is called here rather than by New(): a Config with no
// TriggerStore skips it (server_routes.go), which is exactly the fixture hole
// that hid the forge↔board family from its own class witness.
func triggerRungFixture(t *testing.T) (*Server, *httptest.Server, trigger.SubscriptionStore) {
	t.Helper()
	srv, _ := newRunScopeServer(t)
	store := trigger.NewMemorySubscriptionStore()
	srv.cfg.TriggerStore = store
	srv.cfg.EventsBus = eventbus.NewInProcBus(srv.logger)
	srv.registerTriggerRoutes()
	return srv, fullStack(t, srv), store
}

// triggerRegistrarRoutes derives the registrar's routes from its own SOURCE,
// the way nativeBoardAPIRoutes reads the cloud board's. The routing table
// alone is not enough: recordingMux records only what goes through ITS Handle
// (route_recorder.go), so a route handed the bare *http.ServeMux — the way
// the cloud board's whole family is mounted — never appears there, and a
// witness built from the table walks right past it. The pattern therefore
// matches both spellings, `s.mux.Handle` and `s.mux.ServeMux.Handle`, so the
// caller can hold every mounted route against the table.
func triggerRegistrarRoutes(t *testing.T) []RouteInfo {
	t.Helper()
	const src = "triggers_routes.go"
	code, calls := registrarSource(t, src)
	re := regexp.MustCompile(`(?i:mux)\.Handle\("([A-Z]+) (/[^"]*)"`)
	matches := re.FindAllStringSubmatch(code, -1)
	if len(matches) != calls {
		t.Fatalf("%s holds %d registration call(s) and this witness parsed %d — a registration it cannot read is one it cannot guard; spell the route as one literal pattern on s.mux.Handle", src, calls, len(matches))
	}
	if len(matches) < 6 {
		t.Fatalf("derived %d route(s) from %s — the registrar's shape changed and this enumeration now covers almost nothing", len(matches), src)
	}
	out := make([]RouteInfo, 0, len(matches))
	for _, m := range matches {
		out = append(out, RouteInfo{Method: m[1], Pattern: m[2]})
	}
	return out
}

// triggerClassRoutes is the whole class: every route the trigger registrar
// mounts, taken from the routing table AND from the registrar's source, never
// from a list written here — a list would go stale the day a route is added,
// which is the hole the class witness exists to refuse. The union is what
// makes the witness hard to blind: the table keeps covering routes a future
// registrar adds, and the source keeps covering routes the table cannot see
// (a bare-mux mount) or that the prefixes above stopped matching.
func triggerClassRoutes(t *testing.T, srv *Server) []RouteInfo {
	t.Helper()
	var out []RouteInfo
	seen := map[RouteInfo]bool{}
	add := func(r RouteInfo) {
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	for _, route := range srv.mux.Routes() {
		if strings.HasPrefix(route.Pattern, "/api/v1/triggers") ||
			strings.HasPrefix(route.Pattern, "/api/v1/bots/{name}/triggers") {
			add(route)
		}
	}
	registrar := triggerRegistrarRoutes(t)
	if missing := missingFromTable(routingTable(srv), registrar); len(missing) != 0 {
		t.Fatalf("the registrar mounts %+v, which the routing table does not record: a route handed the bare *http.ServeMux is invisible to recordingMux (route_recorder.go), and so to every witness built from it — mount it on s.mux, or enumerate it here deliberately", missing)
	}
	for _, route := range registrar {
		add(route)
	}
	return out
}

// subsOf lists the subscriptions stored under the tenant a caller's
// credential pins — the tenant handleCreateTrigger stamps (triggerTenant), so
// this is where a write of theirs would have landed.
func subsOf(t *testing.T, store trigger.SubscriptionStore, tenantID string) []trigger.Subscription {
	t.Helper()
	subs, err := store.ListByTenant(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("list subscriptions of %q: %v", tenantID, err)
	}
	return subs
}

// The CLASS, not a sample: every state-changing trigger route refuses a
// viewer with the gate's own 403 token, and every read still answers one. A
// route added to the registrar without the rung turns this red on the route
// that lacks it.
func TestEveryTriggerWriteRouteTakesTheMemberRung(t *testing.T) {
	srv, ts, _ := triggerRungFixture(t)
	viewer := roleGateCaller("u-viewer-a", "tenant-A", identity.RoleViewer)
	writes, reads := 0, 0
	for _, route := range triggerClassRoutes(t, srv) {
		t.Run(route.Method+" "+route.Pattern, func(t *testing.T) {
			path := boardProbePath(route.Pattern)
			// A class member the mux cannot resolve would make this
			// witness vacuous: assert the route is really served.
			if _, pattern := srv.mux.Handler(httptest.NewRequest(route.Method, path, nil)); pattern == "" {
				t.Fatalf("no handler registered for %s %s — the enumeration drifted from the routing table", route.Method, path)
			}
			code, token := boardReq(t, srv, ts, route.Method, path, `{"kind":"probe","bot_id":"review-pr"}`, viewer)
			if !isStateChangingMethod(route.Method) {
				if code == http.StatusForbidden {
					t.Fatalf("a viewer reading %s %s = 403 %q — the rung takes writes, a viewer reads the automation list", route.Method, path, token)
				}
				reads++
				return
			}
			if code != http.StatusForbidden || token != denyMemberRoleRequired {
				t.Fatalf("a viewer writing %s %s = %d %q, want 403 %s", route.Method, path, code, token, denyMemberRoleRequired)
			}
			writes++
		})
	}
	// The floor is the registrar's full set (5 writes: create, update,
	// delete, emit, from-invocation; 3 reads: list, get, health), so a
	// registrar that mounted nothing — or an enumeration whose prefixes
	// stopped matching — is red instead of silently covering less.
	if writes < 5 || reads < 3 {
		t.Fatalf("the class covered %d write route(s) and %d read route(s) under /api/v1/triggers + /api/v1/bots/{name}/triggers — the registrar mounts more", writes, reads)
	}
}

// The round's attack, at the store: a viewer's create used to answer 201 with
// Enabled:true and Origin:"operator" stored, and the spine then launched it
// under its exempt machine principal; update, delete and from-invocation
// walked into their handlers and answered on their own merits. Only emit was
// already refused, inside its handler, by gateLaunch itself — it is here
// because the mount now says so too. The last assertion is the EFFECT, not
// the screen.
func TestViewerCannotArmAutomation(t *testing.T) {
	srv, ts, store := triggerRungFixture(t)
	viewer := roleGateCaller("u-viewer-a", "tenant-A", identity.RoleViewer)
	for _, tc := range []struct{ name, method, path, body string }{
		{"create", http.MethodPost, "/api/v1/triggers", `{"bot_id":"review-pr","match":{"kinds":["issue"]}}`},
		{"update", http.MethodPut, "/api/v1/triggers/sub-1", `{"bot_id":"review-pr","enabled":false}`},
		{"delete", http.MethodDelete, "/api/v1/triggers/sub-1", ``},
		{"emit", http.MethodPost, "/api/v1/triggers/emit", `{"kind":"probe"}`},
		{"from invocation", http.MethodPost, "/api/v1/bots/review-pr/triggers/from-invocation", `{"index":0}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, token := boardReq(t, srv, ts, tc.method, tc.path, tc.body, viewer)
			if code != http.StatusForbidden || token != denyMemberRoleRequired {
				t.Fatalf("a viewer's %s (%s %s) = %d %q, want 403 %s", tc.name, tc.method, tc.path, code, token, denyMemberRoleRequired)
			}
		})
	}
	if subs := subsOf(t, store, "tenant-A"); len(subs) != 0 {
		t.Fatalf("the store holds %d subscription(s) after every viewer write was refused: %+v", len(subs), subs)
	}
}

// The emit is a LAUNCH route (the gate's table names it) that sits behind
// the mount: a viewer is refused there, before gateLaunch runs, so the
// refusal does not reach iterion_launch_denied_total — the series counts the
// gate's own refusals, and only those. The board twin pins the same rule for
// launch-now and proves the series still moves on a refused POST /api/runs.
func TestRefusedEmitAtTheMountIsNotCountedAsARefusedLaunch(t *testing.T) {
	srv, ts, _ := triggerRungFixture(t)
	srv.cfg.Metrics = cloudmetrics.New()
	viewer := roleGateCaller("u-viewer-a", "tenant-A", identity.RoleViewer)
	code, token := boardReq(t, srv, ts, http.MethodPost, "/api/v1/triggers/emit", `{"kind":"probe"}`, viewer)
	if code != http.StatusForbidden || token != denyMemberRoleRequired {
		t.Fatalf("a viewer's emit = %d %q, want 403 %s", code, token, denyMemberRoleRequired)
	}
	if series := launchDeniedSeries(t, srv.cfg.Metrics); len(series) != 0 {
		t.Fatalf("a refused emit at the mount moved iterion_launch_denied_total to %v — only the gate's own refusals count", series)
	}
}

// A viewer may not DISABLE a team's automation either: the delete of a
// subscription that really exists is refused, and the subscription survives.
// Without this the refusal above could be the 404 of an absent id rather than
// the rung.
func TestViewerCannotDeleteAnExistingSubscription(t *testing.T) {
	srv, ts, store := triggerRungFixture(t)
	seeded := trigger.Subscription{ID: "sub-live", TenantID: "tenant-A", BotID: "review-pr", Origin: "operator", Enabled: true}
	if err := store.Create(context.Background(), seeded); err != nil {
		t.Fatalf("seed subscription: %v", err)
	}
	viewer := roleGateCaller("u-viewer-a", "tenant-A", identity.RoleViewer)
	code, token := boardReq(t, srv, ts, http.MethodDelete, "/api/v1/triggers/sub-live", "", viewer)
	if code != http.StatusForbidden || token != denyMemberRoleRequired {
		t.Fatalf("a viewer deleting a live subscription = %d %q, want 403 %s", code, token, denyMemberRoleRequired)
	}
	if subs := subsOf(t, store, "tenant-A"); len(subs) != 1 {
		t.Fatalf("the team's automation holds %d subscription(s) after a refused delete, want the seeded one intact: %+v", len(subs), subs)
	}
}

// The rung admits the role the operator's automation work takes: a member
// really creates, and the subscription really lands enabled in the store. A
// guard that refused everyone would pass the tests above and fail here.
func TestMemberStillArmsAutomation(t *testing.T) {
	srv, ts, store := triggerRungFixture(t)
	member := roleGateCaller("u-member-a", "tenant-A", identity.RoleMember)
	code, token := boardReq(t, srv, ts, http.MethodPost, "/api/v1/triggers", `{"bot_id":"review-pr","match":{"kinds":["issue"]}}`, member)
	if code != http.StatusCreated {
		t.Fatalf("a member creating a subscription = %d %q, want 201", code, token)
	}
	subs := subsOf(t, store, "tenant-A")
	if len(subs) != 1 || subs[0].BotID != "review-pr" || !subs[0].Enabled {
		t.Fatalf("store = %+v, want exactly one enabled subscription on review-pr", subs)
	}
}

// A viewer of the team keeps the automation console: the reads answer 200, so
// the rung refuses actions and not the view.
func TestViewerStillReadsTheAutomationList(t *testing.T) {
	srv, ts, _ := triggerRungFixture(t)
	viewer := roleGateCaller("u-viewer-a", "tenant-A", identity.RoleViewer)
	for _, path := range []string{"/api/v1/triggers", "/api/v1/triggers/health"} {
		code, token := boardReq(t, srv, ts, http.MethodGet, path, "", viewer)
		if code != http.StatusOK {
			t.Fatalf("a viewer reading %s = %d %q, want 200", path, code, token)
		}
	}
}

// The decision itself, principal by principal, on the trigger family's own
// instance: the same predicate, the same exemptions by kind, and the family's
// own detail sentence — so a refused caller is told what they were changing.
func TestTriggerWriteDenialReadsTheMethodAndTheRung(t *testing.T) {
	srv, _ := newRunScopeServer(t)
	if srv.authStore() == nil {
		t.Fatal("fixture has no identity store — it cannot witness the cloud arm")
	}
	for _, tc := range []struct {
		name    string
		method  string
		id      auth.Identity
		refused bool
	}{
		{"viewer creating", http.MethodPost, roleGateCaller("u-viewer-a", "tenant-A", identity.RoleViewer), true},
		{"viewer disabling", http.MethodPut, roleGateCaller("u-viewer-a", "tenant-A", identity.RoleViewer), true},
		{"viewer deleting", http.MethodDelete, roleGateCaller("u-viewer-a", "tenant-A", identity.RoleViewer), true},
		{"config_editor creating", http.MethodPost, roleGateCaller("u-configed-a", "tenant-A", identity.RoleConfigEditor), true},
		{"role-less creating", http.MethodPost, roleGateCaller("u-roleless", "tenant-A", ""), true},
		{"share link creating", http.MethodPost, auth.Identity{UserID: "share:s1", TeamID: "tenant-A", Kind: auth.KindShare}, true},
		{"viewer listing", http.MethodGet, roleGateCaller("u-viewer-a", "tenant-A", identity.RoleViewer), false},
		{"member creating", http.MethodPost, roleGateCaller("u-member-a", "tenant-A", identity.RoleMember), false},
		{"super-admin creating", http.MethodPost, auth.Identity{UserID: "root", TeamID: "tenant-A", IsSuperAdmin: true}, false},
		{"webhook creating", http.MethodPost, auth.Identity{UserID: "webhook:w1", TeamID: "tenant-A", Kind: auth.KindWebhook}, false},
		{"daemon creating", http.MethodPost, auth.Identity{UserID: triggerSpineActor, TeamID: "tenant-A", Kind: auth.KindInternal}, false},
		// Exempt by kind, with no team stamped: the rung reads the kind, not
		// the team. (A USER with no team never reaches this decision —
		// requireAuth's choke point refuses that credential first.)
		{"teamless daemon creating", http.MethodPost, auth.Identity{UserID: triggerSpineActor, Kind: auth.KindInternal}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/api/v1/triggers", nil)
			req = req.WithContext(auth.WithIdentity(req.Context(), tc.id))
			d := srv.triggerWriteDenial(req)
			if tc.refused {
				if d == nil || d.status != http.StatusForbidden || d.reason != denyMemberRoleRequired {
					t.Fatalf("denial = %+v, want 403 %s", d, denyMemberRoleRequired)
				}
				// The family's own sentence: what the caller was changing is
				// what they can act on.
				if !strings.Contains(d.detail, "automation") {
					t.Fatalf("detail = %q: a refused automation write must be told what it was changing, not the board's sentence", d.detail)
				}
				return
			}
			if d != nil {
				t.Fatalf("denial = %+v, want the request served", d)
			}
		})
	}
}

// Local/desktop mode is untouched: with no identity store there is no role to
// read, which is the condition gateLaunch itself opens with. A single
// operator's automation view must not start answering 403 to its own studio.
func TestLocalTriggerWritesAreUntouchedByTheRung(t *testing.T) {
	srv, _ := newTestServer(t)
	if srv.authStore() != nil {
		t.Fatal("fixture carries an identity store — it cannot witness local mode")
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/api/v1/triggers", strings.NewReader(`{"bot_id":"review-pr"}`))
		if d := srv.triggerWriteDenial(req); d != nil {
			t.Fatalf("local mode refused %s /api/v1/triggers (%+v) — a server with no identity store has no rung to read", method, d)
		}
	}
}

// The two families' wrappers are the SAME rung with two sentences: one
// definition decides, so they cannot drift into disagreeing about who may
// spend the org's money, and each still names the surface it guards.
func TestBoardAndTriggerRungsShareOneDecision(t *testing.T) {
	srv, _ := newRunScopeServer(t)
	viewer := roleGateCaller("u-viewer-a", "tenant-A", identity.RoleViewer)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/triggers", nil)
	req = req.WithContext(auth.WithIdentity(req.Context(), viewer))
	board, trig := srv.boardWriteDenial(req), srv.triggerWriteDenial(req)
	if board == nil || trig == nil {
		t.Fatalf("board = %+v, trigger = %+v: both families refuse a viewer's write", board, trig)
	}
	if board.status != trig.status || board.reason != trig.reason {
		t.Fatalf("board = %d %q, trigger = %d %q: the two families answer one rung, with one envelope",
			board.status, board.reason, trig.status, trig.reason)
	}
	if board.detail == trig.detail {
		t.Fatalf("both families say %q — each owes the caller the surface they were changing", board.detail)
	}
}

// The trigger family's twin of the same order: authentication first, the rung
// second, inside triggerWriteAuth itself.
func TestAnonymousTriggerWriteIsUnauthenticatedNotForbidden(t *testing.T) {
	srv, _ := newRunScopeServer(t)
	served := false
	h := srv.triggerWriteAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served = true
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/triggers", strings.NewReader(`{"bot_id":"review-pr"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || denialToken(rec.Body.String()) != "authentication required" {
		t.Fatalf("a bearer-less automation write = %d %q, want 401 %q — the rung must not answer a caller who presented no credential",
			rec.Code, denialToken(rec.Body.String()), "authentication required")
	}
	if served {
		t.Fatal("the trigger handler ran for an unauthenticated caller")
	}
}
