package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/botsource"
	cloudmetrics "github.com/SocialGouv/iterion/pkg/cloud/metrics"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The gauges are read through the registry's own EXPOSITION, never off the
// in-process metric: GaugeVec.WithLabelValues CREATES a missing series at 0,
// so an in-process read cannot tell "this pass published 0" from "no pass ever
// published anything" — which is exactly the difference the per-pass Reset
// makes, and exactly what the alert's `> 0` reads.
func scrapedGauges(t *testing.T, reg *cloudmetrics.Registry) map[string]float64 {
	t.Helper()
	rec := httptest.NewRecorder()
	reg.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics = %d", rec.Code)
	}
	body, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatalf("read /metrics: %v", err)
	}
	out := map[string]float64{}
	for _, line := range strings.Split(string(body), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		v, perr := strconv.ParseFloat(fields[1], 64)
		if perr != nil {
			continue
		}
		out[fields[0]] = v
	}
	return out
}

const (
	shadowsNewerSeries  = `iterion_bot_override_shadows_newer{slug="reviewer"}`
	unorderableSeries   = `iterion_bot_override_unorderable_version{slug="reviewer"}`
	shadowUnavailSeries = `iterion_bot_shadow_check_unavailable`
)

// assertPublishedZero fails unless the series is PRESENT and reads 0 — the
// invariant the pass states ("a row that stopped shadowing must say 0 rather
// than go quiet"), which `got[series] != 0` cannot hold: a Go map yields 0 for
// a key that is not there, so a vector that published NOTHING satisfies it.
// That is the same confusion on the assertion side that these gauges exist to
// end on the exposition side.
func assertPublishedZero(t *testing.T, got map[string]float64, series, why string) {
	t.Helper()
	v, present := got[series]
	switch {
	case !present:
		t.Errorf("%s: %s went quiet instead of publishing an explicit 0 — an absent series is what an unscraped one looks like", why, series)
	case v != 0:
		t.Errorf("%s: %s must read 0; got %v", why, series, v)
	}
}

// assertNoSeries is the other half: a pass that published nothing must leave
// no series at all, which `!= 0` also passes on.
func assertNoSeries(t *testing.T, got map[string]float64, series, why string) {
	t.Helper()
	if v, present := got[series]; present {
		t.Errorf("%s: %s is published (%v) by a pass that must have written nothing", why, series, v)
	}
}

// newShadowObserverServer wires a server whose PLATFORM tier holds one row for
// "reviewer" at storedVersion, against a baked bundle at bakedVersion.
func newShadowObserverServer(t *testing.T, bakedVersion, storedVersion string) *Server {
	t.Helper()
	s, _, _ := newBotSourceTestServer(t)
	s.cfg.Metrics = cloudmetrics.New()
	seedBakedBot(t, s, "reviewer", bakedVersion)
	s.bakedCatalog = s.newBakedCatalogResolver()
	putPlatformOverride(t, s, "reviewer", storedVersion)
	return s
}

func putPlatformOverride(t *testing.T, s *Server, slug, version string) {
	t.Helper()
	pctx := store.WithTenant(context.Background(), botsource.PlatformTenantID)
	files := map[string]string{
		botsource.MainBotFile: testBotMain,
		"manifest.yaml":       "name: " + slug + "\nversion: " + version + "\n",
	}
	existing, err := s.botSources.GetBySlug(pctx, botsource.PlatformTenantID, slug)
	if errors.Is(err, botsource.ErrNotFound) {
		if _, cerr := s.botSources.Create(pctx, botsource.BotSource{
			TenantID: botsource.PlatformTenantID,
			Slug:     slug,
			Files:    files,
		}); cerr != nil {
			t.Fatalf("create platform override: %v", cerr)
		}
		return
	}
	if err != nil {
		t.Fatalf("read platform override: %v", err)
	}
	existing.Files = files
	if _, uerr := s.botSources.Update(pctx, existing); uerr != nil {
		t.Fatalf("update platform override: %v", uerr)
	}
}

// The signal itself: a platform override OLDER than the baked bundle must be
// readable without a human calling the inventory and without a launch — the
// two reporters that came before are both passive, so a bot nobody launches
// rots unseen (#2354). And once the override is re-pushed current, the same
// series must say so.
func TestOverrideShadowObserver_OlderOverrideTripsTheGauge(t *testing.T) {
	s := newShadowObserverServer(t, "0.8.0", "0.7.0")
	ctx := context.Background()

	s.observePlatformOverrideShadows(ctx)
	got := scrapedGauges(t, s.cfg.Metrics)
	if got[shadowsNewerSeries] != 1 {
		t.Errorf("a 0.7.0 override over a 0.8.0 bake must read 1; got %v", got[shadowsNewerSeries])
	}
	assertPublishedZero(t, got, unorderableSeries, "an orderable pair is not the unorderable class")
	if got[shadowUnavailSeries] != 0 {
		t.Errorf("a pass that read both legs must clear the unavailable flag; got %v", got[shadowUnavailSeries])
	}

	// The remedy the alert names: re-push the bundle current. The series must
	// say 0 — going quiet is what an unscraped replica looks like, and the
	// operator who did what the alert asked gets no credit for it.
	putPlatformOverride(t, s, "reviewer", "0.9.0")
	s.observePlatformOverrideShadows(ctx)
	assertPublishedZero(t, scrapedGauges(t, s.cfg.Metrics), shadowsNewerSeries, "a NEWER override shadows nothing")

	// And the equal case, the one an operator reaches by re-pushing the very
	// bundle the image bakes.
	putPlatformOverride(t, s, "reviewer", "0.8.0")
	s.observePlatformOverrideShadows(ctx)
	assertPublishedZero(t, scrapedGauges(t, s.cfg.Metrics), shadowsNewerSeries, "an override EQUAL to the bake shadows nothing")
}

// The other remedy: drop the override and let the image serve. The series must
// DISAPPEAR, not keep its last 1 for the lifetime of the process — a gauge
// vector that is never reset pages forever on a row that no longer exists, and
// the operator who did exactly what the alert asked gets no credit for it.
func TestOverrideShadowObserver_ADroppedOverrideLosesItsSeries(t *testing.T) {
	s := newShadowObserverServer(t, "0.8.0", "0.7.0")
	ctx := context.Background()
	s.observePlatformOverrideShadows(ctx)
	if got := scrapedGauges(t, s.cfg.Metrics); got[shadowsNewerSeries] != 1 {
		t.Fatalf("premise: the shadow must be published first; got %v", got[shadowsNewerSeries])
	}

	pctx := store.WithTenant(ctx, botsource.PlatformTenantID)
	row, err := s.botSources.GetBySlug(pctx, botsource.PlatformTenantID, "reviewer")
	if err != nil {
		t.Fatalf("read the row to drop: %v", err)
	}
	if derr := s.botSources.Delete(pctx, row.ID, 0); derr != nil {
		t.Fatalf("drop the override: %v", derr)
	}

	s.observePlatformOverrideShadows(ctx)
	got := scrapedGauges(t, s.cfg.Metrics)
	if _, present := got[shadowsNewerSeries]; present {
		t.Errorf("the dropped override still carries a series (%v) — the vectors are not reset per pass", got[shadowsNewerSeries])
	}
	if _, present := got[unorderableSeries]; present {
		t.Errorf("the dropped override still carries an unorderable series (%v)", got[unorderableSeries])
	}
}

// A check that could not run must NEVER read as a clean inventory. The
// unavailable flag rises and the per-slug gauges HOLD: zeroing them would
// paint a calm at exactly the moment the check went blind, while the override
// that was shadowing still is.
func TestOverrideShadowObserver_AnUnreadableCatalogHoldsAndFlags(t *testing.T) {
	s := newShadowObserverServer(t, "0.8.0", "0.7.0")
	ctx := context.Background()
	s.observePlatformOverrideShadows(ctx)
	if got := scrapedGauges(t, s.cfg.Metrics); got[shadowsNewerSeries] != 1 {
		t.Fatalf("premise: the shadow must be published first; got %v", got[shadowsNewerSeries])
	}

	// The catalog goes blind from cold, through its REAL producer: a manifest
	// the loader refuses (schema_version claims the bundle for iterion, so
	// the parse failure is loud rather than skipped). versionsBelow then
	// answers unknown, the shape a wedged bot root produces in a pod.
	broken := t.TempDir()
	dir := filepath.Join(broken, "reviewer")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, botsource.MainBotFile), []byte(testBotMain), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("schema_version: 1\nname: [unterminated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.cfg.Bots.Paths = []string{broken}
	s.bakedCatalog = s.newBakedCatalogResolver()
	if _, ok := s.versionsBelow(botsource.PlatformTenantID); ok {
		t.Fatalf("premise: the catalog must be unreadable for this witness")
	}

	s.observePlatformOverrideShadows(ctx)
	got := scrapedGauges(t, s.cfg.Metrics)
	if got[shadowUnavailSeries] != 1 {
		t.Errorf("an unreadable catalog must raise iterion_bot_shadow_check_unavailable; got %v", got[shadowUnavailSeries])
	}
	if got[shadowsNewerSeries] != 1 {
		t.Errorf("the shadow gauge must HOLD its last value while the check is blind; got %v", got[shadowsNewerSeries])
	}
}

// A catalog with no root ON DISK is the same blindness one level up, and the
// one shape a deployment reaches by accident: botregistry skips a missing root
// silently (optimistic default paths), so a dropped ITERION_BOTS_PATH, a
// custom image or an overridden command leaves an EMPTY projection — which
// classifyShadow reads for every slug as "no other tier offers it", the
// byte-identical exposition of a clean deployment from a check that opened
// nothing.
func TestOverrideShadowObserver_ABotRootThatIsNotThereIsNotACleanCatalog(t *testing.T) {
	s, _, _ := newBotSourceTestServer(t)
	s.cfg.Metrics = cloudmetrics.New()
	s.cfg.Bots.Paths = []string{filepath.Join(t.TempDir(), "absent")}
	s.bakedCatalog = s.newBakedCatalogResolver()
	putPlatformOverride(t, s, "reviewer", "0.7.0")

	// The chokepoint all three readers cross — the listing's
	// shadow_check_unavailable is the negation of this very bool, and the
	// resolve-time Warn stays silent on it.
	if _, ok := s.versionsBelow(botsource.PlatformTenantID); ok {
		t.Errorf("a walk that read no root at all must answer unknown, never a current reading")
	}

	s.observePlatformOverrideShadows(context.Background())
	got := scrapedGauges(t, s.cfg.Metrics)
	if got[shadowUnavailSeries] != 1 {
		t.Errorf("a catalog with no root on disk must raise iterion_bot_shadow_check_unavailable; got %v", got[shadowUnavailSeries])
	}
	assertNoSeries(t, got, shadowsNewerSeries, "a blind pass certifies no row")
	assertNoSeries(t, got, unorderableSeries, "a blind pass certifies no row")
}

// The twin that keeps the guard above from being a guess at the whole class:
// a root that IS there and bakes nothing is a VERDICT ("nothing serves below
// this override"), not a blind check — and it is the escape hatch for a
// deployment that deliberately serves stored bundles only, which must not page
// on a condition it cannot clear.
func TestOverrideShadowObserver_AnEmptyBotRootIsAVerdictNotABlindCheck(t *testing.T) {
	s, _, _ := newBotSourceTestServer(t)
	s.cfg.Metrics = cloudmetrics.New()
	empty := filepath.Join(t.TempDir(), "bots")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	s.cfg.Bots.Paths = []string{empty}
	s.bakedCatalog = s.newBakedCatalogResolver()
	putPlatformOverride(t, s, "reviewer", "0.7.0")

	if _, ok := s.versionsBelow(botsource.PlatformTenantID); !ok {
		t.Fatalf("a root that exists and bakes nothing is a readable catalog")
	}
	s.observePlatformOverrideShadows(context.Background())
	got := scrapedGauges(t, s.cfg.Metrics)
	if got[shadowUnavailSeries] != 0 {
		t.Errorf("an empty catalog this replica DID read is a verdict, not an unavailable check; got %v", got[shadowUnavailSeries])
	}
	assertPublishedZero(t, got, shadowsNewerSeries, "nothing serves below an override when the catalog bakes nothing")
	assertPublishedZero(t, got, unorderableSeries, "nothing serves below an override when the catalog bakes nothing")
}

// failingPlatformList is a bot-source store whose listing fails. The other leg
// of the same contract: the catalog read is only half the pass.
type failingPlatformList struct {
	botsource.Store
	err error
}

func (f failingPlatformList) ListByTenant(context.Context, string) ([]botsource.BotSource, error) {
	return nil, f.err
}

func TestOverrideShadowObserver_AnUnreadableRowListHoldsAndFlags(t *testing.T) {
	s := newShadowObserverServer(t, "0.8.0", "0.7.0")
	ctx := context.Background()
	s.observePlatformOverrideShadows(ctx)
	if got := scrapedGauges(t, s.cfg.Metrics); got[shadowsNewerSeries] != 1 {
		t.Fatalf("premise: the shadow must be published first; got %v", got[shadowsNewerSeries])
	}

	s.botSources = failingPlatformList{Store: s.botSources, err: errors.New("bot-source store unavailable")}
	s.observePlatformOverrideShadows(ctx)
	got := scrapedGauges(t, s.cfg.Metrics)
	if got[shadowUnavailSeries] != 1 {
		t.Errorf("an unreadable platform row list must raise iterion_bot_shadow_check_unavailable; got %v", got[shadowUnavailSeries])
	}
	if got[shadowsNewerSeries] != 1 {
		t.Errorf("the shadow gauge must HOLD its last value while the rows are unreadable; got %v", got[shadowsNewerSeries])
	}
}

// The class the comparison cannot judge: Manifest.version is free-form, so a
// suffixed pin orders against nothing. It gets its OWN informational series
// and must never trip the paging one — D29 keeps a deliberate pin on this
// repo's own review bot, and a page nobody can clear is worse than the silence
// it replaces.
func TestOverrideShadowObserver_AnUnorderablePairIsInfoNeverAPage(t *testing.T) {
	s := newShadowObserverServer(t, "0.9.17", "0.9.2-codex-claw.5")
	s.observePlatformOverrideShadows(context.Background())
	got := scrapedGauges(t, s.cfg.Metrics)
	if got[unorderableSeries] != 1 {
		t.Errorf("an unorderable pair must be reported on its own series; got %v", got[unorderableSeries])
	}
	assertPublishedZero(t, got, shadowsNewerSeries, "an unorderable pair must NOT trip the paging gauge")
	if got[shadowUnavailSeries] != 0 {
		t.Errorf("an unorderable pair is a verdict, not an unavailable check; got %v", got[shadowUnavailSeries])
	}
}

// The two shapes of "one side declares no version", each of which outranks
// everything below it forever exactly like a dated pin does: manifest.yaml is
// OPTIONAL for a stored bundle (botsource.Validate only parses one when the
// push carries it), and the baked catalog holds unversioned entries of its own
// (bots/smoke ships a loose board_smoke.bot). Neither can be ordered, so
// neither may read as the clean 0/0/0 of a deployment with nothing to report:
// the question is unanswered, which is the informational class.
func TestOverrideShadowObserver_AVersionlessSideIsInfoNeverClean(t *testing.T) {
	t.Run("the stored bundle carries no manifest", func(t *testing.T) {
		s, _, _ := newBotSourceTestServer(t)
		s.cfg.Metrics = cloudmetrics.New()
		seedBakedBot(t, s, "reviewer", "0.8.0")
		s.bakedCatalog = s.newBakedCatalogResolver()
		pctx := store.WithTenant(context.Background(), botsource.PlatformTenantID)
		if _, err := s.botSources.Create(pctx, botsource.BotSource{
			TenantID: botsource.PlatformTenantID,
			Slug:     "reviewer",
			Files:    map[string]string{botsource.MainBotFile: testBotMain},
		}); err != nil {
			t.Fatalf("premise: a manifest-less bundle is a legal push; got %v", err)
		}
		assertVersionlessPairIsInfo(t, s)
	})

	t.Run("the baked bundle carries no version", func(t *testing.T) {
		s, _, _ := newBotSourceTestServer(t)
		s.cfg.Metrics = cloudmetrics.New()
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "reviewer.bot"), []byte(testBotMain), 0o644); err != nil {
			t.Fatal(err)
		}
		s.cfg.Bots.Paths = []string{root}
		s.bakedCatalog = s.newBakedCatalogResolver()
		// Premise: the catalog is READABLE. Whether the unversioned loose
		// .bot leaves a trace in the projection is the verdict itself, and
		// it is asserted on the gauge below — a projection that drops it
		// reports this override as a clean row.
		if _, ok := s.versionsBelow(botsource.PlatformTenantID); !ok {
			t.Fatalf("premise: the loose .bot must be discoverable")
		}
		putPlatformOverride(t, s, "reviewer", "0.7.0")
		assertVersionlessPairIsInfo(t, s)
	})
}

func assertVersionlessPairIsInfo(t *testing.T, s *Server) {
	t.Helper()
	s.observePlatformOverrideShadows(context.Background())
	got := scrapedGauges(t, s.cfg.Metrics)
	if got[unorderableSeries] != 1 {
		t.Errorf("a pair with no version on one side is the class the comparison cannot judge, and must say so; got %v — which is byte-identical to a healthy row", got[unorderableSeries])
	}
	assertPublishedZero(t, got, shadowsNewerSeries, "an unjudgeable pair must NOT trip the paging gauge")
	if got[shadowUnavailSeries] != 0 {
		t.Errorf("the check RAN: a verdict of 'cannot be ordered' is not an unavailable check; got %v", got[shadowUnavailSeries])
	}
}

// The live shape of a blind catalog, which is NOT the cold one: the resolver
// serves its last successfully read map after a failure, so a bot root that
// wedges after boot keeps answering `ok` forever. A pod is cold for its first
// 30s and warm for the rest of its life, so this is the shape the alert is
// actually about.
func TestOverrideShadowObserver_AWarmCatalogFailureFlagsUnavailable(t *testing.T) {
	s := newShadowObserverServer(t, "0.8.0", "0.7.0")
	ctx := context.Background()
	s.observePlatformOverrideShadows(ctx)
	got := scrapedGauges(t, s.cfg.Metrics)
	if got[shadowsNewerSeries] != 1 || got[shadowUnavailSeries] != 0 {
		t.Fatalf("premise: one healthy pass first; newer=%v unavailable=%v", got[shadowsNewerSeries], got[shadowUnavailSeries])
	}

	// The catalog goes blind AFTER that success, and the TTL expires — the
	// pod shape. The resolver is NOT rebuilt: rebuilding it is a cold start,
	// which production performs exactly once, in server.go.
	broken := t.TempDir()
	dir := filepath.Join(broken, "reviewer")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, botsource.MainBotFile), []byte(testBotMain), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("schema_version: 1\nname: [unterminated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.cfg.Bots.Paths = []string{broken}
	s.bakedCatalog.Invalidate()

	if _, ok := s.versionsBelow(botsource.PlatformTenantID); ok {
		t.Errorf("a walk that failed must not be served as a current reading — the listing's shadow_check_unavailable crosses this same chokepoint")
	}
	s.observePlatformOverrideShadows(ctx)
	got = scrapedGauges(t, s.cfg.Metrics)
	if got[shadowUnavailSeries] != 1 {
		t.Errorf("a catalog that went unreadable AFTER a success must still raise iterion_bot_shadow_check_unavailable; got %v", got[shadowUnavailSeries])
	}
	if got[shadowsNewerSeries] != 1 {
		t.Errorf("the shadow gauge must HOLD its last value while the check is blind; got %v", got[shadowsNewerSeries])
	}
}

// No scrape may ever read an empty inventory for a slug that IS shadowing.
// A Reset-then-rewrite pass is observably non-atomic — prometheus reports the
// family with no children in between — and `max by (slug) (...) > 0` over an
// empty vector evaluates to nothing, which drops the alert and restarts its
// `for: 1h` clock on a condition that never cleared. A ZERO, not a ratio: the
// invariant is absolute, so this can only redden on a real window.
func TestOverrideShadowObserver_NoScrapeEverSeesAnEmptyInventory(t *testing.T) {
	s := newShadowObserverServer(t, "0.8.0", "0.7.0")
	ctx := context.Background()
	s.observePlatformOverrideShadows(ctx)
	if got := scrapedGauges(t, s.cfg.Metrics); got[shadowsNewerSeries] != 1 {
		t.Fatalf("premise: the shadow must be published first; got %v", got[shadowsNewerSeries])
	}

	done := make(chan struct{})
	var passes sync.WaitGroup
	passes.Add(1)
	go func() {
		defer passes.Done()
		for {
			select {
			case <-done:
				return
			default:
				s.observePlatformOverrideShadows(ctx)
			}
		}
	}()

	const scrapes = 3000
	empty := 0
	for i := 0; i < scrapes; i++ {
		if _, present := scrapedGauges(t, s.cfg.Metrics)[shadowsNewerSeries]; !present {
			empty++
		}
	}
	close(done)
	passes.Wait()

	if empty != 0 {
		t.Errorf("%d of %d scrapes read NO series at all for a shadowing override — the pass retires series it is about to rewrite, and an empty vector reads as 'no shadow'", empty, scrapes)
	}
}

// A store outage is a standing condition the gauge carries, so the log only
// owes the edge: the branch runs on every pass of every replica, and an outage
// across the prod HPA ceiling would otherwise write one identical line per pod
// per pass. Same doctrine as staleOverrideWarned next door.
func TestOverrideShadowObserver_TheRowListWarningIsOnePerOutage(t *testing.T) {
	s := newShadowObserverServer(t, "0.8.0", "0.7.0")
	var logs strings.Builder
	s.logger = iterlog.New(iterlog.LevelWarn, &logs)
	ctx := context.Background()
	healthy := s.botSources
	s.botSources = failingPlatformList{Store: healthy, err: errors.New("bot-source store unavailable")}

	for i := 0; i < 5; i++ {
		s.observePlatformOverrideShadows(ctx)
	}
	const marker = "override-shadow observer"
	if n := strings.Count(logs.String(), marker); n != 1 {
		t.Errorf("one outage must cost ONE line, not one per pass; got %d", n)
	}

	// Recovered, then blind again: the next outage is its own event and must
	// speak. A flag that only ever rises would silence every later outage.
	s.botSources = healthy
	s.observePlatformOverrideShadows(ctx)
	s.botSources = failingPlatformList{Store: healthy, err: errors.New("bot-source store unavailable")}
	s.observePlatformOverrideShadows(ctx)
	if n := strings.Count(logs.String(), marker); n != 2 {
		t.Errorf("a SECOND outage after a recovery must warn again; got %d lines", n)
	}
}

// A loop that never ran publishes the same registered zeros as a clean
// deployment. The arming raises the unavailable flag BEFORE the loop is
// spawned, so that window is not a clean bill of health nobody issued.
//
// Deterministic by construction: the shutdown signal has already fired, so
// goUntilShutdown refuses the registration and never spawns the pass — the
// arming is the only thing that can have touched the gauge.
func TestStartOverrideShadowObserver_ArmsUnavailableBeforeTheLoop(t *testing.T) {
	s := newShadowObserverServer(t, "0.8.0", "0.7.0")
	if got := scrapedGauges(t, s.cfg.Metrics); got[shadowUnavailSeries] != 0 {
		t.Fatalf("premise: a fresh registry reads 0, which is also what a clean deployment reads; got %v", got[shadowUnavailSeries])
	}
	close(s.shutdown)

	s.startOverrideShadowObserver()

	if got := scrapedGauges(t, s.cfg.Metrics); got[shadowUnavailSeries] != 1 {
		t.Errorf("an observer that never ran must not publish a clean inventory; got %v", got[shadowUnavailSeries])
	}
}

// Without a stored tier there is no override that could shadow anything, so
// the arming must not claim a check it will not run — a local deployment
// would page on a condition it cannot have. (The no-registry half is
// TestObservePlatformOverrideShadows_NoMetricsNoStoreIsANoOp: with no
// registry there is nothing to scrape.)
func TestStartOverrideShadowObserver_SilentWithoutAStoredTier(t *testing.T) {
	s := newShadowObserverServer(t, "0.8.0", "0.7.0")
	reg := s.cfg.Metrics
	close(s.shutdown)

	s.botSources = nil
	s.startOverrideShadowObserver()
	if got := scrapedGauges(t, reg); got[shadowUnavailSeries] != 0 {
		t.Errorf("no stored tier means no override can shadow anything — the registered zero is honest; got %v", got[shadowUnavailSeries])
	}
}

// A restart re-phases the ticker onto this pod's start time. Without a pass
// BEFORE the first tick, a whole interval goes by with no per-slug series at
// all — which the alert's `> 0` reads as "no shadow". The interval here is an
// hour: only a boot pass can have published anything.
func TestRunOverrideShadowObserver_ABootPassPrecedesTheFirstTick(t *testing.T) {
	s := newShadowObserverServer(t, "0.8.0", "0.7.0")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s.runOverrideShadowObserver(ctx, time.Hour)

	if got := scrapedGauges(t, s.cfg.Metrics); got[shadowsNewerSeries] != 1 {
		t.Errorf("the observer must publish a boot pass before its first tick; got %v", got[shadowsNewerSeries])
	}
}

// A loop defined and never STARTED is this ticket's own defect one level up:
// the two reporters it replaces were each wired and each passive. Asserted on
// the source because ListenAndServe cannot be run here, through this package's
// own sweep (bot_resolver_sweep_test.go) rather than a raw read: sweepServerFiles
// strips line comments before handing the body over, so a call commented OUT
// cannot satisfy the witness — which a `strings.Contains` on the raw bytes
// happily would, leaving the whole feature dead behind a green gate. The `seen`
// guard is the other half: a renamed file would otherwise make this vacuous.
func TestServerLifecycle_ArmsTheOverrideShadowObserver(t *testing.T) {
	seen := false
	sweepServerFiles(t, func(name, body string) {
		if name != "server_lifecycle.go" {
			return
		}
		seen = true
		if !strings.Contains(body, "s.startOverrideShadowObserver()") {
			t.Error("server_lifecycle.go does not arm the override-shadow observer — the gauges would sit at their registered zeros, which is exactly what a clean deployment reads")
		}
	})
	if !seen {
		t.Fatal("server_lifecycle.go was not swept: this witness would pass on a file it never read")
	}
}

// The pass itself must not panic reaching for a registry or a store it does
// not have.
func TestObservePlatformOverrideShadows_NoMetricsNoStoreIsANoOp(t *testing.T) {
	s := newShadowObserverServer(t, "0.8.0", "0.7.0")
	reg := s.cfg.Metrics
	s.cfg.Metrics = nil
	s.observePlatformOverrideShadows(context.Background())
	s.cfg.Metrics, s.botSources = reg, nil
	s.observePlatformOverrideShadows(context.Background())
	assertNoSeries(t, scrapedGauges(t, reg), shadowsNewerSeries, "a no-op pass must publish nothing")
}
