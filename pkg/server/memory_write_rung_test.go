package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/docsguard"
	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/knowledge"
)

// A team's memory space is INSTRUCTION: the engine opens it on behalf of
// every agent run that declares it and reads the documents back into the
// prompt, under a machine principal the rung exempts. So the rung is owed
// where the document is written, exactly as it is on the board's Ready
// column and on a trigger subscription — and the REST reference already
// published it (`member` on every /api/memory/* row, docs/cloud-rest-api.md)
// while the code mounted the seven routes with requireAuth alone: a viewer,
// or a rank-0 config_editor (ADR-078), could PUT, DELETE and import
// documents in the team's own spaces.

// memorySpaceRef is the space the witnesses below write through the public
// route: the ref memoryRef resolves for `?visibility=org&name=<name>` with
// the request's tenant — so a read-back here reads what the HANDLER wrote,
// not a space of this test's own invention.
func memorySpaceRef(tenantID, name string) knowledge.SpaceRef {
	return knowledge.SpaceRef{Visibility: knowledge.VisibilityOrg, TenantID: tenantID, Name: name}
}

// memoryRungFixture is newRunScopeServer (a real auth store over seeded
// memberships: u-viewer-a is a viewer of tenant-A, u-member-a a member,
// u-configed-a a config_editor) with the memory routes as PRODUCTION mounts
// them — registerMemoryRoutes is unconditional (server_routes.go), so New()
// already mounted them and this fixture must not mount them twice; the class
// witness below proves the mount instead, by holding the registrar's own
// source against the routing table.
//
// ITERION_HOME points at a directory the test owns: no cloud MemoryStore is
// wired, so the routes fall back to memory.DefaultFSStore(), which resolves
// the data dir on every use — the documents land under the temp dir and a
// write that clears the rung is really readable back from the store.
func memoryRungFixture(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	t.Setenv("ITERION_HOME", t.TempDir())
	srv, _ := newRunScopeServer(t)
	return srv, fullStack(t, srv)
}

// memoryRegistrarRoutes derives the family from the registrar's own SOURCE,
// the way triggerRegistrarRoutes reads the trigger registrar's: a list
// written here would go stale the day an eighth route is added, which is the
// hole the class witness exists to refuse.
func memoryRegistrarRoutes(t *testing.T) []RouteInfo {
	t.Helper()
	const src = "memory_routes.go"
	code, calls := registrarSource(t, src)
	re := regexp.MustCompile(`(?i:mux)\.Handle\("([A-Z]+) (/[^"]*)"`)
	matches := re.FindAllStringSubmatch(code, -1)
	if len(matches) != calls {
		t.Fatalf("%s holds %d registration call(s) and this witness parsed %d — a registration it cannot read is one it cannot guard; spell the route as one literal pattern on s.mux.Handle", src, calls, len(matches))
	}
	if len(matches) < 7 {
		t.Fatalf("derived %d route(s) from %s — the registrar's shape changed and this enumeration now covers almost nothing", len(matches), src)
	}
	out := make([]RouteInfo, 0, len(matches))
	for _, m := range matches {
		out = append(out, RouteInfo{Method: m[1], Pattern: m[2]})
	}
	return out
}

// memoryClassRoutes is the whole class: every /api/memory route the routing
// table records AND every one the registrar mounts, never a list written
// here. A route the registrar mounts that the table does not record is a raw
// *http.ServeMux mount — invisible to recordingMux (route_recorder.go) and
// so to every witness built from it.
func memoryClassRoutes(t *testing.T, srv *Server) []RouteInfo {
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
		if strings.HasPrefix(route.Pattern, "/api/memory/") {
			add(route)
		}
	}
	registrar := memoryRegistrarRoutes(t)
	if missing := missingFromTable(routingTable(srv), registrar); len(missing) != 0 {
		t.Fatalf("the registrar mounts %+v, which the routing table does not record: a route handed the bare *http.ServeMux is invisible to recordingMux (route_recorder.go), and so to every witness built from it — mount it on s.mux, or enumerate it here deliberately", missing)
	}
	for _, route := range registrar {
		add(route)
	}
	return out
}

// memoryProbe turns a memory route into one concrete request target: every
// route of the family addresses its space by query param, so a probe that
// named none would be answered 400 by the handler and the read rows would
// prove nothing about the rung.
func memoryProbe(pattern string) string {
	return pattern + "?visibility=org&name=notes&path=a.md"
}

// The CLASS, not a sample: every state-changing memory route refuses a
// viewer with the gate's own 403 token, and every read still answers one. A
// route added to the registrar without the rung turns this red on the route
// that lacks it.
func TestEveryMemoryWriteRouteTakesTheMemberRung(t *testing.T) {
	srv, ts := memoryRungFixture(t)
	viewer := roleGateCaller("u-viewer-a", "tenant-A", identity.RoleViewer)
	writes, reads := 0, 0
	for _, route := range memoryClassRoutes(t, srv) {
		t.Run(route.Method+" "+route.Pattern, func(t *testing.T) {
			path := memoryProbe(route.Pattern)
			// A class member the mux cannot resolve would make this
			// witness vacuous: assert the route is really served.
			if _, pattern := srv.mux.Handler(httptest.NewRequest(route.Method, path, nil)); pattern == "" {
				t.Fatalf("no handler registered for %s %s — the enumeration drifted from the routing table", route.Method, path)
			}
			code, token := boardReq(t, srv, ts, route.Method, path, "# shared notes", viewer)
			if !isStateChangingMethod(route.Method) {
				if code == http.StatusForbidden {
					t.Fatalf("a viewer reading %s %s = 403 %q — the rung takes writes, a viewer reads the team's knowledge", route.Method, path, token)
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
	// The floor is the registrar's full set (3 writes: write, delete,
	// import; 4 reads: usage, docs, doc, export), so a registrar that
	// mounted nothing — or an enumeration whose prefix stopped matching —
	// is red instead of silently covering less.
	if writes < 3 || reads < 4 {
		t.Fatalf("the class covered %d write route(s) and %d read route(s) under /api/memory/ — the registrar mounts more", writes, reads)
	}
}

// The round's attack, at the STORE: a viewer's PUT used to answer 200 with
// the document really written into the team's space, and the next agent run
// that opened the space read it back as instruction. The delete and the
// import walked into their handlers the same way. The last assertion of each
// row is the EFFECT, not the screen.
func TestViewerCannotWriteTeamMemory(t *testing.T) {
	srv, ts := memoryRungFixture(t)
	ctx := context.Background()
	viewer := roleGateCaller("u-viewer-a", "tenant-A", identity.RoleViewer)
	ref := memorySpaceRef("tenant-A", "notes")
	seeded, err := srv.memoryStore().WriteDocument(ctx, ref, knowledge.DocumentInput{Path: "a.md", Content: []byte("# the team's own note"), UpdatedBy: "u-member-a"})
	if err != nil {
		t.Fatalf("seed the team's document: %v", err)
	}
	for _, tc := range []struct{ name, method, path, body string }{
		{"write", http.MethodPut, "/api/memory/doc?visibility=org&name=notes&path=injected.md", "# a viewer's instruction"},
		{"delete", http.MethodDelete, "/api/memory/doc?visibility=org&name=notes&path=a.md", ""},
		{"import", http.MethodPost, "/api/memory/import?visibility=org&name=notes", memoryExportTarball(t, srv, "tenant-A")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, token := boardReq(t, srv, ts, tc.method, tc.path, tc.body, viewer)
			if code != http.StatusForbidden || token != denyMemberRoleRequired {
				t.Fatalf("a viewer's %s (%s %s) = %d %q, want 403 %s", tc.name, tc.method, tc.path, code, token, denyMemberRoleRequired)
			}
		})
	}
	if _, err := srv.memoryStore().ReadDocument(ctx, ref, "injected.md"); err == nil {
		t.Fatal("the viewer's document landed in the team's space: the refusal was the screen, not the write")
	}
	doc, err := srv.memoryStore().ReadDocument(ctx, ref, "a.md")
	if err != nil {
		t.Fatalf("the team's own document is gone after a refused delete: %v", err)
	}
	if string(doc.Content) != "# the team's own note" || doc.Meta.Revision != seeded.Revision {
		t.Fatalf("the team's document reads %q at revision %d, want the seeded %q at %d", doc.Content, doc.Meta.Revision, "# the team's own note", seeded.Revision)
	}
}

// A rank-0 config_editor is not a member of the ladder (ADR-078): the
// capability edits its share's config and nothing else, so it writes no
// document into the team's shared knowledge either.
func TestConfigEditorCannotWriteTeamMemory(t *testing.T) {
	srv, ts := memoryRungFixture(t)
	editor := roleGateCaller("u-configed-a", "tenant-A", identity.RoleConfigEditor)
	code, token := boardReq(t, srv, ts, http.MethodPut, "/api/memory/doc?visibility=org&name=notes&path=a.md", "# a config editor's instruction", editor)
	if code != http.StatusForbidden || token != denyMemberRoleRequired {
		t.Fatalf("a config_editor's memory write = %d %q, want 403 %s", code, token, denyMemberRoleRequired)
	}
	if _, err := srv.memoryStore().ReadDocument(context.Background(), memorySpaceRef("tenant-A", "notes"), "a.md"); err == nil {
		t.Fatal("the config_editor's document landed in the team's space")
	}
}

// memoryExportTarball is a REAL export of a tenant's space, built through
// knowledge.ExportSpace so the import rows below drive the handler's own
// reader rather than a payload it would refuse for being malformed — a 422
// would make the "member allowed" row prove nothing about the rung.
func memoryExportTarball(t *testing.T, srv *Server, tenantID string) string {
	t.Helper()
	ctx := context.Background()
	src := memorySpaceRef(tenantID, "exported")
	if _, err := srv.memoryStore().WriteDocument(ctx, src, knowledge.DocumentInput{Path: "imported.md", Content: []byte("# an exported note"), UpdatedBy: "u-member-a"}); err != nil {
		t.Fatalf("seed the export source: %v", err)
	}
	var buf bytes.Buffer
	if _, err := knowledge.ExportSpace(ctx, srv.memoryStore(), src, &buf); err != nil {
		t.Fatalf("export the source space: %v", err)
	}
	return buf.String()
}

// The rung admits the role the operator's knowledge work takes: a member
// really writes, really deletes and really imports, and each effect is read
// back from the store. A guard that refused everyone would pass the rows
// above and fail here.
func TestMemberStillWritesTeamMemory(t *testing.T) {
	srv, ts := memoryRungFixture(t)
	ctx := context.Background()
	member := roleGateCaller("u-member-a", "tenant-A", identity.RoleMember)
	ref := memorySpaceRef("tenant-A", "notes")
	tarball := memoryExportTarball(t, srv, "tenant-A")

	if code, token := boardReq(t, srv, ts, http.MethodPut, "/api/memory/doc?visibility=org&name=notes&path=a.md", "# a member's note", member); code != http.StatusOK {
		t.Fatalf("a member writing a document = %d %q, want 200", code, token)
	}
	doc, err := srv.memoryStore().ReadDocument(ctx, ref, "a.md")
	if err != nil || string(doc.Content) != "# a member's note" {
		t.Fatalf("the member's document reads (%q, %v), want %q", doc.Content, err, "# a member's note")
	}

	if code, token := boardReq(t, srv, ts, http.MethodPost, "/api/memory/import?visibility=org&name=notes&strategy=overwrite", tarball, member); code != http.StatusOK {
		t.Fatalf("a member importing a tarball = %d %q, want 200", code, token)
	}
	if imported, err := srv.memoryStore().ReadDocument(ctx, ref, "imported.md"); err != nil || string(imported.Content) != "# an exported note" {
		t.Fatalf("the imported document reads (%q, %v), want %q", imported.Content, err, "# an exported note")
	}

	if code, token := boardReq(t, srv, ts, http.MethodDelete, "/api/memory/doc?visibility=org&name=notes&path=a.md", "", member); code != http.StatusNoContent {
		t.Fatalf("a member deleting a document = %d %q, want 204", code, token)
	}
	if _, err := srv.memoryStore().ReadDocument(ctx, ref, "a.md"); err == nil {
		t.Fatal("the member's delete answered 204 and the document is still in the space")
	}
}

// A viewer of the team keeps the knowledge console: the four reads answer
// 200, so the rung refuses writes and not the view.
func TestViewerStillReadsTeamMemory(t *testing.T) {
	srv, ts := memoryRungFixture(t)
	viewer := roleGateCaller("u-viewer-a", "tenant-A", identity.RoleViewer)
	ref := memorySpaceRef("tenant-A", "notes")
	if _, err := srv.memoryStore().WriteDocument(context.Background(), ref, knowledge.DocumentInput{Path: "a.md", Content: []byte("# readable"), UpdatedBy: "u-member-a"}); err != nil {
		t.Fatalf("seed the team's document: %v", err)
	}
	for _, path := range []string{
		"/api/memory/usage?visibility=org&name=notes",
		"/api/memory/docs?visibility=org&name=notes",
		"/api/memory/doc?visibility=org&name=notes&path=a.md",
		"/api/memory/export?visibility=org&name=notes",
	} {
		code, token := boardReq(t, srv, ts, http.MethodGet, path, "", viewer)
		if code != http.StatusOK {
			t.Fatalf("a viewer reading %s = %d %q, want 200", path, code, token)
		}
	}
}

// memoryVisibilityCases is one row per visibility the family's store serves
// tenant-scoped, each with the space the handler resolves for it — written
// out here rather than through memory.ResolveSpaceRef, so the read-back is
// independent of the resolver underneath. `private` is out (the store refuses
// it outright) and `global` has its own witness below.
var memoryVisibilityCases = []struct {
	vis   string
	query string
	ref   knowledge.SpaceRef
}{
	{"user", "visibility=user&name=notes", knowledge.SpaceRef{Visibility: knowledge.VisibilityUser, TenantID: "tenant-A", UserID: "u-member-a", Name: "notes"}},
	{"bot", "visibility=bot&name=notes&project=p1&bot=b1", knowledge.SpaceRef{Visibility: knowledge.VisibilityBot, TenantID: "tenant-A", ProjectID: "p1", BotID: "b1", Name: "notes"}},
	{"project", "visibility=project&name=notes&project=p1", knowledge.SpaceRef{Visibility: knowledge.VisibilityProject, TenantID: "tenant-A", ProjectID: "p1", Name: "notes"}},
	{"cross_project", "visibility=cross_project&name=notes", knowledge.SpaceRef{Visibility: knowledge.VisibilityCrossProject, TenantID: "tenant-A", Name: "notes"}},
	{"org", "visibility=org&name=notes", knowledge.SpaceRef{Visibility: knowledge.VisibilityOrg, TenantID: "tenant-A", Name: "notes"}},
}

// The rung is taken on EVERY visibility, and that is a DECISION, not a side
// effect of mounting it at the registrar. `user` is the axis that is nobody
// else's space — "one user's private notes across all projects"
// (knowledge.VisibilityUser) — and a viewer is refused there too: the axis
// keys on the CALLER (memoryRef hands ResolveSpaceRef the context owner,
// never `?user=`, which the last leg drives), so the document a viewer would
// write is instruction for a run of their own, and launching such a run takes
// this very rung (gateLaunch). The decision cannot go stale per visibility —
// mutatingRungDenial reads the METHOD alone, so a visibility added tomorrow
// inherits it — but the refusal must carry a sentence that is TRUE of a
// private space, which "a document in a team space" was not.
func TestTheMemberRungCoversEveryMemoryVisibility(t *testing.T) {
	srv, ts := memoryRungFixture(t)
	ctx := context.Background()
	viewer := roleGateCaller("u-viewer-a", "tenant-A", identity.RoleViewer)
	member := roleGateCaller("u-member-a", "tenant-A", identity.RoleMember)
	if len(memoryVisibilityCases) < 5 {
		t.Fatalf("the table drives %d visibilit(ies) — the tenant-scoped axes of knowledge.Visibility are five", len(memoryVisibilityCases))
	}
	for _, tc := range memoryVisibilityCases {
		t.Run(tc.vis, func(t *testing.T) {
			base := "/api/memory/doc?" + tc.query + "&path="
			code, token := boardReq(t, srv, ts, http.MethodPut, base+"injected.md", "# a viewer's instruction", viewer)
			if code != http.StatusForbidden || token != denyMemberRoleRequired {
				t.Fatalf("a viewer writing visibility=%s = %d %q, want 403 %s", tc.vis, code, token, denyMemberRoleRequired)
			}
			// The member's leg is what keeps the row from passing on a
			// visibility the store serves not at all: the rung is what
			// refused the viewer, and a member's write really lands.
			if code, token := boardReq(t, srv, ts, http.MethodPut, base+"a.md", "# a member's note", member); code != http.StatusOK {
				t.Fatalf("a member writing visibility=%s = %d %q, want 200", tc.vis, code, token)
			}
			doc, err := srv.memoryStore().ReadDocument(ctx, tc.ref, "a.md")
			if err != nil || string(doc.Content) != "# a member's note" {
				t.Fatalf("%s reads (%q, %v), want %q — the row drove a space other than the handler's", tc.ref.ID(), doc.Content, err, "# a member's note")
			}
			if _, err := srv.memoryStore().ReadDocument(ctx, tc.ref, "injected.md"); err == nil {
				t.Fatalf("the viewer's document landed in %s: the refusal was the screen, not the write", tc.ref.ID())
			}
		})
	}

	// The sentence the refusal carries must hold where it is read: a `user`
	// space is not a team space, so a detail that calls it one tells the
	// caller about a surface they were not changing.
	req := httptest.NewRequest(http.MethodPut, "/api/memory/doc?visibility=user&name=notes&path=a.md", nil)
	req = req.WithContext(auth.WithIdentity(req.Context(), viewer))
	d := srv.memoryWriteDenial(req)
	if d == nil {
		t.Fatal("a viewer writing their own user space is refused, so the decision owes a detail")
	}
	if strings.Contains(d.detail, "team") {
		t.Fatalf("detail = %q — the mount refuses a `user` space too, which no team shares; the sentence must hold on every visibility it answers for", d.detail)
	}

	// `?user=` cannot move the axis — the registrar publishes it ("the
	// tenant/user come from the auth identity (never a query param)"), and it
	// is what keeps one member from writing instruction into another's space.
	if code, token := boardReq(t, srv, ts, http.MethodPut, "/api/memory/doc?visibility=user&name=notes&user=u-viewer-a&path=steer.md", "# steering another user", member); code != http.StatusOK {
		t.Fatalf("a member writing ?user=u-viewer-a = %d %q, want 200 into their OWN space", code, token)
	}
	own := knowledge.SpaceRef{Visibility: knowledge.VisibilityUser, TenantID: "tenant-A", UserID: "u-member-a", Name: "notes"}
	victim := knowledge.SpaceRef{Visibility: knowledge.VisibilityUser, TenantID: "tenant-A", UserID: "u-viewer-a", Name: "notes"}
	if _, err := srv.memoryStore().ReadDocument(ctx, own, "steer.md"); err != nil {
		t.Fatalf("the write did not land in the caller's own user space: %v", err)
	}
	if _, err := srv.memoryStore().ReadDocument(ctx, victim, "steer.md"); err == nil {
		t.Fatal("`?user=` moved the space: a member wrote instruction into another user's private notes")
	}
}

// The global space keeps its own, higher gate UNDER the rung: a member of
// the team clears the member rung at the mount and is still refused the
// instance-wide catalogue by requireMemoryWriteAuth, with that gate's own
// sentence — the rung is the floor, not a replacement.
func TestMemoryGlobalWriteStillTakesPlatformAuthority(t *testing.T) {
	srv, ts := memoryRungFixture(t)
	member := roleGateCaller("u-member-a", "tenant-A", identity.RoleMember)
	code, token := boardReq(t, srv, ts, http.MethodPut, "/api/memory/doc?visibility=global&name=catalogue&path=a.md", "# a member's global note", member)
	if code != http.StatusForbidden {
		t.Fatalf("a member writing the global space = %d %q, want 403", code, token)
	}
	if token == denyMemberRoleRequired {
		t.Fatalf("the global refusal reads %q — a member clears the rung; the global space is refused by platform authority", token)
	}
	if _, err := srv.memoryStore().ReadDocument(context.Background(), knowledge.SpaceRef{Visibility: knowledge.VisibilityGlobal, Name: "catalogue"}, "a.md"); err == nil {
		t.Fatal("a member's document landed in the instance-wide catalogue")
	}
}

// The decision itself, principal by principal, on the memory family's own
// instance: the same predicate, the same exemptions by kind, and the
// family's own detail sentence — so a refused caller is told what they were
// changing.
func TestMemoryWriteDenialReadsTheMethodAndTheRung(t *testing.T) {
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
		{"viewer writing", http.MethodPut, roleGateCaller("u-viewer-a", "tenant-A", identity.RoleViewer), true},
		{"viewer deleting", http.MethodDelete, roleGateCaller("u-viewer-a", "tenant-A", identity.RoleViewer), true},
		{"viewer importing", http.MethodPost, roleGateCaller("u-viewer-a", "tenant-A", identity.RoleViewer), true},
		{"config_editor writing", http.MethodPut, roleGateCaller("u-configed-a", "tenant-A", identity.RoleConfigEditor), true},
		{"role-less writing", http.MethodPut, roleGateCaller("u-roleless", "tenant-A", ""), true},
		{"share link writing", http.MethodPut, auth.Identity{UserID: "share:s1", TeamID: "tenant-A", Kind: auth.KindShare}, true},
		{"viewer reading", http.MethodGet, roleGateCaller("u-viewer-a", "tenant-A", identity.RoleViewer), false},
		{"member writing", http.MethodPut, roleGateCaller("u-member-a", "tenant-A", identity.RoleMember), false},
		{"super-admin writing", http.MethodPut, auth.Identity{UserID: "root", TeamID: "tenant-A", IsSuperAdmin: true}, false},
		{"webhook writing", http.MethodPut, auth.Identity{UserID: "webhook:w1", TeamID: "tenant-A", Kind: auth.KindWebhook}, false},
		{"daemon writing", http.MethodPut, auth.Identity{UserID: triggerSpineActor, TeamID: "tenant-A", Kind: auth.KindInternal}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/api/memory/doc?visibility=org&name=notes&path=a.md", nil)
			req = req.WithContext(auth.WithIdentity(req.Context(), tc.id))
			d := srv.memoryWriteDenial(req)
			if tc.refused {
				if d == nil || d.status != http.StatusForbidden || d.reason != denyMemberRoleRequired {
					t.Fatalf("denial = %+v, want 403 %s", d, denyMemberRoleRequired)
				}
				// The family's own sentence: what the caller was changing is
				// what they can act on.
				if !strings.Contains(d.detail, "knowledge") {
					t.Fatalf("detail = %q: a refused knowledge write must be told what it was changing, not the board's sentence", d.detail)
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
// operator's memory console must not start answering 403 to its own studio.
func TestLocalMemoryWritesAreUntouchedByTheRung(t *testing.T) {
	srv, _ := newTestServer(t)
	if srv.authStore() != nil {
		t.Fatal("fixture carries an identity store — it cannot witness local mode")
	}
	for _, method := range []string{http.MethodPut, http.MethodDelete, http.MethodPost} {
		req := httptest.NewRequest(method, "/api/memory/doc?visibility=org&name=notes&path=a.md", strings.NewReader("# note"))
		if d := srv.memoryWriteDenial(req); d != nil {
			t.Fatalf("local mode refused %s /api/memory/doc (%+v) — a server with no identity store has no rung to read", method, d)
		}
	}
}

// The three families' wrappers are the SAME rung with three sentences, and
// the generated annex must publish all three as a rung: a wrapper built on
// mutatingRungAuth that is missing from writeRungWrapperHeads
// (rest_inventory.go) would be printed `session` — a page claiming less than
// the code enforces, with every freshness gate green. The access-control
// page's own guard reads a THIRD list (docsguard.IterionSpec().Gates), so
// this witness holds the family against all three from one derivation.
func TestEveryWriteRungWrapperIsPublishedAsARung(t *testing.T) {
	wrappers := wrappersBuiltOnTheMutatingRung(t)
	if len(wrappers) < 3 {
		t.Fatalf("found %d wrapper(s) built on mutatingRungAuth, floor 3 (board, trigger, memory) — this witness no longer reads the family it guards", len(wrappers))
	}
	gates := map[string]string{}
	for _, g := range docsguard.IterionSpec().Gates {
		gates[g.Symbol] = g.File
	}
	for _, w := range wrappers {
		t.Run(w.name, func(t *testing.T) {
			if !isWriteRungWrapper(w.name) {
				t.Fatalf("%s wraps mutatingRungAuth and is absent from writeRungWrapperHeads (rest_inventory.go) — the annex would publish its routes as `session`", w.name)
			}
			if rung := registrationRung("s."+w.name, http.MethodPost, "/api/probe"); rung != "session + write rung" {
				t.Fatalf("registrationRung(s.%s) = %q, want %q", w.name, rung, "session + write rung")
			}
			if !isAuthWrapperHead("s." + w.name) {
				t.Fatalf("%s is not an auth wrapper head — a registration hiding it behind a local variable would be classified instead of refused", w.name)
			}
			// Both symbols of the family, held against the access-control
			// page's gate list: a symbol absent from it is a row the page can
			// retire or mislabel with docsguard still green (its UndeclaredGate
			// and UndrawnGate refusals only ever read that list).
			for _, sym := range []string{w.name, strings.TrimSuffix(w.name, "Auth") + "Denial"} {
				file, listed := gates[sym]
				if !listed {
					t.Fatalf("%s is absent from docsguard.IterionSpec().Gates — the access-control page's row for this family is unguarded, so it can go stale silently; add {%q, %q}", sym, sym, "pkg/server/"+w.file)
				}
				if want := "pkg/server/" + w.file; file != want {
					t.Fatalf("docsguard lists %s in %q, and the wrapper is declared in %q — the guard would refuse a symbol its file no longer declares", sym, file, want)
				}
			}
		})
	}
}

// rungWrapper is one family wrapper built on the shared rung, with the file
// that declares it — the fact docsguard's gate list also records.
type rungWrapper struct{ name, file string }

// wrappersBuiltOnTheMutatingRung reads the package's own non-test sources
// for the methods whose body calls s.mutatingRungAuth, so the list cannot be
// the one it judges.
//
// It also reads the GESTURE, not only the named-wrapper shape: every
// `s.mutatingRungAuth(` call site must BE one of those wrappers. A family
// that mounted `s.mutatingRungAuth(detail)(h)` straight at its registrar
// would enforce the rung while registrationRung fell through to `session` —
// the same "page claims less than the code enforces" failure, one shape
// removed — so such a site is refused here rather than classified.
func wrappersBuiltOnTheMutatingRung(t *testing.T) []rungWrapper {
	t.Helper()
	shape := regexp.MustCompile(`func \(s \*Server\) (\w+)\(next http\.Handler\) http\.Handler \{\s*return (s\.mutatingRungAuth\()`)
	call := regexp.MustCompile(`s\.mutatingRungAuth\(`)
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the server package: %v", err)
	}
	var out []rungWrapper
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(name) // #nosec G304 -- the package's own directory listing
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		code := lineComments.ReplaceAllString(string(raw), "")
		wrapped := map[int]bool{}
		for _, m := range shape.FindAllStringSubmatchIndex(code, -1) {
			out = append(out, rungWrapper{name: code[m[2]:m[3]], file: name})
			wrapped[m[4]] = true
		}
		for _, at := range call.FindAllStringIndex(code, -1) {
			if wrapped[at[0]] {
				continue
			}
			t.Errorf("%s uses the shared rung outside a family wrapper, at %q — registrationRung (rest_inventory.go) reads the wrapper NAME off the registration line, so this mount enforces the rung and the annex publishes `session`; wrap it in a `func (s *Server) xWriteAuth(next http.Handler) http.Handler` and list it in writeRungWrapperHeads",
				name, strings.TrimSpace(code[max(0, at[0]-120):min(len(code), at[1]+40)]))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// The memory family's twin of the same order: authentication first, the rung
// second, inside memoryWriteAuth itself — a caller who presented no
// credential is told so, not told their role is too low.
func TestAnonymousMemoryWriteIsUnauthenticatedNotForbidden(t *testing.T) {
	srv, _ := newRunScopeServer(t)
	served := false
	h := srv.memoryWriteAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served = true
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPut, "/api/memory/doc?visibility=org&name=notes&path=a.md", strings.NewReader("# note"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || denialToken(rec.Body.String()) != "authentication required" {
		t.Fatalf("a bearer-less memory write = %d %q, want 401 %q — the rung must not answer a caller who presented no credential",
			rec.Code, denialToken(rec.Body.String()), "authentication required")
	}
	if served {
		t.Fatal("the memory handler ran for an unauthenticated caller")
	}
}

// The THREE families' wrappers are the same rung with three sentences: one
// definition decides, so they cannot drift into disagreeing about who may
// spend the org's money, and each still names the surface it guards.
func TestTheThreeWriteRungFamiliesShareOneDecision(t *testing.T) {
	srv, _ := newRunScopeServer(t)
	viewer := roleGateCaller("u-viewer-a", "tenant-A", identity.RoleViewer)
	req := httptest.NewRequest(http.MethodPost, "/api/memory/import?visibility=org&name=notes", nil)
	req = req.WithContext(auth.WithIdentity(req.Context(), viewer))
	families := map[string]*launchDenial{
		"board":   srv.boardWriteDenial(req),
		"trigger": srv.triggerWriteDenial(req),
		"memory":  srv.memoryWriteDenial(req),
	}
	details := map[string]string{}
	for name, d := range families {
		if d == nil || d.status != http.StatusForbidden || d.reason != denyMemberRoleRequired {
			t.Fatalf("the %s family answers %+v, want 403 %s — the three read ONE decision", name, d, denyMemberRoleRequired)
		}
		if other, clash := details[d.detail]; clash {
			t.Fatalf("the %s and %s families both say %q — each owes the caller the surface they were changing", name, other, d.detail)
		}
		details[d.detail] = name
	}
}
