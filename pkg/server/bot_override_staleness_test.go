package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/botsource"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/platformcfg"
	"github.com/SocialGouv/iterion/pkg/store"
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

// shadowsNewerVersionFor resolves the tier below tenantID and compares one
// row against it, failing the test when the catalog could not be read at all.
func shadowsNewerVersionFor(t *testing.T, s *Server, tenantID, slug, stored string) (string, bool) {
	t.Helper()
	versions, ok := s.versionsBelow(tenantID)
	if !ok {
		t.Fatalf("the catalog must be readable in this test")
	}
	return shadowsNewerVersion(versions, slug, stored)
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
	if !got.ShadowsNewerVersion {
		t.Errorf("an override at 0.7.0 against a 0.8.0 bake must report shadows_newer_version; got %+v", got)
	}
	if got.BundleVersion != "0.7.0" || got.ShadowedVersion != "0.8.0" {
		t.Errorf("versions = stored %q / baked %q — want 0.7.0 / 0.8.0", got.BundleVersion, got.ShadowedVersion)
	}

	// Control: caught up. The flag must clear, or it is decoration.
	store("0.8.0")
	if got := row(); got.ShadowsNewerVersion {
		t.Errorf("an override AT the baked version shadows nothing; got %+v", got)
	}

	// Control: ahead of the bake (an operator shipping before a release).
	store("0.9.0")
	if got := row(); got.ShadowsNewerVersion {
		t.Errorf("an override NEWER than the bake shadows nothing; got %+v", got)
	}
}

// A slug the catalog does not carry shadows nothing — the common case for a
// team's own bot, which must not be flagged.
func TestShadowsNewerVersion_StoredOnlySlugIsNotStale(t *testing.T) {
	s, _, _ := newBotSourceTestServer(t)
	seedBakedBot(t, s, "reviewer", "0.8.0")
	if baked, shadowed := shadowsNewerVersionFor(t, s, "t1", "a-bot-only-this-team-has", "0.1.0"); shadowed || baked != "" {
		t.Errorf("a stored-only slug must shadow nothing; got below=%q shadowed=%v", baked, shadowed)
	}
}

// Resolution is team → platform → baked, so what a TEAM row holds back is the
// platform override when one exists — not the bake behind it. Comparing a team
// row against the bake alone called a 0.9.0 team override "current" while it
// shadowed a 1.0.0 platform one (Revi's open question on the parent commit).
func TestVersionsBelow_ATeamRowIsShadowedByThePlatformTier(t *testing.T) {
	s, _, _ := newBotSourceTestServer(t)
	seedBakedBot(t, s, "reviewer", "0.8.0")
	pctx := store.WithTenant(context.Background(), botsource.PlatformTenantID)
	if _, err := s.botSources.Create(pctx, botsource.BotSource{
		TenantID: botsource.PlatformTenantID,
		Slug:     "reviewer",
		Files: map[string]string{
			botsource.MainBotFile: testBotMain,
			"manifest.yaml":       "name: reviewer\nversion: 1.0.0\n",
		},
	}); err != nil {
		t.Fatal(err)
	}
	s.invalidatePlatformBots()

	// A team row NEWER than the bake but OLDER than the platform override.
	below, shadowed := shadowsNewerVersionFor(t, s, "t1", "reviewer", "0.9.0")
	if !shadowed || below != "1.0.0" {
		t.Errorf("a team row at 0.9.0 shadows the 1.0.0 platform override; got below=%q shadowed=%v", below, shadowed)
	}

	// The platform row itself is measured against the bake — never itself.
	below, shadowed = shadowsNewerVersionFor(t, s, botsource.PlatformTenantID, "reviewer", "1.0.0")
	if shadowed || below != "0.8.0" {
		t.Errorf("a platform row compares against the bake; got below=%q shadowed=%v", below, shadowed)
	}
}

// A shadow that appears MID-PROCESS must still be reported. Caching the
// negative verdict burned the key on the first quiet launch, so a platform
// override pushed afterwards was silenced for the process's lifetime — and the
// platform overlay is a TTL cache that every `admin bots push` refills, so
// this is a routine sequence, not a corner (Revi Rac8574).
func TestWarnOverrideShadow_ReportsAShadowThatAppearsMidProcess(t *testing.T) {
	s, _, _ := newBotSourceTestServer(t)
	seedBakedBot(t, s, "reviewer", "0.8.0")
	var buf bytes.Buffer
	s.logger = iterlog.New(iterlog.LevelWarn, &buf)
	staleOverrideWarned.Range(func(k, _ any) bool { staleOverrideWarned.Delete(k); return true })

	// A team row at 0.9.0 is ahead of the 0.8.0 bake: nothing to report yet.
	s.warnIfOverrideShadowsNewerBake("t1", "reviewer", "team", "0.9.0")
	if buf.Len() != 0 {
		t.Fatalf("nothing shadowed yet; got:\n%s", buf.String())
	}

	// A super-admin pushes a platform override at 1.0.0. The SAME team row now
	// holds it back.
	pctx := store.WithTenant(context.Background(), botsource.PlatformTenantID)
	if _, err := s.botSources.Create(pctx, botsource.BotSource{
		TenantID: botsource.PlatformTenantID,
		Slug:     "reviewer",
		Files: map[string]string{
			botsource.MainBotFile: testBotMain,
			"manifest.yaml":       "name: reviewer\nversion: 1.0.0\n",
		},
	}); err != nil {
		t.Fatal(err)
	}
	s.invalidatePlatformBots()

	s.warnIfOverrideShadowsNewerBake("t1", "reviewer", "team", "0.9.0")
	if !strings.Contains(buf.String(), "1.0.0") {
		t.Errorf("a shadow appearing mid-process must warn; got:\n%s", buf.String())
	}
}

// The remedy in the warning must name THIS ROW's tier. Both origins reach this
// warning, but they live at different endpoints — a team row handed the
// platform remedy sends the operator to a 404, or (as a super-admin) to
// deleting the PLATFORM override of that slug, a different row whose removal
// changes what every tenant is served (Revi R03fa85).
func TestWarnOverrideShadow_RemedyNamesTheRowsOwnTier(t *testing.T) {
	s, _, _ := newBotSourceTestServer(t)
	seedBakedBot(t, s, "reviewer", "0.8.0")
	var buf bytes.Buffer
	s.logger = iterlog.New(iterlog.LevelWarn, &buf)
	staleOverrideWarned.Range(func(k, _ any) bool { staleOverrideWarned.Delete(k); return true })

	s.warnIfOverrideShadowsNewerBake("t1", "reviewer", "team", "0.7.0")
	line := buf.String()
	if !strings.Contains(line, "DELETE /api/teams/t1/bot-sources/reviewer") {
		t.Errorf("a team row must be pointed at its own endpoint; got:\n%s", line)
	}
	if strings.Contains(line, "/api/admin/bots") || strings.Contains(line, "admin bots push") {
		t.Errorf("a team row must NOT be pointed at the platform tier; got:\n%s", line)
	}

	buf.Reset()
	s.warnIfOverrideShadowsNewerBake(botsource.PlatformTenantID, "reviewer", "platform", "0.7.0")
	line = buf.String()
	for _, want := range []string{"iterion remote admin bots push bots/reviewer", "DELETE /api/admin/bots/reviewer"} {
		if !strings.Contains(line, want) {
			t.Errorf("a platform row must name %q; got:\n%s", want, line)
		}
	}
	if strings.Contains(line, "/bot-sources/") {
		t.Errorf("a platform row must NOT be pointed at the team tier; got:\n%s", line)
	}
}

// An unreadable catalog must read as UNKNOWN, never as a clean inventory — on
// the very endpoint the runbook calls the check to run after a release
// (Revi Re7858f).
func TestBotSourceListing_AnUnreadableCatalogIsNotACleanInventory(t *testing.T) {
	s, editor, _ := newBotSourceTestServer(t)
	edCtx := auth.WithIdentity(context.Background(), editor)
	seedBakedBot(t, s, "reviewer", "0.8.0")

	files := map[string]string{
		botsource.MainBotFile: testBotMain,
		"manifest.yaml":       "name: reviewer\nversion: 0.7.0\n",
	}
	body, _ := json.Marshal(botSourcePutReq{Files: files})
	r := httptest.NewRequest("PUT", "/api/teams/t1/bot-sources/reviewer", strings.NewReader(string(body))).WithContext(edCtx)
	r.SetPathValue("id", "t1")
	r.SetPathValue("slug", "reviewer")
	w := httptest.NewRecorder()
	s.handlePutBotSource(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("store = %d: %s", w.Code, w.Body.String())
	}

	// Break the catalog: a manifest the loader refuses.
	broken := t.TempDir()
	dir := filepath.Join(broken, "reviewer")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, botsource.MainBotFile), []byte(testBotMain), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("name: [unterminated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.cfg.Bots.Paths = []string{broken}
	s.bakedCatalog = s.newBakedCatalogResolver()

	req := httptest.NewRequest("GET", "/api/teams/t1/bot-sources", nil).WithContext(edCtx)
	req.SetPathValue("id", "t1")
	rec := httptest.NewRecorder()
	s.handleListBotSources(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		ShadowCheckUnavailable bool                `json:"shadow_check_unavailable"`
		BotSources             []botSourceMetaView `json:"bot_sources"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.ShadowCheckUnavailable {
		t.Errorf("an unreadable catalog must be reported, not read as clean; got %s", rec.Body.String())
	}
	for _, v := range got.BotSources {
		if v.ShadowsNewerVersion {
			t.Errorf("no row may claim a verdict when the check could not run; got %+v", v)
		}
	}
}

// docs/platform-bots.md promises the shadow fields on BOTH listings, and one
// handler (listBotSourcesFor) serves both — so both must name the payload's
// type in the generated spec. The team operation was left untyped ("default:
// Response"), which makes the doc a promise with nothing to verify it against:
// the documented-but-unverifiable shape this whole family exists to end.
func TestOpenAPI_BothBotSourceListingsAreTypedWithTheShadowFields(t *testing.T) {
	s := &Server{mux: newRecordingMux()}
	s.mux.Handle("GET /api/admin/bots", http.NotFoundHandler())
	s.mux.Handle("GET /api/teams/{id}/bot-sources", http.NotFoundHandler())

	doc := s.buildOpenAPI()
	paths := doc["paths"].(map[string]any)
	for _, p := range []string{"/api/admin/bots", "/api/teams/{id}/bot-sources"} {
		item, ok := paths[p].(map[string]any)
		if !ok {
			t.Fatalf("%s absent from the spec", p)
		}
		resp, ok := item["get"].(map[string]any)["responses"].(map[string]any)["200"].(map[string]any)
		if !ok {
			t.Errorf("%s has no typed 200 response: %+v", p, item["get"])
			continue
		}
		schema := resp["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
		if ref, _ := schema["$ref"].(string); ref != "#/components/schemas/botSourceListView" {
			t.Errorf("%s 200 $ref = %q, want botSourceListView", p, ref)
		}
	}

	// And the named type must carry the field names the runbook tells an
	// operator to read, or the reference is typed but still not the contract.
	schemas := doc["components"].(map[string]any)["schemas"].(map[string]any)
	props := func(name string) map[string]any {
		t.Helper()
		sch, ok := schemas[name].(map[string]any)
		if !ok {
			t.Fatalf("components.schemas missing %q", name)
		}
		p, _ := sch["properties"].(map[string]any)
		return p
	}
	for _, f := range []string{"bot_sources", "shadow_check_unavailable"} {
		if _, ok := props("botSourceListView")[f]; !ok {
			t.Errorf("botSourceListView is missing %q", f)
		}
	}
	for _, f := range []string{"bundle_version", "shadowed_version", "shadows_newer_version"} {
		if _, ok := props("botSourceMetaView")[f]; !ok {
			t.Errorf("botSourceMetaView is missing %q", f)
		}
	}
}

// A platform row that carries NO version still serves — storedLaunchBot asks
// only for a non-empty main.bot, and forking a loose <name>.bot copies no
// manifest at all (POST /api/admin/bots/{slug}/fork reaches exactly that).
// Reading such a row as absent left the BAKED version standing as "what would
// serve below this team row", so the team row was reported as shadowing a
// bundle that removing it would not serve.
func TestVersionsBelow_AnUnversionedPlatformRowIsPresentNotAbsent(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
	}{
		{"no manifest at all", map[string]string{botsource.MainBotFile: testBotMain}},
		{"a manifest with no version", map[string]string{
			botsource.MainBotFile: testBotMain,
			"manifest.yaml":       "name: reviewer\n",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := newBotSourceTestServer(t)
			seedBakedBot(t, s, "reviewer", "0.8.0")
			pctx := store.WithTenant(context.Background(), botsource.PlatformTenantID)
			if _, err := s.botSources.Create(pctx, botsource.BotSource{
				TenantID: botsource.PlatformTenantID,
				Slug:     "reviewer",
				Files:    tc.files,
			}); err != nil {
				t.Fatal(err)
			}
			s.invalidatePlatformBots()

			below, shadowed := shadowsNewerVersionFor(t, s, "t1", "reviewer", "0.7.0")
			if shadowed {
				t.Errorf("the platform row serves here, so nothing orderable is shadowed; got below=%q", below)
			}
			if below != "" {
				t.Errorf("shadowed_version must not name the bake, which removing the team row would not serve; got %q", below)
			}
		})
	}
}

// A team row is measured against the platform overlay, so an overlay that
// could not be READ is unknown — not "no platform rows". Answering "the bake"
// there reports a team row deliberately pinned to match an older platform
// override as shadowing, and warnIfOverrideShadowsNewerBake caches only
// positive verdicts, so that false line could never be superseded once the
// overlay recovered.
func TestVersionsBelow_AnUnreadablePlatformOverlayIsUnknown(t *testing.T) {
	s, _, _ := newBotSourceTestServer(t)
	seedBakedBot(t, s, "reviewer", "0.8.0")

	// The overlay read fails from cold: the resolver has no last-known value,
	// so Get serves nil — the same shape a Mongo blip produces at boot.
	s.platformBots = platformcfg.NewResolverFunc(func(context.Context) (*platformBotSet, error) {
		return nil, errors.New("bot-source store unavailable")
	}, nil)

	if below, ok := s.versionsBelow("t1"); ok {
		t.Errorf("an unreadable platform overlay must read as unknown; got below=%v ok=%v", below, ok)
	}

	// And the warn path must stay silent rather than name a shadow it cannot
	// establish.
	var buf bytes.Buffer
	s.logger = iterlog.New(iterlog.LevelWarn, &buf)
	staleOverrideWarned.Range(func(k, _ any) bool { staleOverrideWarned.Delete(k); return true })
	s.warnIfOverrideShadowsNewerBake("t1", "reviewer", "team", "0.7.0")
	if strings.Contains(buf.String(), "serves the") {
		t.Errorf("no shadow may be claimed while the overlay is unreadable; got:\n%s", buf.String())
	}

	// The platform tier itself is measured against the bake, which IS
	// readable — the outage must not blind that half too.
	if below, ok := s.versionsBelow(botsource.PlatformTenantID); !ok || below["reviewer"] != "0.8.0" {
		t.Errorf("a platform row still compares against the readable bake; got below=%v ok=%v", below, ok)
	}
}

// The dedup key must carry the tenant. A slug-only key let the FIRST team to
// launch a shadowed override consume it and silenced every other team holding
// the same one — the very silence this file exists to end (Revi R7c09d4).
func TestWarnOverrideShadow_DedupsPerTenantNotPerSlug(t *testing.T) {
	s, _, _ := newBotSourceTestServer(t)
	seedBakedBot(t, s, "reviewer", "0.8.0")
	var buf bytes.Buffer
	s.logger = iterlog.New(iterlog.LevelWarn, &buf)
	staleOverrideWarned.Range(func(k, _ any) bool { staleOverrideWarned.Delete(k); return true })

	s.warnIfOverrideShadowsNewerBake("team-a", "reviewer", "team", "0.7.0")
	s.warnIfOverrideShadowsNewerBake("team-b", "reviewer", "team", "0.7.0")
	// Count LINES, not slug occurrences — the message names the slug more
	// than once (subject + remedy).
	if got := strings.Count(buf.String(), "serves the"); got != 2 {
		t.Errorf("two tenants shadowing the same slug must BOTH warn; got %d line(s):\n%s", got, buf.String())
	}
	for _, want := range []string{"team-a", "team-b", "0.7.0", "0.8.0"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("the warning must name %q; got:\n%s", want, buf.String())
		}
	}

	// Same tenant again: deduped, or a bot serving every webhook drowns its
	// own signal.
	before := buf.Len()
	s.warnIfOverrideShadowsNewerBake("team-a", "reviewer", "team", "0.7.0")
	if buf.Len() != before {
		t.Errorf("a repeat of the same shadow must be deduped; got:\n%s", buf.String()[before:])
	}

	// An override that shadows nothing stays silent.
	buf.Reset()
	s.warnIfOverrideShadowsNewerBake("team-c", "reviewer", "team", "0.9.0")
	if buf.Len() != 0 {
		t.Errorf("an override newer than the bake must not warn; got:\n%s", buf.String())
	}
}
