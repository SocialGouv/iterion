package server

import (
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
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The pipelines control center is the FIFTH launch surface of the #871
// class: its board is selected from the request's active team
// (cloudBoardResolve), so the bot a card names must resolve through the
// same team → platform → baked order every other surface uses. Resolving
// it tenant-free serves a team the ORIGIN of the bot it forked, and makes
// a bot only that team authored impossible to card at all.

// bakeProbeBundle writes the baked catalog's `probe` as a real bundle
// directory (manifest + main.bot) and returns the discovery root. A bundle
// and not a loose .bot: a cloud launch freezes the bot's collection before
// compiling, and that snapshot is rooted at the bundle's main.bot.
func bakeProbeBundle(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "probe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("name: probe\nversion: 1.0.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte(tierBakedBot), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

// pipelineTierEnv is a cloud-shaped pipelines control center for team t1:
// its own board behind CloudBoardFor, a catalog baked with `probe`, and a
// bot-source store the test seeds rows into.
type pipelineTierEnv struct {
	srv   *Server
	board *native.Store
	pub   *tierPublisher
}

func newPipelineTierEnv(t *testing.T) *pipelineTierEnv {
	t.Helper()
	s := newOrgTestServer(t)
	s.cfg.Mode = "cloud"
	seedGate(t, s, gateSpec{id: "t1"})

	s.cfg.Bots.Paths = []string{bakeProbeBundle(t)}
	s.botSources = botsource.NewMemoryStore()

	board, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = board.Close() })
	s.cfg.CloudBoardFor = func(teamID string) native.BoardStore {
		if teamID == "t1" {
			return board
		}
		return nil
	}

	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.Store = rs
	pub := &tierPublisher{}
	s.runs = newTestRunviewService(t, "", runview.WithStore(rs), runview.WithLaunchPublisher(pub))
	return &pipelineTierEnv{srv: s, board: board, pub: pub}
}

// seedRow stores one bot bundle for a tenant.
func (e *pipelineTierEnv) seedRow(t *testing.T, tenant, slug, body string) {
	t.Helper()
	if _, err := e.srv.botSources.Create(store.WithTenant(context.Background(), tenant), botsource.BotSource{
		TenantID: tenant, Slug: slug,
		Files: map[string]string{botsource.MainBotFile: body},
	}); err != nil {
		t.Fatalf("seed %s/%s: %v", tenant, slug, err)
	}
}

// seedRowWithManifest is seedRow for a row that must carry catalog
// metadata — the `enabled:` flag being the only way to produce a
// (found=true, Enabled=false) resolution, which is the disabled-bot path.
func (e *pipelineTierEnv) seedRowWithManifest(t *testing.T, tenant, slug, body, manifest string) {
	t.Helper()
	if _, err := e.srv.botSources.Create(store.WithTenant(context.Background(), tenant), botsource.BotSource{
		TenantID: tenant, Slug: slug,
		Files: map[string]string{botsource.MainBotFile: body, "manifest.yaml": manifest},
	}); err != nil {
		t.Fatalf("seed %s/%s: %v", tenant, slug, err)
	}
}

// countBundleTempDirs counts the materializations a bot resolution leaves
// under the test's private TMPDIR: `storedLaunchBot`'s own dir, and the
// frozen collection `snapshotLaunchBot` writes in cloud mode. Both are
// owned by the resolving call and must be gone when it returns.
func countBundleTempDirs(t *testing.T, tmp string) int {
	t.Helper()
	ents, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatalf("read TMPDIR %s: %v", tmp, err)
	}
	n := 0
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "iterion-launch-bot-") || strings.HasPrefix(e.Name(), "iterion-bundle-snapshot-") {
			n++
		}
	}
	return n
}

// req builds a request authenticated as a member of t1 — the identity the
// board itself is resolved from.
func (e *pipelineTierEnv) req(method, path, body string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	return r.WithContext(auth.WithIdentity(r.Context(), auth.Identity{UserID: "u1", TeamID: "t1", OrgID: "t1"}))
}

// createCard posts a task and returns the created card.
func (e *pipelineTierEnv) createCard(t *testing.T, body string) native.Issue {
	t.Helper()
	w := httptest.NewRecorder()
	e.srv.handlePipelineBoardTaskCreate(w, e.req(http.MethodPost, "/api/v1/pipeline-board/tasks", body))
	if w.Code != http.StatusCreated {
		t.Fatalf("create card = %d %s, want 201", w.Code, w.Body.String())
	}
	var card native.Issue
	if err := json.Unmarshal(w.Body.Bytes(), &card); err != nil {
		t.Fatalf("decode card: %v", err)
	}
	return card
}

// launchCard drives the operator's explicit "launch now".
func (e *pipelineTierEnv) launchCard(t *testing.T, id string) {
	t.Helper()
	r := e.req(http.MethodPost, "/api/v1/pipeline-board/tasks/"+id+"/launch", "")
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	e.srv.handlePipelineBoardTaskLaunch(w, r)
	if w.Code != http.StatusAccepted {
		t.Fatalf("launch card = %d %s, want 202", w.Code, w.Body.String())
	}
}

// Half 1 — a team that forked a catalog bot must run ITS fork from the
// pipelines board. The card is the team's (the board resolves per team);
// serving it the catalog bundle is a silent substitution.
func TestPipelineBoardLaunchServesTheTeamsFork(t *testing.T) {
	env := newPipelineTierEnv(t)
	env.seedRow(t, "t1", "probe", tierForkBot)

	card := env.createCard(t, `{"bot":"probe","title":"fork me"}`)
	env.launchCard(t, card.ID)

	assertServedByTheFork(t, "pipelines control center", env.pub.only(t))
}

// Half 2 — a bot ONLY the team authored has no filesystem path at all
// (materializeBotEntries blanks it), so a lane that cards and launches by
// path cannot serve it: the card is refused at create, and there is no
// second way in.
func TestPipelineBoardCardsAndLaunchesATeamAuthoredBot(t *testing.T) {
	env := newPipelineTierEnv(t)
	env.seedRow(t, "t1", "teamonly", tierForkBot)

	card := env.createCard(t, `{"bot":"teamonly","title":"authored here"}`)
	if card.Bot != "teamonly" {
		t.Fatalf("card bot = %q, want teamonly", card.Bot)
	}
	env.launchCard(t, card.ID)

	spec := env.pub.only(t)
	if !strings.Contains(spec.Source, "TEAMFORK") {
		t.Errorf("the launch ran a bundle that is not the team's own (source: %q)", spec.Source)
	}
	if spec.BotSourceTier != store.BotSourceTierTeam {
		t.Errorf("bot_source_tier = %q, want %q", spec.BotSourceTier, store.BotSourceTierTeam)
	}
	if spec.BotBundle == nil || spec.BotBundle.TenantID != "t1" || spec.BotBundle.Slug != "teamonly" {
		t.Errorf("bundle ref = %+v, want the team's own row so the runner rebuilds it", spec.BotBundle)
	}
}

// Paying for a resolution means owning what it materialized. A DISABLED bot
// is the path where the two diverge: the resolution succeeded — a temp
// bundle dir on disk, and in cloud a second one holding the frozen
// collection — and the caller then refuses the launch. Every operator click
// of "launch now" on such a card leaves one full bundle tree under the
// server pod's TMPDIR, and nothing ever comes back for it.
func TestPipelineBoardDisabledBotLeavesNoBundleBehind(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	env := newPipelineTierEnv(t)
	env.seedRowWithManifest(t, "t1", "parked", tierForkBot, "name: parked\nversion: 1.0.0\nenabled: false\n")

	// Creation admits the card (only `start: true` is refused on a disabled
	// bot) and already reclaims its own materialization.
	card := env.createCard(t, `{"bot":"parked","title":"disabled bot"}`)
	if n := countBundleTempDirs(t, tmp); n != 0 {
		t.Fatalf("card creation left %d materialized bundle dir(s) behind", n)
	}

	r := env.req(http.MethodPost, "/api/v1/pipeline-board/tasks/"+card.ID+"/launch", "")
	r.SetPathValue("id", card.ID)
	w := httptest.NewRecorder()
	env.srv.handlePipelineBoardTaskLaunch(w, r)
	if w.Code != http.StatusConflict {
		t.Fatalf("launch of a card bound to a disabled bot = %d %s, want 409", w.Code, w.Body.String())
	}
	if n := countBundleTempDirs(t, tmp); n != 0 {
		t.Errorf("the refused launch left %d materialized bundle dir(s) behind — one per operator click, with no reaper", n)
	}
}

// Half 2b — the update handler's admission check must reach the same tier
// as the launch: a bot only the team authored is a legal re-binding. It is
// also the THIRD site paying for a materialization it does not run, so it
// carries the same ownership assertion as create and launch: three sites,
// one contract, and the ordering slip that shipped at one of them is only
// caught if every one is checked.
func TestPipelineBoardUpdateAcceptsATeamAuthoredBot(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	env := newPipelineTierEnv(t)
	env.seedRow(t, "t1", "teamonly", tierForkBot)

	card := env.createCard(t, `{"bot":"probe","title":"rebind me"}`)
	r := env.req(http.MethodPatch, "/api/v1/pipeline-board/tasks/"+card.ID, `{"bot":"teamonly"}`)
	r.SetPathValue("id", card.ID)
	w := httptest.NewRecorder()
	env.srv.handlePipelineBoardTaskUpdate(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("update bot = %d %s, want 200 — the team's own bot is not in the catalog and must still bind", w.Code, w.Body.String())
	}
	got, err := env.board.Get(card.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Bot != "teamonly" {
		t.Fatalf("card bot = %q, want teamonly", got.Bot)
	}
	if n := countBundleTempDirs(t, tmp); n != 0 {
		t.Errorf("the re-binding check left %d materialized bundle dir(s) behind — it resolves a bundle only to answer whether the bot exists", n)
	}
}

// The MIDDLE tier the lane never reached either: a stored platform row has
// no filesystem path, so before the tiered resolution a card bound to a
// deployment-wide override compiled from an empty FilePath. This is the
// same class as the team fork, one tier down.
func TestPipelineBoardServesAPlatformOverride(t *testing.T) {
	env := newPipelineTierEnv(t)
	env.seedRow(t, botsource.PlatformTenantID, "probe", strings.Replace(tierBakedBot, "BAKED", "PLATFORM", 1))

	card := env.createCard(t, `{"bot":"probe","title":"override me"}`)
	env.launchCard(t, card.ID)

	spec := env.pub.only(t)
	if !strings.Contains(spec.Source, "PLATFORM") {
		t.Errorf("the launch ran a bundle that is not the deployment's override (source: %q)", spec.Source)
	}
	if spec.BotSourceTier != store.BotSourceTierPlatform {
		t.Errorf("bot_source_tier = %q, want %q", spec.BotSourceTier, store.BotSourceTierPlatform)
	}
}

// The metadata half must describe the artifact the LAUNCH selected. The
// platform tier is where the two can be read through different mechanisms:
// the launch reads the store live, while the catalog overlay is served from
// a 30s cache invalidated only on the replica that wrote. Both cases below
// warm that cache BEFORE the push, which is every other replica's state for
// the window after `iterion remote admin bots push`.
func TestPipelineBoardPlatformMetadataDescribesTheBundleThatRuns(t *testing.T) {
	t.Run("a freshly pushed override is not a launchable bundle nothing describes", func(t *testing.T) {
		env := newPipelineTierEnv(t)
		// Warm the overlay while the platform tenant holds nothing.
		if _, found, err := env.srv.effectiveFindByNameForTeam(context.Background(), "t1", "newbot"); err != nil || found {
			t.Fatalf("precondition: found=%v err=%v, want a cold miss", found, err)
		}
		env.seedRow(t, botsource.PlatformTenantID, "newbot", strings.Replace(tierBakedBot, "BAKED", "PLATFORM", 1))

		// Launchable on the live store; a stale overlay makes it uncardable.
		card := env.createCard(t, `{"bot":"newbot","title":"pushed just now"}`)
		env.launchCard(t, card.ID)
		if spec := env.pub.only(t); !strings.Contains(spec.Source, "PLATFORM") {
			t.Errorf("the launch ran %q, want the override that was just pushed", spec.Source)
		}
	})

	t.Run("an override's own enabled flag wins over the baked twin it shadows", func(t *testing.T) {
		env := newPipelineTierEnv(t)
		// The card is created (and the overlay warmed) against the ENABLED
		// baked `probe`; the override lands after.
		card := env.createCard(t, `{"bot":"probe","title":"shadowed"}`)
		env.seedRowWithManifest(t, botsource.PlatformTenantID, "probe",
			strings.Replace(tierBakedBot, "BAKED", "PLATFORM", 1),
			"name: probe\nversion: 2.0.0\nenabled: false\n")

		r := env.req(http.MethodPost, "/api/v1/pipeline-board/tasks/"+card.ID+"/launch", "")
		r.SetPathValue("id", card.ID)
		w := httptest.NewRecorder()
		env.srv.handlePipelineBoardTaskLaunch(w, r)
		if w.Code != http.StatusConflict {
			t.Fatalf("launch = %d %s, want 409 — the bundle that would run is the DISABLED override, "+
				"and reading `enabled` off the baked twin it shadows launches it anyway", w.Code, w.Body.String())
		}
	})
}

// A catalog that will not PARSE is not a catalog without this bot in it.
// The baked tier reports both the same way — resolveBotTieredRaw turns any
// ResolveBotPath failure into "nothing resolved", and one malformed
// manifest.yaml under the discovery roots fails the walk for every bot — so
// the lane has to ask before it answers "not found". Before this lane went
// through the tiers, findBot propagated that error and the operator was told
// what to fix.
func TestPipelineBoardSaysWhenTheCatalogItselfWillNotParse(t *testing.T) {
	env := newPipelineTierEnv(t)
	// A second bundle, alongside the healthy `probe`, whose manifest is not
	// YAML. Discovery walks the whole root, so `probe` stops resolving too.
	broken := filepath.Join(env.srv.cfg.Bots.Paths[0], "broken")
	if err := os.MkdirAll(broken, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, "manifest.yaml"), []byte("name: broken\nversion: [1.0.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, "main.bot"), []byte(tierBakedBot), 0o600); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	env.srv.handlePipelineBoardTaskCreate(w, env.req(http.MethodPost, "/api/v1/pipeline-board/tasks", `{"bot":"probe","title":"catalog is broken"}`))
	if w.Code == http.StatusNotFound {
		t.Fatalf("create = 404 %s — the catalog could not be READ, and the operator is being sent to look for a bot that is there", w.Body.String())
	}
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("create = %d %s, want 500 carrying the parse failure", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, "manifest") {
		t.Errorf("refusal = %s, want the manifest parse failure that actually blocked the resolution", body)
	}
}

// blipStore fails the Nth GetBySlug of one tenant and serves every other
// read normally — the transient store failure that lands BETWEEN the two
// reads a single resolution makes.
type blipStore struct {
	botsource.Store
	tenant string
	failOn int // 1-based index of the GetBySlug call to fail
	seen   int
}

func (b *blipStore) GetBySlug(ctx context.Context, tenantID, slug string) (botsource.BotSource, error) {
	if tenantID == b.tenant {
		b.seen++
		if b.seen == b.failOn {
			return botsource.BotSource{}, errors.New("mongo: connection reset by peer")
		}
	}
	return b.Store.GetBySlug(ctx, tenantID, slug)
}

// A row whose OWN metadata cannot be read is the other way the two halves
// come apart: the launch resolves the fork (a non-empty main.bot is all it
// asks for), while the metadata read falls THROUGH to the tier below and
// lands on the ORIGIN this fork replaces. Pairing the fork's bundle with the
// origin's `enabled` and canonical name is the substitution the chokepoint
// exists to refuse — and refusing it explicitly is the same contract the
// launch half already keeps, where only ErrNotFound may fall through.
func TestPipelineBoardRefusesAForkWhoseOwnMetadataIsUnreadable(t *testing.T) {
	// Both cases card the slug of an ENABLED baked `probe` — the entry a
	// fall-through hands back, and the reason silence here is not neutral.
	t.Run("a store blip between the two reads", func(t *testing.T) {
		env := newPipelineTierEnv(t)
		env.seedRow(t, "t1", "probe", tierForkBot)
		// The launch half reads the row (call 1) and materializes the fork;
		// the metadata read (call 2) is the one that blips.
		env.srv.botSources = &blipStore{Store: env.srv.botSources, tenant: "t1", failOn: 2}

		assertUnreadableMetadataRefusal(t, env)
	})

	t.Run("a bundle whose metadata does not materialize", func(t *testing.T) {
		env := newPipelineTierEnv(t)
		// No manifest.yaml and two workflows: discovery cannot say which one
		// the row IS, so it describes the row with neither.
		if _, err := env.srv.botSources.Create(store.WithTenant(context.Background(), "t1"), botsource.BotSource{
			TenantID: "t1", Slug: "probe",
			Files: map[string]string{botsource.MainBotFile: tierForkBot, "child.bot": tierForkBot},
		}); err != nil {
			t.Fatalf("seed t1/probe: %v", err)
		}

		assertUnreadableMetadataRefusal(t, env)
	})
}

// assertUnreadableMetadataRefusal drives a card create for `probe` and
// requires an explicit refusal naming the row — never a card admitted on the
// baked twin's metadata, and never a launch of the fork behind it.
func assertUnreadableMetadataRefusal(t *testing.T, env *pipelineTierEnv) {
	t.Helper()
	w := httptest.NewRecorder()
	env.srv.handlePipelineBoardTaskCreate(w, env.req(http.MethodPost, "/api/v1/pipeline-board/tasks", `{"bot":"probe","title":"unreadable fork"}`))
	if w.Code == http.StatusCreated {
		t.Fatalf("the card was created (%d) — the team's fork was admitted on the ORIGIN's metadata, "+
			"which is not the bundle that would run", w.Code)
	}
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("create = %d %s, want 500 naming the row whose metadata cannot be read", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, "t1/probe") {
		t.Errorf("refusal = %s, want the tenant/slug of the row the operator has to fix", body)
	}
}

// The third leg: a platform override DELETED inside the overlay's 30s
// window. The live store no longer has it, so the launch resolves the BAKED
// bundle — while the cached overlay still describes the override on every
// replica that did not serve the delete. Reading `enabled` there refuses a
// card whose bundle is enabled and would run.
func TestPipelineBoardIgnoresADeletedOverrideStillInTheOverlay(t *testing.T) {
	env := newPipelineTierEnv(t)
	created, err := env.srv.botSources.Create(store.WithTenant(context.Background(), botsource.PlatformTenantID), botsource.BotSource{
		TenantID: botsource.PlatformTenantID, Slug: "probe",
		Files: map[string]string{
			botsource.MainBotFile: strings.Replace(tierBakedBot, "BAKED", "PLATFORM", 1),
			"manifest.yaml":       "name: probe\nversion: 2.0.0\nenabled: false\n",
		},
	})
	if err != nil {
		t.Fatalf("seed the override: %v", err)
	}
	// Warm the overlay WITH the override present, then delete it: this is
	// the state of every replica that did not serve the delete.
	if _, _, err := env.srv.effectiveFindByNameForTeam(context.Background(), "t1", "probe"); err != nil {
		t.Fatalf("warm the overlay: %v", err)
	}
	if err := env.srv.botSources.Delete(store.WithTenant(context.Background(), botsource.PlatformTenantID), created.ID); err != nil {
		t.Fatalf("delete the override: %v", err)
	}

	// The baked `probe` is enabled, and it is what the launch now resolves.
	card := env.createCard(t, `{"bot":"probe","title":"override withdrawn"}`)
	r := env.req(http.MethodPost, "/api/v1/pipeline-board/tasks/"+card.ID+"/launch", "")
	r.SetPathValue("id", card.ID)
	w := httptest.NewRecorder()
	env.srv.handlePipelineBoardTaskLaunch(w, r)
	if w.Code != http.StatusAccepted {
		t.Fatalf("launch = %d %s, want 202 — the bundle that runs is the ENABLED baked `probe`, "+
			"and `enabled` was read off an override the store no longer has", w.Code, w.Body.String())
	}
	if spec := env.pub.only(t); !strings.Contains(spec.Source, "BAKED") {
		t.Errorf("the launch ran %q, want the baked bundle the deleted override no longer shadows", spec.Source)
	}
}

// Half 3 — the admission LOOP is local-only (pipelineAdmissionEnabled
// refuses cloud), so its board carries no tenant: it must keep resolving
// platform-over-baked with an empty team, and must never be handed some
// team's row.
func TestPipelineAdmissionLoopResolvesWithNoTeam(t *testing.T) {
	s := newOrgTestServer(t)
	seedGate(t, s, gateSpec{id: "t1"})
	botsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(botsDir, "probe.bot"), []byte(tierBakedBot), 0o600); err != nil {
		t.Fatal(err)
	}
	s.cfg.Bots.Paths = []string{botsDir}
	// A team row exists for the same slug — the loop has no team, so it
	// must not be reachable from here.
	s.botSources = botsource.NewMemoryStore()
	if _, err := s.botSources.Create(store.WithTenant(context.Background(), "t1"), botsource.BotSource{
		TenantID: "t1", Slug: "probe",
		Files: map[string]string{botsource.MainBotFile: tierForkBot},
	}); err != nil {
		t.Fatal(err)
	}

	board, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = board.Close() })
	s.cfg.NativeTrackerStore = board
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.Store = rs
	pub := &tierPublisher{}
	s.runs = newTestRunviewService(t, "", runview.WithStore(rs), runview.WithLaunchPublisher(pub))

	if !s.pipelineAdmissionEnabled() {
		t.Fatal("precondition: the local admission loop must be enabled here")
	}
	iss, err := board.Create(native.Issue{Title: "local ticket", State: native.StateReady, Bot: "probe"})
	if err != nil {
		t.Fatal(err)
	}
	s.admitReadyPipelines()

	spec := pub.only(t)
	if !strings.Contains(spec.Source, "BAKED") {
		t.Errorf("the loop launched %q — with no team it must serve the baked catalog, never a team's row", spec.Source)
	}
	if spec.BotSourceTier != store.BotSourceTierBaked {
		t.Errorf("bot_source_tier = %q, want %q", spec.BotSourceTier, store.BotSourceTierBaked)
	}
	got, err := board.Get(iss.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != native.StateInProgress {
		t.Fatalf("ticket state = %q, want in_progress — the loop did not launch it", got.State)
	}
}
