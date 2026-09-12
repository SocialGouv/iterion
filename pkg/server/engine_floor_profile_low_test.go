package server

import (
	"net/http"
	"strings"
	"testing"
)

// A floor that is declared but below the release that reads the profile is
// refused like no floor at all (409, forced with a warning): `>= 0.0.1` on a
// profile-2 child would admit every pod that cannot parse it.
func TestAdminBotsPush_RefusesAProfileTwoBundleWithAFloorBelowTheRelease(t *testing.T) {
	pinServerBuild(t, "v3.141.0+deadbeef")
	s, admin, _ := adminBotsServer(t)
	s.runnerBuilds = &fakeBuildObserver{builds: []string{"v3.141.0+abc123"}}

	w := adminBotsPutQuery(s, admin, "lowfloor", "", pushProfileBundle(">= 0.0.1"))
	if w.Code != http.StatusConflict {
		t.Fatalf("push = %d %s, want 409 — a floor below the profile's release admits pods that cannot parse the child", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "0.0.1") || !strings.Contains(w.Body.String(), "below 3.141.0") {
		t.Fatalf("refusal does not name the floor and the release: %s", w.Body.String())
	}
	w = adminBotsPutQuery(s, admin, "lowfloor2", "force=1", pushProfileBundle(">= 0.0.1"))
	if w.Code/100 != 2 || !strings.Contains(w.Body.String(), "FORCED past the profile-floor guard") || !strings.Contains(w.Body.String(), "3.141.0") {
		t.Fatalf("forced push = %d %s", w.Code, w.Body.String())
	}
}
