package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/botsource"
)

// The push guard: `iterion remote admin bots push` must refuse a bundle the
// deployment's engine cannot run, instead of storing it and letting the first
// launch die on an expression the runner's evaluator does not have.

// fakeBuildObserver stands in for the Mongo store's ObservedRunnerBuilds — the
// runners' own report, read off the runs they stamped.
type fakeBuildObserver struct {
	builds []string
	err    error
	since  time.Time
}

func (f *fakeBuildObserver) ObservedRunnerBuilds(_ context.Context, since time.Time, _ int) ([]string, error) {
	f.since = since
	return f.builds, f.err
}

// pinServerBuild pins this process's declared build so the floor is decidable
// under test — `go test` reports the unorderable "dev".
func pinServerBuild(t *testing.T, v string) {
	t.Helper()
	prev := serverBuild
	serverBuild = func() string { return v }
	t.Cleanup(func() { serverBuild = prev })
}

func pushBundle(requires string) string {
	manifest := "name: needy\nversion: 1.6.0\n"
	if requires != "" {
		manifest += "requires:\n  iterion: \"" + requires + "\"\n"
	}
	body, _ := json.Marshal(map[string]any{"files": map[string]string{
		botsource.MainBotFile: "workflow main:\n  entry: done\n",
		"manifest.yaml":       manifest,
	}})
	return string(body)
}

func adminBotsPutQuery(s *Server, id auth.Identity, slug, query, body string) *httptest.ResponseRecorder {
	ctx := auth.WithIdentity(context.Background(), id)
	url := "/api/admin/bots/" + slug
	if query != "" {
		url += "?" + query
	}
	r := httptest.NewRequest("PUT", url, strings.NewReader(body)).WithContext(ctx)
	r.SetPathValue("slug", slug)
	w := httptest.NewRecorder()
	s.handleAdminPutPlatformBot(w, r)
	return w
}

// A bundle requiring more than the OLDEST build observed on the fleet is
// refused, and the refusal names the arithmetic.
func TestAdminBotsPush_RefusesABundleTheFleetCannotRun(t *testing.T) {
	pinServerBuild(t, "v3.116.4+deadbeef")
	s, admin, _ := adminBotsServer(t)
	s.runnerBuilds = &fakeBuildObserver{builds: []string{"v99.0.0", "v3.112.7+abc123"}}

	w := adminBotsPutQuery(s, admin, "needy", "", pushBundle(">= 3.112.14"))
	if w.Code != http.StatusConflict {
		t.Fatalf("push = %d %s, want 409 — a bundle the fleet cannot run must not be stored", w.Code, w.Body.String())
	}
	var errResp struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &errResp); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{">= 3.112.14", "3.112.7", "--force"} {
		if !strings.Contains(errResp.Error, want) {
			t.Errorf("refusal %q does not mention %q", errResp.Error, want)
		}
	}
	// And nothing was persisted: a refused push must not half-land.
	if _, err := s.botSources.GetBySlug(context.Background(), botsource.PlatformTenantID, "needy"); err == nil {
		t.Fatal("the refused bundle was stored anyway")
	}
}

// --force is the escape hatch this repo's doctrine requires — but it is never
// silent: the push succeeds carrying the warning.
func TestAdminBotsPush_ForceOverridesButWarns(t *testing.T) {
	pinServerBuild(t, "v3.116.4+deadbeef")
	s, admin, _ := adminBotsServer(t)
	s.runnerBuilds = &fakeBuildObserver{builds: []string{"v3.112.7+abc123"}}

	w := adminBotsPutQuery(s, admin, "needy", "force=1", pushBundle(">= 3.112.14"))
	if w.Code != http.StatusOK {
		t.Fatalf("forced push = %d %s, want 200", w.Code, w.Body.String())
	}
	var resp struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(resp.Warnings, " | ")
	if !strings.Contains(joined, "FORCED") || !strings.Contains(joined, ">= 3.112.14") {
		t.Fatalf("forced push warnings = %q, want the overridden requirement named — a forced push must never be silent", joined)
	}
}

// A manifest that does not DECODE must not be stored either. BotSource.Manifest
// returns nil for an undecodable manifest, so a bundle carrying an unreadable
// `requires:` would otherwise sail past the guard, land in the store, and fail
// at every launch with "cannot open bundle" — the requirement dropped exactly
// where it was supposed to bite.
func TestAdminBotsPush_RefusesAnUndecodableManifest(t *testing.T) {
	pinServerBuild(t, "v3.116.4+deadbeef")
	s, admin, _ := adminBotsServer(t)
	s.runnerBuilds = &fakeBuildObserver{builds: []string{"v3.116.4+deadbeef"}}

	body, _ := json.Marshal(map[string]any{"files": map[string]string{
		botsource.MainBotFile: "workflow main:\n  entry: done\n",
		// An operator grammar iterion does not enforce.
		"manifest.yaml": "name: needy\nrequires:\n  iterion: \"^3.1\"\n",
	}})
	w := adminBotsPutQuery(s, admin, "needy", "", string(body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("push = %d %s, want 400 — an unreadable manifest must be refused, not stored with its contract dropped", w.Code, w.Body.String())
	}
	if _, err := s.botSources.GetBySlug(context.Background(), botsource.PlatformTenantID, "needy"); err == nil {
		t.Fatal("the bundle with the unreadable manifest was stored anyway")
	}
}

// The negative case: a bundle the fleet CAN run is stored as before. A guard
// that refuses everything is not a guard.
func TestAdminBotsPush_AdmitsWhatTheFleetCanRun(t *testing.T) {
	pinServerBuild(t, "v3.116.4+deadbeef")
	s, admin, _ := adminBotsServer(t)
	s.runnerBuilds = &fakeBuildObserver{builds: []string{"v3.116.4+deadbeef"}}

	for _, requires := range []string{">= 3.112.14", ""} {
		w := adminBotsPutQuery(s, admin, "needy", "", pushBundle(requires))
		if w.Code != http.StatusOK {
			t.Fatalf("push with requires %q = %d %s, want 200", requires, w.Code, w.Body.String())
		}
	}
}

// The server's OWN build is part of the floor: it compiles the bot at launch,
// so a server below the requirement is as blocking as an old runner. With no
// observed runner at all (a fresh deployment) it is the whole floor — which is
// what keeps the check from silently abstaining.
func TestEngineFloor_TakesTheMinimumOfServerAndRunners(t *testing.T) {
	pinServerBuild(t, "v3.116.4+deadbeef")
	s, _, _ := adminBotsServer(t)
	s.runnerBuilds = &fakeBuildObserver{builds: []string{"v0.0.1"}}
	build, sources := s.engineFloor(context.Background())
	if build != "v0.0.1" {
		t.Fatalf("floor = %q, want the OLDEST build — a run can land on any pod", build)
	}
	if len(sources) == 0 {
		t.Fatal("the floor must say where it came from")
	}

	s.runnerBuilds = &fakeBuildObserver{builds: nil}
	build, _ = s.engineFloor(context.Background())
	if build != "v3.116.4+deadbeef" {
		t.Fatalf("floor with no observed runner = %q, want this server's own build", build)
	}
}

// A build the floor cannot order (a `dev` server, a fork) must not silently
// drop out of the minimum and leave an ORDERABLE peer as the answer: an
// unorderable participant makes the whole floor unknown, which the push guard
// reports rather than passes.
func TestEngineFloor_AnUnorderableParticipantIsNotDropped(t *testing.T) {
	pinServerBuild(t, "v3.116.4+deadbeef")
	s, _, _ := adminBotsServer(t)
	s.runnerBuilds = &fakeBuildObserver{builds: []string{"v99.0.0", "some-fork-build"}}
	build, _ := s.engineFloor(context.Background())
	if orderableBuild(build) {
		t.Fatalf("floor = %q (orderable), want an unorderable answer: one pod of the fleet carries a build nothing can order", build)
	}
}

// The lookback the observer is asked for must be a bounded, recent window —
// a build that last ran a month ago is not evidence about today's fleet.
func TestEngineFloor_AsksForARecentWindow(t *testing.T) {
	pinServerBuild(t, "v3.116.4+deadbeef")
	s, _, _ := adminBotsServer(t)
	obs := &fakeBuildObserver{builds: []string{"v3.116.4"}}
	s.runnerBuilds = obs
	s.engineFloor(context.Background())
	age := time.Since(obs.since)
	if age < time.Hour || age > 30*24*time.Hour {
		t.Fatalf("observer asked for runs since %v ago, want a bounded recent window", age)
	}
}
