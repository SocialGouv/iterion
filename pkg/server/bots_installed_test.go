package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// An installed package's manifest is the package's: an update (install
// --force) replaces .botz/<name> whole. The manifest edit refuses it — a
// disable there would come back enabled with the next update — and names
// the overlay toggle, whose disable the update leaves alone: the listing and
// the card door read the bot disabled after it.
func TestAnInstalledBotsDisableIsTheOverlaysAndOutlivesItsUpdate(t *testing.T) {
	work := t.TempDir()
	src := t.TempDir()
	pkg := "name: pkg-bot\nversion: 1.0.0\ndescription: a package\n"
	writeFiles(t, src, map[string]string{"pkg-bot/main.bot": metaBakedBot, "pkg-bot/manifest.yaml": pkg})
	writeFiles(t, work, map[string]string{
		"bots/own-bot/main.bot":      metaBakedBot,
		"bots/own-bot/manifest.yaml": "name: own-bot\nversion: 1.0.0\n",
	})
	s := newOrgTestServer(t)
	s.cfg.WorkDir = work
	install := func(force bool) {
		t.Helper()
		body := fmt.Sprintf(`{"url":%q,"force":%v}`, filepath.Join(src, "pkg-bot"), force)
		w := httptest.NewRecorder()
		s.handleBotInstall(w, httptest.NewRequest(http.MethodPost, "/api/v1/bots/install", strings.NewReader(body)))
		if w.Code != http.StatusOK {
			t.Fatalf("install (force=%v) = %d %s", force, w.Code, w.Body.String())
		}
	}
	install(false)

	put := httptest.NewRequest(http.MethodPut, "/api/v1/bots/pkg-bot", strings.NewReader(`{"enabled":false}`))
	put.SetPathValue("name", "pkg-bot")
	w := httptest.NewRecorder()
	s.handleBotsPut(w, put)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "/api/v1/bots/pkg-bot/overlay") {
		t.Fatalf("the manifest edit of an installed package = %d %s, want the 409 naming the overlay toggle", w.Code, w.Body.String())
	}
	assertFiles(t, filepath.Join(work, ".botz", "pkg-bot"), map[string]string{"manifest.yaml": pkg}, "after the refused manifest edit")

	list := httptest.NewRecorder()
	s.handleBotsList(list, httptest.NewRequest(http.MethodGet, "/api/v1/bots", nil))
	var listed struct {
		Bots []struct {
			Name      string `json:"name"`
			Installed bool   `json:"installed"`
		} `json:"bots"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	installed := map[string]bool{}
	for _, b := range listed.Bots {
		installed[b.Name] = b.Installed
	}
	if want := map[string]bool{"pkg-bot": true, "own-bot": false}; fmt.Sprint(installed) != fmt.Sprint(want) {
		t.Fatalf("the listing's installed flags = %v, want %v", installed, want)
	}

	overlay := httptest.NewRequest(http.MethodPut, "/api/v1/bots/pkg-bot/overlay", strings.NewReader(`{"enabled":false}`))
	overlay.SetPathValue("name", "pkg-bot")
	w = httptest.NewRecorder()
	s.handleBotOverlay(w, overlay)
	if w.Code != http.StatusOK {
		t.Fatalf("the overlay's disable = %d %s", w.Code, w.Body.String())
	}
	install(true)
	entry, ok, err := s.findBot("pkg-bot")
	if err != nil || !ok || entry.Enabled {
		t.Fatalf("after the update the listing reads pkg-bot listed=%v enabled=%v (%v), want it disabled", ok, entry.Enabled, err)
	}
	bot, found, err := s.resolvePipelineBot(context.Background(), "", "pkg-bot")
	if err != nil || !found {
		t.Fatalf("the card door does not resolve pkg-bot: found=%v %v", found, err)
	}
	defer bot.Launch.Cleanup()
	if bot.Enabled {
		t.Fatal("after the update the card door reads pkg-bot enabled, want the overlay's disable")
	}
}
