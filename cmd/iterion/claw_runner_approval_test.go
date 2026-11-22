package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/permission"
)

// The approval channel (#3513): the launcher → runner pre-task envelope
// that relaxes C136 for launcher-placed tools, and its fail-closed
// position. These are the POSITION tests: protocol-level witnesses for
// the announcement and its absence, written against the bootstrap loop.
// The ask-then-block round trip has its own file, later in this series.

// TestClawRunner_ApprovalChannelAcceptedPreTask: a well-formed v1
// announcement must NOT fatal the bootstrap — the runner proceeds to the
// model phase (which fails on the unresolvable registry here, an error
// that names the model, not the announcement).
func TestClawRunner_ApprovalChannelAcceptedPreTask(t *testing.T) {
	env, err := delegate.NewApprovalChannelEnvelope()
	if err != nil {
		t.Fatalf("build envelope: %v", err)
	}
	res := driveClawRunner(t, []delegate.Envelope{env}, delegate.IOTask{NodeID: "x", Model: "bogus-provider/nope"})
	if res.Error == "" {
		t.Fatal("expected a model-resolution error in this credential-less test env")
	}
	if strings.Contains(res.Error, "approval") {
		t.Fatalf("valid announcement was blamed by the bootstrap: %q", res.Error)
	}
}

// TestClawRunner_AskPolicyWithoutApprovalChannelFatals is the runner-side
// twin of the fail-closed position (the gateway-twin for the approval
// channel): an ask-capable policy arriving WITHOUT the announcement means
// the launcher will not serve pauses — a host that predates the channel.
// The bootstrap must fail here, before any model call, and name the skew
// instead of degrading every ask to a denial (the silent downgrade C136
// refuses).
func TestClawRunner_AskPolicyWithoutApprovalChannelFatals(t *testing.T) {
	cfg := permission.PolicyConfig{Mode: "deny", Ask: []string{"WebFetch(*)"}}
	polEnv, err := delegate.NewPermissionPolicyEnvelope(cfg)
	if err != nil {
		t.Fatal(err)
	}
	res := driveClawRunner(t, []delegate.Envelope{polEnv}, delegate.IOTask{NodeID: "x", Model: "bogus-provider/nope"})
	if res.Error == "" || !strings.Contains(res.Error, "approval channel") {
		t.Fatalf("expected the approval-channel skew fatal, got %q", res.Error)
	}
}

// TestClawRunner_AskPolicyWithApprovalChannelIsAccepted: with the
// announcement, an ask-capable policy passes the bootstrap — the run
// proceeds to the model phase (model-resolution error here, not the
// skew fatal).
func TestClawRunner_AskPolicyWithApprovalChannelIsAccepted(t *testing.T) {
	env, err := delegate.NewApprovalChannelEnvelope()
	if err != nil {
		t.Fatal(err)
	}
	cfg := permission.PolicyConfig{Mode: "deny", Ask: []string{"WebFetch(*)"}}
	polEnv, perr := delegate.NewPermissionPolicyEnvelope(cfg)
	if perr != nil {
		t.Fatal(perr)
	}
	res := driveClawRunner(t, []delegate.Envelope{env, polEnv}, delegate.IOTask{NodeID: "x", Model: "bogus-provider/nope"})
	if res.Error == "" {
		t.Fatal("expected a model-resolution error in this credential-less test env")
	}
	for _, blamed := range []string{"approval channel", "unexpected envelope"} {
		if strings.Contains(res.Error, blamed) {
			t.Fatalf("announcement+ask policy was blamed by the bootstrap: %q", res.Error)
		}
	}
}

// TestClawRunner_UndecodableApprovalChannelFatals: garbage bytes in the
// announcement payload are refused the same way the policy envelope's
// garbage is — a fatal before any model call.
func TestClawRunner_UndecodableApprovalChannelFatals(t *testing.T) {
	env := delegate.Envelope{
		Type: delegate.EnvelopeApprovalChannel,
		Data: json.RawMessage(`"not-an-object"`),
	}
	res := driveClawRunner(t, []delegate.Envelope{env}, delegate.IOTask{NodeID: "x", Model: "bogus-provider/nope"})
	if res.Error == "" || !strings.Contains(res.Error, "approval_channel") {
		t.Fatalf("expected a decode fatal, got %q", res.Error)
	}
}

// TestClawRunner_UnknownApprovalChannelVersionFatals: the version rides
// the payload so a future revision fails closed on the version check.
func TestClawRunner_UnknownApprovalChannelVersionFatals(t *testing.T) {
	env := delegate.Envelope{
		Type: delegate.EnvelopeApprovalChannel,
		Data: json.RawMessage(`{"version":99}`),
	}
	res := driveClawRunner(t, []delegate.Envelope{env}, delegate.IOTask{NodeID: "x", Model: "bogus-provider/nope"})
	if res.Error == "" || !strings.Contains(res.Error, "version") {
		t.Fatalf("expected a version-skew fatal, got %q", res.Error)
	}
}

// The dispatcher fail-closed behavior while an approval ask blocks.

func newTestDispatcher() *proxyDispatcher {
	stdinR, _ := io.Pipe()
	_, stdoutW := io.Pipe()
	return newProxyDispatcher(stdinR, stdoutW)
}

// TestProxyDispatcher_RouteRefusesPendingApprovalOnUnknownTraffic: while
// an approval ask blocks, mid-stream traffic naming no pending call is a
// protocol breach - THIS ask is refused, never silently ignored.
func TestProxyDispatcher_RouteRefusesPendingApprovalOnUnknownTraffic(t *testing.T) {
	d := newTestDispatcher()
	ch := d.beginApprovalAsk("approval-1")
	if ch == nil {
		t.Fatal("register the ask")
	}
	d.route(delegate.Envelope{Type: delegate.EnvelopeEvent})
	select {
	case env := <-ch:
		if env.Type == delegate.EnvelopePermissionDecision {
			t.Fatal("a refusal injection must not look like a decision")
		}
		if env.Type != delegate.EnvelopeEvent {
			t.Fatalf("the injected refusal forwards the real envelope, got %v", env.Type)
		}
	default:
		t.Fatal("the pending ask must have been refused")
	}
}

// TestProxyDispatcher_RouteDropsStaleRepliesWhileApprovalPending: a
// correlated reply to an already-consumed call is ordinary mid-stream
// traffic - dropped, the pending ask keeps blocking.
func TestProxyDispatcher_RouteDropsStaleRepliesWhileApprovalPending(t *testing.T) {
	d := newTestDispatcher()
	ch := d.beginApprovalAsk("approval-1")
	defer d.endApprovalAsk("approval-1")
	d.route(delegate.Envelope{Type: delegate.EnvelopeToolResult, ID: "gone"})
	select {
	case env := <-ch:
		t.Fatalf("a stale reply must not unblock the ask: %v", env.Type)
	default:
	}
}

// TestProxyDispatcher_RouteDeliversTheCorrelatedDecision: the launcher
// decision arrives with the ask ID and must reach the blocking round
// trip - the ask channel is not in pending, so route() delivers it on
// its own branch.
func TestProxyDispatcher_RouteDeliversTheCorrelatedDecision(t *testing.T) {
	d := newTestDispatcher()
	ch := d.beginApprovalAsk("approval-1")
	if ch == nil {
		t.Fatal("register the ask")
	}
	denv, err := delegate.NewPermissionDecisionEnvelope("approval-1", delegate.PermissionDecisionData{Decision: delegate.PermissionDecisionAllow, Output: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	d.route(denv)
	select {
	case got := <-ch:
		if got.Type != delegate.EnvelopePermissionDecision {
			t.Fatalf("decision type = %q", got.Type)
		}
		var data delegate.PermissionDecisionData
		if err := json.Unmarshal(got.Data, &data); err != nil || data.Output != "ok" {
			t.Fatalf("decision payload = %s (%v)", got.Data, err)
		}
	default:
		t.Fatal("the correlated decision must unblock the ask")
	}
}

// TestProxyDispatcher_FailAllPendingUnblocksApprovalAsk: a dying channel
// unblocks the gate mid-ask with the reason.
func TestProxyDispatcher_FailAllPendingUnblocksApprovalAsk(t *testing.T) {
	d := newTestDispatcher()
	ch := d.beginApprovalAsk("approval-1")
	d.failAllPending(errors.New("channel died"))
	select {
	case env := <-ch:
		var res delegate.IOResult
		if json.Unmarshal(env.Data, &res) != nil || !strings.Contains(res.Error, "channel died") {
			t.Fatalf("synthetic refusal must carry the reason: %s", env.Data)
		}
	default:
		t.Fatal("the approval ask must unblock on channel death")
	}
}

// TestProxyDispatcher_BeginApprovalAskRefusesInterleave: a second ask
// while one blocks is a runner-side protocol bug and must be refused,
// not interleaved.
func TestProxyDispatcher_BeginApprovalAskRefusesInterleave(t *testing.T) {
	d := newTestDispatcher()
	if d.beginApprovalAsk("approval-1") == nil {
		t.Fatal("first ask registers")
	}
	defer d.endApprovalAsk("approval-1")
	if d.beginApprovalAsk("approval-2") != nil {
		t.Fatal("a second pending ask must be refused")
	}
}

// TestProxyDispatcher_EndApprovalAskAllowsReRegistration: end clears
// only its own id, and a cleared ask can be followed by a new one.
func TestProxyDispatcher_EndApprovalAskAllowsReRegistration(t *testing.T) {
	d := newTestDispatcher()
	d.beginApprovalAsk("approval-1")
	d.endApprovalAsk("approval-1")
	if ch := d.beginApprovalAsk("approval-2"); ch == nil {
		t.Fatal("after end, a new ask registers")
	}
	d.endApprovalAsk("approval-2")
}

// TestClawRunner_ApprovalChannelAllowCarriesTheLauncherResult is the
// runner-side integration round trip: the gate Asks, the peer answers
// allow with the REAL result, the runner appends it to the transcript
// (the next LLM request carries it) and finishes the run without
// executing anything itself.
func TestClawRunner_ApprovalChannelAllowCarriesTheLauncherResult(t *testing.T) {
	var reqMu sync.Mutex
	var bodies []string
	firstToolCallChunk, mErr := json.Marshal(map[string]any{
		"choices": []map[string]any{
			{
				"index": 0,
				"delta": map[string]any{
					"role": "assistant",
					"tool_calls": []map[string]any{{
						"index": 0, "id": "call_1", "type": "function",
						"function": map[string]any{"name": "bash", "arguments": `{"command":"pwd"}`},
					}},
				},
				"finish_reason": "tool_calls",
			},
		},
		"usage": map[string]any{"prompt_tokens": 1200, "completion_tokens": 300},
	})
	if mErr != nil {
		t.Fatal(mErr)
	}
	doneChunk, mErr := json.Marshal(map[string]any{
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant", "content": "done"}, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": 1500, "completion_tokens": 20},
	})
	if mErr != nil {
		t.Fatal(mErr)
	}
	turn := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		reqMu.Lock()
		bodies = append(bodies, string(body))
		chunk := doneChunk
		if turn == 0 {
			chunk = firstToolCallChunk
		}
		turn++
		reqMu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: "+string(chunk)+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OPENAI_API_KEY", "sk-test")
	t.Setenv("OPENAI_BASE_URL", srv.URL)
	t.Setenv("ITERION_OPENAI_USE_OAUTH", "")

	approvalEnv, aErr := delegate.NewApprovalChannelEnvelope()
	if aErr != nil {
		t.Fatal(aErr)
	}
	polEnv, pErr := delegate.NewPermissionPolicyEnvelope(permission.PolicyConfig{Mode: "ask"})
	if pErr != nil {
		t.Fatal(pErr)
	}
	task := delegate.IOTask{
		NodeID: "copi", Model: "openai/gpt-5", UserPrompt: "hello",
		HasTools: true, ToolMaxSteps: 5,
		ToolDefs: []delegate.IOToolDef{{Name: "bash", Description: "shell",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}}}`)}},
	}

	// Drive the runner with a peer that answers every permission_ask with
	// an allow carrying the launcher result.
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	var runWG sync.WaitGroup
	runWG.Add(1)
	var runErr error
	go func() {
		defer runWG.Done()
		runErr = runClawRunner(context.Background(), stdinR, stdoutW, io.Discard)
		_ = stdoutW.Close()
	}()
	writer := delegate.NewEnvelopeWriter(stdinW)
	go func() {
		for _, env := range []delegate.Envelope{approvalEnv, polEnv} {
			if werr := writer.Write(env); werr != nil {
				return
			}
		}
		taskEnv, terr := delegate.NewTaskEnvelope(task)
		if terr == nil {
			_ = writer.Write(taskEnv)
		}
	}()
	type askRecord struct {
		id  string
		ask delegate.PermissionAskData
	}
	var (
		asks    []askRecord
		res     delegate.IOResult
		haveRes bool
		readErr error
		resMu   sync.Mutex
	)
	reader := delegate.NewEnvelopeReader(stdoutR)
	for {
		env, rerr := reader.Read()
		if rerr != nil {
			readErr = rerr
			break
		}
		if env.Type == delegate.EnvelopePermissionAsk {
			var ask delegate.PermissionAskData
			if uerr := json.Unmarshal(env.Data, &ask); uerr != nil {
				t.Fatalf("decode permission_ask: %v", uerr)
			}
			resMu.Lock()
			asks = append(asks, askRecord{id: env.ID, ask: ask})
			resMu.Unlock()
			decision, derr := delegate.NewPermissionDecisionEnvelope(env.ID, delegate.PermissionDecisionData{
				Decision: delegate.PermissionDecisionAllow, Output: "approved-pwd",
			})
			if derr != nil {
				t.Fatalf("build decision: %v", derr)
			}
			if werr := writer.Write(decision); werr != nil {
				t.Fatalf("send decision: %v", werr)
			}
			continue
		}
		if env.Type == delegate.EnvelopeResult {
			if uerr := json.Unmarshal(env.Data, &res); uerr != nil {
				t.Fatalf("decode result: %v", uerr)
			}
			haveRes = true
			break
		}
	}
	_ = stdinW.Close()
	runWG.Wait()
	if !haveRes {
		t.Fatalf("no terminal result (read error %v, run error %v)", readErr, runErr)
	}
	if res.Error != "" {
		t.Fatalf("the run must finish clean through the approved call: %q", res.Error)
	}
	if len(asks) != 1 {
		t.Fatalf("want exactly one permission ask, got %d", len(asks))
	}
	if asks[0].ask.ToolName != "bash" || asks[0].ask.Placement != delegate.PermissionPlacementSandbox {
		t.Fatalf("ask binding = %+v", asks[0].ask)
	}
	if string(asks[0].ask.Input) != `{"command":"pwd"}` {
		t.Fatalf("ask input = %s, want the exact canonical bytes", asks[0].ask.Input)
	}
	reqMu.Lock()
	defer reqMu.Unlock()
	if len(bodies) < 2 {
		t.Fatalf("want two LLM requests, got %d", len(bodies))
	}
	if !strings.Contains(bodies[1], "approved-pwd") {
		t.Fatalf("the second LLM request must carry the launcher result as the tool_result: %.400s", bodies[1])
	}
}
