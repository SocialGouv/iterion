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
	"time"

	"github.com/SocialGouv/iterion/pkg/audit"
	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/budgetfloor"
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

// The budget floor is the one family whose read-modify-write belongs to the
// CLIENT (the CLI reads the whole policy, edits one entry, PUTs it back), so
// the document a second admin sends carries every OTHER reservation as they
// last read it. An unconditional ReplaceOne there does not merge — it DELETES
// the reservation the first admin just wrote, silently, and a capacity
// reservation vanishing unnoticed is the failure the whole family exists to
// prevent.
// The default axis is the one that can hold nothing: capPolicyFor lowers a
// window only where usagecap already enforces one, so on a deployment that set
// no cap — or whose kill switch disarmed it — a window reserve is stored and
// inert. Correct (a floor may not invent a ceiling) and, until this, silent:
// the operator's whole reason for reserving is that "nothing was held" is hard
// to notice.
func TestAdminBudgetFloor_WarnsWhenTheWindowItReservesIsNotCapped(t *testing.T) {
	get := func(t *testing.T, s *Server) []string {
		t.Helper()
		admin := auth.WithIdentity(context.Background(), auth.Identity{UserID: "root", IsSuperAdmin: true})
		r := httptest.NewRequest("GET", "/api/admin/settings/budget-floor", nil).WithContext(admin)
		w := httptest.NewRecorder()
		s.handleAdminGetBudgetFloor(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("GET = %d: %s", w.Code, w.Body.String())
		}
		var body struct {
			Warnings []string `json:"warnings"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return body.Warnings
	}
	newSrv := func(t *testing.T, res []budgetfloor.Reservation) *Server {
		t.Helper()
		st := platformcfg.NewMemoryStore[budgetfloor.Policy]()
		if len(res) > 0 {
			if err := st.Put(context.Background(), budgetfloor.Policy{Reservations: res}); err != nil {
				t.Fatal(err)
			}
		}
		return New(Config{SkipProjectRegistration: true, BudgetFloorSettings: st},
			iterlog.New(iterlog.LevelError, nil))
	}
	fiveHourReserve := []budgetfloor.Reservation{
		{BotID: "review-pr", Reserve: budgetfloor.Reserve{FiveHourPercent: 20}},
	}

	t.Run("no cap configured: the reserve is named as holding nothing", func(t *testing.T) {
		t.Setenv("ITERION_USAGE_CAP_5H_PCT", "")
		t.Setenv("ITERION_USAGE_CAP_WEEK_PCT", "")
		warns := get(t, newSrv(t, fiveHourReserve))
		if len(warns) != 1 || !strings.Contains(warns[0], "five_hour") {
			t.Fatalf("warnings = %v, want one naming the five_hour window", warns)
		}
		if !strings.Contains(warns[0], "ITERION_USAGE_CAP") {
			t.Errorf("warning = %q, want it to name the way out", warns[0])
		}
	})

	t.Run("the kill switch counts as no cap", func(t *testing.T) {
		// A percentage with mode `off` is a guard the operator DISARMED, and
		// a reserve may not re-arm it — so it holds nothing and says so.
		t.Setenv("ITERION_USAGE_CAP_5H_PCT", "80")
		t.Setenv("ITERION_USAGE_CAP", "off")
		if warns := get(t, newSrv(t, fiveHourReserve)); len(warns) != 1 {
			t.Fatalf("warnings = %v, want one — the kill switch left the reserve inert", warns)
		}
	})

	t.Run("a cap that is enforced warns about nothing", func(t *testing.T) {
		t.Setenv("ITERION_USAGE_CAP_5H_PCT", "80")
		if warns := get(t, newSrv(t, fiveHourReserve)); len(warns) != 0 {
			t.Fatalf("warnings = %v on an enforced cap, want none", warns)
		}
	})

	t.Run("only the reserved window is reported", func(t *testing.T) {
		// A deployment capping neither window still hears about only the one
		// an operator actually reserved — a warning about a window nobody
		// named is noise that teaches people to skip the field.
		t.Setenv("ITERION_USAGE_CAP_5H_PCT", "")
		t.Setenv("ITERION_USAGE_CAP_WEEK_PCT", "")
		warns := get(t, newSrv(t, fiveHourReserve))
		for _, w := range warns {
			if strings.Contains(w, "week") {
				t.Fatalf("warned about the week window, which nothing reserves: %v", warns)
			}
		}
		// And a policy reserving nothing on a window says nothing at all.
		if warns := get(t, newSrv(t, []budgetfloor.Reservation{
			{BotID: "review-pr", Reserve: budgetfloor.Reserve{ConcurrentRuns: 2}},
		})); len(warns) != 0 {
			t.Fatalf("warnings = %v for a slot-only reserve, want none — it is enforced elsewhere", warns)
		}
	})

	// The opposite misconfiguration, and the worse one: the reserves take the
	// deployment's whole cap. Unreserved work then gets no lowered ceiling but
	// an outright refusal of the credential (the heldOut path), which is a
	// fleet-wide outage for every bot the policy does not name.
	//
	// Policy.Validate passes it — it knows the 100% window, never THIS
	// deployment's cap — so nothing between the operator and production said
	// so, while the milder "holds nothing" case above was already reported.
	t.Run("reserves that swallow the whole cap are named too", func(t *testing.T) {
		t.Setenv("ITERION_USAGE_CAP_5H_PCT", "50")
		warns := get(t, newSrv(t, []budgetfloor.Reservation{
			{BotID: "review-pr", Reserve: budgetfloor.Reserve{FiveHourPercent: 30}},
			{BotID: "feature-dev", Reserve: budgetfloor.Reserve{FiveHourPercent: 25}},
		}))
		if len(warns) != 1 {
			t.Fatalf("warnings = %v, want one — 55%% reserved of a 50%% cap leaves unreserved work nothing", warns)
		}
		for _, want := range []string{"five_hour", "55%", "50%", "refused"} {
			if !strings.Contains(warns[0], want) {
				t.Errorf("warning = %q, want it to contain %q", warns[0], want)
			}
		}
		// It must not double up with the inert warning: they are the two ends
		// of one axis, and an operator hearing both would trust neither.
		if strings.Contains(warns[0], "holds nothing") {
			t.Errorf("warning = %q says both that nothing is held and that everything is", warns[0])
		}
	})

	t.Run("a reserve exactly at the cap is swallowed, one below is not", func(t *testing.T) {
		// The boundary WindowCeiling draws: `reserved >= capPct` is heldOut,
		// so equality is the refusing side. A warning drawing it one point
		// off would clear precisely the policy that refuses everything.
		t.Setenv("ITERION_USAGE_CAP_5H_PCT", "50")
		at := get(t, newSrv(t, []budgetfloor.Reservation{
			{BotID: "review-pr", Reserve: budgetfloor.Reserve{FiveHourPercent: 50}},
		}))
		if len(at) != 1 {
			t.Errorf("warnings = %v at reserve == cap, want one: WindowCeiling holds it out at equality", at)
		}
		below := get(t, newSrv(t, []budgetfloor.Reservation{
			{BotID: "review-pr", Reserve: budgetfloor.Reserve{FiveHourPercent: 49}},
		}))
		if len(below) != 0 {
			t.Errorf("warnings = %v one point below the cap, want none — 1%% is a thin band, not an absent one", below)
		}
	})
}

func TestAdminBudgetFloor_ConcurrentPutIsA409NotALostReservation(t *testing.T) {
	st := platformcfg.NewMemoryStore[budgetfloor.Policy]()
	auditStore := audit.NewMemoryStore()
	s := New(Config{SkipProjectRegistration: true, BudgetFloorSettings: st, Audit: auditStore},
		iterlog.New(iterlog.LevelError, nil))
	admin := auth.WithIdentity(context.Background(), auth.Identity{UserID: "root", IsSuperAdmin: true})

	put := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("PUT", "/api/admin/settings/budget-floor", strings.NewReader(body)).WithContext(admin)
		w := httptest.NewRecorder()
		s.handleAdminPutBudgetFloor(w, r)
		return w
	}
	// The first write of a deployment carries no token and needs none.
	if w := put(`{"reservations":[{"bot_id":"review-pr","reserve":{"five_hour_percent":20}}]}`); w.Code != http.StatusOK {
		t.Fatalf("seed = %d: %s", w.Code, w.Body.String())
	}
	rec, _ := st.Get(context.Background())
	tokenBothAdminsRead := rec.UpdatedAt.UTC().Format(time.RFC3339Nano)

	// Admin B lands first, adding a reservation of their own.
	if w := put(`{"updated_at":"` + tokenBothAdminsRead + `","reservations":[` +
		`{"bot_id":"review-pr","reserve":{"five_hour_percent":20}},` +
		`{"bot_id":"feature-dev","reserve":{"five_hour_percent":10}}]}`); w.Code != http.StatusOK {
		t.Fatalf("admin B = %d: %s", w.Code, w.Body.String())
	}
	// Admin A now writes the document they read BEFORE B — which no longer
	// mentions feature-dev at all.
	w := put(`{"updated_at":"` + tokenBothAdminsRead + `","reservations":[` +
		`{"bot_id":"review-pr","reserve":{"five_hour_percent":30}}]}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("stale write = %d, want 409: %s", w.Code, w.Body.String())
	}
	after, _ := st.Get(context.Background())
	if _, ok := after.Reserved("feature-dev"); !ok {
		t.Fatalf("the concurrent admin's reservation was dropped: %+v", after.Reservations)
	}

	// And the write is audited like every other platform-settings mutation:
	// a super-admin redistributing capacity across the deployment must leave
	// a record of who reserved what.
	deadline := time.Now().Add(2 * time.Second)
	for {
		events, err := auditStore.ListPlatform(context.Background(), audit.Page{Limit: 10})
		if err != nil {
			t.Fatalf("audit list: %v", err)
		}
		if len(events) > 0 {
			e := events[0]
			if e.Action != "platform.settings.budget_floor.updated" {
				t.Fatalf("action = %q", e.Action)
			}
			if e.ActorID != "root" || e.ActorKind != "super_admin" {
				t.Fatalf("actor = %q/%q", e.ActorID, e.ActorKind)
			}
			if e.TargetID != platformcfg.FamilyBudgetFloor {
				t.Fatalf("target id = %q, want the family", e.TargetID)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no audit row for a super-admin capacity write")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
