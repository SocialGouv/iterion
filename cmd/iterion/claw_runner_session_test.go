package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
)

// The launcher's session_replay snapshot has to reach the model: it is the
// history CompactAndRetry shrank for this attempt, and the whole reason the
// store exists. The runner seeds its local store under the task's session
// key — the explicit session_slot when the node declares one, the node id
// otherwise — because that is the key the in-container backend reads before
// its first call. Seeded under the bare node id, a slot node's replay is
// unreachable and the model is asked again with none of it.
func TestClawRunner_SeededReplayReachesTheModel(t *testing.T) {
	for _, tc := range []struct {
		name string
		slot string
	}{
		{name: "an explicit session slot", slot: "assistant_conversation"},
		{name: "no slot: the node id is the key", slot: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var bodies []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				mu.Lock()
				bodies = append(bodies, string(body))
				mu.Unlock()
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
			}))
			t.Cleanup(srv.Close)
			t.Setenv("OPENAI_API_KEY", "sk-test")
			t.Setenv("OPENAI_BASE_URL", srv.URL)
			t.Setenv("ITERION_OPENAI_USE_OAUTH", "")

			snapshot, err := json.Marshal([]api.Message{{
				Role:    "user",
				Content: []api.ContentBlock{{Type: "text", Text: "replayed-earlier-turn"}},
			}})
			if err != nil {
				t.Fatalf("marshal the launcher's snapshot: %v", err)
			}

			res := driveClawRunner(t,
				[]delegate.Envelope{delegate.NewSessionReplayEnvelope(snapshot)},
				delegate.IOTask{NodeID: "copi", SessionSlot: tc.slot, Model: "openai/gpt-test", UserPrompt: "hello"})
			if res.Error != "" {
				t.Fatalf("the runner ended on %q, want the answer", res.Error)
			}

			mu.Lock()
			defer mu.Unlock()
			if len(bodies) != 1 {
				t.Fatalf("the provider saw %d requests, want exactly 1", len(bodies))
			}
			if !strings.Contains(bodies[0], "replayed-earlier-turn") {
				t.Fatalf("the model's request carries none of the replayed history — the launcher's snapshot was seeded under a key the in-container backend does not read (request: %s)", bodies[0])
			}
		})
	}
}

// The launcher believes a crossing it seeded has that conversation. A replay
// this runner cannot decode breaks that belief, and the model answers from
// nothing — on a crossing that SUCCEEDS, where the launcher reads this
// process's stderr nowhere. So the gap crosses as a session_mirror_gap event,
// which the launcher reports whatever the outcome.
func TestClawRunner_AnUndecodableReplayRelaysTheGap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OPENAI_API_KEY", "sk-test")
	t.Setenv("OPENAI_BASE_URL", srv.URL)
	t.Setenv("ITERION_OPENAI_USE_OAUTH", "")

	// A snapshot the launcher's own bound accepts (one NDJSON line, in
	// budget) that is not a conversation: what a version skew or a corrupt
	// store entry puts on the wire.
	snapshot := []byte(`{"not":"a conversation"}`)
	res, before, stderr := driveClawRunnerCollecting(t,
		[]delegate.Envelope{delegate.NewSessionReplayEnvelope(snapshot)},
		delegate.IOTask{NodeID: "copi", SessionSlot: "assistant_conversation", Model: "openai/gpt-test", UserPrompt: "hello"})
	if res.Error != "" {
		t.Fatalf("the runner ended on %q — an undecodable replay must not fail the node", res.Error)
	}
	if !strings.Contains(stderr, "decode session_replay") {
		t.Errorf("the container's own log does not name the gap: %q", stderr)
	}
	var relayed *delegate.EventData
	for _, env := range before {
		if env.Type != delegate.EnvelopeEvent {
			continue
		}
		var ev delegate.EventData
		if err := json.Unmarshal(env.Data, &ev); err != nil {
			t.Fatalf("decode a relayed event: %v", err)
		}
		if ev.Type == model.SessionMirrorGapEvent {
			relayed = &ev
			break
		}
	}
	if relayed == nil {
		t.Fatalf("no %s event crossed: the launcher is left believing this crossing replayed the conversation it sent (stderr: %q)",
			model.SessionMirrorGapEvent, stderr)
	}
	if relayed.Payload["reason"] != model.MirrorGapReplayUndecodable {
		t.Errorf("the gap's reason is %v, want %q", relayed.Payload["reason"], model.MirrorGapReplayUndecodable)
	}
	if got, want := relayed.Payload["bytes"], float64(len(snapshot)); got != want {
		t.Errorf("the gap is reported over %v bytes, want the %v replay it could not read", got, want)
	}
}
