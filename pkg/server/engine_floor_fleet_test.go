package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The engine floor reads the LIVE fleet registry: the processes that can take
// a run launched now. A pod finishing its last run, or a pod below the
// publishers' epoch, must not refuse a push the rest of the fleet can run —
// and an unseen half of the deployment is said on the push, never passed in
// silence.

// The server's OWN build is part of the floor: it compiles the bot at launch,
// so a server below the requirement is as blocking as an old runner. With no
// live runner at all it is the whole floor, and the reading says the runners
// went unchecked.
func TestEngineFloor_TakesTheMinimumOfServerAndRunners(t *testing.T) {
	pinServerBuild(t, "v3.116.4+deadbeef")
	s, _, _ := adminBotsServer(t)
	s.fleet = liveRunners("v0.0.1")
	rd := s.engineFloor(context.Background())
	if rd.build != "v0.0.1" {
		t.Fatalf("floor = %q, want the OLDEST build — a run can land on any pod", rd.build)
	}
	if len(rd.sources) == 0 || !strings.Contains(rd.sources[0], "runner-0") {
		t.Fatalf("sources = %q, want the floor's holder named", rd.sources)
	}
	if len(rd.unchecked) != 0 {
		t.Fatalf("unchecked = %q, want none: a live runner was read", rd.unchecked)
	}

	s.fleet = &fakeFleet{}
	rd = s.engineFloor(context.Background())
	if rd.build != "v3.116.4+deadbeef" {
		t.Fatalf("floor with no live runner = %q, want this server's own build", rd.build)
	}
	if len(rd.unchecked) == 0 || !strings.Contains(rd.unchecked[0], "no live runner") {
		t.Fatalf("unchecked = %q, want the missing runner half said", rd.unchecked)
	}
}

// A build the floor cannot order (a `dev` server, a fork) must not silently
// drop out of the minimum and leave an ORDERABLE peer as the answer: an
// unorderable participant makes the whole floor unknown, which the push guard
// reports rather than passes.
func TestEngineFloor_AnUnorderableParticipantIsNotDropped(t *testing.T) {
	pinServerBuild(t, "v3.116.4+deadbeef")
	s, _, _ := adminBotsServer(t)
	s.fleet = liveRunners("v99.0.0", "some-fork-build")
	if rd := s.engineFloor(context.Background()); orderableBuild(rd.build) {
		t.Fatalf("floor = %q (orderable), want an unorderable answer: one pod of the fleet carries a build nothing can order", rd.build)
	}
}

// rolledFleet is the state right after a roll from v7.40.1 (epoch 53) to
// v7.40.3 (epoch 54): the new servers and runners live, plus whatever the
// test adds of the old generation.
func rolledFleet(t *testing.T, old ...queue.FleetMember) *Server {
	t.Helper()
	pinServerBuild(t, "v7.40.3+afe06da483db")
	s, _, _ := adminBotsServer(t)
	s.cfg.RunnerEpoch = 54
	members := []queue.FleetMember{
		serverMember("iterion-a", "v7.40.3+afe06da483db", 54),
		runnerMember("runner-new-a", "v7.40.3+afe06da483db", 54, false),
		runnerMember("runner-new-b", "v7.40.3+afe06da483db", 54, false),
	}
	s.fleet = &fakeFleet{members: append(members, old...)}
	return s
}

// A draining runner has stopped fetching: it only finishes the run it holds,
// so its build says nothing about the next launch.
func TestEngineFloor_ADrainingRunnerDoesNotHoldTheFloor(t *testing.T) {
	// Same epoch as the publishers, so only the draining rule can exclude it.
	s := rolledFleet(t, runnerMember("runner-old", "v7.40.1+0d1e2f3a4b5c", 54, true))
	rd := s.engineFloor(context.Background())
	if rd.build != "v7.40.3+afe06da483db" {
		t.Fatalf("floor = %q, want v7.40.3 — the draining v7.40.1 runner takes no new run", rd.build)
	}
	if !strings.Contains(strings.Join(rd.sources, "; "), "not counted, draining: runner runner-old v7.40.1") {
		t.Fatalf("sources = %q, want the draining runner named as not counted", rd.sources)
	}
}

// A runner below every live publisher's epoch refuses each message they
// publish (the rollout fence), so it cannot execute a new launch either.
func TestEngineFloor_ARunnerBelowThePublishersEpochDoesNotHoldTheFloor(t *testing.T) {
	// Not draining, so only the epoch rule can exclude it.
	s := rolledFleet(t, runnerMember("runner-old", "v7.40.1+0d1e2f3a4b5c", 53, false))
	rd := s.engineFloor(context.Background())
	if rd.build != "v7.40.3+afe06da483db" {
		t.Fatalf("floor = %q, want v7.40.3 — an epoch-53 runner refuses every epoch-54 message", rd.build)
	}
	if !strings.Contains(strings.Join(rd.sources, "; "), "below the publishers' epoch 54") {
		t.Fatalf("sources = %q, want the fenced runner named as not counted", rd.sources)
	}
}

// While an old-epoch server is still live it still publishes at its epoch,
// and old-epoch runners accept those messages: they count again. The old
// server's build is pinned to the new one so only the runner can lower the
// floor.
func TestEngineFloor_AnOldEpochServerKeepsItsRunnersCounted(t *testing.T) {
	s := rolledFleet(t,
		serverMember("iterion-old", "v7.40.3+afe06da483db", 53),
		runnerMember("runner-old", "v7.40.1+0d1e2f3a4b5c", 53, false),
	)
	rd := s.engineFloor(context.Background())
	if rd.build != "v7.40.1+0d1e2f3a4b5c" {
		t.Fatalf("floor = %q, want v7.40.1 — a live epoch-53 server can still hand that runner a launch", rd.build)
	}
	if !strings.Contains(rd.sources[0], "runner runner-old") {
		t.Fatalf("sources = %q, want the old runner named as the floor's holder", rd.sources)
	}
}

// Every live server counts, whatever its epoch: it compiles the launches it
// serves.
func TestEngineFloor_AnOlderLiveServerHoldsTheFloor(t *testing.T) {
	s := rolledFleet(t, serverMember("iterion-old", "v7.40.1+0d1e2f3a4b5c", 54))
	if rd := s.engineFloor(context.Background()); rd.build != "v7.40.1+0d1e2f3a4b5c" {
		t.Fatalf("floor = %q, want the older live server's build", rd.build)
	}
}

// A record of a role this build does not know is counted, not dropped.
func TestEngineFloor_AnUnknownRoleIsCounted(t *testing.T) {
	s := rolledFleet(t, queue.FleetMember{Role: "scheduler", Instance: "x", Build: "v7.0.0", Epoch: 54})
	if rd := s.engineFloor(context.Background()); rd.build != "v7.0.0" {
		t.Fatalf("floor = %q, want the unknown-role record's build counted", rd.build)
	}
}

func pushWarnings(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var resp struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return strings.Join(resp.Warnings, " | ")
}

// End to end through the push route: with the old generation draining, a
// bundle requiring the new build is stored without a warning, and one
// requiring a build nobody runs is refused with the live holder named.
func TestAdminBotsPush_TheDrainingGenerationDoesNotRefuse(t *testing.T) {
	s := rolledFleet(t, runnerMember("runner-old", "v7.40.1+0d1e2f3a4b5c", 53, true))
	admin := auth.Identity{UserID: "root", IsSuperAdmin: true, TeamID: "t1"}

	w := adminBotsPutQuery(s, admin, "needy", "", pushBundle(">= 7.40.3"))
	if w.Code != http.StatusOK {
		t.Fatalf("push = %d %s, want 200 — only a draining runner is below the requirement", w.Code, w.Body.String())
	}
	if got := pushWarnings(t, w); got != "" {
		t.Fatalf("warnings = %q, want none: every process that can take a launch was read", got)
	}

	w = adminBotsPutQuery(s, admin, "future", "", pushBundle(">= 7.40.4"))
	if w.Code != http.StatusConflict {
		t.Fatalf("push = %d %s, want 409", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, "held by this server") || !strings.Contains(body, "runner-new-a") {
		t.Fatalf("refusal %s does not name the live holders of the floor", body)
	}
}

// An unreadable registry or no live runner: the push passes on this server's
// build and SAYS the runners went unchecked.
func TestAdminBotsPush_AnUnseenRunnerHalfIsSaid(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fleet *fakeFleet
		want  string
	}{
		{"unreadable", &fakeFleet{err: errors.New("nats: no responders")}, "the fleet registry could not be read (nats: no responders)"},
		{"no live runner", &fakeFleet{members: []queue.FleetMember{runnerMember("r", "v3.116.4+deadbeef", 0, true)}}, "no live runner can take a new launch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pinServerBuild(t, "v3.116.4+deadbeef")
			s, admin, _ := adminBotsServer(t)
			s.fleet = tc.fleet
			w := adminBotsPutQuery(s, admin, "needy", "", pushBundle(">= 3.112.14"))
			if w.Code != http.StatusOK {
				t.Fatalf("push = %d %s, want 200", w.Code, w.Body.String())
			}
			if got := pushWarnings(t, w); !strings.Contains(got, tc.want) || !strings.Contains(got, ">= 3.112.14") {
				t.Fatalf("warnings = %q, want %q with the requirement named", got, tc.want)
			}
		})
	}
}

// The read model answers the registry and the floor of the same snapshot.
func TestAdminFleet_ReturnsMembersAndTheirFloor(t *testing.T) {
	s := rolledFleet(t, runnerMember("runner-old", "v7.40.1+0d1e2f3a4b5c", 53, true))
	admin := auth.Identity{UserID: "root", IsSuperAdmin: true, TeamID: "t1"}
	r := httptest.NewRequest("GET", "/api/admin/fleet", nil).WithContext(auth.WithIdentity(context.Background(), admin))
	w := httptest.NewRecorder()
	s.handleAdminFleet(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("GET = %d %s", w.Code, w.Body.String())
	}
	var view adminFleetView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Members) != 4 || view.Floor.Build != "v7.40.3+afe06da483db" {
		t.Fatalf("view = %+v, want 4 members and the v7.40.3 floor", view)
	}

	s.fleet = &fakeFleet{err: errors.New("nats: timeout")}
	w = httptest.NewRecorder()
	s.handleAdminFleet(w, r)
	// A 5xx body is redacted by httpError; the cause goes to the log.
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET on an unreadable registry = %d %s, want 503", w.Code, w.Body.String())
	}
}

// A floor nothing can order already passes "unverified"; the half of the
// deployment the registry did not show is said besides, not dropped.
func TestAdminBotsPush_AnUnorderableFloorStillSaysTheUnseenHalf(t *testing.T) {
	pinServerBuild(t, "dev")
	s, admin, _ := adminBotsServer(t)
	s.fleet = &fakeFleet{err: errors.New("nats: no responders")}
	w := adminBotsPutQuery(s, admin, "needy", "", pushBundle(">= 3.112.14"))
	got := pushWarnings(t, w)
	if w.Code != http.StatusOK || !strings.Contains(got, "unverified") || !strings.Contains(got, "the fleet registry could not be read") {
		t.Fatalf("push = %d warnings=%q, want the unverified pass AND the registry gap", w.Code, got)
	}
	// An unverified push did not pass anything: no sentence may say so.
	if strings.Contains(got, "passed") {
		t.Fatalf("warnings = %q: an unverified push is not a pass", got)
	}
}

// Every build nothing can order is a reason the check cannot run: an
// unorderable floor names all of them, not the last one compared.
func TestEngineFloor_AnUnorderableFloorNamesEveryUnorderableHolder(t *testing.T) {
	pinServerBuild(t, "dev")
	s, _, _ := adminBotsServer(t)
	s.fleet = liveRunners("some-fork-build", "v7.40.3+afe06da483db")
	rd := s.engineFloor(context.Background())
	if orderableBuild(rd.build) {
		t.Fatalf("floor = %q, want an unorderable answer", rd.build)
	}
	if held := rd.sources[0]; !strings.Contains(held, "this server") || !strings.Contains(held, "runner runner-0") || strings.Contains(held, "runner-1") {
		t.Fatalf("holders = %q, want this server and runner-0 (both unorderable), not the orderable runner-1", held)
	}
}

// Another commit of the same version holds the floor as much: the refusal
// names it, or the operator rolls the wrong half first.
func TestEngineFloor_AnEqualVersionPeerHoldsTheFloorToo(t *testing.T) {
	pinServerBuild(t, "v7.40.3+afe06da483db")
	s, admin, _ := adminBotsServer(t)
	s.fleet = liveRunners("v7.40.3+beefcafe1111")
	w := adminBotsPutQuery(s, admin, "needy", "", pushBundle(">= 7.40.4"))
	if w.Code != http.StatusConflict {
		t.Fatalf("push = %d %s, want 409", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, "this server") || !strings.Contains(body, "runner runner-0") {
		t.Fatalf("refusal %s does not name both holders of v7.40.3", body)
	}
}

// The assistant's authoring commit runs the push routes' floor guard, and
// returns what that guard passed with — never a silent save.
func TestAuthoringCommit_CarriesTheFloorWarnings(t *testing.T) {
	for _, tc := range []struct {
		name, build, raiseTo string
		fleet                *fakeFleet
		want                 string
	}{
		{"registry unreadable", "v3.116.4+deadbeef", ">= 3.116.4", &fakeFleet{err: errors.New("nats: no responders")}, "the fleet registry could not be read"},
		{"no live runner", "v3.116.4+deadbeef", ">= 3.116.4", &fakeFleet{}, "no live runner can take a new launch"},
		{"unorderable floor", "dev", ">= 3.112.14", liveRunners("v3.116.4+deadbeef"), "unverified"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pinServerBuild(t, tc.build)
			s, editor, _ := newBotSourceTestServer(t)
			s.fleet = tc.fleet
			ctx := auth.WithIdentity(t.Context(), editor)
			const manifest = "schema_version: 1\nname: rf\nrequires:\n  iterion: \">= 3.112.14\"\nauthoring:\n  editable_files:\n    - {scope: bundle, path: main.bot}\n    - {scope: bundle, path: manifest.yaml}\n"
			if _, err := s.botSources.Create(store.WithTenant(t.Context(), "t1"), botsource.BotSource{
				TenantID: "t1", Slug: "rf",
				Files: map[string]string{botsource.MainBotFile: "workflow main:\n  entry: done\n", "manifest.yaml": manifest},
			}); err != nil {
				t.Fatal(err)
			}
			const editorPath = "botsource://t1/rf/manifest.yaml"
			body, _ := json.Marshal(authoringSnapshotRequest{EditorPath: editorPath})
			rec := httptest.NewRecorder()
			s.handleAuthoringSnapshot(rec, httptest.NewRequest("POST", "/api/v1/assistant/authoring/snapshot", bytes.NewReader(body)).WithContext(ctx))
			var snap authoringSnapshotResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
				t.Fatalf("snapshot %d: %v %s", rec.Code, err, rec.Body.String())
			}
			var sha string
			for _, f := range snap.Files {
				if f.Path == "manifest.yaml" {
					sha = f.SHA256
				}
			}
			edit := authoringFileChange{Scope: "bundle", Path: "manifest.yaml", ExpectedSHA256: sha,
				Replacements: []authoringReplacement{{Before: `">= 3.112.14"`, After: `"` + tc.raiseTo + `"`}}}
			body, _ = json.Marshal(authoringChangeRequest{EditorPath: editorPath, Version: snap.Version, Changes: []authoringFileChange{edit}})
			rec = httptest.NewRecorder()
			s.handleAuthoringCommit(rec, httptest.NewRequest("POST", "/api/v1/assistant/authoring/commit", bytes.NewReader(body)).WithContext(ctx))
			if rec.Code != http.StatusOK {
				t.Fatalf("commit = %d %s, want 200", rec.Code, rec.Body.String())
			}
			var resp authoringChangeResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if !resp.Saved || !strings.Contains(strings.Join(resp.Warnings, " | "), tc.want) {
				t.Fatalf("commit saved=%v warnings=%q, want %q said", resp.Saved, resp.Warnings, tc.want)
			}
		})
	}
}
