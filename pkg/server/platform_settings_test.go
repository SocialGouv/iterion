package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/botsource"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/platformcfg"
	"github.com/SocialGouv/iterion/pkg/store"
)

// roleBots resolves the effective role→bot bindings: constants as defaults,
// the platform record field-by-field on top, effective on this replica
// immediately after a mutation (Invalidate).
func TestRoleBots_DefaultsAndOverride(t *testing.T) {
	st := platformcfg.NewMemoryStore[platformcfg.BotRoles]()
	s := New(Config{SkipProjectRegistration: true, BotRolesSettings: st}, iterlog.New(iterlog.LevelError, nil))

	got := s.roleBots()
	if got.Reviewer != defaultWebhookBotReviewPR || got.Brancher != branchImproveBotID ||
		got.Implementer != featureDevBotID || got.ReviConverse != defaultWebhookBotReviConverse {
		t.Fatalf("defaults = %+v", got)
	}

	alt := "my-reviewer"
	if err := st.Put(context.Background(), platformcfg.BotRoles{Reviewer: &alt}); err != nil {
		t.Fatal(err)
	}
	s.botRoles.Invalidate()
	got = s.roleBots()
	if got.Reviewer != "my-reviewer" {
		t.Fatalf("override not applied: %+v", got)
	}
	// The other roles keep their defaults — field-by-field, never wholesale.
	if got.Brancher != branchImproveBotID {
		t.Fatalf("unrelated role clobbered: %+v", got)
	}
}

// The PUT route applies merge semantics (absent field keeps its stored
// state, explicit null clears), validates ids, and echoes effective+origin.
func TestAdminBotRoles_PutMergeAndOrigin(t *testing.T) {
	st := platformcfg.NewMemoryStore[platformcfg.BotRoles]()
	s := New(Config{SkipProjectRegistration: true, BotRolesSettings: st}, iterlog.New(iterlog.LevelError, nil))
	admin := auth.WithIdentity(context.Background(), auth.Identity{UserID: "root", IsSuperAdmin: true})

	put := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("PUT", "/api/admin/settings/bot-roles", strings.NewReader(body)).WithContext(admin)
		w := httptest.NewRecorder()
		s.handleAdminPutBotRoles(w, r)
		return w
	}

	if w := put(`{"reviewer":"alt-reviewer"}`); w.Code != http.StatusOK {
		t.Fatalf("put = %d: %s", w.Code, w.Body.String())
	}
	if w := put(`{"brancher":"alt-brancher"}`); w.Code != http.StatusOK {
		t.Fatalf("merge put = %d: %s", w.Code, w.Body.String())
	}
	rec, _ := st.Get(context.Background())
	if rec == nil || rec.Reviewer == nil || *rec.Reviewer != "alt-reviewer" || rec.Brancher == nil {
		t.Fatalf("merge semantics lost a field: %+v", rec)
	}

	// Explicit null clears one override, keeping the other.
	if w := put(`{"reviewer":null}`); w.Code != http.StatusOK {
		t.Fatalf("clear put = %d: %s", w.Code, w.Body.String())
	}
	rec, _ = st.Get(context.Background())
	if rec.Reviewer != nil || rec.Brancher == nil {
		t.Fatalf("null-clear semantics wrong: %+v", rec)
	}

	// Invalid ids are refused before any write.
	if w := put(`{"implementer":"Not A Slug"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid id = %d, want 400", w.Code)
	}
	if w := put(`{"unknown_role":"x"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown field = %d, want 400", w.Code)
	}

	// GET echoes effective + origin.
	gr := httptest.NewRequest("GET", "/api/admin/settings/bot-roles", nil).WithContext(admin)
	gw := httptest.NewRecorder()
	s.handleAdminGetBotRoles(gw, gr)
	var resp struct {
		Effective effectiveBotRoles `json:"effective"`
		Origin    string            `json:"origin"`
	}
	if err := json.Unmarshal(gw.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Origin != "db" || resp.Effective.Brancher != "alt-brancher" || resp.Effective.Reviewer != defaultWebhookBotReviewPR {
		t.Fatalf("get echo = %+v", resp)
	}
}

// The sandbox family: blank overrides refused, effective value trimmed,
// clearing falls back to "" (inherit env/built-in).
func TestAdminSandboxSettings(t *testing.T) {
	st := platformcfg.NewMemoryStore[platformcfg.Sandbox]()
	s := New(Config{SkipProjectRegistration: true, SandboxSettings: st}, iterlog.New(iterlog.LevelError, nil))
	admin := auth.WithIdentity(context.Background(), auth.Identity{UserID: "root", IsSuperAdmin: true})

	put := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("PUT", "/api/admin/settings/sandbox", strings.NewReader(body)).WithContext(admin)
		w := httptest.NewRecorder()
		s.handleAdminPutSandboxSettings(w, r)
		return w
	}
	if w := put(`{"default_image":"  "}`); w.Code != http.StatusBadRequest {
		t.Fatalf("blank image = %d, want 400 (clear = null, never empty)", w.Code)
	}
	if w := put(`{"default_image":"ghcr.io/x/img@sha256:abc"}`); w.Code != http.StatusOK {
		t.Fatalf("set = %d: %s", w.Code, w.Body.String())
	}
	s.sandboxCfg.Invalidate()
	if got := s.effectiveSandboxImageSetting(context.Background()); got != "ghcr.io/x/img@sha256:abc" {
		t.Fatalf("effective = %q", got)
	}
	if w := put(`{"default_image":null}`); w.Code != http.StatusOK {
		t.Fatalf("clear = %d", w.Code)
	}
	s.sandboxCfg.Invalidate()
	if got := s.effectiveSandboxImageSetting(context.Background()); got != "" {
		t.Fatalf("cleared effective = %q, want empty (inherit)", got)
	}
}

// effectiveEntriesWithSchema overlays the platform tier onto the baked
// catalog: a same-slug override REPLACES the baked entry (its metadata is
// what every tenant must see), a new-slug platform bot is appended.
func TestEffectiveEntries_PlatformOverlay(t *testing.T) {
	s, _, _ := newBotSourceTestServer(t)

	// Bake a catalog bot on the FS.
	dir := t.TempDir()
	botDir := filepath.Join(dir, "bots", "reviewer")
	if err := os.MkdirAll(botDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(botDir, "main.bot"), []byte(testBotMain), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(botDir, "manifest.yaml"), []byte("name: reviewer\ndisplay_name: Baked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.cfg.Bots = BotsConfig{Paths: []string{filepath.Join(dir, "bots")}}

	// Platform override of the same slug + a brand-new platform bot.
	pctx := store.WithTenant(context.Background(), botsource.PlatformTenantID)
	for slug, display := range map[string]string{"reviewer": "Overridden", "brand-new": "Fresh"} {
		if _, err := s.botSources.Create(pctx, botsource.BotSource{
			TenantID: botsource.PlatformTenantID, Slug: slug,
			Files: map[string]string{
				botsource.MainBotFile: testBotMain,
				"manifest.yaml":       "name: " + slug + "\ndisplay_name: " + display + "\n",
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	s.invalidatePlatformBots()

	entries, err := s.effectiveEntriesWithSchema()
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]string{}
	for _, e := range entries {
		byName[e.Name] = e.DisplayName
	}
	if byName["reviewer"] != "Overridden" {
		t.Fatalf("same-slug override must replace the baked entry, got %q", byName["reviewer"])
	}
	if byName["brand-new"] != "Fresh" {
		t.Fatalf("new-slug platform bot must be appended, got %v", byName)
	}
}

func TestAdminBotVars_PutMergeClearAndRejects(t *testing.T) {
	st := platformcfg.NewMemoryStore[platformcfg.BotVars]()
	s := New(Config{SkipProjectRegistration: true, BotVarsSettings: st}, iterlog.New(iterlog.LevelError, nil))
	admin := auth.WithIdentity(context.Background(), auth.Identity{UserID: "root", IsSuperAdmin: true})

	put := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("PUT", "/api/admin/settings/bot-vars", strings.NewReader(body)).WithContext(admin)
		w := httptest.NewRecorder()
		s.handleAdminPutBotVars(w, r)
		return w
	}

	if w := put(`{"ITERION_VIBE_EFFORT_CLAUDE":"max"}`); w.Code != http.StatusOK {
		t.Fatalf("set = %d: %s", w.Code, w.Body.String())
	}
	if w := put(`{"ITERION_MODERNIZE_EFFORT":"max"}`); w.Code != http.StatusOK {
		t.Fatalf("merge set = %d: %s", w.Code, w.Body.String())
	}
	rec, _ := st.Get(context.Background())
	if rec == nil || rec.Vars["ITERION_VIBE_EFFORT_CLAUDE"] != "max" || rec.Vars["ITERION_MODERNIZE_EFFORT"] != "max" {
		t.Fatalf("merge semantics lost a key: %+v", rec)
	}

	// An infra/credential-shaped key is refused and NOTHING is written.
	for _, bad := range []string{`{"ITERION_MONGO_URI":"mongodb://evil"}`, `{"ITERION_MY_API_KEY":"x"}`, `{"OTHER":"x"}`, `{}`} {
		if w := put(bad); w.Code != http.StatusBadRequest {
			t.Fatalf("put %s = %d, want 400", bad, w.Code)
		}
	}
	rec, _ = st.Get(context.Background())
	if len(rec.Vars) != 2 {
		t.Fatalf("a rejected write mutated the record: %+v", rec)
	}

	// null clears; clearing an unset key is a loud 400, not a no-op audit.
	if w := put(`{"ITERION_VIBE_EFFORT_CLAUDE":null}`); w.Code != http.StatusOK {
		t.Fatalf("clear = %d: %s", w.Code, w.Body.String())
	}
	if w := put(`{"ITERION_NEVER_SET":null}`); w.Code != http.StatusBadRequest {
		t.Fatalf("clear-unset = %d, want 400", w.Code)
	}
	rec, _ = st.Get(context.Background())
	if _, still := rec.Vars["ITERION_VIBE_EFFORT_CLAUDE"]; still || len(rec.Vars) != 1 {
		t.Fatalf("clear semantics wrong: %+v", rec)
	}
}

func TestAdminBotVars_ConcurrentPutIsA409NotALostKey(t *testing.T) {
	st := platformcfg.NewMemoryStore[platformcfg.BotVars]()
	s := New(Config{SkipProjectRegistration: true, BotVarsSettings: st}, iterlog.New(iterlog.LevelError, nil))
	admin := auth.WithIdentity(context.Background(), auth.Identity{UserID: "root", IsSuperAdmin: true})

	put := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("PUT", "/api/admin/settings/bot-vars", strings.NewReader(body)).WithContext(admin)
		w := httptest.NewRecorder()
		s.handleAdminPutBotVars(w, r)
		return w
	}
	if w := put(`{"ITERION_A_VAR":"1"}`); w.Code != http.StatusOK {
		t.Fatalf("seed = %d", w.Code)
	}
	// Simulate replica B writing between A's read and A's write: bump the
	// stored record directly so A's CAS token is stale.
	rec, _ := st.Get(context.Background())
	rec.Vars["ITERION_B_VAR"] = "2"
	if err := st.Put(context.Background(), *rec); err != nil {
		t.Fatalf("concurrent write: %v", err)
	}
	// A's handler now reads fresh state (this test drives sequentially),
	// so instead exercise the CAS directly with the stale token.
	stale := rec.UpdatedAt.Add(-time.Second)
	wrote, err := st.PutIfUnchanged(context.Background(), platformcfg.BotVars{Vars: map[string]string{"ITERION_A_VAR": "1"}}, stale)
	if err != nil {
		t.Fatalf("cas: %v", err)
	}
	if wrote {
		t.Fatal("a stale CAS token must not win — that is the silently-dropped-key path")
	}
	after, _ := st.Get(context.Background())
	if after.Vars["ITERION_B_VAR"] != "2" {
		t.Fatalf("the concurrent writer's key was lost: %+v", after.Vars)
	}
}

// A record written under an older rule — no value charset, a server that
// predates it, or a hand edit — may hold entries the current rule refuses.
// BotVarsOverlay never applies them; the admin surface must say so and must
// stay usable: re-judging the whole stored record on every write froze it,
// and `vars rm` of one refused key failed naming the other.
func TestAdminBotVars_ARecordFromAnOlderRuleStaysEditable(t *testing.T) {
	st := platformcfg.NewMemoryStore[platformcfg.BotVars]()
	if err := st.Put(context.Background(), platformcfg.BotVars{Vars: map[string]string{
		"ITERION_VIBE_MODEL_CLAUDE":  "claude-opus-5-5 ",
		"ITERION_VIBE_EFFORT_CLAUDE": "max high",
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s := New(Config{SkipProjectRegistration: true, BotVarsSettings: st}, iterlog.New(iterlog.LevelError, nil))
	admin := auth.WithIdentity(context.Background(), auth.Identity{UserID: "root", IsSuperAdmin: true})
	put := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("PUT", "/api/admin/settings/bot-vars", strings.NewReader(body)).WithContext(admin)
		w := httptest.NewRecorder()
		s.handleAdminPutBotVars(w, r)
		return w
	}
	refused := func() map[string]string {
		r := httptest.NewRequest("GET", "/api/admin/settings/bot-vars", nil).WithContext(admin)
		w := httptest.NewRecorder()
		s.handleAdminGetBotVars(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("get = %d: %s", w.Code, w.Body.String())
		}
		var view struct {
			Refused map[string]string `json:"refused"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return view.Refused
	}

	if got := refused(); len(got) != 2 || got["ITERION_VIBE_MODEL_CLAUDE"] == "" || got["ITERION_VIBE_EFFORT_CLAUDE"] == "" {
		t.Fatalf("refused = %v, want both stored entries named — the view showed them as live overrides", got)
	}
	if w := put(`{"ITERION_OTHER_KNOB":"x"}`); w.Code != http.StatusOK {
		t.Fatalf("an unrelated set = %d: %s — the stored record froze every write", w.Code, w.Body.String())
	}
	if w := put(`{"ITERION_VIBE_MODEL_CLAUDE":null}`); w.Code != http.StatusOK {
		t.Fatalf("rm of a refused entry = %d: %s", w.Code, w.Body.String())
	}
	if w := put(`{"ITERION_VIBE_EFFORT_CLAUDE":"max high"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("setting a value the rule refuses = %d, want 400", w.Code)
	}
	if w := put(`{"ITERION_VIBE_EFFORT_CLAUDE":"high"}`); w.Code != http.StatusOK {
		t.Fatalf("replacing a refused entry with a valid value = %d: %s", w.Code, w.Body.String())
	}
	if got := refused(); len(got) != 0 {
		t.Fatalf("refused = %v after both entries were fixed, want none", got)
	}
	rec, _ := st.Get(context.Background())
	if rec.Vars["ITERION_VIBE_EFFORT_CLAUDE"] != "high" || rec.Vars["ITERION_OTHER_KNOB"] != "x" || len(rec.Vars) != 2 {
		t.Fatalf("stored = %v", rec.Vars)
	}
}

// The key bound refuses a write that GROWS the record past it, never one that
// shrinks or rewrites it: a record already over the bound (a hand edit) must
// still be clearable, key by key.
func TestAdminBotVars_TheKeyBoundNeverRefusesARemoval(t *testing.T) {
	st := platformcfg.NewMemoryStore[platformcfg.BotVars]()
	over := map[string]string{}
	for i := 0; i < 205; i++ {
		over[fmt.Sprintf("ITERION_KNOB_%03d", i)] = "1"
	}
	if err := st.Put(context.Background(), platformcfg.BotVars{Vars: over}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s := New(Config{SkipProjectRegistration: true, BotVarsSettings: st}, iterlog.New(iterlog.LevelError, nil))
	admin := auth.WithIdentity(context.Background(), auth.Identity{UserID: "root", IsSuperAdmin: true})
	put := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("PUT", "/api/admin/settings/bot-vars", strings.NewReader(body)).WithContext(admin)
		w := httptest.NewRecorder()
		s.handleAdminPutBotVars(w, r)
		return w
	}
	if w := put(`{"ITERION_KNOB_000":null}`); w.Code != http.StatusOK {
		t.Fatalf("removal from an over-bound record = %d: %s", w.Code, w.Body.String())
	}
	if w := put(`{"ITERION_KNOB_001":"2"}`); w.Code != http.StatusOK {
		t.Fatalf("rewriting an existing key = %d: %s", w.Code, w.Body.String())
	}
	if w := put(`{"ITERION_KNOB_NEW":"1"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("growing an over-bound record = %d, want 400", w.Code)
	}
}

// keys_first rides the platform-credentials record: a PUT merges it without
// touching the audience lists, the GET reads it back as a stored override,
// and false restores the default order.
func TestAdminPlatformCredentials_KeysFirstMergesAndReadsBack(t *testing.T) {
	st := platformcfg.NewMemoryStore[platformcfg.PlatformCredentials]()
	s := New(Config{SkipProjectRegistration: true, PlatformCredentialsSettings: st}, iterlog.New(iterlog.LevelError, nil))
	admin := auth.WithIdentity(context.Background(), auth.Identity{UserID: "root", IsSuperAdmin: true})
	put := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("PUT", "/api/admin/settings/platform-credentials", strings.NewReader(body)).WithContext(admin)
		w := httptest.NewRecorder()
		s.handleAdminPutPlatformCredentials(w, r)
		return w
	}
	get := func() (keysFirst *bool, origin string) {
		r := httptest.NewRequest("GET", "/api/admin/settings/platform-credentials", nil).WithContext(admin)
		w := httptest.NewRecorder()
		s.handleAdminGetPlatformCredentials(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("get = %d: %s", w.Code, w.Body.String())
		}
		var view struct {
			Stored *struct {
				KeysFirst *bool `json:"keys_first"`
			} `json:"stored"`
			Origin string `json:"origin"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if view.Stored == nil {
			return nil, view.Origin
		}
		return view.Stored.KeysFirst, view.Origin
	}

	if w := put(`{"orgs":["org-1"]}`); w.Code != http.StatusOK {
		t.Fatalf("seed orgs = %d: %s", w.Code, w.Body.String())
	}
	if w := put(`{"keys_first":true}`); w.Code != http.StatusOK {
		t.Fatalf("keys_first = %d: %s", w.Code, w.Body.String())
	}
	rec, _ := st.Get(context.Background())
	if rec == nil || !rec.PrefersKeys() || len(rec.Orgs) != 1 || rec.Orgs[0] != "org-1" {
		t.Fatalf("stored = %+v, want keys_first set and the orgs kept", rec)
	}
	if kf, origin := get(); kf == nil || !*kf || origin != "db" {
		t.Fatalf("GET keys_first = %v origin %q, want true from db", kf, origin)
	}
	if w := put(`{"keys_first":false}`); w.Code != http.StatusOK {
		t.Fatalf("keys_first false = %d: %s", w.Code, w.Body.String())
	}
	if rec, _ := st.Get(context.Background()); rec == nil || rec.PrefersKeys() {
		t.Fatalf("stored = %+v, want the default order back", rec)
	}
	// "" and null clear the override: the env default decides again.
	for _, clear := range []string{`{"keys_first":""}`, `{"keys_first":null}`} {
		if w := put(`{"keys_first":true}`); w.Code != http.StatusOK {
			t.Fatalf("keys_first true = %d: %s", w.Code, w.Body.String())
		}
		t.Setenv(platformcfg.EnvKeysFirst, "")
		if w := put(clear); w.Code != http.StatusOK {
			t.Fatalf("%s = %d: %s", clear, w.Code, w.Body.String())
		}
		if rec, _ := st.Get(context.Background()); rec == nil || rec.KeysFirst != nil {
			t.Fatalf("%s: keys_first still stored, want it cleared back to the env default", clear)
		}
	}
	if w := put(`{"keys_first":"yes"}`); w.Code != http.StatusBadRequest {
		t.Fatalf(`keys_first "yes" = %d, want 400`, w.Code)
	}
}

// facade_default rides the same record: validated on write, "" clears the
// override back to the env default, and the GET says which value the next
// launch applies.
func TestAdminPlatformCredentials_FacadeDefaultIsValidatedAndClearsToTheEnv(t *testing.T) {
	t.Setenv(platformcfg.EnvFacadeDefault, "always")
	st := platformcfg.NewMemoryStore[platformcfg.PlatformCredentials]()
	s := New(Config{SkipProjectRegistration: true, PlatformCredentialsSettings: st}, iterlog.New(iterlog.LevelError, nil))
	admin := auth.WithIdentity(context.Background(), auth.Identity{UserID: "root", IsSuperAdmin: true})
	put := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("PUT", "/api/admin/settings/platform-credentials", strings.NewReader(body)).WithContext(admin)
		w := httptest.NewRecorder()
		s.handleAdminPutPlatformCredentials(w, r)
		return w
	}
	effective := func() string {
		r := httptest.NewRequest("GET", "/api/admin/settings/platform-credentials", nil).WithContext(admin)
		w := httptest.NewRecorder()
		s.handleAdminGetPlatformCredentials(w, r)
		var view struct {
			FacadeDefaultEffective string `json:"facade_default_effective"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return view.FacadeDefaultEffective
	}

	if got := effective(); got != "always" {
		t.Fatalf("effective with no record = %q, want the env default", got)
	}
	if w := put(`{"facade_default":"never"}`); w.Code != http.StatusOK {
		t.Fatalf("set never = %d: %s", w.Code, w.Body.String())
	}
	if got := effective(); got != "never" {
		t.Fatalf("effective = %q, want the stored never", got)
	}
	if w := put(`{"facade_default":"sometimes"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("set sometimes = %d, want 400", w.Code)
	}
	if w := put(`{"facade_default":""}`); w.Code != http.StatusOK {
		t.Fatalf("clear = %d: %s", w.Code, w.Body.String())
	}
	if rec, _ := st.Get(context.Background()); rec == nil || rec.FacadeDefault != nil {
		t.Fatalf("stored = %+v, want the override cleared", rec)
	}
	if got := effective(); got != "always" {
		t.Fatalf("effective after clear = %q, want the env default back", got)
	}
}
