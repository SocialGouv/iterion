package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The push guard: `iterion remote admin bots push` must refuse a bundle the
// deployment's engine cannot run, instead of storing it and letting the first
// launch die on an expression the runner's evaluator does not have.

// fakeFleet stands in for the live fleet registry (natsq.Conn.FleetMembers).
type fakeFleet struct {
	members []queue.FleetMember
	err     error
}

func (f *fakeFleet) FleetMembers(context.Context) ([]queue.FleetMember, error) {
	return f.members, f.err
}

// liveRunners is a registry holding one live, fetching runner per build, at
// the epoch of a server built without one (0): every one can take a launch.
func liveRunners(builds ...string) *fakeFleet {
	f := &fakeFleet{}
	for i, b := range builds {
		f.members = append(f.members, runnerMember(fmt.Sprintf("runner-%d", i), b, 0, false))
	}
	return f
}

func runnerMember(instance, build string, epoch uint64, draining bool) queue.FleetMember {
	return queue.FleetMember{Role: queue.FleetRoleRunner, Instance: instance, Build: build, Epoch: epoch, Draining: draining}
}

func serverMember(instance, build string, epoch uint64) queue.FleetMember {
	return queue.FleetMember{Role: queue.FleetRoleServer, Instance: instance, Build: build, Epoch: epoch}
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

// adminStoredIDFor is the platform override's id, what an update presents
// (the digest rule); a bot that is not there has none.
func adminStoredIDFor(s *Server, slug string) string {
	bs, err := s.botSources.GetBySlug(store.WithTenant(context.Background(), botsource.PlatformTenantID), botsource.PlatformTenantID, slug)
	if err != nil {
		return ""
	}
	return bs.ID
}

// adminStoredDigestsFor is the platform override's per-file digests as
// stored — what an update presents for the files it rewrites.
func adminStoredDigestsFor(s *Server, slug string) map[string]string {
	bs, err := s.botSources.GetBySlug(store.WithTenant(context.Background(), botsource.PlatformTenantID), botsource.PlatformTenantID, slug)
	if err != nil {
		return nil
	}
	out := make(map[string]string, len(bs.Files))
	for path, content := range bs.Files {
		out[path] = contentSHA256(content)
	}
	return out
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
	s.fleet = liveRunners("v99.0.0", "v3.112.7+abc123")

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
	s.fleet = liveRunners("v3.112.7+abc123")

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
	s.fleet = liveRunners("v3.116.4+deadbeef")

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
	s.fleet = liveRunners("v3.116.4+deadbeef")

	for i, requires := range []string{">= 3.112.14", ""} {
		body := pushBundle(requires)
		if i > 0 {
			// The digest rule: an update presents the id and the stored
			// digests its read answered — the manifest this push rewrites
			// among them.
			var m map[string]any
			if err := json.Unmarshal([]byte(body), &m); err != nil {
				t.Fatal(err)
			}
			m["id"] = adminStoredIDFor(s, "needy")
			m["stored_sha256"] = adminStoredDigestsFor(s, "needy")
			bodyBytes, _ := json.Marshal(m)
			body = string(bodyBytes)
		}
		w := adminBotsPutQuery(s, admin, "needy", "", body)
		if w.Code != http.StatusOK {
			t.Fatalf("push with requires %q = %d %s, want 200", requires, w.Code, w.Body.String())
		}
	}
}
