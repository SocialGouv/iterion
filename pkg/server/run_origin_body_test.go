package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/assistantmission"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runwatch"
	"github.com/SocialGouv/iterion/pkg/server/projects"
	"github.com/SocialGouv/iterion/pkg/store"
)

// T6: a run a request carries in its BODY (not its path) is compared with
// the birth the body expects for it, right after the handler loads it. A
// stale expectation answers 409 run_origin_lost naming the run, and the
// store is untouched; no expectation behaves as before.

// staleBirth is a well-formed birth no fixture run holds — the birth of an
// incarnation the ID no longer names.
const staleBirth = "AAAAAAAAAAAAAAAAAAAAAAAAAA"

func gatePost(t *testing.T, url, body string) gateReply {
	t.Helper()
	resp, err := http.Post(url, "application/json", bytes.NewBufferString(body))
	mustWS(t, err)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := gateReply{status: resp.StatusCode, body: string(raw)}
	var env struct {
		Code  string `json:"error_code"`
		RunID string `json:"run_id"`
	}
	_ = json.Unmarshal(raw, &env)
	out.code, out.runID = env.Code, env.RunID
	return out
}

// The assistant context resolver: `run/<id>@<birth>` and
// `node/<id>@<birth>/<node>` compare the birth after the load; a replaced or
// deleted run resolves to run_origin_lost with nothing of the successor.
func TestAssistantContextReferenceCarriesItsBirth(t *testing.T) {
	fx := newGateFixture(t, true)
	ctx := t.Context()
	mustWS(t, seedWSOriginRun(ctx, fx.native, "r", "A", store.LegacyValueFormat, store.RunStatusFinished, 0))
	stale := birthOf(t, fx.native, "r")
	mustWS(t, replaceWSOriginRun(ctx, fx.native, "r", "B", store.LegacyValueFormat, store.RunStatusFinished, 0))
	current := birthOf(t, fx.native, "r")
	mustWS(t, seedWSOriginRun(ctx, fx.native, "gone", "G", store.LegacyValueFormat, store.RunStatusFinished, 0))
	gone := birthOf(t, fx.native, "gone")
	mustWS(t, fx.native.DeleteRun(ctx, "gone"))

	refs := []string{"run/r@" + stale, "node/r@" + stale + "/n1", "run/r@" + current, "run/r", "run/gone@" + gone, "run/r@not-a-birth"}
	body, _ := json.Marshal(map[string]any{"references": refs})
	got := gatePost(t, fx.hs.URL+"/api/v1/assistant/context/resolve", string(body))
	if got.status != http.StatusOK {
		t.Fatalf("resolve: %d %s", got.status, got.body)
	}
	var out assistantContextResolveResponse
	mustWS(t, json.Unmarshal([]byte(got.body), &out))
	byRef := map[string]assistantResolvedReference{}
	for _, ref := range out.References {
		byRef[ref.Reference] = ref
	}
	for _, ref := range []string{refs[0], refs[1], refs[4]} {
		r := byRef[ref]
		if r.Resolved || r.Reason != wsErrRunOriginLost || r.Run != nil {
			t.Fatalf("%s resolved %+v, want run_origin_lost and no run", ref, r)
		}
	}
	for _, ref := range []string{refs[2], refs[3]} {
		if r := byRef[ref]; !r.Resolved || r.Run == nil || r.Run.WorkflowName != "B" {
			t.Fatalf("%s resolved %+v, want the current run", ref, r)
		}
	}
	if r := byRef[refs[5]]; r.Resolved || r.Reason != "invalid_reference" {
		t.Fatalf("a malformed birth resolved %+v, want invalid_reference", r)
	}
}

// The assistant watch's assistant_run_birth.
func TestAssistantWatchCarriesTheAssistantBirth(t *testing.T) {
	srv, hs := newTestServer(t)
	seedRun(t, srv, "target", "target-workflow", store.RunStatusRunning)
	seedAssistantWatchChat(t, srv, "assistant", store.RunStatusPausedWaitingHuman)
	birth := birthOf(t, srv.runs.RunStore(), "assistant")
	got := gatePost(t, hs.URL+"/api/runs/target/assistant-watches", `{"assistant_run_id":"assistant","assistant_run_birth":"`+staleBirth+`","mode":"diagnose"}`)
	wantOriginLost(t, "stale assistant birth", got, "assistant")
	watches, err := srv.assistantWatches.ListActive(context.Background(), 100)
	mustWS(t, err)
	if len(watches) != 0 {
		t.Fatalf("a refused watch was created: %+v", watches)
	}
	if got := gatePost(t, hs.URL+"/api/runs/target/assistant-watches", `{"assistant_run_id":"assistant","assistant_run_birth":"`+birth+`","mode":"diagnose"}`); got.status != http.StatusOK {
		t.Fatalf("current assistant birth: %d %s", got.status, got.body)
	}
	if got := gatePost(t, hs.URL+"/api/runs/target/assistant-watches", `{"assistant_run_id":"assistant","assistant_run_birth":"x-y","mode":"diagnose"}`); got.status != http.StatusBadRequest || !strings.Contains(got.body, errCodeInvalidExpectedBirth) {
		t.Fatalf("malformed assistant birth: %d %s", got.status, got.body)
	}
}

// The assistant mission's assistant_run_birth.
func TestAssistantMissionCarriesTheAssistantBirth(t *testing.T) {
	srv, hs := newTestServer(t)
	seedRun(t, srv, "mission-target", "target", store.RunStatusFailedResumable)
	seedAssistantWatchChat(t, srv, "mission-assistant", store.RunStatusPausedWaitingHuman)
	birth := birthOf(t, srv.runs.RunStore(), "mission-assistant")
	now := time.Now().UTC()
	watch := runwatch.Watch{ID: "mission-watch", OwnerID: "local", TargetRunID: "mission-target", AssistantRunID: "mission-assistant", Mode: runwatch.ModePropose, State: runwatch.WatchActive, CreatedAt: now, UpdatedAt: now}
	mustWS(t, srv.assistantWatches.CreateWatch(context.Background(), watch))
	got := gatePost(t, hs.URL+"/api/runs/mission-target/assistant-missions", `{"invocation_key":"goal:a","watch_id":"mission-watch","assistant_run_birth":"`+staleBirth+`","ttl_seconds":600}`)
	wantOriginLost(t, "stale assistant birth", got, "mission-assistant")
	scope := assistantmission.Scope{OperatorID: "local", TargetRunID: "mission-target"}
	if missions, err := srv.assistantMissions.List(context.Background(), scope, 10); err != nil || len(missions) != 0 {
		t.Fatalf("a refused mission was created: %+v %v", missions, err)
	}
	if got := gatePost(t, hs.URL+"/api/runs/mission-target/assistant-missions", `{"invocation_key":"goal:b","watch_id":"mission-watch","assistant_run_birth":"`+birth+`","ttl_seconds":600}`); got.status != http.StatusOK {
		t.Fatalf("current assistant birth: %d %s", got.status, got.body)
	}
	if missions, err := srv.assistantMissions.List(context.Background(), scope, 10); err != nil || len(missions) != 1 {
		t.Fatalf("the admitted mission is not listed under the probe's scope: %+v %v", missions, err)
	}
}

// delegatedWorkerSource declares what a delegated launch requires of its
// worker (compileDelegatedWorker).
const delegatedWorkerSource = `
vars:
  failure_context: string = ""
  delegation_instructions: string = ""
workflow repair:
  worktree: auto
  entry: done
`

// The launch's source_run_birth: compared on the load the delegation's
// failure envelope is built from. A stale source is refused 409 before any
// run is created; the current birth reaches the delegation's own checks.
func TestDelegatedLaunchCarriesTheSourceBirth(t *testing.T) {
	srv, hs := newTestServer(t)
	seedRun(t, srv, "src", "broken", store.RunStatusRunning)
	birth := birthOf(t, srv.runs.RunStore(), "src")
	before, err := srv.runs.RunStore().ListRuns(context.Background())
	mustWS(t, err)
	launch := func(sourceBirth string) gateReply {
		body, _ := json.Marshal(map[string]any{"source": delegatedWorkerSource, "source_run_id": "src", "source_run_birth": sourceBirth, "instructions": "repair it"})
		return gatePost(t, hs.URL+"/api/runs", string(body))
	}
	wantOriginLost(t, "stale source birth", launch(staleBirth), "src")
	after, err := srv.runs.RunStore().ListRuns(context.Background())
	mustWS(t, err)
	if len(after) != len(before) {
		t.Fatalf("a refused launch created runs: %v → %v", before, after)
	}
	if got := launch(birth); got.status != http.StatusUnprocessableEntity || !strings.Contains(got.body, "source run must be failed") {
		t.Fatalf("current source birth: %d %s, want the delegation's own refusal of a running source", got.status, got.body)
	}
}

// The workspace handoff: source_run_birth on create, destination_run_birth on
// bind and complete — served by WorkspaceHost, outside the Server mux.
func TestWorkspaceHandoffCarriesRunBirths(t *testing.T) {
	ctx := context.Background()
	registry := &projects.Config{
		Version: 1,
		RecentProjects: []projects.Project{
			{ID: "source", Name: "Source", Dir: t.TempDir(), StoreDir: t.TempDir()},
			{ID: "target", Name: "Target", Dir: t.TempDir(), StoreDir: t.TempDir()},
		},
		CurrentProjectID: "source",
	}
	host, err := NewWorkspaceHost(registry, func(project projects.Project) (*Server, error) {
		return New(Config{WorkDir: project.Dir, StoreDir: project.StoreDir, DisableAuth: true, SkipProjectRegistration: true, RecoveryPassive: true}, iterlog.Nop()), nil
	}, iterlog.Nop())
	mustWS(t, err)
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = host.Shutdown(shutdownCtx)
	})
	seed := func(projectID, runID, clientID, conversationID string) string {
		rs := host.runtimes[projectID].server.runs.RunStore()
		run, err := rs.CreateRun(ctx, runID, "copilot", nil)
		mustWS(t, err)
		run.Source = &store.RunSource{Kind: store.RunSourceKindStudioChat, ClientID: clientID, ConversationID: conversationID}
		mustWS(t, rs.SaveRun(ctx, run))
		return birthOf(t, rs, runID)
	}
	sourceBirth := seed("source", "source-run", "source-client", "source-conversation")
	targetBirth := seed("target", "target-run", "target-client", "target-conversation")
	request := func(path, body string) gateReply {
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
		req.Header.Set("Origin", "http://iterion.test")
		req.Host = "iterion.test"
		rec := httptest.NewRecorder()
		host.ServeHTTP(rec, req)
		out := gateReply{status: rec.Code, body: rec.Body.String()}
		var env struct {
			Code  string `json:"error_code"`
			RunID string `json:"run_id"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &env)
		out.code, out.runID = env.Code, env.RunID
		return out
	}
	wantOriginLost(t, "create with a stale source", request("/x/source/api/workspace/handoffs",
		`{"destination_project":"target","summary":"s","source_run_id":"source-run","source_run_birth":"`+staleBirth+`"}`), "source-run")
	if len(host.handoffs) != 0 {
		t.Fatalf("a refused create recorded a handoff: %+v", host.handoffs)
	}
	created := request("/x/source/api/workspace/handoffs",
		`{"destination_project":"target","summary":"s","source_run_id":"source-run","source_run_birth":"`+sourceBirth+`"}`)
	var creation struct {
		Ticket    string `json:"ticket"`
		HandoffID string `json:"handoff_id"`
	}
	if created.status != http.StatusOK || json.Unmarshal([]byte(created.body), &creation) != nil {
		t.Fatalf("create: %d %s", created.status, created.body)
	}
	if got := request("/x/target/api/workspace/handoffs/redeem",
		`{"ticket":"`+creation.Ticket+`","destination_client_id":"target-client","destination_conversation_id":"target-conversation"}`); got.status != http.StatusOK {
		t.Fatalf("redeem: %d %s", got.status, got.body)
	}
	wantOriginLost(t, "bind with a stale destination", request("/x/target/api/workspace/handoffs/bind",
		`{"handoff_id":"`+creation.HandoffID+`","destination_run_id":"target-run","destination_run_birth":"`+staleBirth+`"}`), "target-run")
	if bound := host.handoffs[creation.HandoffID].DestinationRunID; bound != "" {
		t.Fatalf("a refused bind bound %q", bound)
	}
	wantOriginLost(t, "complete with a stale destination", request("/x/target/api/workspace/handoffs/complete",
		`{"handoff_id":"`+creation.HandoffID+`","destination_run_id":"target-run","destination_run_birth":"`+staleBirth+`","status":"completed","summary":"done"}`), "target-run")
	if outcome := host.handoffs[creation.HandoffID].Outcome; outcome != nil {
		t.Fatalf("a refused complete recorded %+v", outcome)
	}
	if got := request("/x/target/api/workspace/handoffs/bind",
		`{"handoff_id":"`+creation.HandoffID+`","destination_run_id":"target-run","destination_run_birth":"`+targetBirth+`"}`); got.status != http.StatusOK {
		t.Fatalf("bind with the current destination: %d %s", got.status, got.body)
	}
	if got := request("/x/target/api/workspace/handoffs/complete",
		`{"handoff_id":"`+creation.HandoffID+`","destination_run_id":"target-run","destination_run_birth":"`+targetBirth+`","status":"completed","summary":"done"}`); got.status != http.StatusOK {
		t.Fatalf("complete with the current destination: %d %s", got.status, got.body)
	}
}
