package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cloudsched"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/store"
)

// mintedGrantSpecSentinel stands in for the bearer a server mints at launch:
// a value no real grant can take, so finding it on a launch spec proves the
// grant travelled from a stored row instead of from the mint.
const mintedGrantSpecSentinel = "sentinel-spec-grant-2036"

// A schedule row mints nothing. The publish bearer and the three endpoints it
// is presented to are the server's own, minted per launch — so a value stored
// under those names on the row is a stale token, or an endpoint of whoever
// edited the row. Neither reaches the run, and neither is trusted by name.
func TestTheScheduledLaunchSpecCarriesNoMintedGrant(t *testing.T) {
	vars := map[string]string{"mode": "digest"}
	for _, name := range store.ServerMintedLaunchVars {
		vars[name] = mintedGrantSpecSentinel
	}
	sb := cloudsched.ScheduledBot{ID: "sb-1", TenantID: "team-A", BotID: "feed-watch", Vars: vars}

	spec := buildScheduledLaunchSpec(sb, "/tmp/feed-watch/main.bot", "workflow: {}", nil, nil)

	for _, name := range store.ServerMintedLaunchVars {
		if got, ok := spec.Vars[name]; ok {
			t.Errorf("the scheduled launch carries %s = %q", name, got)
		}
		for _, trusted := range spec.TrustedVarNames {
			if trusted == name {
				t.Errorf("%s is trusted by name on a launch it does not reach", name)
			}
		}
	}
	if spec.Vars["mode"] != "digest" {
		t.Errorf("the schedule's own vars were lost: %v", spec.Vars)
	}
	if vars[store.ForgePublishTokenVar] != mintedGrantSpecSentinel {
		t.Errorf("the schedule row's own map was mutated: %v", vars)
	}
	var trustsMode bool
	for _, trusted := range spec.TrustedVarNames {
		if trusted == "mode" {
			trustsMode = true
		}
	}
	if !trustsMode {
		t.Errorf("the schedule's own var stopped being trusted: %v", spec.TrustedVarNames)
	}
}

// The spine mints nothing either: a grant var in a plan's vars came from
// outside — an emit payload, a card the promote lane read — and the launch
// mints its own where it needs one.
func TestTheSpineLaunchCarriesNoMintedGrantOntoTheRun(t *testing.T) {
	pub := &spinePublisher{}
	s := newGatedSpineServer(t, gateSpec{id: "t1"}, pub)
	plan := spinePlan("t1")
	plan.Vars = map[string]string{"loop_cap": "5"}
	for _, name := range store.ServerMintedLaunchVars {
		plan.Vars[name] = mintedGrantSpecSentinel
	}

	if _, err := s.triggerLauncher().Launch(spineCtx(t), plan); err != nil {
		t.Fatalf("Launch = %v, want nil", err)
	}
	spec := pub.lastSpec()
	for _, name := range store.ServerMintedLaunchVars {
		if got, ok := spec.Vars[name]; ok {
			t.Errorf("the spine launched with %s = %q", name, got)
		}
	}
	if spec.Vars["loop_cap"] != "5" {
		t.Errorf("the plan's own vars were lost: %v", spec.Vars)
	}
	if plan.Vars[store.ForgePublishTokenVar] != mintedGrantSpecSentinel {
		t.Errorf("the plan's own map was mutated: %v", plan.Vars)
	}
}

// plantLegacyCardBotArgs puts bot args on a STORED card the way only a card
// written BEFORE the board dropped the publish grant can carry them: into the
// store's own file, read back by a Reconcile (which takes the index's value
// only for an id written while a scan is in flight, so a settled card is read
// from disk).
//
// Create and Update drop those vars now (native.BotArgsForWrite), so a fixture
// that needs a card CARRYING a grant cannot go through them — and must not: it
// would assert against a shape no writer can produce any more, and pass while
// proving nothing. The retroactive half of #2036 is exactly this card.
func plantLegacyCardBotArgs(t *testing.T, board *native.Store, id string, args map[string]string) *native.Issue {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(board.Root(), "issues", "*.json"))
	if err != nil {
		t.Fatalf("glob the board's cards: %v", err)
	}
	planted := false
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("unmarshal %s: %v", f, err)
		}
		if doc["id"] != id {
			continue
		}
		doc["bot_args"] = args
		next, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			t.Fatalf("marshal %s: %v", f, err)
		}
		if err := os.WriteFile(f, next, 0o600); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
		planted = true
		break
	}
	if !planted {
		t.Fatalf("no stored card has id %s — nothing was planted", id)
	}
	if err := board.Reconcile(); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	cur, err := board.Get(id)
	if err != nil {
		t.Fatalf("Get %s: %v", id, err)
	}
	for k, v := range args {
		if cur.BotArgs[k] != v {
			t.Fatalf("the planted card reads %s = %q, want %q — the fixture did not take", k, cur.BotArgs[k], v)
		}
	}
	return cur
}

// The pipeline-board task routes answer with the card they touched: the
// studio reads a card there as it reads it from the native REST API, and a
// card planted before the store dropped the grant still carries one. Driven
// through the real routes, which is where a studio's Edit form reads it.
func TestThePipelineBoardTaskRoutesMaskACardsPublishGrant(t *testing.T) {
	const sentinel = "sentinel-pipeline-card-grant-2036"
	for _, route := range []struct {
		what, method, path, body string
	}{
		{"PATCH /tasks/{id}", http.MethodPatch, "", `{"title":"Review PR 7 (edited)"}`},
		{"POST /tasks/{id}/ready", http.MethodPost, "/ready", `{}`},
		{"POST /tasks/{id}/reset", http.MethodPost, "/reset", `{}`},
	} {
		t.Run(route.what, func(t *testing.T) {
			env := newPipelineBoardTestEnv(t)
			card, err := env.board.Create(native.Issue{Title: "Review PR 7", State: native.StateReady, Bot: "review"})
			if err != nil {
				t.Fatal(err)
			}
			plantLegacyCardBotArgs(t, env.board, card.ID, map[string]string{
				"pr_url":                   "https://github.com/o/r/pull/7",
				store.ForgePublishTokenVar: sentinel,
			})

			resp := sendJSON(t, route.method, env.http.URL+"/api/v1/pipeline-board/tasks/"+card.ID+route.path, route.body)
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("%s = %d: %s", route.what, resp.StatusCode, body)
			}
			if bytes.Contains(body, []byte(sentinel)) {
				t.Errorf("%s hands out the bearer: %s", route.what, body)
			}
			if !bytes.Contains(body, []byte(store.RedactedLaunchVar)) {
				t.Errorf("%s says nothing about the grant the card holds (no mask): %s", route.what, body)
			}
			if !bytes.Contains(body, []byte("pr_url")) {
				t.Errorf("%s lost the card's own bot args: %s", route.what, body)
			}
			// The record keeps its bearer: the launch lane reads it to
			// refuse a crossing, and a response may not scrub the card.
			cur, err := env.board.Get(card.ID)
			if err != nil {
				t.Fatal(err)
			}
			if cur.BotArgs[store.ForgePublishTokenVar] != sentinel {
				t.Errorf("%s rewrote the card's grant: %v", route.what, cur.BotArgs)
			}
		})
	}
}
