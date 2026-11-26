package server

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/identity"
)

// routeParam matches a path parameter of a Go 1.22 ServeMux pattern.
var routeParam = regexp.MustCompile(`\{[^}]+\}`)

// TestPinnedTokenTeamRoutes_NoSiblingTeamAnswers2xx measures the pin's
// promise on EVERY team-addressed route the server registers, not on the
// routes a review happened to name: a personal access token pinned to team
// ta never gets a 2xx on /api/teams/tb/…, though its owner holds every right
// in tb (admin of tb, admin of their shared org). A gate that authorizes a
// team action on the org standing alone (canManageOrg, which reads the org
// pin only) is the shape this catches — a sibling team deleted, or the org's
// pool rewritten through a team the token cannot read.
//
// Each route runs on a fresh fixture, so a write that succeeds (a delete)
// cannot hide the next route's answer. The session of the same owner, which
// pins nothing, is the unpinned arm: the routes it is admitted on (2xx) are
// the ones this census MEASURES; the others refused both arms on their
// placeholder parameters or body and prove nothing either way, so their
// count is reported and kept from growing silently.
func TestPinnedTokenTeamRoutes_NoSiblingTeamAnswers2xx(t *testing.T) {
	probe, _ := pinnedOrgFixture(t)
	var routes []RouteInfo
	for _, r := range probe.s.mux.Routes() {
		if strings.HasPrefix(r.Pattern, "/api/teams/{id}") {
			routes = append(routes, r)
		}
	}
	if len(routes) < 20 {
		t.Fatalf("the census found %d team-addressed routes; the fixture no longer registers them", len(routes))
	}
	measured, unmeasured := 0, 0
	for _, r := range routes {
		method := r.Method
		if method == "" {
			method = http.MethodGet
		}
		path := strings.Replace(r.Pattern, "{id}", "tb", 1)
		path = routeParam.ReplaceAllString(path, "x")
		body := ""
		if method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch {
			body = "{}"
		}
		call := func(pinned bool) (int, string) {
			f, patToken := pinnedOrgFixture(t)
			if err := f.s.authStore().UpsertMembership(context.Background(), identity.Membership{
				UserID: f.user.User.ID, TeamID: "tb", Role: identity.RoleOwner,
			}); err != nil {
				t.Fatalf("grant the owner tb: %v", err)
			}
			bearer := f.user.AccessToken
			if pinned {
				bearer = patToken
			}
			code, _, resp := f.call(t, method, path, bearer, body)
			return code, resp
		}
		if code, resp := call(true); code/100 == 2 {
			t.Errorf("%s %s answered %d to a token pinned to ta: %.200s", method, path, code, resp)
		}
		if code, _ := call(false); code/100 == 2 {
			measured++
		} else {
			unmeasured++
		}
	}
	t.Logf("%d team-addressed routes: %d measured (the unpinned session is admitted), %d refused both arms on placeholders", len(routes), measured, unmeasured)
}
