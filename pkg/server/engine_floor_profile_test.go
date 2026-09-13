package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/botsource"
)

func pushProfileBundle(requires string) string {
	manifest := "name: needy\nversion: 1.6.0\n"
	if requires != "" {
		manifest += "requires:\n  iterion: \"" + requires + "\"\n"
	}
	body, _ := json.Marshal(map[string]any{"files": map[string]string{
		botsource.MainBotFile: "subbot child:\n  source: \"kids/c.bot\"\n\nworkflow main:\n  entry: child\n  child -> done\n",
		"kids/c.bot":          "dsl: 2\n\nworkflow w:\n  entry: done\n",
		"manifest.yaml":       manifest,
	}})
	return string(body)
}

// A bundle whose subbot child is written in profile 2 and whose manifest
// declares no engine floor is refused at push (409): the child would be
// re-parsed as text on a pod older than the profile, after admission. With
// a floor the fleet can hold, or forced, the push proceeds — forced with a
// warning that names the gap.
func TestAdminBotsPush_RefusesAProfileTwoBundleWithoutAFloor(t *testing.T) {
	pinServerBuild(t, "v3.141.0+deadbeef")
	s, admin, _ := adminBotsServer(t)
	s.runnerBuilds = &fakeBuildObserver{builds: []string{"v3.141.0+abc123"}}

	w := adminBotsPutQuery(s, admin, "needy", "", pushProfileBundle(""))
	if w.Code != http.StatusConflict {
		t.Fatalf("push = %d %s, want 409 — a profile-2 child with no floor must not be stored", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "dsl profile 2 (kids/c.bot)") || !strings.Contains(w.Body.String(), "requires.iterion") {
		t.Fatalf("refusal does not name the child and the remedy: %s", w.Body.String())
	}

	w = adminBotsPutQuery(s, admin, "needy", "", pushProfileBundle(">= 3.141.0"))
	if w.Code/100 != 2 {
		t.Fatalf("push with a floor = %d %s", w.Code, w.Body.String())
	}

	w = adminBotsPutQuery(s, admin, "needy2", "force=1", pushProfileBundle(""))
	if w.Code/100 != 2 || !strings.Contains(w.Body.String(), "FORCED past the profile-floor guard") {
		t.Fatalf("forced push = %d %s", w.Code, w.Body.String())
	}
}
