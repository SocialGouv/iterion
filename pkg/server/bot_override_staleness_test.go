package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/botsource"
)

func TestBundleVersionOrder(t *testing.T) {
	cases := []struct {
		a, b string
		want int
		ok   bool
	}{
		{"0.7.0", "0.8.0", -1, true},
		{"0.8.0", "0.7.0", 1, true},
		{"0.8.0", "0.8.0", 0, true},
		// A string compare gets this one backwards, and it is the shape a
		// bot reaches on its tenth minor release.
		{"0.9.0", "0.10.0", -1, true},
		// A shorter version is the same release, not an older one.
		{"1.2", "1.2.0", 0, true},
		{"v1.3.0", "1.4.0", -1, true},
		// Free-form versions are UNORDERED, never guessed.
		{"2026-09-04", "0.8.0", 0, false},
		{"0.8.0-rc1", "0.8.0", 0, false},
		{"", "0.8.0", 0, false},
		{"nightly", "stable", 0, false},
	}
	for _, c := range cases {
		got, ok := bundleVersionOrder(c.a, c.b)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("bundleVersionOrder(%q,%q) = %d,%v — want %d,%v", c.a, c.b, got, ok, c.want, c.ok)
		}
	}
}

// seedBakedBot writes a one-bot catalog and pins the server to it.
func seedBakedBot(t *testing.T, s *Server, slug, version string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, botsource.MainBotFile), []byte(testBotMain), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := "name: " + slug + "\nversion: " + version + "\n"
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	s.cfg.Bots.Paths = []string{root}
}

// The regression this guards: a stored override outranks the baked catalog
// forever, so a bundle pushed once keeps serving after a later release bakes a
// newer one — measured in prod on 2026-09-06, where a review-pr override
// pinned at 0.7.0 shadowed the 0.8.0 review tiers for 29 hours while the
// operator's own inventory showed nothing wrong.
func TestBotSourceListing_ReportsAnOverrideShadowingANewerBake(t *testing.T) {
	s, editor, _ := newBotSourceTestServer(t)
	edCtx := auth.WithIdentity(context.Background(), editor)
	seedBakedBot(t, s, "reviewer", "0.8.0")

	store := func(version string) {
		t.Helper()
		files := map[string]string{
			botsource.MainBotFile: testBotMain,
			"manifest.yaml":       "name: reviewer\nversion: " + version + "\n",
		}
		body, _ := json.Marshal(botSourcePutReq{Files: files})
		r := httptest.NewRequest("PUT", "/api/teams/t1/bot-sources/reviewer", strings.NewReader(string(body))).WithContext(edCtx)
		r.SetPathValue("id", "t1")
		r.SetPathValue("slug", "reviewer")
		w := httptest.NewRecorder()
		s.handlePutBotSource(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("store %s = %d: %s", version, w.Code, w.Body.String())
		}
	}

	row := func() botSourceMetaView {
		t.Helper()
		r := httptest.NewRequest("GET", "/api/teams/t1/bot-sources", nil).WithContext(edCtx)
		r.SetPathValue("id", "t1")
		w := httptest.NewRecorder()
		s.handleListBotSources(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("list = %d: %s", w.Code, w.Body.String())
		}
		var got struct {
			BotSources []botSourceMetaView `json:"bot_sources"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		for _, v := range got.BotSources {
			if v.Slug == "reviewer" {
				return v
			}
		}
		t.Fatal("reviewer absent from the listing")
		return botSourceMetaView{}
	}

	// An override OLDER than the bake is the defect: it serves, and the
	// inventory must say the newer bundle is being held back.
	store("0.7.0")
	got := row()
	if !got.ShadowsNewerBake {
		t.Errorf("an override at 0.7.0 against a 0.8.0 bake must report shadows_newer_bake; got %+v", got)
	}
	if got.BundleVersion != "0.7.0" || got.BakedVersion != "0.8.0" {
		t.Errorf("versions = stored %q / baked %q — want 0.7.0 / 0.8.0", got.BundleVersion, got.BakedVersion)
	}

	// Control: caught up. The flag must clear, or it is decoration.
	store("0.8.0")
	if got := row(); got.ShadowsNewerBake {
		t.Errorf("an override AT the baked version shadows nothing; got %+v", got)
	}

	// Control: ahead of the bake (an operator shipping before a release).
	store("0.9.0")
	if got := row(); got.ShadowsNewerBake {
		t.Errorf("an override NEWER than the bake shadows nothing; got %+v", got)
	}
}

// A slug the image does not bake shadows nothing — the common case for a
// team's own bot, which must not be flagged.
func TestOverrideShadowsNewerBake_StoredOnlySlugIsNotStale(t *testing.T) {
	s, _, _ := newBotSourceTestServer(t)
	seedBakedBot(t, s, "reviewer", "0.8.0")
	if baked, shadowed := s.overrideShadowsNewerBake("a-bot-only-this-team-has", "0.1.0"); shadowed || baked != "" {
		t.Errorf("a stored-only slug must shadow nothing; got baked=%q shadowed=%v", baked, shadowed)
	}
}
