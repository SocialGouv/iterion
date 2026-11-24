package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The forge enable decodes its routing block at the edge, before any forge
// I/O: a binding-level locks: is refused 422 and the message names every
// holder of the lock primitive (routingLockHolders) — the same list the
// schedule door quotes (#2688).
func TestForgeEnable_RoutingLocksNameEveryHolder(t *testing.T) {
	s := newForgeTestServer(t)
	w := httptest.NewRecorder()
	s.handleEnableForgeRepoBots(w, forgeReq(superAdminCtx(), "POST", "/api/teams/t1/forge/repo-bots",
		`{"connection_id":"c1","repo":"group/api","bot_ids":["review-pr"],"routing":{"locks":["strict"]}}`, "t1"))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("locks enable = %d: %s", w.Code, w.Body.String())
	}
	for _, holder := range []string{"author's manifest", "team and org routing records", "platform record"} {
		if !strings.Contains(w.Body.String(), holder) {
			t.Fatalf("locks enable message does not name %s: %s", holder, w.Body.String())
		}
	}
}
