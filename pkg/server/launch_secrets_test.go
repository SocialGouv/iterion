package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/forge"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	natsq "github.com/SocialGouv/iterion/pkg/queue/nats"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// publishGrantSentinel stands in for a minted forge publish grant: a value no
// real token takes, so finding it in a response proves a leak.
const publishGrantSentinel = "sentinel-publish-token-1997"

// getOK fetches url and returns its body, failing unless the answer is 200.
func getOK(t *testing.T, url string) []byte {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status = %d, body = %s", url, resp.StatusCode, body)
	}
	return body
}

// sendJSON issues method on url with a JSON body and returns the response.
func sendJSON(t *testing.T, method, url, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

// GET /api/runs/{id} is what the studio's run view, the remote CLI and the
// MCP run tools read. A run launched on a pull request holds its publish grant
// in its inputs and, for a bot that declares it, in its checkpoint vars: the
// response says a grant was minted without handing it out, and the stored
// record keeps it for the runner, a resume and a fork.
func TestGetRun_NeverHandsOutThePublishGrant(t *testing.T) {
	srv, hs := newTestServer(t)
	st, err := store.New(srv.cfg.StoreDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := st.CreateRun(ctx, "run-grant", "review", map[string]any{
		"pr_url":                   "https://github.com/o/r/pull/7",
		store.ForgePublishTokenVar: publishGrantSentinel,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.PauseRun(ctx, "run-grant", &store.Checkpoint{
		NodeID: "approve", InteractionID: "int-1",
		Vars: map[string]any{store.ForgePublishTokenVar: publishGrantSentinel},
	}); err != nil {
		t.Fatal(err)
	}

	body := getOK(t, hs.URL+"/api/runs/run-grant")
	if bytes.Contains(body, []byte(publishGrantSentinel)) {
		t.Fatalf("GET /api/runs/{id} hands out the publish grant: %s", body)
	}
	var snap runview.RunSnapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := snap.Run.Inputs[store.ForgePublishTokenVar]; got != store.RedactedLaunchVar {
		t.Errorf("run.inputs.%s = %v, want %q", store.ForgePublishTokenVar, got, store.RedactedLaunchVar)
	}
	if snap.Run.Inputs["pr_url"] != "https://github.com/o/r/pull/7" {
		t.Errorf("run.inputs lost pr_url: %v", snap.Run.Inputs)
	}
	if snap.Run.Checkpoint == nil || snap.Run.Checkpoint.Vars[store.ForgePublishTokenVar] != store.RedactedLaunchVar {
		t.Errorf("run.checkpoint = %+v, want its vars masked", snap.Run.Checkpoint)
	}

	stored, err := st.LoadRun(ctx, "run-grant")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Inputs[store.ForgePublishTokenVar] != publishGrantSentinel || stored.Checkpoint.Vars[store.ForgePublishTokenVar] != publishGrantSentinel {
		t.Fatal("serving the run rewrote the stored grant — the runner and a resume read it there")
	}
}

// The pipeline board is served to every member of the team. A card's
// entry_input is built from its run's inputs — queued or not — or, when the
// run holds none, from the card's bot args; a restaged ticket shows its failed
// attempt's inputs. None of those four sources may put a grant on the wire.
func TestPipelineBoard_EntryInputNeverCarriesThePublishGrant(t *testing.T) {
	env := newPipelineBoardTestEnv(t)
	grantInputs := func(scope string) func(*store.Run) {
		return func(r *store.Run) {
			r.Inputs = map[string]any{"scope": scope, store.ForgePublishTokenVar: publishGrantSentinel}
		}
	}
	env.seedRun(t, "run-queued", "review", store.RunStatusQueued, grantInputs("queued"))
	env.seedRun(t, "run-live", "review", store.RunStatusRunning, grantInputs("live"))

	// A card written before its write paths dropped the grant, launched into
	// a run that holds no inputs of its own.
	legacy, err := env.board.Create(native.Issue{
		Title: "Legacy card", State: native.StateInProgress, Bot: "review",
		BotArgs: map[string]string{"scope": "legacy", store.ForgePublishTokenVar: publishGrantSentinel},
	})
	if err != nil {
		t.Fatal(err)
	}
	env.seedRun(t, "run-from-card", "review", store.RunStatusRunning, nil)
	if err := env.board.SetLastRun(legacy.ID, "run-from-card", ""); err != nil {
		t.Fatal(err)
	}

	// A card of the same vintage still waiting in Ready: no run yet, so its
	// entry_input is its own bot args.
	waiting, err := env.board.Create(native.Issue{
		Title: "Waiting card", State: native.StateReady, Bot: "review",
		BotArgs: map[string]string{"scope": "waiting", store.ForgePublishTokenVar: publishGrantSentinel},
	})
	if err != nil {
		t.Fatal(err)
	}

	// A ticket restaged for a retry after its run failed, carrying no bot args.
	restaged, err := env.board.Create(native.Issue{Title: "Retry me", State: native.StateReady, Bot: "review"})
	if err != nil {
		t.Fatal(err)
	}
	env.seedRun(t, "run-failed", "review", store.RunStatusFailed, grantInputs("prior"))
	if err := env.board.SetLastRun(restaged.ID, "run-failed", ""); err != nil {
		t.Fatal(err)
	}

	body := getOK(t, env.http.URL+"/api/v1/pipeline-board")
	var projection PipelineBoardResponse
	if err := json.Unmarshal(body, &projection); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, want := range []struct{ card, scope string }{
		{"run:run-queued", "queued"},
		{"run:run-live", "live"},
		{"run:run-from-card", "legacy"},
		{"task:" + waiting.ID, "waiting"},
		{"task:" + restaged.ID, "prior"},
	} {
		card := findPipelineCard(t, projection.Cards, want.card)
		if got := card.EntryInput[store.ForgePublishTokenVar]; got != store.RedactedLaunchVar {
			t.Errorf("%s: entry_input.%s = %v, want %q", want.card, store.ForgePublishTokenVar, got, store.RedactedLaunchVar)
		}
		if card.EntryInput["scope"] != want.scope {
			t.Errorf("%s: entry_input = %v, want scope %q from its own source", want.card, card.EntryInput, want.scope)
		}
	}
	if bytes.Contains(body, []byte(publishGrantSentinel)) {
		t.Errorf("the pipeline board hands out the publish grant: %s", body)
	}
}

// A board card never carries the publish grant: the launch mints one per run.
// Whatever a client sends under its name — the mask a view showed it, or a
// token copied before the views masked it — is dropped on create, on the
// planner's upsert and on an edit, so the stored card holds no such key.
func TestPipelineBoardTasks_NeverStoreThePublishGrant(t *testing.T) {
	tasksURL := func(env *pipelineBoardTestEnv) string { return env.http.URL + "/api/v1/pipeline-board/tasks" }
	storedArgs := func(t *testing.T, env *pipelineBoardTestEnv, id string) map[string]string {
		t.Helper()
		iss, err := env.board.Get(id)
		if err != nil {
			t.Fatalf("load card %s: %v", id, err)
		}
		if _, ok := iss.BotArgs[store.ForgePublishTokenVar]; ok {
			t.Fatalf("the stored card carries %s: %v", store.ForgePublishTokenVar, iss.BotArgs)
		}
		return iss.BotArgs
	}
	decodeIssue := func(t *testing.T, resp *http.Response, want int) native.Issue {
		t.Helper()
		if resp.StatusCode != want {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Fatalf("status = %d, want %d: %s", resp.StatusCode, want, b)
		}
		var iss native.Issue
		decodeJSONResp(t, resp, &iss)
		return iss
	}

	for _, sent := range []string{publishGrantSentinel, store.RedactedLaunchVar} {
		t.Run("create/"+sent, func(t *testing.T) {
			env := newPipelineBoardTestEnv(t)
			created := decodeIssue(t, sendJSON(t, http.MethodPost, tasksURL(env), fmt.Sprintf(
				`{"bot":"review","title":"Review PR 7","bot_args":{"pr_url":"https://github.com/o/r/pull/7",%q:%q}}`,
				store.ForgePublishTokenVar, sent)), http.StatusCreated)
			if _, ok := created.BotArgs[store.ForgePublishTokenVar]; ok {
				t.Fatalf("the created card answers with %s: %v", store.ForgePublishTokenVar, created.BotArgs)
			}
			if args := storedArgs(t, env, created.ID); args["pr_url"] != "https://github.com/o/r/pull/7" {
				t.Errorf("the other bot args were not kept: %v", args)
			}
		})

		t.Run("upsert/"+sent, func(t *testing.T) {
			env := newPipelineBoardTestEnv(t)
			first := decodeIssue(t, sendJSON(t, http.MethodPost, tasksURL(env),
				`{"bot":"review","title":"Asset fort","bot_args":{"input_path":"requests/fort.json"},"upsert":true}`), http.StatusCreated)
			updated := decodeIssue(t, sendJSON(t, http.MethodPost, tasksURL(env), fmt.Sprintf(
				`{"bot":"review","title":"Asset fort (rev2)","bot_args":{"input_path":"requests/fort.json","revision_id":"2",%q:%q},"upsert":true}`,
				store.ForgePublishTokenVar, sent)), http.StatusOK)
			if updated.ID != first.ID {
				t.Fatalf("the upsert created %s instead of updating %s", updated.ID, first.ID)
			}
			if args := storedArgs(t, env, first.ID); args["revision_id"] != "2" {
				t.Errorf("the upsert's other bot args were not applied: %v", args)
			}
		})

		t.Run("edit/"+sent, func(t *testing.T) {
			env := newPipelineBoardTestEnv(t)
			draft, err := env.board.Create(native.Issue{
				Title: "Draft", State: native.StateInbox, Bot: "review",
				BotArgs: map[string]string{"scope": "old"},
			})
			if err != nil {
				t.Fatal(err)
			}
			resp := sendJSON(t, http.MethodPatch, tasksURL(env)+"/"+draft.ID, fmt.Sprintf(
				`{"bot_args":{"scope":"new",%q:%q}}`, store.ForgePublishTokenVar, sent))
			decodeIssue(t, resp, http.StatusOK)
			if args := storedArgs(t, env, draft.ID); args["scope"] != "new" {
				t.Errorf("the edit's other bot args were not applied: %v", args)
			}
		})
	}
}

// parkPayload parks a message whose raw payload the test chooses.
func (q *fakeDLQQueue) parkPayload(seq uint64, runID string, payload json.RawMessage) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.parked[seq] = parkedMsg{
		view: natsq.DLQMessage{
			Seq: seq, RunID: runID, TenantID: "team-1", Reason: "max deliver exhausted",
			ParkedAt: time.Unix(1700000000, 0).UTC(), Size: len(payload),
		},
		payload: payload,
	}
}

// The DLQ peek hands a super-admin the parked message as the queue holds it,
// and a run message carries the run's launch vars — its publish grant among
// them. The admin reads that a grant rode the message, never the grant; and a
// payload that is not a run message, whose vars cannot be told apart, is not
// echoed at all.
func TestDLQPeek_NeverHandsOutThePublishGrant(t *testing.T) {
	w := newDLQAdminServer(t)
	peek := func(t *testing.T, seq uint64) json.RawMessage {
		t.Helper()
		code, body := dlqDo(t, w.hs, http.MethodGet, fmt.Sprintf("/api/admin/dlq/%d", seq), w.admin)
		if code != http.StatusOK {
			t.Fatalf("peek %d: status = %d, body = %s", seq, code, body)
		}
		if bytes.Contains(body, []byte(publishGrantSentinel)) {
			t.Fatalf("the DLQ peek hands out the publish grant: %s", body)
		}
		var out struct {
			Message natsq.DLQMessage `json:"message"`
			Payload json.RawMessage  `json:"payload"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("decode peek %d: %v (%s)", seq, err, body)
		}
		if out.Message.Seq != seq {
			t.Fatalf("peek returned seq %d, want %d", out.Message.Seq, seq)
		}
		return out.Payload
	}

	t.Run("a run message is shown with its grant masked", func(t *testing.T) {
		payload, err := json.Marshal(queue.RunMessage{
			V: queue.SchemaVersion, RunID: "run-parked", WorkflowName: "review", TenantID: "team-1",
			Vars: map[string]any{"pr_url": "https://github.com/o/r/pull/7", store.ForgePublishTokenVar: publishGrantSentinel},
		})
		if err != nil {
			t.Fatal(err)
		}
		w.seedRun(t, "run-parked", store.RunStatusQueued)
		w.q.parkPayload(21, "run-parked", payload)

		var msg queue.RunMessage
		if err := json.Unmarshal(peek(t, 21), &msg); err != nil {
			t.Fatalf("the peeked payload is no longer the run message: %v", err)
		}
		if msg.RunID != "run-parked" || msg.WorkflowName != "review" {
			t.Errorf("the run message lost its identity: %+v", msg)
		}
		if got := msg.Vars[store.ForgePublishTokenVar]; got != store.RedactedLaunchVar {
			t.Errorf("vars.%s = %v, want %q", store.ForgePublishTokenVar, got, store.RedactedLaunchVar)
		}
		if msg.Vars["pr_url"] != "https://github.com/o/r/pull/7" {
			t.Errorf("the other vars were not kept: %v", msg.Vars)
		}
	})

	t.Run("every other value is shown as parked", func(t *testing.T) {
		payload, err := json.Marshal(queue.RunMessage{
			V: queue.SchemaVersion, RunID: "run-precise", WorkflowName: "review", TenantID: "team-1",
			Vars: map[string]any{"max_tokens": int64(9007199254740993), "note": "<a & b>", store.ForgePublishTokenVar: publishGrantSentinel},
		})
		if err != nil {
			t.Fatal(err)
		}
		w.seedRun(t, "run-precise", store.RunStatusQueued)
		w.q.parkPayload(23, "run-precise", payload)

		dec := json.NewDecoder(bytes.NewReader(peek(t, 23)))
		dec.UseNumber()
		var msg struct {
			Vars map[string]any `json:"vars"`
		}
		if err := dec.Decode(&msg); err != nil {
			t.Fatal(err)
		}
		if n, _ := msg.Vars["max_tokens"].(json.Number); n.String() != "9007199254740993" {
			t.Errorf("vars.max_tokens = %v, want 9007199254740993 as parked", msg.Vars["max_tokens"])
		}
		if msg.Vars["note"] != "<a & b>" {
			t.Errorf("vars.note = %v, want it as parked", msg.Vars["note"])
		}
	})

	t.Run("a payload that is not a run message is withheld", func(t *testing.T) {
		w.seedRun(t, "run-garbled", store.RunStatusQueued)
		w.q.parkPayload(22, "run-garbled", json.RawMessage(store.ForgePublishTokenVar+"="+publishGrantSentinel))

		var note string
		if err := json.Unmarshal(peek(t, 22), &note); err != nil {
			t.Fatalf("the payload is not the withheld notice: %v", err)
		}
		if !strings.HasPrefix(note, "payload withheld") {
			t.Errorf("payload = %q, want the withheld notice", note)
		}
	})
}

// The mask a read surface shows is not a grant. A client relaunching with the
// inputs it was shown sends it back as a pin; honoured, the run would carry
// the mask as its bearer and could never publish. The launch mints its own —
// and a real pin of the launching team is still honoured.
func TestInjectForgePublishVars_TheMaskIsNotAPin(t *testing.T) {
	s, _ := newForgePublishTestServer(t)
	s.cfg.PublicURL = "https://iterion.test"
	launch := func(t *testing.T, pin string) map[string]string {
		t.Helper()
		out, _, err := s.injectForgePublishVars(context.Background(), "team1", "", "review-pr",
			map[string]string{"pr_url": "https://github.com/o/r/pull/42", forgePublishVarToken: pin}, nil, store.RunTrustDefault)
		if err != nil {
			t.Fatalf("pin %q: %v", pin, err)
		}
		return out
	}

	for _, pin := range []string{store.RedactedLaunchVar, " " + store.RedactedLaunchVar + " "} {
		out := launch(t, pin)
		tok := out[forgePublishVarToken]
		g, ok := s.forgePublishTokens.lookup(tok)
		if !ok || g.TeamID != "team1" || g.Repo != "o/r" {
			t.Fatalf("pin %q: the run carries %q, which is no grant minted for this launch (ok=%v g=%+v)", pin, tok, ok, g)
		}
		if out[forgePublishVarURL] != "https://iterion.test/api/v1/forge/publish-review" {
			t.Errorf("pin %q: publish endpoint = %q, want the minted one", pin, out[forgePublishVarURL])
		}
	}

	registerPublishToken(t, s, "tok-team1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})
	if got := launch(t, "tok-team1")[forgePublishVarToken]; got != "tok-team1" {
		t.Fatalf("the launching team's own pin was overwritten: %q", got)
	}
}

// boardCardProbeBot is a one-node bot a board card can launch in-process.
// worktree: none keeps the run off the live checkout.
const boardCardProbeBot = "schema probe_out:\n  ok: string\n\ntool noop:\n  command: `printf '{\"ok\":\"yes\"}'`\n  output: probe_out\n\nworkflow board_probe:\n  worktree: none\n  entry: noop\n  noop -> done\n"

// launchBoardCardAndSettle drives the cloud coordinator's real
// processBoardCard for iss on team1 and returns the run it launched once
// that run is terminal. A card refused before any run exists fails the test
// with the refusal.
func launchBoardCardAndSettle(t *testing.T, s *Server, rs store.RunStore, iss native.Issue) *store.Run {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var cardErr error
	go func() {
		defer close(done)
		cardErr = s.processBoardCard(ctx, "team1", iss)
	}()
	t.Cleanup(func() { cancel(); <-done })

	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		if ids, err := rs.ListRuns(ctx); err == nil && len(ids) > 0 {
			if run, err := rs.LoadRun(ctx, ids[0]); err == nil && run.Status.IsTerminal() {
				return run
			}
		} else {
			select {
			case <-done:
				t.Fatalf("the card launched no run: %v", cardErr)
			default:
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no run settled from the card")
	return nil
}

// A card can still hold a publish grant when it is claimed: the mask a client
// sent back, a token the registry no longer resolves, an earlier grant of the
// card's own team. None of them is re-pinned — the launch mints the run its
// own grant, exactly as for a card that never held one. A live grant of
// ANOTHER team is not in this set: it is refused, see
// TestBoardDispatcher_PRLookupForeignGrantRemainsTerminal.
func TestProcessBoardCard_AStaleCardGrantIsNotRepinned(t *testing.T) {
	for _, tc := range []struct {
		name, carried string
		ownTeamGrant  bool
	}{
		{"the mask a client sent back", store.RedactedLaunchVar, false},
		{"an expired grant", "stale-publish-token", false},
		{"an earlier grant of the card's own team", "tok-team1-earlier", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newForgePublishTestServer(t)
			s.cfg.PublicURL = "https://iterion.example"
			s.forgeIntegrations = forge.NewMemoryRepoIntegrationStore()
			s.webhookConfigs = webhooks.NewMemoryConfigStore()
			botsDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(botsDir, "fixer.bot"), []byte(boardCardProbeBot), 0o600); err != nil {
				t.Fatal(err)
			}
			s.cfg.Bots.Paths = []string{botsDir}
			s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) {
				return &fakeGateClient{headSHA: "abc"}, nil
			}
			rs, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			s.runs = newTestRunviewService(t, "", runview.WithStore(rs))
			if tc.ownTeamGrant {
				registerPublishToken(t, s, tc.carried, ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})
			}

			run := launchBoardCardAndSettle(t, s, rs, native.Issue{
				ID: "card1", Bot: "fixer", State: native.StateReady,
				BotArgs: map[string]string{"pr_url": "https://github.com/o/r/pull/7", forgePublishVarToken: tc.carried},
			})
			tok, _ := run.Inputs[forgePublishVarToken].(string)
			if tok == tc.carried {
				t.Fatalf("the card's grant %q was re-pinned onto the run", tc.carried)
			}
			if g, ok := s.forgePublishTokens.lookup(tok); !ok || g.TeamID != "team1" || g.Repo != "o/r" {
				t.Fatalf("the run carries no grant minted for its own launch (ok=%v g=%+v)", ok, g)
			}
		})
	}
}

// A launch that mints nothing — no pull request, or a server with no forge
// connection — never stores the mask as its token either.
func TestInjectForgePublishVars_TheMaskIsDroppedWhenNothingIsMinted(t *testing.T) {
	unwired := New(Config{}, iterlog.New(iterlog.LevelError, nil))
	for name, vars := range map[string]map[string]string{
		"no pull request":     {store.ForgePublishTokenVar: store.RedactedLaunchVar},
		"no forge wired in":   {store.ForgePublishTokenVar: store.RedactedLaunchVar, "pr_url": "https://github.com/o/r/pull/7"},
		"a pin is left as is": {store.ForgePublishTokenVar: "operator-pinned"},
	} {
		t.Run(name, func(t *testing.T) {
			pinned := vars[store.ForgePublishTokenVar]
			got, _, err := unwired.injectForgePublishVars(context.Background(), "t1", "", "review", vars, nil, store.RunTrustDefault)
			if err != nil {
				t.Fatal(err)
			}
			want := pinned
			if pinned == store.RedactedLaunchVar {
				want = ""
			}
			if got[store.ForgePublishTokenVar] != want {
				t.Fatalf("%s = %q, want %q", store.ForgePublishTokenVar, got[store.ForgePublishTokenVar], want)
			}
		})
	}
}
