package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/SocialGouv/iterion/pkg/auth"
	cloudmetrics "github.com/SocialGouv/iterion/pkg/cloud/metrics"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// The cloud board's Ready column IS a launch queue: the board dispatcher
// drains StateReady (boarddispatch.go) under a machine principal the launch
// gate exempts, so a card created ready — or moved there — is a launch
// request whoever wrote it made the platform perform. Authorization
// therefore happens where the card is WRITTEN. The three registrars that
// mount a board route — the studio's /api/v1/pipeline-board/…, the
// native.BoardAPI at /api/v1/native/… and the forge↔board per-card
// endpoints under the same prefix — take the same member rung on every
// state-changing method, with the same token and envelope gateLaunch
// answers; a viewer keeps every read.

// boardRungFixture is newRunScopeServer (a real auth store over seeded
// memberships: u-viewer-a is a viewer of tenant-A, u-member-a a member) with
// ALL THREE board registrars mounted as production mounts them — the
// pipeline-board routes from New(), the cloud board and the forge↔board
// endpoints through their own production registrars — over a real filesystem
// board, so a write that clears the rung really writes and a refused one
// leaves the board empty.
//
// The two forge stores are wired BEFORE registerBoardForgeRoutes because that
// registrar early-returns without them (board_forge.go) and its handlers
// dereference both: a fixture that skipped them would register nothing, and
// the class witness below would enumerate an empty set for that family — the
// exact hole it exists to refuse.
//
// cfg.Mode is left alone: with NativeTrackerStore nil and CloudBoardFor set,
// resolvePipelineBoardStore reaches the very same cloudBoardResolve the
// "cloud" arm uses, and the rest of the server keeps its test defaults.
func boardRungFixture(t *testing.T) (*Server, *httptest.Server, *native.Store) {
	t.Helper()
	srv, _ := newRunScopeServer(t)
	board, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("native board store: %v", err)
	}
	srv.cfg.CloudBoardFor = func(string) native.BoardStore { return board }
	srv.registerCloudBoardRoutes()
	srv.forgeIntegrations = forge.NewMemoryRepoIntegrationStore()
	srv.forgeConnections = forge.NewMemoryConnectionStore()
	srv.registerBoardForgeRoutes()
	return srv, fullStack(t, srv), board
}

// boardReq mints a bearer for id and runs one request through the auth
// chain a production request walks, returning the status and the stable
// reason token of the response envelope.
func boardReq(t *testing.T, srv *Server, ts *httptest.Server, method, path, body string, id auth.Identity) (int, string) {
	t.Helper()
	token, _, err := srv.signer.IssueAccess(id)
	if err != nil {
		t.Fatalf("mint bearer: %v", err)
	}
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, ts.URL+path, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, denialToken(string(raw))
}

// boardProbePath turns a registered pattern into one concrete path: every
// wildcard segment — {id}, {name}, {label}, the trailing {path...} — becomes
// the same probe value.
func boardProbePath(pattern string) string {
	segs := strings.Split(pattern, "/")
	for i, seg := range segs {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			segs[i] = "probe-1"
		}
	}
	return strings.Join(segs, "/")
}

// nativeBoardAPIRoutes derives the cloud board's routes from the registrar
// that mounts them. They are registered on the raw *http.ServeMux
// (board_cloud_routes.go hands it s.mux.ServeMux), so the recording mux
// never sees them — a list written out here instead would go stale the day a
// route is added, which is the hole the class witness exists to refuse. Each
// derived route is checked against the server's own routing table by the
// caller, so a parse that drifts from the code fails loudly rather than
// covering nothing.
func nativeBoardAPIRoutes(t *testing.T) []RouteInfo {
	t.Helper()
	src := filepath.Join("..", "dispatcher", "native", "http.go")
	code, calls := registrarSource(t, src)
	re := regexp.MustCompile(`mux\.Handle\("([A-Z]+) "\+p\+"([^"]*)"`)
	matches := re.FindAllStringSubmatch(code, -1)
	if len(matches) != calls {
		t.Fatalf("%s holds %d registration call(s) and this witness parsed %d — a registration it cannot read is one it cannot guard; spell the route as one literal pattern on mux.Handle", src, calls, len(matches))
	}
	if len(matches) < 20 {
		t.Fatalf("derived %d routes from %s — the registrar's shape changed and this enumeration now covers almost nothing", len(matches), src)
	}
	out := make([]RouteInfo, 0, len(matches))
	for _, m := range matches {
		out = append(out, RouteInfo{Method: m[1], Pattern: "/api/v1/native" + m[2]})
	}
	return out
}

// boardRegistrarRoutes derives the board routes the two in-package
// registrars mount, from their own SOURCE: pipeline_boards.go and
// board_forge.go. The class is the PREFIX, whatever a line wraps the handler
// in and whichever mux it is handed — a route mounted on the bare
// *http.ServeMux with requireAuth alone is exactly the hole, and it is
// invisible to the routing table (recordingMux records only its own Handle,
// route_recorder.go). board_forge.go's /api/teams/… forge-config routes are
// NOT board writes and stay out, by the same prefix rule.
func boardRegistrarRoutes(t *testing.T) []RouteInfo {
	t.Helper()
	// Every literal registration, whatever its prefix, so the count holds the
	// whole file to the one spelling this witness reads. The class then drops
	// ONLY the team-scoped integration routes board_forge.go also mounts
	// (/api/teams/{id}/forge/…), whose handlers take canManageTeam — not the
	// queue's routes, and not this rung's. Anything else these two files
	// mount is in the class by default: a board surface under a new prefix
	// in either reddens here until it takes the rung or moves out. (A board
	// surface in a NEW file under a NEW prefix matches no leg of this
	// witness — the three registrar files and the two prefixes are its
	// whole class.)
	re := regexp.MustCompile(`(?i:mux)\.Handle\("([A-Z]+) (/[^"]*)"`)
	var out []RouteInfo
	for _, f := range []struct {
		src   string
		floor int
	}{
		{"pipeline_boards.go", 12},
		{"board_forge.go", 5},
	} {
		code, calls := registrarSource(t, f.src)
		matches := re.FindAllStringSubmatch(code, -1)
		if len(matches) != calls {
			t.Fatalf("%s holds %d registration call(s) and this witness parsed %d — a registration it cannot read is one it cannot guard; spell the route as one literal pattern on s.mux.Handle", f.src, calls, len(matches))
		}
		n := 0
		for _, m := range matches {
			if !strings.HasPrefix(m[2], "/api/teams/") {
				out = append(out, RouteInfo{Method: m[1], Pattern: m[2]})
				n++
			}
		}
		if n < f.floor {
			t.Fatalf("derived %d board route(s) from %s — the registrar's shape changed and this enumeration now covers almost nothing", n, f.src)
		}
	}
	return out
}

// registrarSource reads one registrar's source with its line comments
// stripped — a retired route quoted in a comment is not a registration — and
// counts the registration CALLS it holds, whatever the receiver is called —
// s.mux, s.mux.ServeMux, a local alias of either — so an alias cannot hide a
// registration from the count. A derivation parses one literal spelling; a
// call it counted but did not parse is a route it cannot guard, and the
// caller reddens on the difference rather than certifying a file it only
// half read.
func registrarSource(t *testing.T, path string) (string, int) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the registrar %s: %v", path, err)
	}
	code := lineComments.ReplaceAllString(string(raw), "")
	return code, len(registrationCalls.FindAllString(code, -1))
}

var (
	lineComments      = regexp.MustCompile(`(?m)//.*$`)
	registrationCalls = regexp.MustCompile(`\.Handle(?:Func)?\(`)
)

// missingFromTable reports the routes a registrar mounts that the recording
// mux never saw. Both families' witnesses call it: such a route is served by
// production and absent from every enumeration built on srv.mux.Routes(), so
// a walk of the table alone would pass right over it.
func missingFromTable(table map[RouteInfo]bool, routes []RouteInfo) []RouteInfo {
	var out []RouteInfo
	for _, r := range routes {
		if !table[r] {
			out = append(out, r)
		}
	}
	return out
}

// routingTable is srv.mux.Routes() as a set.
func routingTable(srv *Server) map[RouteInfo]bool {
	out := map[RouteInfo]bool{}
	for _, route := range srv.mux.Routes() {
		out[route] = true
	}
	return out
}

// boardClassRoutes is the whole class: every route the three board
// registrars mount, derived from the routing table (the recorded
// pipeline-board and forge↔board registrations), from those two registrars'
// SOURCE and from the cloud board's own registrar — never from the production
// guard's own table, which would make the witness mute on a change to it.
//
// The source legs are what keep the witness from going quietly blind: the
// recording mux cannot see a route handed the bare *http.ServeMux, and the
// prefixes below cannot see a family that moved. The cloud board's family IS
// mounted that way deliberately (board_cloud_routes.go), which is why it is
// enumerated from source and not held against the table; the two in-package
// registrars use the recorded mux, and a route of theirs missing from the
// table is a raw mount — the hole, not a style.
func boardClassRoutes(t *testing.T, srv *Server) []RouteInfo {
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
		if strings.HasPrefix(route.Pattern, "/api/v1/pipeline-board") || strings.HasPrefix(route.Pattern, "/api/v1/native") {
			add(route)
		}
	}
	registrar := boardRegistrarRoutes(t)
	if missing := missingFromTable(routingTable(srv), registrar); len(missing) != 0 {
		t.Fatalf("the board registrars mount %+v, which the routing table does not record: a route handed the bare *http.ServeMux is invisible to recordingMux (route_recorder.go), and so to every witness built from it — mount it on s.mux, or enumerate it here deliberately", missing)
	}
	for _, route := range registrar {
		add(route)
	}
	for _, route := range nativeBoardAPIRoutes(t) {
		add(route)
	}
	return out
}

// The presence check has teeth: a route a registrar mounts but the routing
// table does not record is REPORTED, not passed over. The real path is
// witnessed by the trigger family (a raw-mux mount reddens its class walk);
// this is the predicate both families call, refusing its own example.
func TestMissingFromTableReportsAnUnrecordedRoute(t *testing.T) {
	recorded := RouteInfo{Method: http.MethodPost, Pattern: "/api/v1/native/issues"}
	raw := RouteInfo{Method: http.MethodPost, Pattern: "/api/v1/native/raw-probe"}
	table := map[RouteInfo]bool{recorded: true}
	if got := missingFromTable(table, []RouteInfo{recorded}); len(got) != 0 {
		t.Fatalf("missing = %+v, want none — that route IS recorded", got)
	}
	got := missingFromTable(table, []RouteInfo{recorded, raw})
	if len(got) != 1 || got[0] != raw {
		t.Fatalf("missing = %+v, want exactly [%+v]", got, raw)
	}
}

// The CLASS, not a sample: every state-changing route the three board
// registrars mount refuses a viewer with the gate's own 403 token, and every
// read still answers one. A route added to any of them without the rung
// turns this red on the route that lacks it.
func TestEveryBoardWriteRouteTakesTheMemberRung(t *testing.T) {
	srv, ts, _ := boardRungFixture(t)
	viewer := roleGateCaller("u-viewer-a", "tenant-A", identity.RoleViewer)
	writes, reads := 0, 0
	for _, route := range boardClassRoutes(t, srv) {
		t.Run(route.Method+" "+route.Pattern, func(t *testing.T) {
			path := boardProbePath(route.Pattern)
			// A class member the mux cannot resolve would make this
			// witness vacuous: assert the route is really served.
			if _, pattern := srv.mux.Handler(httptest.NewRequest(route.Method, path, nil)); pattern == "" {
				t.Fatalf("no handler registered for %s %s — the enumeration drifted from the routing table", route.Method, path)
			}
			code, token := boardReq(t, srv, ts, route.Method, path, "{}", viewer)
			if !isStateChangingMethod(route.Method) {
				if code == http.StatusForbidden {
					t.Fatalf("a viewer reading %s %s = 403 %q — the rung takes writes, a viewer reads the board", route.Method, path, token)
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
	// The floor is set so that ONE registrar going silent is red: the three
	// together cover 32 writes and 10 reads, and the smallest of them (the
	// forge↔board endpoints) carries 3 writes and 2 reads. A registrar that
	// early-returns in this fixture — the hole that hid the forge family
	// from this witness — drops the count under the floor instead of
	// quietly shrinking the class.
	if writes < 30 || reads < 9 {
		t.Fatalf("the class covered %d write route(s) and %d read route(s) — the three board registrars (pipeline-board, cloud board, forge↔board) together register far more; one of them mounted nothing", writes, reads)
	}
}

// The round's attack, door by door: each of these writes the Ready column
// (directly, or by creating a card already in it), and each used to answer a
// viewer 2xx. The last assertion is the EFFECT, not the screen: nothing the
// viewer sent reached the board.
func TestViewerCannotFillTheReadyColumn(t *testing.T) {
	srv, ts, board := boardRungFixture(t)
	viewer := roleGateCaller("u-viewer-a", "tenant-A", identity.RoleViewer)
	for _, tc := range []struct{ name, method, path, body string }{
		{"create with start", http.MethodPost, "/api/v1/pipeline-board/tasks", `{"title":"probe","bot":"review-pr","start":true}`},
		{"ready", http.MethodPost, "/api/v1/pipeline-board/tasks/task-1/ready", `{}`},
		{"reset", http.MethodPost, "/api/v1/pipeline-board/tasks/task-1/reset", `{}`},
		{"bulk ready", http.MethodPost, "/api/v1/pipeline-board/bulk/ready", `{"ids":["task-1"]}`},
		{"recompute deps", http.MethodPost, "/api/v1/pipeline-board/bulk/recompute-deps", `{}`},
		{"launch now", http.MethodPost, "/api/v1/pipeline-board/tasks/task-1/launch", `{}`},
		{"native create ready", http.MethodPost, "/api/v1/native/issues", `{"title":"probe","state":"ready"}`},
		{"native transition", http.MethodPost, "/api/v1/native/issues/i-1/transition", `{"state":"ready"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, token := boardReq(t, srv, ts, tc.method, tc.path, tc.body, viewer)
			if code != http.StatusForbidden || token != denyMemberRoleRequired {
				t.Fatalf("a viewer's %s (%s %s) = %d %q, want 403 %s", tc.name, tc.method, tc.path, code, token, denyMemberRoleRequired)
			}
		})
	}
	issues, err := board.List(native.ListFilter{})
	if err != nil {
		t.Fatalf("list the board: %v", err)
	}
	if len(issues) != 0 {
		t.Fatalf("the board holds %d card(s) after every viewer write was refused: %+v", len(issues), issues)
	}
}

// The rung admits the role the operator's board work takes: a member really
// writes, and the card really lands in the Ready column the dispatcher
// drains. A guard that refused everyone would pass the test above and fail
// here.
func TestMemberStillFillsTheReadyColumn(t *testing.T) {
	srv, ts, board := boardRungFixture(t)
	member := roleGateCaller("u-member-a", "tenant-A", identity.RoleMember)
	code, token := boardReq(t, srv, ts, http.MethodPost, "/api/v1/native/issues", `{"title":"probe","state":"`+native.StateReady+`"}`, member)
	if code != http.StatusCreated {
		t.Fatalf("a member creating a ready card = %d %q, want 201", code, token)
	}
	issues, err := board.List(native.ListFilter{})
	if err != nil {
		t.Fatalf("list the board: %v", err)
	}
	if len(issues) != 1 || issues[0].State != native.StateReady {
		t.Fatalf("board = %+v, want exactly one card in %q", issues, native.StateReady)
	}
}

// A viewer of the team keeps the board: the reads of both families answer
// 200, so the rung refuses actions and not the console.
func TestViewerStillReadsTheBoard(t *testing.T) {
	srv, ts, _ := boardRungFixture(t)
	viewer := roleGateCaller("u-viewer-a", "tenant-A", identity.RoleViewer)
	for _, path := range []string{
		"/api/v1/native/issues",
		"/api/v1/native/board",
		"/api/v1/native/labels",
		"/api/v1/pipeline-board",
	} {
		code, token := boardReq(t, srv, ts, http.MethodGet, path, "", viewer)
		if code != http.StatusOK {
			t.Fatalf("a viewer reading %s = %d %q, want 200", path, code, token)
		}
	}
}

// The decision itself, principal by principal: the rung is read from the
// METHOD and the caller's standing, and the exemptions are the gate's own
// (a webhook delivery, a daemon of this process, a super-admin). A principal
// with no workspace at all takes the gate's own more specific refusal, so the
// mount's promise of "the same token and envelope a denied launch carries"
// holds for that population too.
func TestBoardWriteDenialReadsTheMethodAndTheRung(t *testing.T) {
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
		{"viewer writing", http.MethodPost, roleGateCaller("u-viewer-a", "tenant-A", identity.RoleViewer), true},
		{"config_editor writing", http.MethodPatch, roleGateCaller("u-configed-a", "tenant-A", identity.RoleConfigEditor), true},
		{"role-less writing", http.MethodDelete, roleGateCaller("u-roleless", "tenant-A", ""), true},
		{"share link writing", http.MethodPost, auth.Identity{UserID: "share:s1", TeamID: "tenant-A", Kind: auth.KindShare}, true},
		{"viewer reading", http.MethodGet, roleGateCaller("u-viewer-a", "tenant-A", identity.RoleViewer), false},
		{"viewer HEADing", http.MethodHead, roleGateCaller("u-viewer-a", "tenant-A", identity.RoleViewer), false},
		{"member writing", http.MethodPost, roleGateCaller("u-member-a", "tenant-A", identity.RoleMember), false},
		{"super-admin writing", http.MethodPost, auth.Identity{UserID: "root", TeamID: "tenant-A", IsSuperAdmin: true}, false},
		{"webhook writing", http.MethodPost, auth.Identity{UserID: "webhook:w1", TeamID: "tenant-A", Kind: auth.KindWebhook}, false},
		{"daemon writing", http.MethodPost, auth.Identity{UserID: boardDispatcherActor, TeamID: "tenant-A", Kind: auth.KindInternal}, false},
		// Exempt by kind, with no team stamped: the rung reads the kind, not
		// the team. (A USER with no team never reaches this decision —
		// requireAuth's choke point refuses that credential first.)
		{"teamless daemon writing", http.MethodPost, auth.Identity{UserID: boardDispatcherActor, Kind: auth.KindInternal}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/api/v1/native/issues", nil)
			req = req.WithContext(auth.WithIdentity(req.Context(), tc.id))
			d := srv.boardWriteDenial(req)
			if tc.refused {
				if d == nil || d.status != http.StatusForbidden || d.reason != denyMemberRoleRequired {
					t.Fatalf("denial = %+v, want 403 %s", d, denyMemberRoleRequired)
				}
				return
			}
			if d != nil {
				t.Fatalf("denial = %+v, want the request served", d)
			}
		})
	}
}

// Local/desktop mode is untouched: with no identity store there is no role
// to read, which is the condition gateLaunch itself opens with. A single
// operator's board must not start answering 403 to its own studio.
func TestLocalBoardWritesAreUntouchedByTheRung(t *testing.T) {
	srv, _ := newTestServer(t)
	if srv.authStore() != nil {
		t.Fatal("fixture carries an identity store — it cannot witness local mode")
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/native/issues", strings.NewReader(`{"title":"probe"}`))
	if d := srv.boardWriteDenial(req); d != nil {
		t.Fatalf("local mode refused a board write (%+v) — a server with no identity store has no rung to read", d)
	}
}

// launchDeniedSeries reads the whole iterion_launch_denied_total series —
// every reason child with its value — because that is what the alert reads:
// IterionLaunchDeniesSpiking sums the counter with NO reason filter
// (charts/iterion/templates/prometheus-rule.yaml). A refusal that landed
// under another token would still move the alert, so the witness below reads
// the sum rather than one child.
func launchDeniedSeries(t *testing.T, reg *cloudmetrics.Registry) map[string]float64 {
	t.Helper()
	ch := make(chan prometheus.Metric, 64)
	reg.LaunchDeniedTotal.Collect(ch)
	close(ch)
	out := map[string]float64{}
	for m := range ch {
		var pb dto.Metric
		if err := m.Write(&pb); err != nil {
			t.Fatalf("read the launch-denial counter: %v", err)
		}
		reason := ""
		for _, l := range pb.GetLabel() {
			if l.GetName() == "reason" {
				reason = l.GetValue()
			}
		}
		out[reason] = pb.GetCounter().GetValue()
	}
	return out
}

// The counter belongs to the ADMISSION gate. A board write refused at the
// MOUNT is not a run launch denied: four viewer clicks on a console — the
// round's probe — would otherwise read as four refused launches on
// iterion_launch_denied_total, which the starter alert sums with no reason
// filter. The gate's own refusal, on the same server and the same series,
// still counts: that is the second half, and it is what makes the first half
// a statement about WHICH refusals count rather than about a dead counter.
func TestRefusedBoardWriteIsNotCountedAsARefusedLaunch(t *testing.T) {
	srv, ts, _ := boardRungFixture(t)
	srv.cfg.Metrics = cloudmetrics.New()
	viewer := roleGateCaller("u-viewer-a", "tenant-A", identity.RoleViewer)
	for _, tc := range []struct{ name, method, path, body string }{
		{"delete a saved view", http.MethodDelete, "/api/v1/native/board/views/mine", ""},
		{"rename a label", http.MethodPost, "/api/v1/native/labels/rename", `{"from":"a","to":"b"}`},
		{"patch a board field", http.MethodPatch, "/api/v1/native/board/fields/size", `{"label":"Size"}`},
		{"comment a card", http.MethodPost, "/api/v1/native/issues/i-1/comments", `{"body":"hi"}`},
		// A LAUNCH route that sits behind the mount: the viewer is refused
		// there, before the gate runs, so this refusal is not counted either
		// — the series counts the gate's own refusals, and only those.
		{"launch-now of a card", http.MethodPost, "/api/v1/pipeline-board/tasks/task-1/launch", `{}`},
	} {
		code, token := boardReq(t, srv, ts, tc.method, tc.path, tc.body, viewer)
		if code != http.StatusForbidden || token != denyMemberRoleRequired {
			t.Fatalf("a viewer's %s (%s %s) = %d %q, want 403 %s", tc.name, tc.method, tc.path, code, token, denyMemberRoleRequired)
		}
	}
	if series := launchDeniedSeries(t, srv.cfg.Metrics); len(series) != 0 {
		t.Fatalf("five refused board writes moved iterion_launch_denied_total to %v — the counter counts the admission gate's refusals (the rung at the gate included), not a mount's", series)
	}
	// A body that names a bot: the launch handler decodes it before the gate.
	code, token := boardReq(t, srv, ts, http.MethodPost, "/api/runs", `{"bot_id":"no-such-bot"}`, viewer)
	if code != http.StatusForbidden || token != denyMemberRoleRequired {
		t.Fatalf("a viewer's POST /api/runs = %d %q, want 403 %s", code, token, denyMemberRoleRequired)
	}
	if series := launchDeniedSeries(t, srv.cfg.Metrics); len(series) != 1 || series[denyMemberRoleRequired] != 1 {
		t.Fatalf("the series after ONE refused launch = %v, want exactly {%s: 1} — a refused launch is what this counter is for", series, denyMemberRoleRequired)
	}
}

// requireAuth comes BEFORE the rung inside the wrapper itself: a bearer-less
// write is "authentication required" (401), not an authorization verdict
// about a role nobody presented (403) — and the handler does not run. The
// witness exercises boardWriteAuth alone, because the umbrella
// authMiddleware answers 401 ahead of it in production (middleware.go): go
// through a route and the two layers of this mount could be swapped without
// anything noticing.
func TestAnonymousBoardWriteIsUnauthenticatedNotForbidden(t *testing.T) {
	srv, _ := newRunScopeServer(t)
	served := false
	h := srv.boardWriteAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served = true
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/native/issues", strings.NewReader(`{"title":"probe"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || denialToken(rec.Body.String()) != "authentication required" {
		t.Fatalf("a bearer-less board write = %d %q, want 401 %q — the rung must not answer a caller who presented no credential",
			rec.Code, denialToken(rec.Body.String()), "authentication required")
	}
	if served {
		t.Fatal("the board handler ran for an unauthenticated caller")
	}
}
